package miplay

import (
	"bytes"
	"testing"
)

// testSessionEndpoints is the fixed TCP endpoint context every session under
// test is built with, so the endpoint-derived SafetyData keys are
// reproducible.
const (
	testLocalHost = "127.0.0.1"
	testLocalPort = 5000
	testPeerHost  = "127.0.0.1"
	testPeerPort  = 6000
)

const testChallenge = "123456789012345"

// testSessionVolume is the initial volume every session under test starts
// from.
const testSessionVolume = 64

// writeRecorder captures every control frame a session emits.
type writeRecorder struct {
	frames [][]byte
}

func (w *writeRecorder) write(frames [][]byte) error {
	for _, frame := range frames {
		w.frames = append(w.frames, append([]byte(nil), frame...))
	}
	return nil
}

// take returns the frames captured so far and resets the recorder.
func (w *writeRecorder) take() [][]byte {
	frames := w.frames
	w.frames = nil
	return frames
}

// newAuthenticatedSession returns a session driven through the legacy
// challenge, ready for business commands.
func newAuthenticatedSession(t *testing.T) (*LegacyReceiverSession, *writeRecorder) {
	t.Helper()
	session, err := NewLegacyReceiverSession([]byte(testChallenge), 0, "miplay test", testSessionVolume,
		testLocalHost, testLocalPort, testPeerHost, testPeerPort)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &writeRecorder{}
	if err := session.Start(recorder.write); err != nil {
		t.Fatal(err)
	}
	result, err := session.Step(CommandFrame{
		Command:  CmdLegacyChallengeAck,
		Sequence: 0,
		Payload:  LegacyChallengeResponse([]byte(testChallenge)),
	}, recorder.write)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted {
		t.Fatalf("legacy authentication was rejected: %s", result.Reason)
	}
	recorder.take()
	return session, recorder
}

// stepAccepted processes one frame and fails the test when the session would
// have been stopped (Accepted false tears the connection down).
func stepAccepted(t *testing.T, session *LegacyReceiverSession, recorder *writeRecorder, frame CommandFrame) ControlResult {
	t.Helper()
	result, err := session.Step(frame, recorder.write)
	if err != nil {
		t.Fatalf("step %s: %v", frame.Command, err)
	}
	if !result.Accepted {
		t.Fatalf("step %s was rejected and would have stopped the session: %s", frame.Command, result.Reason)
	}
	return result
}

// TestGetVolumeAnswersCurrentLevel pins the volume-query contract: the reply
// carries the session's current level whatever the inbound payload looks
// like - a pure query must never end the session - and a wired provider
// always wins so the answer tracks console-side changes live.
func TestGetVolumeAnswersCurrentLevel(t *testing.T) {
	session, recorder := newAuthenticatedSession(t)
	shapes := map[string][]byte{
		"empty":            nil,
		"five-byte scalar": EncodeScalar(30),
		"garbage blob":     bytes.Repeat([]byte{0xAB}, 25),
	}
	for name, payload := range shapes {
		result := stepAccepted(t, session, recorder, CommandFrame{Command: CmdGetVolume, Sequence: 9, Payload: payload})
		if len(result.Writes) != 1 {
			t.Fatalf("%s: writes = %d, want the volume acknowledgement", name, len(result.Writes))
		}
		reply := decodeOneCommandFrame(t, result.Writes[0])
		if reply.Command != CmdGetVolumeAck || reply.Sequence != 9 {
			t.Fatalf("%s: reply = %+v", name, reply)
		}
		if !bytes.Equal(reply.Payload, EncodeScalar(testSessionVolume)) {
			t.Fatalf("%s: reply payload = % x, want the session level", name, reply.Payload)
		}
	}

	// A wired provider answers the live hardware level on every query.
	live := 33
	session.SetVolumeProvider(func() (int, bool) { return live, true })
	result := stepAccepted(t, session, recorder, CommandFrame{Command: CmdGetVolume, Sequence: 10})
	reply := decodeOneCommandFrame(t, result.Writes[0])
	if !bytes.Equal(reply.Payload, EncodeScalar(33)) {
		t.Fatalf("provider read: reply payload = % x, want the live level 33", reply.Payload)
	}
	live = 47
	result = stepAccepted(t, session, recorder, CommandFrame{Command: CmdGetVolume, Sequence: 11, Payload: EncodeScalar(30)})
	reply = decodeOneCommandFrame(t, result.Writes[0])
	if !bytes.Equal(reply.Payload, EncodeScalar(47)) {
		t.Fatalf("provider re-read: reply payload = % x, want the live level 47", reply.Payload)
	}

	// A provider that cannot read the hardware falls back to the session copy.
	session.SetVolumeProvider(func() (int, bool) { return 0, false })
	result = stepAccepted(t, session, recorder, CommandFrame{Command: CmdGetVolume, Sequence: 12})
	reply = decodeOneCommandFrame(t, result.Writes[0])
	if !bytes.Equal(reply.Payload, EncodeScalar(testSessionVolume)) {
		t.Fatalf("failing provider: reply payload = % x, want the session level", reply.Payload)
	}
}

// TestSetVolumeDecodesRecoveredShapes pins the SET_VOLUME decoding ladder:
// the bare four-byte big-endian integer, the five-byte 0x00-tagged scalar,
// and the SafetyData blob wrapping a scalar are accepted and recorded; every
// malformed or out-of-range shape is acknowledged and ignored so the phone
// slider never triggers a reconnect loop.
func TestSetVolumeDecodesRecoveredShapes(t *testing.T) {
	cases := []struct {
		name     string
		payload  []byte
		accepted bool
		want     int
	}{
		{"four-byte integer", []byte{0x00, 0x00, 0x00, 0x2A}, true, 42},
		{"five-byte scalar", EncodeScalar(64), true, 64},
		{"empty", nil, false, 0},
		{"garbage blob", bytes.Repeat([]byte{0xAB}, 25), false, 0},
		{"out of range", []byte{0x00, 0x00, 0x00, 0x65}, false, 0},
	}
	for _, testCase := range cases {
		session, recorder := newAuthenticatedSession(t)
		result := stepAccepted(t, session, recorder,
			CommandFrame{Command: CmdSetVolume, Sequence: 7, Payload: testCase.payload})
		if len(result.Writes) != 1 {
			t.Fatalf("%s: writes = %d, want the acknowledgement", testCase.name, len(result.Writes))
		}
		reply := decodeOneCommandFrame(t, result.Writes[0])
		if reply.Command != CmdSetVolumeAck || reply.Sequence != 7 {
			t.Fatalf("%s: reply = %+v", testCase.name, reply)
		}
		if result.VolumeSet != testCase.accepted {
			t.Fatalf("%s: VolumeSet = %v, want %v", testCase.name, result.VolumeSet, testCase.accepted)
		}
		if testCase.accepted && result.Volume != testCase.want {
			t.Fatalf("%s: Volume = %d, want %d", testCase.name, result.Volume, testCase.want)
		}
		if testCase.accepted && session.Volume() != testCase.want {
			t.Fatalf("%s: session volume = %d, want %d", testCase.name, session.Volume(), testCase.want)
		}
		followUp := stepAccepted(t, session, recorder, CommandFrame{Command: CmdHeartbeat, Sequence: 8})
		if len(followUp.Writes) != 1 {
			t.Fatalf("%s: the session did not survive the command", testCase.name)
		}
	}
}

// TestSetVolumeDecodesSafetyDataBlob pins the blob probe: a SET_VOLUME
// wrapped in a SafetyData blob - decrypted on a fresh cipher so the live
// channel state is untouched - decodes with the first IV candidate.
func TestSetVolumeDecodesSafetyDataBlob(t *testing.T) {
	session, recorder := newAuthenticatedSession(t)
	authKey := deriveType1AuthKey(testLocalHost, testLocalPort, testPeerHost, testPeerPort)
	key := authKey[:16]
	encryptor, err := newSafetyCipher(key, authKey[16:])
	if err != nil {
		t.Fatal(err)
	}
	blob, err := encryptor.Encrypt(EncodeScalar(47))
	if err != nil {
		t.Fatal(err)
	}
	result := stepAccepted(t, session, recorder, CommandFrame{Command: CmdSetVolume, Sequence: 4, Payload: blob})
	if !result.VolumeSet || result.Volume != 47 {
		t.Fatalf("VolumeSet = %v, Volume = %d; want the blob level 47", result.VolumeSet, result.Volume)
	}
	if session.Volume() != 47 {
		t.Fatalf("session volume = %d, want 47", session.Volume())
	}
}

// TestMediaStartedFiresOncePerSession pins the first-payload notification the
// receiver triggers when media starts flowing: the two NOTIFY frames go out
// exactly once, and only once the WFD session reached the opened phase.
func TestMediaStartedFiresOncePerSession(t *testing.T) {
	session, recorder := newAuthenticatedSession(t)

	// Before OPEN the notification must stay silent.
	if err := session.MediaStarted(recorder.write); err != nil {
		t.Fatal(err)
	}
	if frames := recorder.take(); len(frames) != 0 {
		t.Fatalf("frames before the opened phase = %d, want none", len(frames))
	}

	stepAccepted(t, session, recorder, CommandFrame{Command: CmdSetPlaySource, Sequence: 1, Payload: []byte(`{"source":"phone"}`)})
	stepAccepted(t, session, recorder, CommandFrame{Command: CmdOpen, Sequence: 2, Payload: []byte("wfd://192.168.1.20:7236?mirrorMode=0\x00")})
	recorder.take()

	if err := session.MediaStarted(recorder.write); err != nil {
		t.Fatal(err)
	}
	frames := recorder.take()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want the two media-started notifications", len(frames))
	}
	first := decodeOneCommandFrame(t, frames[0])
	if first.Command != CmdNotify || first.Sequence != 1 || !bytes.Equal(first.Payload, EncodeNotifyScalar("first-audiopcm", 1)) {
		t.Fatalf("first notification = %+v", first)
	}
	second := decodeOneCommandFrame(t, frames[1])
	if second.Command != CmdNotify || second.Sequence != 2 || !bytes.Equal(second.Payload, EncodeNotifyScalar("state", 2)) {
		t.Fatalf("second notification = %+v", second)
	}

	// Exactly once: replaying the trigger must not emit again.
	if err := session.MediaStarted(recorder.write); err != nil {
		t.Fatal(err)
	}
	if frames := recorder.take(); len(frames) != 0 {
		t.Fatalf("frames on the second call = %d, want none", len(frames))
	}
}
