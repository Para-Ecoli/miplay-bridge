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
	session, err := NewLegacyReceiverSession([]byte(testChallenge), 0, "miplay test", 30,
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

// TestGetVolumeAnswersEveryPayloadShape pins the volume-query contract: the
// reply always carries the current volume, whatever the inbound payload looks
// like - a pure query must never end the session.
func TestGetVolumeAnswersEveryPayloadShape(t *testing.T) {
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
		if !bytes.Equal(reply.Payload, EncodeScalar(30)) {
			t.Fatalf("%s: reply payload = % x, want the current volume", name, reply.Payload)
		}
		if result.VolumeSet {
			t.Fatalf("%s: a query must not report a volume change", name)
		}
	}
}

// TestGetVolumeAnswersTheLiveHardwareLevel pins the cross-surface sync behind
// the spring-back fix: a wired provider makes GET_VOLUME answer the live
// hardware level - so a level the console changed after the phone connected is
// reported truthfully - and any provider miss or nonsense reading falls back to
// the session's own copy.
func TestGetVolumeAnswersTheLiveHardwareLevel(t *testing.T) {
	session, recorder := newAuthenticatedSession(t)
	level := 71
	available := true
	session.SetVolumeProvider(func() (int, bool) { return level, available })

	result := stepAccepted(t, session, recorder, CommandFrame{Command: CmdGetVolume, Sequence: 9})
	reply := decodeOneCommandFrame(t, result.Writes[0])
	if reply.Command != CmdGetVolumeAck || !bytes.Equal(reply.Payload, EncodeScalar(71)) {
		t.Fatalf("reply = %+v, want the live hardware level", reply)
	}

	// The console moves the slider mid-cast: the very next query already
	// reports the new level.
	level = 12
	result = stepAccepted(t, session, recorder, CommandFrame{Command: CmdGetVolume, Sequence: 10})
	reply = decodeOneCommandFrame(t, result.Writes[0])
	if !bytes.Equal(reply.Payload, EncodeScalar(12)) {
		t.Fatalf("reply payload = % x, want the updated live level", reply.Payload)
	}

	// A provider without a hardware mixer leaves the session copy in charge.
	available = false
	result = stepAccepted(t, session, recorder, CommandFrame{Command: CmdGetVolume, Sequence: 11})
	reply = decodeOneCommandFrame(t, result.Writes[0])
	if !bytes.Equal(reply.Payload, EncodeScalar(30)) {
		t.Fatalf("reply payload = % x, want the session fallback", reply.Payload)
	}

	// A nonsensical reading is ignored just the same.
	available = true
	level = 171
	result = stepAccepted(t, session, recorder, CommandFrame{Command: CmdGetVolume, Sequence: 12})
	reply = decodeOneCommandFrame(t, result.Writes[0])
	if !bytes.Equal(reply.Payload, EncodeScalar(30)) {
		t.Fatalf("reply payload = % x, want the session fallback for an out-of-range reading", reply.Payload)
	}
}

// TestSetVolumeAcceptsBothScalarShapes walks the plaintext encodings: the
// bare four-byte integer and the five-byte 0x00-tagged scalar modern sources
// send.
func TestSetVolumeAcceptsBothScalarShapes(t *testing.T) {
	session, recorder := newAuthenticatedSession(t)
	cases := []struct {
		name    string
		payload []byte
		want    int
	}{
		{"four-byte integer", []byte{0x00, 0x00, 0x00, 0x2A}, 42},
		{"five-byte scalar", EncodeScalar(64), 64},
	}
	for _, testCase := range cases {
		result := stepAccepted(t, session, recorder, CommandFrame{Command: CmdSetVolume, Sequence: 4, Payload: testCase.payload})
		if !result.VolumeSet || result.Volume != testCase.want {
			t.Fatalf("%s: result = %+v, want volume %d", testCase.name, result, testCase.want)
		}
		if got := session.Volume(); got != testCase.want {
			t.Fatalf("%s: session volume = %d, want %d", testCase.name, got, testCase.want)
		}
		if len(result.Writes) != 1 {
			t.Fatalf("%s: writes = %d, want the acknowledgement", testCase.name, len(result.Writes))
		}
		reply := decodeOneCommandFrame(t, result.Writes[0])
		if reply.Command != CmdSetVolumeAck || reply.Sequence != 4 {
			t.Fatalf("%s: reply = %+v", testCase.name, reply)
		}
	}
}

// TestSetVolumeDecryptsSafetyDataPayload pins the wrapped-command path seen
// in the field: MIUI wraps SET_VOLUME in a SafetyData blob (25 bytes: nine-
// byte header plus one CBC block) keyed by the TCP endpoints. Without an
// active safety channel the probe must decrypt it anyway, whichever IV mode
// the phone chose and whichever scalar shape it wrapped.
func TestSetVolumeDecryptsSafetyDataPayload(t *testing.T) {
	authKey := deriveType1AuthKey(testLocalHost, testLocalPort, testPeerHost, testPeerPort)
	key := authKey[:16]
	cases := []struct {
		name  string
		iv    []byte
		inner []byte
		want  int
	}{
		{"type-2 scalar", authKey[16:], EncodeScalar(55), 55},
		{"type-2 four-byte", authKey[16:], []byte{0x00, 0x00, 0x00, 0x28}, 40},
		{"type-1 scalar", key, EncodeScalar(21), 21},
		{"type-1 four-byte", key, []byte{0x00, 0x00, 0x00, 0x50}, 80},
	}
	for _, testCase := range cases {
		session, recorder := newAuthenticatedSession(t)
		cipher, err := newSafetyCipher(key, testCase.iv)
		if err != nil {
			t.Fatal(err)
		}
		blob, err := cipher.Encrypt(testCase.inner)
		if err != nil {
			t.Fatal(err)
		}
		if len(blob) != 25 {
			t.Fatalf("%s: blob = %d bytes, want the observed 25-byte shape", testCase.name, len(blob))
		}
		result := stepAccepted(t, session, recorder, CommandFrame{Command: CmdSetVolume, Sequence: 6, Payload: blob})
		if !result.VolumeSet || result.Volume != testCase.want {
			t.Fatalf("%s: result = %+v, want volume %d", testCase.name, result, testCase.want)
		}
		if got := session.Volume(); got != testCase.want {
			t.Fatalf("%s: session volume = %d, want %d", testCase.name, got, testCase.want)
		}
	}
}

// TestSetVolumeMalformedPayloadsAreIgnored pins the never-fatal policy: a
// volume command that decodes to nothing is acknowledged and skipped, and the
// session keeps working - no stop, no volume change.
func TestSetVolumeMalformedPayloadsAreIgnored(t *testing.T) {
	wrongKey := deriveType1AuthKey(testLocalHost, testLocalPort, testPeerHost, 1)
	wrongCipher, err := newSafetyCipher(wrongKey[:16], wrongKey[16:])
	if err != nil {
		t.Fatal(err)
	}
	foreignBlob, err := wrongCipher.Encrypt(EncodeScalar(90))
	if err != nil {
		t.Fatal(err)
	}
	shapes := map[string][]byte{
		"empty":                nil,
		"garbage blob":         bytes.Repeat([]byte{0xAB}, 25),
		"foreign-key blob":     foreignBlob,
		"truncated blob":       bytes.Repeat([]byte{0x01}, 10),
		"out-of-range integer": {0x00, 0x00, 0x01, 0x2C},
		"mis-tagged scalar":    {0x01, 0x00, 0x00, 0x00, 0x2A},
	}
	for name, payload := range shapes {
		session, recorder := newAuthenticatedSession(t)
		result := stepAccepted(t, session, recorder, CommandFrame{Command: CmdSetVolume, Sequence: 7, Payload: payload})
		if result.VolumeSet {
			t.Fatalf("%s: a malformed payload must not set a volume", name)
		}
		if got := session.Volume(); got != 30 {
			t.Fatalf("%s: volume = %d, want it unchanged", name, got)
		}
		if len(result.Writes) != 1 {
			t.Fatalf("%s: writes = %d, want the acknowledgement", name, len(result.Writes))
		}
		reply := decodeOneCommandFrame(t, result.Writes[0])
		if reply.Command != CmdSetVolumeAck || reply.Sequence != 7 {
			t.Fatalf("%s: reply = %+v", name, reply)
		}
		followUp := stepAccepted(t, session, recorder, CommandFrame{Command: CmdHeartbeat, Sequence: 8})
		if len(followUp.Writes) != 1 {
			t.Fatalf("%s: the session did not survive the malformed command", name)
		}
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
