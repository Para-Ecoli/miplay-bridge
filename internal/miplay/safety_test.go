package miplay

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestCRC32MPEG2KnownVectors(t *testing.T) {
	if got := crc32MPEG2(nil); got != 0xFFFFFFFF {
		t.Fatalf("crc32MPEG2(empty) = 0x%08X, want 0xFFFFFFFF", got)
	}
	if got := crc32MPEG2([]byte("123456789")); got != 0x0376E6E7 {
		t.Fatalf("crc32MPEG2(123456789) = 0x%08X, want 0x0376E6E7", got)
	}
}

func TestSafetyCipherRoundTripAndIVChaining(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("fedcba9876543210")
	encryptor, err := newSafetyCipher(key, iv)
	if err != nil {
		t.Fatal(err)
	}
	decryptor, err := newSafetyCipher(key, iv)
	if err != nil {
		t.Fatal(err)
	}

	first, err := encryptor.Encrypt([]byte("hello safety"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := decryptor.Decrypt(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "hello safety" {
		t.Fatalf("plaintext = %q", plaintext)
	}

	// The IV chains to the last ciphertext block: the same plaintext must
	// produce a different blob on the second call, and still decrypt.
	second, err := encryptor.Encrypt([]byte("hello safety"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("the CBC IV must advance between operations")
	}
	plaintext, err = decryptor.Decrypt(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "hello safety" {
		t.Fatalf("second plaintext = %q", plaintext)
	}
}

func TestSafetyCipherRejectsTampering(t *testing.T) {
	key := []byte("0123456789abcdef")
	encryptor, err := newSafetyCipher(key, key)
	if err != nil {
		t.Fatal(err)
	}
	decryptor, err := newSafetyCipher(key, key)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := encryptor.Encrypt([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decryptor.Decrypt(blob[:8]); err == nil {
		t.Fatal("a truncated blob must be rejected")
	}
	tampered := append([]byte{}, blob...)
	tampered[len(tampered)-1] ^= 0xFF
	fresh, err := newSafetyCipher(key, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Decrypt(tampered); err == nil {
		t.Fatal("a ciphertext with a broken integrity check must be rejected")
	}
}

func TestSafetyEnvelopeRoundTrip(t *testing.T) {
	payload := []byte(`{"result":"0"}`)
	envelope := safetyEnvelope(payload, false)
	decoded, err := decodeSafetyEnvelope(envelope, boolPointer(false))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded = %q", decoded)
	}
	if _, err := decodeSafetyEnvelope(envelope, boolPointer(true)); err == nil {
		t.Fatal("the command direction must not satisfy an acknowledgement")
	}
	if _, err := decodeSafetyEnvelope(envelope[:4], nil); err == nil {
		t.Fatal("a truncated envelope must be rejected")
	}
	badType := append([]byte{}, envelope...)
	badType[1+3] = 0x2F
	if _, err := decodeSafetyEnvelope(badType, boolPointer(false)); err == nil {
		t.Fatal("an unsupported value type must be rejected")
	}
}

func TestDeriveType1AuthKeyFixedVectors(t *testing.T) {
	cases := []struct {
		localHost string
		localPort int
		peerHost  string
		peerPort  int
		want      string
	}{
		{"127.0.0.1", 8899, "127.0.0.1", 54321, "935b8b69b84f0e21402eab9b3bf78e04"},
		{"192.168.1.10", 8899, "192.168.1.20", 54321, "cc29ed79c60e6ab0d592123a325b3797"},
	}
	for _, testCase := range cases {
		got := string(deriveType1AuthKey(testCase.localHost, testCase.localPort, testCase.peerHost, testCase.peerPort))
		if got != testCase.want {
			t.Fatalf("deriveType1AuthKey(%s,%d,%s,%d) = %q, want %q",
				testCase.localHost, testCase.localPort, testCase.peerHost, testCase.peerPort, got, testCase.want)
		}
	}
}

// ---------------------------------------------------------------------------
// mutual-authentication handshake simulation
// ---------------------------------------------------------------------------

func decodeOneCommandFrame(t *testing.T, wire []byte) CommandFrame {
	t.Helper()
	decoder := NewFrameDecoder()
	frames, err := decoder.Feed(wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	return frames[0]
}

func hmacSHA256Hex(key []byte, message string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// TestModernSafetyReceiverHandshakeAndBusinessFrames walks the full mutual
// authentication against a simulated phone, then exercises an encrypted
// business command in both directions.
func TestModernSafetyReceiverHandshakeAndBusinessFrames(t *testing.T) {
	const peerPort = 54321
	receiver, err := NewModernSafetyReceiver("127.0.0.1", 8899, "127.0.0.1", peerPort)
	if err != nil {
		t.Fatal(err)
	}
	authKey := deriveType1AuthKey("127.0.0.1", 8899, "127.0.0.1", peerPort)
	key := authKey[:16]
	// The phone decrypts receiver frames with the type-1 compatible IV (the
	// captured quirk), and encrypts its own frames with the selected type-2 IV.
	phoneInbound, err := newSafetyCipher(key, key)
	if err != nil {
		t.Fatal(err)
	}
	phoneOutbound, err := newSafetyCipher(key, authKey[16:])
	if err != nil {
		t.Fatal(err)
	}

	// Step 1: the phone offers SafetyInfo capabilities; the receiver answers
	// with the selection and its encrypted challenge.
	offer, err := json.Marshal(map[string]any{
		"authKeyTypes": 1, "authAlgorithmTypes": 4, "integrityTypes": 1, "aesKeyTypes": 1, "aesIvTypes": 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := receiver.AcceptInfo(CommandFrame{Command: CmdSafetyInfo, Sequence: 1, Payload: safetyEnvelope(offer, false)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.writes) != 2 {
		t.Fatalf("writes = %d, want 2", len(result.writes))
	}
	selectionFrame := decodeOneCommandFrame(t, result.writes[0])
	if selectionFrame.Command != CmdSafetyInfoAck {
		t.Fatalf("first write = %s", selectionFrame.Command)
	}
	selectionPayload, err := decodeSafetyEnvelope(selectionFrame.Payload, boolPointer(true))
	if err != nil {
		t.Fatal(err)
	}
	var selection map[string]string
	if err := json.Unmarshal(selectionPayload, &selection); err != nil {
		t.Fatal(err)
	}
	if selection["result"] != "0" || selection["aesIvType"] != "2" {
		t.Fatalf("selection = %v", selection)
	}

	challengeFrame := decodeOneCommandFrame(t, result.writes[1])
	if challengeFrame.Command != CmdSafetyAuth {
		t.Fatalf("second write = %s", challengeFrame.Command)
	}
	challengePlain, err := phoneInbound.Decrypt(challengeFrame.Payload)
	if err != nil {
		t.Fatalf("phone cannot decrypt the receiver challenge: %v", err)
	}
	challengeEnvelope, err := decodeSafetyEnvelope(challengePlain, boolPointer(false))
	if err != nil {
		t.Fatal(err)
	}
	var localChallenge safetyChallenge
	if err := json.Unmarshal(challengeEnvelope, &localChallenge); err != nil {
		t.Fatal(err)
	}
	if len(localChallenge.AuthMsg) != 32 {
		t.Fatalf("receiver challenge = %q", localChallenge.AuthMsg)
	}
	if diagnostics := receiver.Diagnostics(); diagnostics.Phase != "awaiting-mutual-auth" {
		t.Fatalf("phase = %q", diagnostics.Phase)
	}

	// Step 2: the phone sends its own challenge; the receiver acknowledges it.
	peerAuthMessage := "00112233445566778899aabbccddeeff"
	peerChallenge, err := json.Marshal(safetyChallenge{AuthMsg: peerAuthMessage})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := phoneOutbound.Encrypt(safetyEnvelope(peerChallenge, false))
	if err != nil {
		t.Fatal(err)
	}
	result, err = receiver.Process(CommandFrame{Command: CmdSafetyAuth, Sequence: 2, Payload: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.writes) != 1 {
		t.Fatalf("writes = %d, want the acknowledgement", len(result.writes))
	}
	ackFrame := decodeOneCommandFrame(t, result.writes[0])
	if ackFrame.Command != CmdSafetyAuthAck || ackFrame.Sequence != 2 {
		t.Fatalf("ack frame = %+v", ackFrame)
	}
	ackPlain, err := phoneInbound.Decrypt(ackFrame.Payload)
	if err != nil {
		t.Fatalf("phone cannot decrypt the peer-challenge acknowledgement: %v", err)
	}
	ackEnvelope, err := decodeSafetyEnvelope(ackPlain, boolPointer(true))
	if err != nil {
		t.Fatal(err)
	}
	var ack safetyAuthAck
	if err := json.Unmarshal(ackEnvelope, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Result != "0" || ack.AuthMsgAck != hmacSHA256Hex(authKey, peerAuthMessage) {
		t.Fatalf("ack = %+v", ack)
	}

	// Step 3: the phone acknowledges the receiver challenge (ascii-full key).
	ackBytes, err := json.Marshal(safetyAuthAck{Result: "0", AuthMsgAck: hmacSHA256Hex(authKey, localChallenge.AuthMsg)})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err = phoneOutbound.Encrypt(safetyEnvelope(ackBytes, true))
	if err != nil {
		t.Fatal(err)
	}
	result, err = receiver.Process(CommandFrame{Command: CmdSafetyAuthAck, Sequence: 3, Payload: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.writes) != 0 {
		t.Fatalf("writes = %d, want none for ascii-full acknowledgements", len(result.writes))
	}
	diagnostics := receiver.Diagnostics()
	if !diagnostics.MutualAuthComplete || diagnostics.Phase != "ready" {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
	if diagnostics.InboundIVMode != "type-2" || diagnostics.AuthKeyMode != "ascii-full" || diagnostics.PeerAckShape != "lower-hex-64" {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}

	// Step 4: an encrypted business command arrives and must surface
	// decrypted into the plaintext slot.
	businessPayload := EncodeScalar(55)
	encrypted, err = phoneOutbound.Encrypt(businessPayload)
	if err != nil {
		t.Fatal(err)
	}
	result, err = receiver.Process(CommandFrame{Command: CmdSetVolume, Sequence: 4, Payload: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	if result.plaintext == nil || result.plaintext.Command != CmdSetVolume || !bytes.Equal(result.plaintext.Payload, businessPayload) {
		t.Fatalf("plaintext = %+v", result.plaintext)
	}

	// Step 5: outbound frames wrap into the same cipher chain the phone
	// tracks after decrypting the challenge and the acknowledgement.
	wrapped, err := receiver.WrapWrites([][]byte{EncodeCommand(CmdSetVolumeAck, 4, businessPayload)})
	if err != nil {
		t.Fatal(err)
	}
	if len(wrapped) != 1 {
		t.Fatalf("wrapped = %d, want 1", len(wrapped))
	}
	outboundFrame := decodeOneCommandFrame(t, wrapped[0])
	if outboundFrame.Command != CmdSetVolumeAck {
		t.Fatalf("outbound frame = %+v", outboundFrame)
	}
	outboundPlain, err := phoneInbound.Decrypt(outboundFrame.Payload)
	if err != nil {
		t.Fatalf("phone cannot decrypt the wrapped outbound frame: %v", err)
	}
	if !bytes.Equal(outboundPlain, businessPayload) {
		t.Fatalf("outbound plaintext = % x", outboundPlain)
	}
}

func TestModernSafetyReceiverRejectsDisorderedMessages(t *testing.T) {
	receiver, err := NewModernSafetyReceiver("127.0.0.1", 8899, "127.0.0.1", 54321)
	if err != nil {
		t.Fatal(err)
	}
	// A business command before mutual authentication must stop the session.
	if _, err := receiver.Process(CommandFrame{Command: CmdGetState, Sequence: 1, Payload: []byte("x")}); err == nil {
		t.Fatal("a pre-authentication business command must be rejected")
	}
	// AcceptInfo only accepts SafetyInfo.
	if _, err := receiver.AcceptInfo(CommandFrame{Command: CmdSafetyAuth, Sequence: 1}); err == nil {
		t.Fatal("AcceptInfo must reject non-SafetyInfo commands")
	}
	// An unsupported capability offer must be rejected.
	offer, err := json.Marshal(map[string]any{
		"authKeyTypes": 1, "authAlgorithmTypes": 4, "integrityTypes": 1, "aesKeyTypes": 1, "aesIvTypes": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.AcceptInfo(CommandFrame{Command: CmdSafetyInfo, Sequence: 1, Payload: safetyEnvelope(offer, false)}); err == nil {
		t.Fatal("an offer without aesIvType 2 must be rejected")
	}
}
