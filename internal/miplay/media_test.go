package miplay

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func encodeMediaFrame(payload []byte) []byte {
	frame := []byte{MediaMagic, byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload))}
	return append(frame, payload...)
}

func TestMediaFrameDecoderRoundTrip(t *testing.T) {
	payloads := [][]byte{[]byte("first"), []byte("second-frame")}
	stream := []byte{}
	for _, payload := range payloads {
		stream = append(stream, encodeMediaFrame(payload)...)
	}

	decoder := NewMediaFrameDecoder()
	var frames [][]byte
	for _, chunk := range [][]byte{stream[:5], stream[5:11], stream[11:]} {
		parsed, err := decoder.Feed(chunk)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, parsed...)
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	for index, payload := range payloads {
		if !bytes.Equal(frames[index], payload) {
			t.Fatalf("frames[%d] = %q, want %q", index, frames[index], payload)
		}
	}
}

func TestMediaFrameDecoderRejectsCorruption(t *testing.T) {
	decoder := NewMediaFrameDecoder()
	if _, err := decoder.Feed([]byte{0x25, 0, 0, 1, 0xAA}); err == nil || !IsProtocolError(err) {
		t.Fatalf("err = %v, want a media-magic protocol error", err)
	}
	decoder = NewMediaFrameDecoder()
	if _, err := decoder.Feed([]byte{MediaMagic, 0, 0, 0}); err == nil {
		t.Fatal("a zero-length media frame must be rejected")
	}
}

func buildRTPPacket(transportStream []byte) []byte {
	packet := make([]byte, rtpHeaderSize+len(transportStream))
	packet[0] = 0x80
	packet[1] = rtpPayloadTypeMPEGTS | 0x80 // marker bit set
	binary.BigEndian.PutUint16(packet[2:4], 77)
	binary.BigEndian.PutUint32(packet[4:8], 9000)
	binary.BigEndian.PutUint32(packet[8:12], 0xAABBCCDD)
	copy(packet[rtpHeaderSize:], transportStream)
	return packet
}

func buildTransportStream(packets int) []byte {
	transportStream := make([]byte, packets*mpegtsPacketSize)
	for offset := 0; offset < len(transportStream); offset += mpegtsPacketSize {
		transportStream[offset] = 0x47
	}
	return transportStream
}

func TestDecodeRTPMPEGTS(t *testing.T) {
	transportStream := buildTransportStream(2)
	decoded, err := DecodeRTPMPEGTS(buildRTPPacket(transportStream))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Sequence != 77 || decoded.Timestamp != 9000 || decoded.SSRC != 0xAABBCCDD {
		t.Fatalf("decoded = %+v", decoded)
	}
	if !decoded.Marker {
		t.Fatal("marker bit must be decoded")
	}
	if !bytes.Equal(decoded.TransportStream, transportStream) {
		t.Fatal("transport stream payload mismatch")
	}
}

func TestDecodeRTPMPEGTSRejectsCorruption(t *testing.T) {
	transportStream := buildTransportStream(1)

	badFlags := buildRTPPacket(transportStream)
	badFlags[0] = 0x81
	if _, err := DecodeRTPMPEGTS(badFlags); err == nil {
		t.Fatal("an unsupported RTP version must be rejected")
	}

	badType := buildRTPPacket(transportStream)
	badType[1] = 96 // dynamic payload type, not MPEG-TS
	if _, err := DecodeRTPMPEGTS(badType); err == nil {
		t.Fatal("a non-MPEG-TS payload type must be rejected")
	}

	badSync := buildRTPPacket(transportStream)
	badSync[rtpHeaderSize] = 0x46
	if _, err := DecodeRTPMPEGTS(badSync); err == nil {
		t.Fatal("a missing MPEG-TS sync byte must be rejected")
	}

	oddPayload := buildRTPPacket(append(buildTransportStream(1), 0x00))
	if _, err := DecodeRTPMPEGTS(oddPayload); err == nil {
		t.Fatal("a payload that is not a whole number of 188-byte packets must be rejected")
	}

	if _, err := DecodeRTPMPEGTS(make([]byte, 100)); err == nil {
		t.Fatal("a truncated RTP packet must be rejected")
	}
}

func TestFFmpegPipelineUsesTheReferenceLowLatencyProfile(t *testing.T) {
	want := []string{"-flags", "low_delay", "-probesize", "4096", "-analyzeduration", "0"}
	if len(FFmpegLowLatencyInputArgs) != len(want) {
		t.Fatalf("args = %v", FFmpegLowLatencyInputArgs)
	}
	for index := range want {
		if FFmpegLowLatencyInputArgs[index] != want[index] {
			t.Fatalf("args = %v, want %v", FFmpegLowLatencyInputArgs, want)
		}
	}
	if PCMReadSize != 3840 {
		t.Fatalf("PCMReadSize = %d, want 3840", PCMReadSize)
	}
}

func TestAlsaOutputUsesTheJitterBufferingProfile(t *testing.T) {
	if alsaPeriodUS != 40_000 {
		t.Fatalf("alsaPeriodUS = %d, want 40000", alsaPeriodUS)
	}
	if alsaStartUS != 100_000 {
		t.Fatalf("alsaStartUS = %d, want 100000: one period xruns on the ES1371 at start, the full ring delays recovery", alsaStartUS)
	}
	cfg := config.Default()
	if got := cfg.MiPlayAlsaBufferUS; got != 500_000 {
		t.Fatalf("default aplay buffer = %d, want 500000", got)
	}
	if got := cfg.MiPlayPrebufferUS; got != 1_000_000 {
		t.Fatalf("default prebuffer = %d, want 1000000", got)
	}
	pipeline := NewPipeline(cfg, discardLogger())
	if got := pipeline.prebufferBytes(); got != 192_000 {
		t.Fatalf("prebuffer bytes = %d, want 192000 (1s of 48kHz stereo S16)", got)
	}
	if pipe := pipeline.aplayPipeBytes(); pipe < pipeline.prebufferBytes()+alsaPipeSlack {
		t.Fatalf("aplay pipe = %d bytes, want room for the prebuffer plus slack", pipe)
	}
	args := strings.Join(pipeline.aplayArgs(), " ")
	if !strings.Contains(args, "-B 500000") || !strings.Contains(args, "-F 40000") {
		t.Fatalf("aplay args = %s, want a 500ms buffer and 40ms periods", args)
	}
	if !strings.Contains(args, "-R 100000") {
		t.Fatalf("aplay args = %s, want a 100ms start threshold so xrun recovery starts promptly", args)
	}
}

func TestAplayPipeIsBounded(t *testing.T) {
	cfg := config.Default()
	cfg.MiPlayPrebufferUS = 10_000_000
	pipeline := NewPipeline(cfg, discardLogger())
	if got := pipeline.aplayPipeBytes(); got != alsaPipeMax {
		t.Fatalf("aplay pipe = %d, want the %d-byte cap", got, alsaPipeMax)
	}
}

func TestLogPipeCountsAplayUnderruns(t *testing.T) {
	var underruns atomic.Int64
	pipe := &logPipe{ring: newLineRing(4), logger: discardLogger(), prefix: "aplay", underruns: &underruns}
	if _, err := pipe.Write([]byte("underrun!!! (at least 82.349 ms long)\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := pipe.Write([]byte("some other diagnostic\n")); err != nil {
		t.Fatal(err)
	}
	if got := underruns.Load(); got != 1 {
		t.Fatalf("underruns = %d, want 1", got)
	}
}

func TestMediaEncodersMatchTheWireContract(t *testing.T) {
	transportStream := buildTransportStream(2)
	rtp, err := EncodeRTPMPEGTS(7, 1920, 0xDEADBEEF, transportStream, true)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := DecodeRTPMPEGTS(rtp)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Sequence != 7 || packet.Timestamp != 1920 || packet.SSRC != 0xDEADBEEF || !packet.Marker {
		t.Fatalf("packet = %+v", packet)
	}
	if !bytes.Equal(packet.TransportStream, transportStream) {
		t.Fatal("transport stream mismatch")
	}

	frame, err := EncodeMediaFrame(rtp)
	if err != nil {
		t.Fatal(err)
	}
	if frame[0] != MediaMagic || int(frame[1])<<16|int(frame[2])<<8|int(frame[3]) != len(rtp) {
		t.Fatalf("frame header = % x", frame[:4])
	}
	frames, err := NewMediaFrameDecoder().Feed(frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || !bytes.Equal(frames[0], rtp) {
		t.Fatalf("frames = %d", len(frames))
	}

	if _, err := EncodeMediaFrame(nil); err == nil {
		t.Fatal("an empty media frame must be rejected")
	}
	if _, err := EncodeRTPMPEGTS(0, 0, 0, buildTransportStream(MaxTSPacketsPerRTP+1), false); err == nil {
		t.Fatal("more than seven TS packets per RTP must be rejected")
	}
}

func TestPipelineLifecycleWithoutBinaries(t *testing.T) {
	cfg := config.Default()
	cfg.FFmpegPath = "/nonexistent/ffmpeg-for-test"
	cfg.AplayPath = "/nonexistent/aplay-for-test"
	pipeline := NewPipeline(cfg, discardLogger())

	if err := pipeline.Start(); err == nil {
		t.Fatal("Start must fail when the child binaries are missing")
	}
	if pipeline.Running() {
		t.Fatal("a failed Start must not report a running pipeline")
	}
	if err := pipeline.Write(buildTransportStream(1)); err == nil {
		t.Fatal("Write before Start must fail")
	}
	pipeline.Stop() // must be idempotent on a stopped pipeline
	if stats := pipeline.Stats(); stats.Running || stats.FFmpegAlive || stats.AplayAlive {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestApplyGainS16LE(t *testing.T) {
	samples := []int16{1000, -1000, 32767, -32768}
	input := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(input[index*2:], uint16(sample))
	}

	// Unity gain is a byte-identical pass-through (bit-perfect at 100%).
	unity := append([]byte(nil), input...)
	applyGainS16LE(unity, 100)
	if !bytes.Equal(unity, input) {
		t.Fatalf("unity gain changed the PCM: % x -> % x", input, unity)
	}

	half := append([]byte(nil), input...)
	applyGainS16LE(half, 50)
	want := []int16{500, -500, 16383, -16384}
	for index, expected := range want {
		got := int16(binary.LittleEndian.Uint16(half[index*2:]))
		if got != expected {
			t.Fatalf("sample %d at 50%% = %d, want %d", index, got, expected)
		}
	}

	silent := append([]byte(nil), input...)
	applyGainS16LE(silent, 0)
	if !bytes.Equal(silent, make([]byte, len(input))) {
		t.Fatalf("0%% gain must silence the stream: % x", silent)
	}
}

func TestPipelineGainIsLiveAndClamped(t *testing.T) {
	cfg := config.Default()
	cfg.VolumeDefault = 80
	pipeline := NewPipeline(cfg, discardLogger())
	if got := pipeline.Stats().Gain; got != 80 {
		t.Fatalf("initial gain = %d, want 80", got)
	}
	pipeline.SetGain(35)
	if got := pipeline.currentGain(); got != 35 {
		t.Fatalf("gain = %d, want 35", got)
	}
	pipeline.SetGain(150)
	if got := pipeline.currentGain(); got != 100 {
		t.Fatalf("clamped gain = %d, want 100", got)
	}
	pipeline.SetGain(-5)
	if got := pipeline.currentGain(); got != 0 {
		t.Fatalf("clamped gain = %d, want 0", got)
	}
}
