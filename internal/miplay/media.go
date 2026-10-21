package miplay

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
)

// Wire media framing constants.
const (
	// MediaMagic is the first byte of every media frame (same 0x24 marker).
	MediaMagic = 0x24
	// MediaHeaderSize is the media frame header: magic (1) + length (3).
	MediaHeaderSize = 4
	// MaxMediaPayload bounds one media frame.
	MaxMediaPayload = 0xFFFFFF

	rtpHeaderSize        = 12
	rtpPayloadTypeMPEGTS = 33
	mpegtsPacketSize     = 188
	maxTSPacketsPerRTP   = 7
)

// ALSA output shaping: a large ring with short periods, plus a silence
// pre-roll. 48kHz stereo S16 is 192000 bytes per second.
const (
	alsaPeriodUS = 40_000
	// alsaStartUS is the playback start threshold. The default (a whole ring,
	// ~341ms on an ES1371) delays sound after an xrun by the same amount, but
	// one period reliably triggers a startup xrun on that card; 100ms stays
	// clean while keeping post-stall recovery short.
	alsaStartUS        = 100_000
	alsaBytesPerSecond = 48_000 * 2 * 2
	// alsaPipeSlack keeps room in the aplay stdin pipe beyond the pre-roll so
	// an incoming burst can queue before the pump blocks.
	alsaPipeSlack = 64 * 1024
	alsaPipeMax   = 1 << 20
)

// Exported aliases for callers that build test streams or source simulators.
const (
	RTPHeaderSize        = rtpHeaderSize
	RTPPayloadTypeMPEGTS = rtpPayloadTypeMPEGTS
	MPEGTSPacketSize     = mpegtsPacketSize
	MaxTSPacketsPerRTP   = maxTSPacketsPerRTP
)

// PCMReadSize is one 20 ms block of 48 kHz stereo s16le PCM, matching the
// reference decoder's read size.
const PCMReadSize = 48_000 * 2 * 2 / 50

// FFmpegLowLatencyInputArgs keeps ffmpeg from spending seconds probing a
// realtime MPEG-TS stream before the first decoded sample. MiPlay always
// delivers a validated MPEG-TS/AAC stream, so the reference implementation's
// small probe is sufficient.
var FFmpegLowLatencyInputArgs = []string{
	"-flags", "low_delay",
	"-probesize", "4096",
	"-analyzeduration", "0",
}

// MediaFrameDecoder incrementally splits the media TCP stream into payloads
// framed as '0x24' + three-byte big-endian length.
type MediaFrameDecoder struct {
	buffer []byte
}

// NewMediaFrameDecoder returns an empty decoder.
func NewMediaFrameDecoder() *MediaFrameDecoder {
	return &MediaFrameDecoder{buffer: make([]byte, 0, 64*1024)}
}

// Feed appends data and returns every complete media frame.
func (d *MediaFrameDecoder) Feed(data []byte) ([][]byte, error) {
	d.buffer = append(d.buffer, data...)
	frames := [][]byte{}
	for len(d.buffer) >= MediaHeaderSize {
		if source := d.buffer[0]; source != MediaMagic {
			d.buffer = d.buffer[:0]
			return nil, protocolErrorf("invalid media-frame magic 0x%02x", source)
		}
		length := int(d.buffer[1])<<16 | int(d.buffer[2])<<8 | int(d.buffer[3])
		if length == 0 || length > MaxMediaPayload {
			d.buffer = d.buffer[:0]
			return nil, protocolErrorf("media payload of %d bytes exceeds the safety limit", length)
		}
		frameLength := MediaHeaderSize + length
		if len(d.buffer) < frameLength {
			break
		}
		frame := make([]byte, length)
		copy(frame, d.buffer[MediaHeaderSize:frameLength])
		d.buffer = append(d.buffer[:0], d.buffer[frameLength:]...)
		frames = append(frames, frame)
	}
	return frames, nil
}

// EncodeMediaFrame serialises one wire media frame: '0x24' plus the payload
// length as a three-byte big-endian integer.
func EncodeMediaFrame(payload []byte) ([]byte, error) {
	if len(payload) == 0 || len(payload) > MaxMediaPayload {
		return nil, protocolErrorf("media payload length out of range")
	}
	frame := make([]byte, MediaHeaderSize+len(payload))
	frame[0] = MediaMagic
	frame[1] = byte(len(payload) >> 16)
	frame[2] = byte(len(payload) >> 8)
	frame[3] = byte(len(payload))
	copy(frame[MediaHeaderSize:], payload)
	return frame, nil
}

// EncodeRTPMPEGTS serialises one RTP packet carrying MPEG-TS. The payload
// must be a whole number of 188-byte packets, with no more than the captured
// seven-packet limit.
func EncodeRTPMPEGTS(sequence uint16, timestamp, ssrc uint32, transportStream []byte, marker bool) ([]byte, error) {
	if err := validateTransportStream(transportStream); err != nil {
		return nil, err
	}
	if len(transportStream) > mpegtsPacketSize*maxTSPacketsPerRTP {
		return nil, protocolErrorf("RTP MPEG-TS payload exceeds the captured packet limit")
	}
	packet := make([]byte, rtpHeaderSize+len(transportStream))
	packet[0] = 0x80
	packet[1] = rtpPayloadTypeMPEGTS
	if marker {
		packet[1] |= 0x80
	}
	binary.BigEndian.PutUint16(packet[2:4], sequence)
	binary.BigEndian.PutUint32(packet[4:8], timestamp)
	binary.BigEndian.PutUint32(packet[8:12], ssrc)
	copy(packet[rtpHeaderSize:], transportStream)
	return packet, nil
}

// RTPPacket is one validated RTP packet carrying MPEG-TS.
type RTPPacket struct {
	Sequence        uint16
	Timestamp       uint32
	SSRC            uint32
	Marker          bool
	TransportStream []byte
}

// DecodeRTPMPEGTS validates and decodes an RTP packet whose payload must be a
// whole number of 188-byte MPEG-TS packets with intact sync bytes.
func DecodeRTPMPEGTS(data []byte) (RTPPacket, error) {
	if len(data) < rtpHeaderSize+mpegtsPacketSize {
		return RTPPacket{}, protocolErrorf("RTP packet is truncated")
	}
	if data[0] != 0x80 {
		return RTPPacket{}, protocolErrorf("unsupported RTP header flags 0x%02x", data[0])
	}
	payloadType := data[1] & 0x7F
	if payloadType != rtpPayloadTypeMPEGTS {
		return RTPPacket{}, protocolErrorf("unsupported RTP payload type %d", payloadType)
	}
	transportStream := data[rtpHeaderSize:]
	if err := validateTransportStream(transportStream); err != nil {
		return RTPPacket{}, err
	}
	sequence := binary.BigEndian.Uint16(data[2:4])
	timestamp := binary.BigEndian.Uint32(data[4:8])
	ssrc := binary.BigEndian.Uint32(data[8:12])
	return RTPPacket{
		Sequence:        sequence,
		Timestamp:       timestamp,
		SSRC:            ssrc,
		Marker:          data[1]&0x80 != 0,
		TransportStream: transportStream,
	}, nil
}

func validateTransportStream(payload []byte) error {
	if len(payload) == 0 || len(payload)%mpegtsPacketSize != 0 {
		return protocolErrorf("MPEG-TS payload must contain complete 188-byte packets")
	}
	for offset := 0; offset < len(payload); offset += mpegtsPacketSize {
		if payload[offset] != 0x47 {
			return protocolErrorf("MPEG-TS sync byte is missing at offset %d", offset)
		}
	}
	return nil
}

// PipelineStats is the realtime pipeline snapshot for /api/status.
type PipelineStats struct {
	Running      bool      `json:"running"`
	Frames       int64     `json:"frames"`
	Bytes        int64     `json:"bytes"`
	FirstFrameAt time.Time `json:"first_frame_at,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	FFmpegAlive  bool      `json:"ffmpeg_alive"`
	AplayAlive   bool      `json:"aplay_alive"`
	// Underruns counts the aplay xrun reports of the current pipeline run.
	// A non-zero value means the ALSA ring drained despite live media.
	Underruns int64 `json:"underruns"`
	// Gain is the software gain percentage currently applied between ffmpeg
	// and aplay. It is independent of the ALSA hardware volume.
	Gain int `json:"gain"`
}

// lineRing keeps the last log lines of a child process.
type lineRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func newLineRing(max int) *lineRing { return &lineRing{max: max, lines: make([]string, 0, max)} }

func (r *lineRing) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

func (r *lineRing) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, " | ")
}

// logPipe forwards a child process stream into the ring buffer and the
// container log.
type logPipe struct {
	ring      *lineRing
	logger    *slog.Logger
	prefix    string
	underruns *atomic.Int64
	buffer    []byte
}

func (p *logPipe) Write(data []byte) (int, error) {
	p.buffer = append(p.buffer, data...)
	for {
		index := bytes.IndexByte(p.buffer, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimRight(string(p.buffer[:index]), "\r")
		p.buffer = p.buffer[index+1:]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if p.underruns != nil && strings.Contains(strings.ToLower(line), "underrun") {
			p.underruns.Add(1)
		}
		p.ring.add(line)
		p.logger.Warn(p.prefix, "line", line)
	}
	if len(p.buffer) > 8*1024 {
		p.buffer = p.buffer[len(p.buffer)-4096:]
	}
	return len(data), nil
}

// Pipeline is the realtime playback path of MiPlay audio:
//
//	media frames -> ffmpeg (MPEG-TS/AAC -> s16le) -> software gain -> aplay (exclusive hw)
//
// ffmpeg runs with low-latency input flags; aplay writes to the exclusive
// hardware with short periods. MIPLAY_PREBUFFER_US establishes the playback
// lead (ring plus an enlarged aplay stdin pipe), which is what carries
// playback across Wi-Fi delivery stalls; MIPLAY_ALSA_BUFFER_US only caps the
// ALSA ring. The gain stage lives in this process so the protocol volume
// never touches an ALSA mixer control: the hardware volume remains the
// console's exclusive domain.
type Pipeline struct {
	cfg    config.Config
	logger *slog.Logger

	mu sync.Mutex
	// procMu serialises process generations: Start waits for a previous Stop
	// to finish draining, so two aplay processes never contend for the
	// exclusive sound card.
	procMu sync.Mutex
	ffmpeg *exec.Cmd
	aplay  *exec.Cmd
	stdin  io.WriteCloser

	// gain is the current software gain percentage (0-100), applied to every
	// decoded PCM chunk. Atomic so a slider move takes effect on the next
	// chunk without restarting ffmpeg or aplay.
	gain atomic.Int32

	// underruns counts aplay xrun reports for the current run; the status API
	// reports it so silence caused by a drained ALSA ring is visible without
	// scraping container logs.
	underruns atomic.Int64

	stats    PipelineStats
	ffmpegEP *lineRing
	aplayEP  *lineRing
}

// NewPipeline creates the pipeline for the configured ALSA device. The gain
// starts at BRIDGE_VOLUME_DEFAULT, the level reported to the phone before it
// changes anything.
func NewPipeline(cfg config.Config, logger *slog.Logger) *Pipeline {
	if logger == nil {
		logger = slog.Default()
	}
	pipeline := &Pipeline{
		cfg:      cfg,
		logger:   logger,
		ffmpegEP: newLineRing(30),
		aplayEP:  newLineRing(30),
	}
	pipeline.gain.Store(int32(clampGain(cfg.VolumeDefault)))
	return pipeline
}

// SetGain updates the software gain percentage. The next decoded chunk uses
// it; no process restart and no ALSA control write.
func (p *Pipeline) SetGain(percent int) {
	percent = clampGain(percent)
	p.gain.Store(int32(percent))
	p.logger.Debug("miplay software gain updated", "gain", percent)
}

func (p *Pipeline) currentGain() int {
	return int(p.gain.Load())
}

func clampGain(percent int) int {
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
}

// applyGainS16LE scales interleaved little-endian S16 samples in place. Gain
// 100 is a byte-identical pass-through (bit-perfect at unity); lower values
// attenuate each sample. The caller always passes a whole number of 4-byte
// stereo frames.
func applyGainS16LE(data []byte, percent int) {
	if percent >= 100 || len(data) == 0 {
		return
	}
	if percent < 0 {
		percent = 0
	}
	scale := int32(percent)
	for offset := 0; offset+1 < len(data); offset += 2 {
		sample := int32(int16(binary.LittleEndian.Uint16(data[offset:])))
		scaled := sample * scale / 100
		binary.LittleEndian.PutUint16(data[offset:], uint16(int16(scaled)))
	}
}

// pump copies ffmpeg's decoded PCM into aplay, applying the live gain. Reads
// are whole frames apart from a possible trailing partial frame, which is
// carried to the next chunk.
func (p *Pipeline) pump(reader io.Reader, writer io.WriteCloser) {
	defer writer.Close()
	buffer := make([]byte, PCMReadSize)
	var carry []byte
	for {
		read, err := reader.Read(buffer)
		if read > 0 {
			chunk := buffer[:read]
			if len(carry) > 0 {
				combined := make([]byte, 0, len(carry)+read)
				combined = append(combined, carry...)
				combined = append(combined, chunk...)
				chunk = combined
				carry = nil
			}
			whole := len(chunk) - len(chunk)%4
			if whole < len(chunk) {
				carry = append(carry, chunk[whole:]...)
			}
			if whole > 0 {
				applyGainS16LE(chunk[:whole], p.currentGain())
				if _, writeErr := writer.Write(chunk[:whole]); writeErr != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// aplayArgs builds the raw-PCM output command. The requested buffer is only a
// ceiling (cards may clamp it); the jitter cushion is the pre-roll held in the
// enlarged stdin pipe, while the short period keeps wakeups frequent. The
// start threshold is 100ms: after an xrun aplay otherwise waits until the
// whole (hardware-clamped) buffer refills before sound resumes.
func (p *Pipeline) aplayArgs() []string {
	return []string{
		"-D", p.cfg.ALSAHardwareDevice(),
		"-t", "raw",
		"-f", "S16_LE",
		"-r", "48000",
		"-c", "2",
		"-B", fmt.Sprintf("%d", p.cfg.MiPlayAlsaBufferUS),
		"-F", fmt.Sprintf("%d", alsaPeriodUS),
		"-R", fmt.Sprintf("%d", alsaStartUS),
		"-q",
	}
}

// prebufferBytes is the silence written before the first decoded sample. The
// pump adds new PCM only as fast as the source delivers it, so this pre-roll
// is the lead playback draws down during a delivery stall. The ALSA ring
// cannot hold it on cards that clamp the buffer (an ES1371 stops at 341ms),
// which is why the rest lives in an enlarged aplay stdin pipe.
func (p *Pipeline) prebufferBytes() int {
	return alsaBytesPerSecond * p.cfg.MiPlayPrebufferUS / 1_000_000
}

// aplayPipeBytes sizes the aplay stdin pipe to hold the whole pre-roll plus
// slack for one incoming burst. The cap bounds latency creep: at most a pipe
// full of PCM can queue ahead of playback beyond the pre-roll.
func (p *Pipeline) aplayPipeBytes() int {
	size := p.prebufferBytes() + alsaPipeSlack
	if size > alsaPipeMax {
		size = alsaPipeMax
	}
	return size
}

// newAplayStdin creates the aplay stdin pipe enlarged beyond the kernel
// default. The returned capacity is what the kernel granted, which bounds the
// pre-roll when the resize is refused.
func newAplayStdin(want int) (*os.File, *os.File, int, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, nil, 0, err
	}
	if want > 0 {
		_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, writer.Fd(), syscall.F_SETPIPE_SZ, uintptr(want))
	}
	capacity := 0
	if value, _, errno := syscall.Syscall(syscall.SYS_FCNTL, writer.Fd(), syscall.F_GETPIPE_SZ, 0); errno == 0 {
		capacity = int(value)
	}
	if capacity <= 0 {
		capacity = 64 * 1024
	}
	return reader, writer, capacity, nil
}

// Start launches ffmpeg and aplay. Starting twice is a no-op. procMu makes a
// new generation wait for a previous Stop to finish draining, so it never
// fights the exclusive sound card.
func (p *Pipeline) Start() error {
	p.procMu.Lock()
	defer p.procMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ffmpeg != nil {
		return nil
	}
	aplay := exec.Command(p.cfg.AplayPath, p.aplayArgs()...)
	p.underruns.Store(0)
	aplay.Stderr = &logPipe{ring: p.aplayEP, logger: p.logger, prefix: "aplay", underruns: &p.underruns}

	ffmpegArgs := []string{"-hide_banner", "-loglevel", "error", "-f", "mpegts"}
	ffmpegArgs = append(ffmpegArgs, FFmpegLowLatencyInputArgs...)
	ffmpegArgs = append(ffmpegArgs,
		"-i", "pipe:0",
		"-vn",
		"-acodec", "pcm_s16le",
		"-ar", "48000",
		"-ac", "2",
		"-f", "s16le",
		"pipe:1",
	)
	ffmpeg := exec.Command(p.cfg.FFmpegPath, ffmpegArgs...)
	ffmpeg.Stderr = &logPipe{ring: p.ffmpegEP, logger: p.logger, prefix: "ffmpeg"}

	decoded, err := ffmpeg.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open ffmpeg stdout: %w", err)
	}
	aplayRead, aplayInput, pipeCapacity, err := newAplayStdin(p.aplayPipeBytes())
	if err != nil {
		return fmt.Errorf("create aplay stdin: %w", err)
	}
	aplay.Stdin = aplayRead
	stdin, err := ffmpeg.StdinPipe()
	if err != nil {
		_ = aplayRead.Close()
		_ = aplayInput.Close()
		return fmt.Errorf("open ffmpeg stdin: %w", err)
	}

	if err := aplay.Start(); err != nil {
		_ = aplayRead.Close()
		_ = aplayInput.Close()
		return fmt.Errorf("start %s on %s: %w (aplay log: %s)", p.cfg.AplayPath, p.cfg.ALSAHardwareDevice(), err, p.aplayEP.String())
	}
	_ = aplayRead.Close()
	if pipeCapacity < p.aplayPipeBytes() {
		p.logger.Warn("aplay stdin pipe could not hold the requested prebuffer",
			"requested_bytes", p.aplayPipeBytes(), "capacity_bytes", pipeCapacity)
	}
	// Prefill silence before ffmpeg starts. It primes the ALSA ring and, once
	// the ring is full, leaves the remainder queued in the enlarged pipe as
	// the cushion against delivery stalls.
	prefill := p.prebufferBytes()
	if prefill > pipeCapacity {
		prefill = pipeCapacity
	}
	if prefill > 0 {
		if _, err := aplayInput.Write(make([]byte, prefill)); err != nil {
			_ = aplayInput.Close()
			_ = aplay.Process.Kill()
			_, _ = aplay.Process.Wait()
			return fmt.Errorf("prefill aplay: %w (aplay log: %s)", err, p.aplayEP.String())
		}
	}
	if err := ffmpeg.Start(); err != nil {
		_ = aplayInput.Close()
		_ = aplay.Process.Kill()
		_, _ = aplay.Process.Wait()
		return fmt.Errorf("start %s: %w", p.cfg.FFmpegPath, err)
	}
	go p.pump(decoded, aplayInput)

	p.aplay = aplay
	p.ffmpeg = ffmpeg
	p.stdin = stdin
	p.stats = PipelineStats{Running: true, FFmpegAlive: true, AplayAlive: true}
	p.logger.Info("miplay realtime pipeline started",
		"device", p.cfg.ALSAHardwareDevice(),
		"aplay_buffer_us", p.cfg.MiPlayAlsaBufferUS,
		"aplay_period_us", alsaPeriodUS,
		"prebuffer_us", p.cfg.MiPlayPrebufferUS,
		"prebuffer_bytes", prefill,
		"aplay_pipe_bytes", pipeCapacity,
		"software_gain", p.currentGain(),
		"ffmpeg_pid", ffmpeg.Process.Pid,
		"aplay_pid", aplay.Process.Pid,
	)
	return nil
}

// Write feeds one validated MPEG-TS payload into ffmpeg.
func (p *Pipeline) Write(transportStream []byte) error {
	p.mu.Lock()
	ffmpeg := p.ffmpeg
	stdin := p.stdin
	if ffmpeg == nil || stdin == nil {
		p.mu.Unlock()
		return protocolErrorf("pipeline is not running")
	}
	if p.stats.FirstFrameAt.IsZero() {
		p.stats.FirstFrameAt = time.Now().UTC()
	}
	p.stats.Frames++
	p.stats.Bytes += int64(len(transportStream))
	p.mu.Unlock()

	if _, err := stdin.Write(transportStream); err != nil {
		p.mu.Lock()
		// A write that loses the race with Stop is teardown noise, not a
		// playback fault; only report failures of the current generation.
		if p.ffmpeg == ffmpeg {
			p.stats.LastError = fmt.Sprintf("ffmpeg stdin: %v (ffmpeg log: %s)", err, p.ffmpegEP.String())
		}
		p.mu.Unlock()
		return fmt.Errorf("write to ffmpeg: %w", err)
	}
	return nil
}

// Stop releases the pipeline: closing ffmpeg's stdin lets both processes
// drain and exit; a bounded grace period is followed by SIGKILL. It holds
// procMu across the waits so a new generation cannot start until the card is
// free.
func (p *Pipeline) Stop() {
	p.procMu.Lock()
	defer p.procMu.Unlock()
	p.mu.Lock()
	ffmpeg := p.ffmpeg
	aplay := p.aplay
	stdin := p.stdin
	p.ffmpeg = nil
	p.aplay = nil
	p.stdin = nil
	running := ffmpeg != nil || aplay != nil
	p.stats.Running = false
	p.stats.FFmpegAlive = false
	p.stats.AplayAlive = false
	p.mu.Unlock()

	if !running {
		return
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	waitWithGrace(ffmpeg, 3*time.Second)
	waitWithGrace(aplay, 3*time.Second)
	p.logger.Info("miplay realtime pipeline stopped",
		"frames", p.stats.Frames, "bytes", p.stats.Bytes,
		"ffmpeg_log_tail", p.ffmpegEP.String(), "aplay_log_tail", p.aplayEP.String())
}

func waitWithGrace(command *exec.Cmd, grace time.Duration) {
	if command == nil || command.Process == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		_ = command.Process.Kill()
		<-done
	}
}

// Running reports whether both child processes are live.
func (p *Pipeline) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ffmpeg != nil
}

// Stats returns the pipeline snapshot.
func (p *Pipeline) Stats() PipelineStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := p.stats
	stats.Gain = p.currentGain()
	stats.Underruns = p.underruns.Load()
	if p.ffmpeg != nil && p.ffmpeg.Process != nil && p.ffmpeg.ProcessState == nil {
		stats.FFmpegAlive = true
	}
	if p.aplay != nil && p.aplay.Process != nil && p.aplay.ProcessState == nil {
		stats.AplayAlive = true
	}
	return stats
}
