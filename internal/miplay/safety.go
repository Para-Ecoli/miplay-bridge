package miplay

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // protocol key derivation, fixed by the wire contract
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// safetyValueType is the value-type tag of a SafetyData JSON envelope.
const safetyValueType = 0x1E

// safetyHeaderSize is the fixed SafetyData blob header: header length (2) +
// version (1) + flags (1) + padding (1) + integrity (4).
const safetyHeaderSize = 9

const safetyFlags = 0xE0

// safetySelection is the receiver capability selection returned in the
// SAFETY_INFO acknowledgement. Field order matters for byte-identical
// reproduction of the captured traffic.
type safetySelection struct {
	Result            string `json:"result"`
	AuthKeyType       string `json:"authKeyType"`
	AuthAlgorithmType string `json:"authAlgorithmType"`
	IntegrityType     string `json:"integrityType"`
	AesKeyType        string `json:"aesKeyType"`
	AesIvType         string `json:"aesIvType"`
}

var receiverSelection = safetySelection{
	Result:            "0",
	AuthKeyType:       "1",
	AuthAlgorithmType: "4",
	IntegrityType:     "1",
	AesKeyType:        "1",
	AesIvType:         "2",
}

type safetyChallenge struct {
	AuthMsg string `json:"authMsg"`
}

type safetyAuthAck struct {
	Result     string `json:"result"`
	AuthMsgAck string `json:"authMsgAck"`
}

// crc32MPEG2 computes CRC-32/MPEG-2 (polynomial 0x04C11DB7, initial value
// 0xFFFFFFFF, no output xor, non-reflected). Go's hash/crc32 does not offer
// this variant.
func crc32MPEG2(data []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, value := range data {
		crc ^= uint32(value) << 24
		for bit := 0; bit < 8; bit++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// safetyCipher is a stateful AES-128-CBC codec. Each direction of the channel
// has its own instance and the IV chains to the last ciphertext block. The IV
// advances only after a successful operation so failed probe attempts leave
// the state untouched.
type safetyCipher struct {
	key []byte
	iv  []byte
}

func newSafetyCipher(key, iv []byte) (*safetyCipher, error) {
	if len(key) != 16 || len(iv) != 16 {
		return nil, protocolErrorf("SafetyData requires a 16-byte AES key and IV")
	}
	instance := &safetyCipher{key: append([]byte(nil), key...), iv: append([]byte(nil), iv...)}
	return instance, nil
}

// Encrypt returns the full SafetyData blob: 9-byte header plus ciphertext,
// with zero padding of 1..16 bytes.
func (c *safetyCipher) Encrypt(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, fmt.Errorf("safety aes: %w", err)
	}
	padding := 16 - len(plaintext)%16
	padded := make([]byte, len(plaintext)+padding)
	copy(padded, plaintext)
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, c.iv).CryptBlocks(ciphertext, padded)
	c.iv = append(c.iv[:0], ciphertext[len(ciphertext)-16:]...)

	blob := make([]byte, safetyHeaderSize+len(ciphertext))
	binary.BigEndian.PutUint16(blob[0:2], 7)
	blob[2] = 1
	blob[3] = safetyFlags
	blob[4] = byte(padding)
	binary.LittleEndian.PutUint32(blob[5:9], crc32MPEG2(ciphertext))
	copy(blob[safetyHeaderSize:], ciphertext)
	return blob, nil
}

// Decrypt validates and decrypts a SafetyData blob.
func (c *safetyCipher) Decrypt(blob []byte) ([]byte, error) {
	if len(blob) < safetyHeaderSize+16 {
		return nil, protocolErrorf("SafetyData is truncated")
	}
	headerLength := binary.BigEndian.Uint16(blob[0:2])
	version := blob[2]
	flags := blob[3]
	padding := int(blob[4])
	integrity := binary.LittleEndian.Uint32(blob[5:9])
	ciphertext := blob[safetyHeaderSize:]
	if headerLength != 7 || version != 1 || flags != safetyFlags ||
		padding < 1 || padding > 16 || len(ciphertext)%16 != 0 ||
		crc32MPEG2(ciphertext) != integrity {
		return nil, protocolErrorf("SafetyData header or integrity is invalid")
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, fmt.Errorf("safety aes: %w", err)
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, c.iv).CryptBlocks(plaintext, ciphertext)
	if padding > len(plaintext) || !allZero(plaintext[len(plaintext)-padding:]) {
		return nil, protocolErrorf("SafetyData padding is invalid")
	}
	c.iv = append(c.iv[:0], ciphertext[len(ciphertext)-16:]...)
	return plaintext[:len(plaintext)-padding], nil
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

// safetyEnvelope encodes [tag length][tag "cmd"/"ack"][0x1E][length u32]
// [payload].
func safetyEnvelope(payload []byte, acknowledgement bool) []byte {
	tag := "cmd"
	if acknowledgement {
		tag = "ack"
	}
	envelope := make([]byte, 0, 5+len(tag)+len(payload))
	envelope = append(envelope, byte(len(tag)))
	envelope = append(envelope, tag...)
	envelope = append(envelope, safetyValueType)
	envelope = binary.BigEndian.AppendUint32(envelope, uint32(len(payload)))
	return append(envelope, payload...)
}

// decodeSafetyEnvelope parses and validates an envelope. When
// acknowledgement is non-nil the tag must match the expected direction.
func decodeSafetyEnvelope(data []byte, acknowledgement *bool) ([]byte, error) {
	if len(data) < 9 {
		return nil, protocolErrorf("Safety envelope is truncated")
	}
	tagLength := int(data[0])
	headerLength := 1 + tagLength + 1 + 4
	if len(data) < headerLength {
		return nil, protocolErrorf("Safety envelope header is truncated")
	}
	tag := string(data[1 : 1+tagLength])
	if tag != "cmd" && tag != "ack" {
		return nil, protocolErrorf("Safety envelope tag is invalid")
	}
	if acknowledgement != nil && (tag == "ack") != *acknowledgement {
		return nil, protocolErrorf("Safety envelope direction is invalid")
	}
	if data[1+tagLength] != safetyValueType {
		return nil, protocolErrorf("Safety envelope value type is unsupported")
	}
	payloadLength := binary.BigEndian.Uint32(data[2+tagLength : headerLength])
	if payloadLength > MaxPayload || len(data) != headerLength+int(payloadLength) {
		return nil, protocolErrorf("Safety envelope length is invalid")
	}
	return data[headerLength:], nil
}

// deriveType1AuthKey derives the shared key material from the TCP endpoints:
// localIP | localPort | peerIP | peerPort with digits 0-9 translated to
// a-j, hashed with MD5 and rendered as 32 lowercase ASCII hex characters.
func deriveType1AuthKey(localHost string, localPort int, peerHost string, peerPort int) []byte {
	translation := strings.NewReplacer(
		"0", "a", "1", "b", "2", "c", "3", "d", "4", "e",
		"5", "f", "6", "g", "7", "h", "8", "i", "9", "j",
	)
	material := fmt.Sprintf("%s%d%s%d", localHost, localPort, peerHost, peerPort)
	digest := md5.Sum([]byte(translation.Replace(material))) //nolint:gosec // protocol contract
	return []byte(hex.EncodeToString(digest[:]))
}

type decryptCandidate struct {
	mode   string
	cipher *safetyCipher
}

// SafetyDiagnostics exposes the encrypted-channel state for logs and
// /api/status.
type SafetyDiagnostics struct {
	Phase              string `json:"phase"`
	MutualAuthComplete bool   `json:"mutual_auth_complete"`
	InboundIVMode      string `json:"inbound_iv_mode,omitempty"`
	OutboundIVMode     string `json:"outbound_iv_mode,omitempty"`
	AuthKeyMode        string `json:"auth_key_mode,omitempty"`
	PeerAckShape       string `json:"peer_ack_shape,omitempty"`
	PeerAckResult      string `json:"peer_ack_result,omitempty"`
}

// safetyResult carries the frames a safety step wants written to the control
// connection plus, for business commands, the decrypted frame.
type safetyResult struct {
	writes    [][]byte
	plaintext *CommandFrame
}

// ModernSafetyReceiver is one mutual-authentication and encrypted
// business-command session on the control connection.
type ModernSafetyReceiver struct {
	authKey []byte

	encrypt *safetyCipher

	decryptCandidates []decryptCandidate
	decrypt           *safetyCipher
	inboundIVMode     string

	localAuthMessage    string
	peerAuthMessage     string
	peerAuthSequence    uint16
	hasPeerAuthSequence bool
	authKeyMode         string
	peerAckShape        string
	peerAckResult       string

	peerChallengeAcknowledged bool
	localChallengeVerified    bool
	phase                     string
}

// NewModernSafetyReceiver builds the encrypted channel for one TCP session.
func NewModernSafetyReceiver(localHost string, localPort int, peerHost string, peerPort int) (*ModernSafetyReceiver, error) {
	authKey := deriveType1AuthKey(localHost, localPort, peerHost, peerPort)
	key := authKey[:16]
	nominalIV := authKey[16:]

	encryptCipher, err := newSafetyCipher(key, key)
	if err != nil {
		return nil, err
	}
	// Real receivers keep the type-1 material as the initial outbound IV even
	// after selecting aesIvType=2, and phone sources mirror that quirk when
	// decrypting a receiver challenge.
	type2, err := newSafetyCipher(key, nominalIV)
	if err != nil {
		return nil, err
	}
	type1Compat, err := newSafetyCipher(key, key)
	if err != nil {
		return nil, err
	}
	return &ModernSafetyReceiver{
		authKey: authKey,
		encrypt: encryptCipher,
		decryptCandidates: []decryptCandidate{
			{mode: "type-2", cipher: type2},
			{mode: "type-1-compat", cipher: type1Compat},
		},
		phase: "created",
	}, nil
}

// Diagnostics returns the channel state.
func (r *ModernSafetyReceiver) Diagnostics() SafetyDiagnostics {
	return SafetyDiagnostics{
		Phase:              r.phase,
		MutualAuthComplete: r.mutualAuthComplete(),
		InboundIVMode:      r.inboundIVMode,
		OutboundIVMode:     "type-1-compat",
		AuthKeyMode:        r.authKeyMode,
		PeerAckShape:       r.peerAckShape,
		PeerAckResult:      r.peerAckResult,
	}
}

func (r *ModernSafetyReceiver) mutualAuthComplete() bool {
	return r.peerChallengeAcknowledged && r.localChallengeVerified
}

func (r *ModernSafetyReceiver) updatePhase() {
	if r.mutualAuthComplete() {
		r.phase = "ready"
	} else {
		r.phase = "awaiting-mutual-auth"
	}
}

// AcceptInfo handles the SAFETY_INFO offer: validate the advertised
// capability masks, return the selection acknowledgement and the encrypted
// receiver challenge.
func (r *ModernSafetyReceiver) AcceptInfo(frame CommandFrame) (safetyResult, error) {
	if r.phase != "created" || frame.Command != CmdSafetyInfo {
		return safetyResult{}, protocolErrorf("unexpected SafetyInfo command")
	}
	plaintext, err := decodeSafetyEnvelope(frame.Payload, boolPointer(false))
	if err != nil {
		return safetyResult{}, err
	}
	offer, err := parseSafetyOffer(plaintext)
	if err != nil {
		return safetyResult{}, err
	}
	required := []struct {
		field    string
		selected uint32
	}{
		{"authKeyTypes", 1},
		{"authAlgorithmTypes", 4},
		{"integrityTypes", 1},
		{"aesKeyTypes", 1},
		{"aesIvTypes", 2},
	}
	for _, item := range required {
		value, ok := offer[item.field]
		if !ok {
			return safetyResult{}, protocolErrorf("SafetyInfo offer omits %s", item.field)
		}
		mask, err := maskValue(value, item.field)
		if err != nil {
			return safetyResult{}, err
		}
		if mask&item.selected != item.selected {
			return safetyResult{}, protocolErrorf("SafetyInfo %s does not support the receiver selection", item.field)
		}
	}

	timestampMicros := time.Now().UnixMicro()
	localDigest := md5.Sum([]byte(strconv.FormatInt(timestampMicros, 10))) //nolint:gosec // protocol contract
	r.localAuthMessage = hex.EncodeToString(localDigest[:])

	selectionBytes, err := json.Marshal(receiverSelection)
	if err != nil {
		return safetyResult{}, fmt.Errorf("encode safety selection: %w", err)
	}
	selectionEnvelope := safetyEnvelope(selectionBytes, true)

	challengeBytes, err := json.Marshal(safetyChallenge{AuthMsg: r.localAuthMessage})
	if err != nil {
		return safetyResult{}, fmt.Errorf("encode safety challenge: %w", err)
	}
	encryptedChallenge, err := r.encrypt.Encrypt(safetyEnvelope(challengeBytes, false))
	if err != nil {
		return safetyResult{}, err
	}

	r.phase = "awaiting-mutual-auth"
	return safetyResult{writes: [][]byte{
		EncodeCommand(CmdSafetyInfoAck, frame.Sequence, selectionEnvelope),
		EncodeCommand(CmdSafetyAuth, 0, encryptedChallenge),
	}}, nil
}

// Process handles SAFETY_AUTH / SAFETY_AUTH_ACK and, once the mutual
// authentication is complete, decrypted business commands.
func (r *ModernSafetyReceiver) Process(frame CommandFrame) (safetyResult, error) {
	switch frame.Command {
	case CmdSafetyAuth:
		if r.peerChallengeAcknowledged {
			return safetyResult{}, protocolErrorf("duplicate peer SafetyAuth challenge")
		}
		payload, err := r.decryptEnvelope(frame.Payload, false)
		if err != nil {
			return safetyResult{}, err
		}
		var challenge safetyChallenge
		if err := json.Unmarshal(payload, &challenge); err != nil {
			return safetyResult{}, protocolErrorf("peer SafetyAuth payload is not JSON")
		}
		if len(challenge.AuthMsg) != 32 {
			return safetyResult{}, protocolErrorf("peer SafetyAuth challenge is invalid")
		}
		r.peerAuthMessage = challenge.AuthMsg
		r.peerAuthSequence = frame.Sequence
		r.hasPeerAuthSequence = true
		digest := r.authDigest(challenge.AuthMsg, "ascii-full")
		ackBytes, err := json.Marshal(safetyAuthAck{Result: "0", AuthMsgAck: digest})
		if err != nil {
			return safetyResult{}, fmt.Errorf("encode safety auth ack: %w", err)
		}
		encryptedAck, err := r.encrypt.Encrypt(safetyEnvelope(ackBytes, true))
		if err != nil {
			return safetyResult{}, err
		}
		r.peerChallengeAcknowledged = true
		r.updatePhase()
		return safetyResult{writes: [][]byte{
			EncodeCommand(CmdSafetyAuthAck, frame.Sequence, encryptedAck),
		}}, nil

	case CmdSafetyAuthAck:
		if r.localAuthMessage == "" || r.localChallengeVerified {
			return safetyResult{}, protocolErrorf("unsolicited or duplicate SafetyAuth acknowledgement")
		}
		payload, err := r.decryptEnvelope(frame.Payload, true)
		if err != nil {
			return safetyResult{}, err
		}
		var acknowledgement safetyAuthAck
		if err := json.Unmarshal(payload, &acknowledgement); err != nil {
			return safetyResult{}, protocolErrorf("peer SafetyAuth acknowledgement is not JSON")
		}
		r.peerAckResult = acknowledgement.Result
		received := acknowledgement.AuthMsgAck
		r.peerAckShape = classifyAckShape(received)
		mode := ""
		if received != "" {
			mode = r.matchAuthAck(received)
		}
		if (acknowledgement.Result != "0" && acknowledgement.Result != "1") || mode == "" {
			return safetyResult{}, protocolErrorf("peer SafetyAuth acknowledgement failed verification")
		}
		r.authKeyMode = mode
		r.localChallengeVerified = true
		r.updatePhase()
		// Some phone sources acknowledge with a different native key
		// representation; repeat the peer acknowledgement immediately with
		// that representation and continued CBC state.
		if mode != "ascii-full" && r.peerAuthMessage != "" {
			digest := r.authDigest(r.peerAuthMessage, mode)
			ackBytes, err := json.Marshal(safetyAuthAck{Result: "0", AuthMsgAck: digest})
			if err != nil {
				return safetyResult{}, fmt.Errorf("encode safety re-ack: %w", err)
			}
			encryptedAck, err := r.encrypt.Encrypt(safetyEnvelope(ackBytes, true))
			if err != nil {
				return safetyResult{}, err
			}
			sequence := frame.Sequence
			if r.hasPeerAuthSequence {
				sequence = r.peerAuthSequence
			}
			return safetyResult{writes: [][]byte{
				EncodeCommand(CmdSafetyAuthAck, sequence, encryptedAck),
			}}, nil
		}
		return safetyResult{}, nil

	default:
		if !r.mutualAuthComplete() {
			return safetyResult{}, protocolErrorf("business command arrived before mutual SafetyAuth")
		}
		if r.decrypt == nil {
			return safetyResult{}, protocolErrorf("SafetyData inbound cipher is not selected")
		}
		plaintext, err := r.decrypt.Decrypt(frame.Payload)
		if err != nil {
			return safetyResult{}, err
		}
		return safetyResult{plaintext: &CommandFrame{
			Command:  frame.Command,
			Sequence: frame.Sequence,
			Payload:  plaintext,
		}}, nil
	}
}

// WrapWrites encrypts outbound business frames: each encoded command frame is
// parsed and re-emitted with an encrypted payload.
func (r *ModernSafetyReceiver) WrapWrites(writes [][]byte) ([][]byte, error) {
	wrapped := make([][]byte, 0, len(writes))
	decoder := NewFrameDecoder()
	for _, wire := range writes {
		frames, err := decoder.Feed(wire)
		if err != nil {
			return nil, err
		}
		if len(frames) != 1 {
			return nil, protocolErrorf("outbound command encoding is incomplete")
		}
		encrypted, err := r.encrypt.Encrypt(frames[0].Payload)
		if err != nil {
			return nil, err
		}
		wrapped = append(wrapped, EncodeCommand(frames[0].Command, frames[0].Sequence, encrypted))
	}
	return wrapped, nil
}

// decryptEnvelope decrypts a blob with the selected inbound cipher, or probes
// the candidate IV modes and locks onto the first one that validates.
func (r *ModernSafetyReceiver) decryptEnvelope(payload []byte, acknowledgement bool) ([]byte, error) {
	if r.decrypt != nil {
		plaintext, err := r.decrypt.Decrypt(payload)
		if err != nil {
			return nil, err
		}
		return decodeSafetyEnvelope(plaintext, boolPointer(acknowledgement))
	}
	var lastErr error
	for _, candidate := range r.decryptCandidates {
		plaintext, err := candidate.cipher.Decrypt(payload)
		if err != nil {
			lastErr = err
			continue
		}
		decoded, err := decodeSafetyEnvelope(plaintext, boolPointer(acknowledgement))
		if err != nil {
			lastErr = err
			continue
		}
		r.decrypt = candidate.cipher
		r.inboundIVMode = candidate.mode
		r.decryptCandidates = nil
		return decoded, nil
	}
	if lastErr == nil {
		lastErr = protocolErrorf("SafetyAuth payload did not decrypt with supported IV modes")
	}
	return nil, fmt.Errorf("SafetyAuth payload did not decrypt with supported IV modes: %w", lastErr)
}

// matchAuthAck verifies the peer acknowledgement of our challenge, trying the
// three native key representations in order.
func (r *ModernSafetyReceiver) matchAuthAck(received string) string {
	for _, mode := range []string{"ascii-full", "ascii-half", "binary-md5"} {
		expected := r.authDigest(r.localAuthMessage, mode)
		if hmac.Equal([]byte(strings.ToLower(received)), []byte(expected)) {
			return mode
		}
	}
	return ""
}

// authDigest computes HMAC-SHA256 over the challenge with one of the three
// native key representations.
func (r *ModernSafetyReceiver) authDigest(message string, mode string) string {
	var key []byte
	switch mode {
	case "ascii-full":
		key = r.authKey
	case "ascii-half":
		key = r.authKey[:16]
	case "binary-md5":
		decoded, err := hex.DecodeString(string(r.authKey))
		if err != nil {
			return ""
		}
		key = decoded
	default:
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

func classifyAckShape(received string) string {
	if received == "" {
		return "non-string"
	}
	if len(received) == 64 && isLowerHex(received) {
		return "lower-hex-64"
	}
	if len(received) == 64 && isUpperHex(received) {
		return "upper-hex-64"
	}
	return fmt.Sprintf("other-%d", len(received))
}

func isLowerHex(value string) bool {
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func isUpperHex(value string) bool {
	for _, character := range value {
		if !strings.ContainsRune("0123456789ABCDEF", character) {
			return false
		}
	}
	return true
}

// parseSafetyOffer decodes the offer object keeping raw JSON numbers.
func parseSafetyOffer(payload []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var offer map[string]any
	if err := decoder.Decode(&offer); err != nil {
		return nil, protocolErrorf("Safety payload is not JSON")
	}
	return offer, nil
}

// maskValue converts an offer field to a uint32 capability bitmask, accepting
// both JSON numbers and numeric strings like the reference decoder.
func maskValue(value any, field string) (uint32, error) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseUint(typed.String(), 10, 32)
		if err != nil {
			return 0, protocolErrorf("SafetyInfo %s is invalid", field)
		}
		return uint32(parsed), nil
	case float64:
		if typed < 0 || typed > 0xFFFFFFFF || typed != float64(uint64(typed)) {
			return 0, protocolErrorf("SafetyInfo %s is out of range", field)
		}
		return uint32(typed), nil
	case string:
		parsed, err := strconv.ParseUint(typed, 10, 32)
		if err != nil {
			return 0, protocolErrorf("SafetyInfo %s is invalid", field)
		}
		return uint32(parsed), nil
	default:
		return 0, protocolErrorf("SafetyInfo %s is invalid", field)
	}
}

func boolPointer(value bool) *bool { return &value }
