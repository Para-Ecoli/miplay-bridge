package miplay

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
)

// controlTestHarness wires a Receiver to a real TCP pair the same way
// acceptLoop hands connections to handleControl; mDNS and the HTTP layer stay
// out of the picture.
type controlTestHarness struct {
	receiver *Receiver
	client   net.Conn
	done     chan struct{}
}

func newControlTestHarness(t *testing.T, handshakeTimeout time.Duration) *controlTestHarness {
	t.Helper()
	cfg := config.Default()
	cfg.MiPlayHandshakeTimeout = handshakeTimeout
	receiver := NewReceiver(cfg, discardLogger(), Hooks{})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		receiver.handleControl(context.Background(), connection)
	}()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &controlTestHarness{receiver: receiver, client: client, done: done}
}

func (h *controlTestHarness) readChallenge(t *testing.T) []byte {
	t.Helper()
	frame := readCommandFrame(t, h.client)
	if frame.Command != CmdLegacyChallenge {
		t.Fatalf("first frame = %s, want the legacy challenge", frame.Command)
	}
	return frame.Payload
}

func (h *controlTestHarness) authenticate(t *testing.T) {
	t.Helper()
	challenge := h.readChallenge(t)
	ack := EncodeCommand(CmdLegacyChallengeAck, 0, LegacyChallengeResponse(challenge))
	if _, err := h.client.Write(ack); err != nil {
		t.Fatalf("write challenge ack: %v", err)
	}
}

func (h *controlTestHarness) wait(t *testing.T) {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("control session did not finish")
	}
}

func (h *controlTestHarness) lastSession(t *testing.T) *SessionSummary {
	t.Helper()
	h.receiver.mu.Lock()
	defer h.receiver.mu.Unlock()
	return h.receiver.lastSession
}

func readCommandFrame(t *testing.T, connection net.Conn) CommandFrame {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	header := make([]byte, FrameHeaderSize)
	if _, err := io.ReadFull(connection, header); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	if header[0] != Magic {
		t.Fatalf("frame magic = 0x%02x, want 0x%02x", header[0], Magic)
	}
	length := binary.BigEndian.Uint32(header[5:9])
	payload := make([]byte, length)
	if _, err := io.ReadFull(connection, payload); err != nil {
		t.Fatalf("read frame payload: %v", err)
	}
	return CommandFrame{
		Command:  Command(binary.BigEndian.Uint16(header[1:3])),
		Sequence: binary.BigEndian.Uint16(header[3:5]),
		Payload:  payload,
	}
}

// TestControlHandshakeTimeoutGuardsUnauthenticatedConnections pins the
// authentication-window contract: a peer that never answers the challenge is
// dropped when MIPLAY_HANDSHAKE_TIMEOUT_SECONDS elapses.
func TestControlHandshakeTimeoutGuardsUnauthenticatedConnections(t *testing.T) {
	harness := newControlTestHarness(t, 250*time.Millisecond)
	harness.readChallenge(t)

	_ = harness.client.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	buffer := make([]byte, 1)
	if _, err := harness.client.Read(buffer); err == nil {
		t.Fatal("unauthenticated connection was not closed")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("connection closed after %v, want the handshake deadline", elapsed)
	}

	harness.wait(t)
	summary := harness.lastSession(t)
	if summary == nil || summary.Error != "handshake timed out" {
		t.Fatalf("summary = %+v, want a handshake timeout", summary)
	}
}

// TestControlAuthenticatedSessionOutlivesHandshakeTimeout pins the behaviour
// that broke real casting: once the challenge is answered the phone may keep
// the session open (and heartbeating) far past the handshake timeout while the
// user pre-loads playback, and a source-side pause must be acknowledged
// without tearing the session down.
func TestControlAuthenticatedSessionOutlivesHandshakeTimeout(t *testing.T) {
	harness := newControlTestHarness(t, 250*time.Millisecond)
	harness.authenticate(t)

	time.Sleep(3 * 250 * time.Millisecond)

	if _, err := harness.client.Write(EncodeCommand(CmdHeartbeat, 7, nil)); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}
	frame := readCommandFrame(t, harness.client)
	if frame.Command != CmdHeartbeatAck || frame.Sequence != 7 {
		t.Fatalf("reply = %s seq %d, want the heartbeat ack seq 7", frame.Command, frame.Sequence)
	}

	if _, err := harness.client.Write(EncodeCommand(CmdPause, 8, nil)); err != nil {
		t.Fatalf("write pause: %v", err)
	}
	frame = readCommandFrame(t, harness.client)
	if frame.Command != CmdPauseAck || frame.Sequence != 8 {
		t.Fatalf("reply = %s seq %d, want the pause ack seq 8", frame.Command, frame.Sequence)
	}

	_ = harness.client.Close()
	harness.wait(t)
	summary := harness.lastSession(t)
	if summary == nil {
		t.Fatal("missing session summary")
	}
	if summary.Error == "handshake timed out" {
		t.Fatalf("authenticated session was cut by the handshake deadline: %+v", summary)
	}
	if !summary.Authenticated || summary.ControlFrames < 2 {
		t.Fatalf("summary = %+v, want an authenticated session with observed frames", summary)
	}
}
