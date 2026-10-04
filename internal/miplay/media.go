package miplay

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
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
	ring   *lineRing
	logger *slog.Logger
	prefix string
	buffer []byte
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
//	media frames -> ffmpeg (MPEG-TS/AAC -> s16le) -> aplay (exclusive hw)
//
// ffmpeg runs with low-latency input flags; aplay owns an ALSA buffer sized
// by MIPLAY_ALSA_BUFFER_US, the primary latency knob.
type Pipeline struct {
	cfg    config.Config
	logger *slog.Logger

	mu     sync.Mutex
	ffmpeg *exec.Cmd
	aplay  *exec.Cmd
	stdin  io.WriteCloser

	stats    PipelineStats
	ffmpegEP *lineRing
	aplayEP  *lineRing
}

// NewPipeline creates the pipeline for the configured ALSA device.
func NewPipeline(cfg config.Config, logger *slog.Logger) *Pipeline {
	if logger == nil {
		logger = slog.Default()
	}
	return &Pipeline{
		cfg:      cfg,
		logger:   logger,
		ffmpegEP: newLineRing(30),
		aplayEP:  newLineRing(30),
	}
}

// Start launches ffmpeg and aplay. Starting twice is a no-op.
func (p *Pipeline) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ffmpeg != nil {
		return nil
	}
	aplay := exec.Command(p.cfg.AplayPath,
		"-D", p.cfg.ALSAHardwareDevice(),
		"-t", "raw",
		"-f", "S16_LE",
		"-r", "48000",
		"-c", "2",
		"-B", fmt.Sprintf("%d", p.cfg.MiPlayAlsaBufferUS),
		"-q",
	)
	aplay.Stderr = &logPipe{ring: p.aplayEP, logger: p.logger, prefix: "aplay"}

	ffmpeg := exec.Command(p.cfg.FFmpegPath,
		"-hide_banner",
		"-loglevel", "error",
		"-f", "mpegts",
		"-flags", "low_delay",
		"-probesize", "4096",
		"-analyzeduration", "0",
		"-i", "pipe:0",
		"-vn",
		"-acodec", "pcm_s16le",
		"-ar", "48000",
		"-ac", "2",
		"-f", "s16le",
		"pipe:1",
	)
	ffmpeg.Stderr = &logPipe{ring: p.ffmpegEP, logger: p.logger, prefix: "ffmpeg"}

	decoded, err := ffmpeg.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open ffmpeg stdout: %w", err)
	}
	aplay.Stdin = decoded
	stdin, err := ffmpeg.StdinPipe()
	if err != nil {
		return fmt.Errorf("open ffmpeg stdin: %w", err)
	}

	if err := aplay.Start(); err != nil {
		return fmt.Errorf("start %s on %s: %w (aplay log: %s)", p.cfg.AplayPath, p.cfg.ALSAHardwareDevice(), err, p.aplayEP.String())
	}
	if err := ffmpeg.Start(); err != nil {
		_ = aplay.Process.Kill()
		_, _ = aplay.Process.Wait()
		return fmt.Errorf("start %s: %w", p.cfg.FFmpegPath, err)
	}

	p.aplay = aplay
	p.ffmpeg = ffmpeg
	p.stdin = stdin
	p.stats = PipelineStats{Running: true, FFmpegAlive: true, AplayAlive: true}
	p.logger.Info("miplay realtime pipeline started",
		"device", p.cfg.ALSAHardwareDevice(),
		"aplay_buffer_us", p.cfg.MiPlayAlsaBufferUS,
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
		p.stats.LastError = fmt.Sprintf("ffmpeg stdin: %v (ffmpeg log: %s)", err, p.ffmpegEP.String())
		p.mu.Unlock()
		return fmt.Errorf("write to ffmpeg: %w", err)
	}
	return nil
}

// Stop releases the pipeline: closing ffmpeg's stdin lets both processes
// drain and exit; a bounded grace period is followed by SIGKILL.
func (p *Pipeline) Stop() {
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
	if p.ffmpeg != nil && p.ffmpeg.Process != nil && p.ffmpeg.ProcessState == nil {
		stats.FFmpegAlive = true
	}
	if p.aplay != nil && p.aplay.Process != nil && p.aplay.ProcessState == nil {
		stats.AplayAlive = true
	}
	return stats
}
