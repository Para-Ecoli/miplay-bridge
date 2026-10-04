package miplay

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncodeCommandRoundTrip(t *testing.T) {
	wire := EncodeCommand(CmdSetVolume, 7, []byte{0x00, 0x00, 0x00, 0x00, 0x2A})
	if wire[0] != Magic || len(wire) != FrameHeaderSize+5 {
		t.Fatalf("wire = % x", wire)
	}
	decoder := NewFrameDecoder()
	frames, err := decoder.Feed(wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	frame := frames[0]
	if frame.Command != CmdSetVolume || frame.Sequence != 7 || !bytes.Equal(frame.Payload, wire[FrameHeaderSize:]) {
		t.Fatalf("frame = %+v", frame)
	}
}

func TestFrameDecoderReassemblesSplitFeeds(t *testing.T) {
	first := EncodeCommand(CmdHeartbeat, 1, nil)
	second := EncodeCommand(CmdGetVolume, 2, EncodeScalar(9))
	stream := append(append([]byte{}, first...), second...)

	decoder := NewFrameDecoder()
	var frames []CommandFrame
	for _, chunk := range [][]byte{stream[:3], stream[3:8], stream[8:12], stream[12:]} {
		parsed, err := decoder.Feed(chunk)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, parsed...)
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %+v, want 2", frames)
	}
	if frames[0].Command != CmdHeartbeat || frames[1].Command != CmdGetVolume {
		t.Fatalf("frames = %+v", frames)
	}
}

func TestFrameDecoderRejectsBadMagic(t *testing.T) {
	decoder := NewFrameDecoder()
	_, err := decoder.Feed([]byte{0x25, 0, 0, 0, 0, 0, 0, 0, 0})
	if err == nil || !IsProtocolError(err) {
		t.Fatalf("err = %v, want a protocol error", err)
	}
}

func TestFrameDecoderRejectsOversizePayload(t *testing.T) {
	decoder := NewFrameDecoder()
	header := []byte{Magic, 0, 0x10, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := decoder.Feed(header); err == nil || !IsProtocolError(err) {
		t.Fatalf("err = %v, want an oversize protocol error", err)
	}
}

func TestLegacyChallengeResponseFixedVectors(t *testing.T) {
	// Vectors computed independently with md5sum + openssl (see README).
	cases := []struct{ challenge, want string }{
		{"123456789012345", "3bc94de4f054fc1c5bfcc839ec4c66774bf8d3bc"},
		{"98765432109876543210", "1a8cb350617d9c273c3b8299b2153e371199b8b2"},
	}
	for _, testCase := range cases {
		if got := string(LegacyChallengeResponse([]byte(testCase.challenge))); got != testCase.want {
			t.Fatalf("LegacyChallengeResponse(%q) = %q, want %q", testCase.challenge, got, testCase.want)
		}
	}
}

func TestGenerateLegacyChallengeShape(t *testing.T) {
	seen := map[string]bool{}
	for iteration := 0; iteration < 32; iteration++ {
		challenge, err := GenerateLegacyChallenge()
		if err != nil {
			t.Fatal(err)
		}
		if len(challenge) != 15 {
			t.Fatalf("challenge = %q, want 15 digits", challenge)
		}
		for _, character := range challenge {
			if character < '0' || character > '9' {
				t.Fatalf("challenge = %q, want ASCII digits only", challenge)
			}
		}
		seen[string(challenge)] = true
	}
	if len(seen) < 2 {
		t.Fatal("challenges must not repeat across calls")
	}
}

func TestScalarRoundTrip(t *testing.T) {
	encoded := EncodeScalar(4321)
	if len(encoded) != 5 || encoded[0] != 0 {
		t.Fatalf("encoded = % x", encoded)
	}
	value, err := DecodeScalar(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if value != 4321 {
		t.Fatalf("value = %d", value)
	}
	if _, err := DecodeScalar([]byte{1, 2, 3, 4, 5}); err == nil {
		t.Fatal("a scalar with a non-zero tag must be rejected")
	}
	if _, err := DecodeScalar([]byte{0, 2}); err == nil {
		t.Fatal("a truncated scalar must be rejected")
	}
}

func TestDeviceInfoRoundTrip(t *testing.T) {
	fields := []DeviceInfoField{
		{Name: "name", Value: "妙播桥"},
		{Name: "version", Value: "65545"},
		{Name: "volume", Value: ""},
	}
	encoded, err := EncodeDeviceInfo(fields)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeDeviceInfo(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range fields {
		if got := decoded[field.Name]; got != field.Value {
			t.Fatalf("decoded[%q] = %q, want %q", field.Name, got, field.Value)
		}
	}
}

func TestDeviceInfoRejectsMalformedContainers(t *testing.T) {
	valid, err := EncodeDeviceInfo([]DeviceInfoField{{Name: "name", Value: "x"}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := DecodeDeviceInfo(valid[:2]); err == nil {
		t.Fatal("a truncated header must be rejected")
	}
	mismatched := append([]byte{}, valid...)
	mismatched[2]++ // declared body length no longer matches
	if _, err := DecodeDeviceInfo(mismatched); err == nil {
		t.Fatal("a mismatched declared length must be rejected")
	}
	badType := append([]byte{}, valid...)
	badType[3+1+4] = 0x0D // corrupt the value-type tag of the first field
	if _, err := DecodeDeviceInfo(badType); err == nil {
		t.Fatal("an unsupported value type must be rejected")
	}
	if _, err := EncodeDeviceInfo(nil); err == nil {
		t.Fatal("an empty field list must be rejected")
	}
	if _, err := EncodeDeviceInfo([]DeviceInfoField{{Name: "", Value: "x"}}); err == nil {
		t.Fatal("an empty field name must be rejected")
	}
	if _, err := EncodeDeviceInfo([]DeviceInfoField{{Name: "名", Value: "x"}}); err == nil {
		t.Fatal("a non-ASCII field name must be rejected")
	}

	// Two containers concatenated produce a duplicate-field error.
	dup := append(append([]byte{}, valid[3:]...), valid[3:]...)
	dupPayload := append([]byte{0, 0, byte(len(dup))}, dup...)
	if _, err := DecodeDeviceInfo(dupPayload); err == nil {
		t.Fatal("a duplicate field must be rejected")
	}
}

func TestEncodeNotifyScalar(t *testing.T) {
	payload := EncodeNotifyScalar("first-audiopcm", 1)
	want := append([]byte{byte(len("first-audiopcm"))}, "first-audiopcm"...)
	want = append(want, 0x03, 0x01)
	if !bytes.Equal(payload, want) {
		t.Fatalf("payload = % x, want % x", payload, want)
	}
}

func TestParseOpenDeviceRequest(t *testing.T) {
	request, err := ParseOpenDeviceRequest([]byte("wfd://192.168.1.20:7236?mirrorMode=0\x00"), false)
	if err != nil {
		t.Fatal(err)
	}
	if request.Host != "192.168.1.20" || request.Port != 7236 || request.MirrorMode != 0 {
		t.Fatalf("request = %+v", request)
	}

	// Senders that completed the Safety handshake may omit the NUL, but only
	// when the caller allows it.
	if _, err := ParseOpenDeviceRequest([]byte("wfd://192.168.1.20:7236?mirrorMode=1"), true); err != nil {
		t.Fatalf("allowMissingNul: %v", err)
	}
	if _, err := ParseOpenDeviceRequest([]byte("wfd://192.168.1.20:7236?mirrorMode=1"), false); err == nil {
		t.Fatal("a missing NUL must be rejected when it is required")
	}

	invalid := []string{
		"wfd://host:7236?mirrorMode=0\x00",
		"wfd://192.168.1.20?mirrorMode=0\x00",
		"wfd://192.168.1.20:0?mirrorMode=0\x00",
		"wfd://192.168.1.20:65536?mirrorMode=0\x00",
		"wfd://192.168.1.20:7236?mirrorMode=x\x00",
		"wfd://192.168.1.20:7236?mirrorMode=0\x00trailing",
		"wfd://192.168.1.2\x000:7236?mirrorMode=0\x00",
	}
	for _, raw := range invalid {
		if _, err := ParseOpenDeviceRequest([]byte(raw), false); err == nil {
			t.Fatalf("ParseOpenDeviceRequest(%q) must fail", raw)
		}
	}
}

func TestCommandString(t *testing.T) {
	if got := CmdSafetyInfo.String(); got != "0x1400" {
		t.Fatalf("CmdSafetyInfo = %q", got)
	}
	if got := CmdClose.String(); got != "0x0002" {
		t.Fatalf("CmdClose = %q", got)
	}
}

func TestIsProtocolErrorMatchesOnlyWireViolations(t *testing.T) {
	if IsProtocolError(nil) {
		t.Fatal("nil must not be a protocol error")
	}
	if !IsProtocolError(protocolErrorf("boom")) {
		t.Fatal("protocolErrorf result must match")
	}
	if IsProtocolError(errors.New("boom")) {
		t.Fatal("a plain error must not match")
	}
}
