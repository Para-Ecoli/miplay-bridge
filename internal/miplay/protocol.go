// Package miplay implements the Xiaomi 妙播 (MiPlay) receiver: mDNS
// advertising, the binary TCP control protocol on port 8899, the optional
// SafetyData encrypted channel and the reverse-WFD realtime audio pipeline.
//
// The wire behaviour implemented here was recovered from publicly documented
// capture-driven research (see README "协议研究与致谢"). The protocol is
// proprietary and undocumented by Xiaomi; the implementation is an
// independent clean-room Go rewrite of the observed byte contracts, written
// against the constraints captured in the reference notes. No third-party
// protocol code is vendored.
package miplay

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // required by the legacy challenge, not for security
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// Magic is the first byte of every MiPlay command frame and of every wire
// media frame.
const Magic = 0x24

// FrameHeaderSize is the fixed command-frame header size: magic (1) +
// command (2) + sequence (2) + payload length (4).
const FrameHeaderSize = 9

// MaxPayload bounds a single command payload. Anything larger is treated as a
// stream corruption and tears the session down instead of allocating.
const MaxPayload = 4 * 1024 * 1024

// deviceInfoString is the value-type tag of a device-info string field.
const deviceInfoString = 0x0C

// protocolError marks every wire-contract violation. A protocol error on a
// control connection ends the session; it never panics.
type protocolError struct{ reason string }

func (e *protocolError) Error() string { return e.reason }

func protocolErrorf(format string, args ...any) error {
	return &protocolError{reason: fmt.Sprintf(format, args...)}
}

// IsProtocolError reports whether err is a wire-contract violation.
func IsProtocolError(err error) bool {
	var target *protocolError
	return errors.As(err, &target)
}

// Command is one MiPlay control command id.
type Command uint16

// The recovered command table.
const (
	CmdOpen                      Command = 0x0000
	CmdClose                     Command = 0x0002
	CmdPause                     Command = 0x0004
	CmdPauseAck                  Command = 0x0005
	CmdResume                    Command = 0x0006
	CmdResumeAck                 Command = 0x0007
	CmdSetVolume                 Command = 0x000C
	CmdSetVolumeAck              Command = 0x000D
	CmdGetVolume                 Command = 0x000E
	CmdGetVolumeAck              Command = 0x000F
	CmdGetPosition               Command = 0x0010
	CmdGetPositionAck            Command = 0x0011
	CmdSetMediaInfo              Command = 0x0012
	CmdSetMediaInfoAck           Command = 0x0013
	CmdGetMediaInfo              Command = 0x0014
	CmdGetMediaInfoAck           Command = 0x0015
	CmdHeartbeat                 Command = 0x001A
	CmdHeartbeatAck              Command = 0x001B
	CmdGetState                  Command = 0x001C
	CmdGetStateAck               Command = 0x001D
	CmdGetDeviceInfo             Command = 0x001E
	CmdGetDeviceInfoAck          Command = 0x001F
	CmdNotify                    Command = 0x0022
	CmdLegacyChallenge           Command = 0x0028
	CmdLegacyChallengeAck        Command = 0x0029
	CmdAddMirror                 Command = 0x002E
	CmdAddMirrorAck              Command = 0x002F
	CmdGetMirrorMode             Command = 0x0034
	CmdGetMirrorModeAck          Command = 0x0035
	CmdSourceVersion             Command = 0x0036
	CmdSourceVersionAck          Command = 0x0037
	CmdSetPlaySource             Command = 0x0040
	CmdSetPlaySourceAck          Command = 0x0041
	CmdSetLocalDeviceInfo        Command = 0x0058
	CmdSetLocalDeviceInfoAck     Command = 0x0059
	CmdSourceCapabilityUpdate    Command = 0x0416
	CmdSourceCapabilityUpdateAck Command = 0x0417
	CmdSafetyInfo                Command = 0x1400
	CmdSafetyInfoAck             Command = 0x1401
	CmdSafetyAuth                Command = 0x1402
	CmdSafetyAuthAck             Command = 0x1403
)

// String renders the command for logs, e.g. "0x1400".
func (c Command) String() string {
	return fmt.Sprintf("0x%04x", uint16(c))
}

// CommandFrame is one decoded control frame.
type CommandFrame struct {
	Command  Command
	Sequence uint16
	Payload  []byte
}

// EncodeCommand serialises a command frame.
func EncodeCommand(command Command, sequence uint16, payload []byte) []byte {
	frame := make([]byte, FrameHeaderSize+len(payload))
	frame[0] = Magic
	binary.BigEndian.PutUint16(frame[1:3], uint16(command))
	binary.BigEndian.PutUint16(frame[3:5], sequence)
	binary.BigEndian.PutUint32(frame[5:9], uint32(len(payload)))
	copy(frame[FrameHeaderSize:], payload)
	return frame
}

// FrameDecoder incrementally splits a TCP byte stream into command frames.
type FrameDecoder struct {
	buffer []byte
}

// NewFrameDecoder returns an empty decoder.
func NewFrameDecoder() *FrameDecoder { return &FrameDecoder{buffer: make([]byte, 0, 4096)} }

// Feed appends data and returns every complete frame. A framing violation
// clears the buffer and returns a protocol error.
func (d *FrameDecoder) Feed(data []byte) ([]CommandFrame, error) {
	d.buffer = append(d.buffer, data...)
	frames := []CommandFrame{}
	for len(d.buffer) >= FrameHeaderSize {
		if source := d.buffer[0]; source != Magic {
			d.buffer = d.buffer[:0]
			return nil, protocolErrorf("invalid command-frame magic 0x%02x", source)
		}
		command := binary.BigEndian.Uint16(d.buffer[1:3])
		sequence := binary.BigEndian.Uint16(d.buffer[3:5])
		payloadLength := binary.BigEndian.Uint32(d.buffer[5:9])
		if payloadLength > MaxPayload {
			d.buffer = d.buffer[:0]
			return nil, protocolErrorf("command payload of %d bytes exceeds the safety limit", payloadLength)
		}
		frameLength := FrameHeaderSize + int(payloadLength)
		if len(d.buffer) < frameLength {
			break
		}
		payload := make([]byte, payloadLength)
		copy(payload, d.buffer[FrameHeaderSize:frameLength])
		d.buffer = append(d.buffer[:0], d.buffer[frameLength:]...)
		frames = append(frames, CommandFrame{Command: Command(command), Sequence: sequence, Payload: payload})
	}
	return frames, nil
}

// LegacyChallengeResponse computes the pre-shared 0x0029 answer: the lowercase
// HMAC-SHA1 hex digest keyed by the ASCII hex md5 of "0.0.0.0".
func LegacyChallengeResponse(challenge []byte) []byte {
	legacyKey := md5.Sum([]byte("0.0.0.0")) //nolint:gosec // protocol constant, not a security primitive
	key := hex.EncodeToString(legacyKey[:])
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write(challenge)
	return []byte(hex.EncodeToString(mac.Sum(nil)))
}

// GenerateLegacyChallenge returns the session challenge: exactly 15 ASCII
// digits so it fits the observed 12..17 digit window.
func GenerateLegacyChallenge() ([]byte, error) {
	lower := new(big.Int).Exp(big.NewInt(10), big.NewInt(14), nil)
	span := new(big.Int).Mul(big.NewInt(9), lower)
	value, err := rand.Int(rand.Reader, span)
	if err != nil {
		return nil, fmt.Errorf("generate legacy challenge: %w", err)
	}
	value.Add(value, lower)
	return []byte(value.String()), nil
}

// ---------------------------------------------------------------------------
// scalars, device info and notifications
// ---------------------------------------------------------------------------

// EncodeScalar encodes the five-byte scalar shape: 0x00 followed by a
// big-endian uint32.
func EncodeScalar(value uint32) []byte {
	encoded := make([]byte, 5)
	binary.BigEndian.PutUint32(encoded[1:], value)
	return encoded
}

// DecodeScalar decodes a five-byte scalar.
func DecodeScalar(payload []byte) (uint32, error) {
	if len(payload) != 5 || payload[0] != 0 {
		return 0, protocolErrorf("invalid five-byte scalar")
	}
	return binary.BigEndian.Uint32(payload[1:]), nil
}

// DeviceInfoField is one name/value pair of a device-info body.
type DeviceInfoField struct {
	Name  string
	Value string
}

// EncodeDeviceInfo serialises the observed device-info container: a
// three-byte big-endian length followed by repeated
// [name length][name][0x0C][value length u16][value] fields.
func EncodeDeviceInfo(fields []DeviceInfoField) ([]byte, error) {
	if len(fields) == 0 {
		return nil, protocolErrorf("device info requires at least one field")
	}
	body := make([]byte, 0, 64)
	for _, field := range fields {
		if !isASCII(field.Name) || len(field.Name) == 0 || len(field.Name) > 255 {
			return nil, protocolErrorf("device-info name %q is not a valid ASCII field name", field.Name)
		}
		value := []byte(field.Value)
		if len(value) > 0xFFFF {
			return nil, protocolErrorf("device-info value %q is too long", field.Name)
		}
		body = append(body, byte(len(field.Name)))
		body = append(body, field.Name...)
		body = append(body, deviceInfoString)
		body = binary.BigEndian.AppendUint16(body, uint16(len(value)))
		body = append(body, value...)
	}
	if len(body) > 0xFFFFFF {
		return nil, protocolErrorf("device-info body exceeds the three-byte length container")
	}
	encoded := make([]byte, 3, 3+len(body))
	encoded[0] = byte(len(body) >> 16)
	encoded[1] = byte(len(body) >> 8)
	encoded[2] = byte(len(body))
	return append(encoded, body...), nil
}

// DecodeDeviceInfo parses a device-info container.
func DecodeDeviceInfo(payload []byte) (map[string]string, error) {
	if len(payload) < 3 {
		return nil, protocolErrorf("device-info length header is truncated")
	}
	bodyLength := int(payload[0])<<16 | int(payload[1])<<8 | int(payload[2])
	if len(payload) != 3+bodyLength {
		return nil, protocolErrorf("device-info declared length does not match the payload")
	}
	result := map[string]string{}
	offset := 3
	for offset < len(payload) {
		nameLength := int(payload[offset])
		offset++
		if nameLength == 0 || offset+nameLength+3 > len(payload) {
			return nil, protocolErrorf("device-info field length is invalid")
		}
		name := string(payload[offset : offset+nameLength])
		offset += nameLength
		if payload[offset] != deviceInfoString {
			return nil, protocolErrorf("unsupported device-info value type 0x%02x", payload[offset])
		}
		offset++
		valueLength := int(binary.BigEndian.Uint16(payload[offset : offset+2]))
		offset += 2
		if offset+valueLength > len(payload) {
			return nil, protocolErrorf("device-info value length is truncated")
		}
		if _, duplicate := result[name]; duplicate {
			return nil, protocolErrorf("duplicate device-info field %q", name)
		}
		result[name] = string(payload[offset : offset+valueLength])
		offset += valueLength
	}
	return result, nil
}

// EncodeNotifyScalar encodes a NOTIFY payload: [label length][label][0x03]
// [single-byte value].
func EncodeNotifyScalar(label string, value byte) []byte {
	payload := make([]byte, 0, len(label)+3)
	payload = append(payload, byte(len(label)))
	payload = append(payload, label...)
	payload = append(payload, 0x03, value)
	return payload
}

// ---------------------------------------------------------------------------
// OPEN request
// ---------------------------------------------------------------------------

var openURLPattern = regexp.MustCompile(`^wfd://([^:/?]+):([0-9]+)\?mirrorMode=([0-9]+)$`)

// OpenDeviceRequest is the parsed payload of a 0x0000 OPEN command: the
// reverse-WFD endpoint the receiver must dial back.
type OpenDeviceRequest struct {
	Host       string
	Port       int
	MirrorMode int
}

// ParseOpenDeviceRequest parses `wfd://<ipv4>:<port>?mirrorMode=<n>` with an
// optional trailing NUL (senders that completed the Safety handshake may omit
// it).
func ParseOpenDeviceRequest(payload []byte, allowMissingNul bool) (OpenDeviceRequest, error) {
	content := payload
	if hasNul := len(content) > 0 && content[len(content)-1] == 0; hasNul {
		content = content[:len(content)-1]
	} else if !allowMissingNul {
		return OpenDeviceRequest{}, protocolErrorf("Open payload must have one NUL terminator")
	}
	if strings.IndexByte(string(content), 0) >= 0 {
		return OpenDeviceRequest{}, protocolErrorf("Open payload contains an embedded NUL")
	}
	match := openURLPattern.FindStringSubmatch(string(content))
	if match == nil {
		return OpenDeviceRequest{}, protocolErrorf("invalid Open WFD URL %q", truncateForLog(string(content), 120))
	}
	host := match[1]
	if !isIPv4(host) {
		return OpenDeviceRequest{}, protocolErrorf("Open WFD host %q must be an IPv4 address", host)
	}
	port, err := strconv.Atoi(match[2])
	if err != nil || port < 1 || port > 65535 {
		return OpenDeviceRequest{}, protocolErrorf("Open WFD port out of range")
	}
	mode, err := strconv.Atoi(match[3])
	if err != nil {
		return OpenDeviceRequest{}, protocolErrorf("Open WFD mirror mode is not numeric")
	}
	return OpenDeviceRequest{Host: host, Port: port, MirrorMode: mode}, nil
}

func isIPv4(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 3 {
			return false
		}
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || value > 255 {
			return false
		}
	}
	return true
}

func isASCII(value string) bool {
	for _, character := range value {
		if character > 0x7F {
			return false
		}
	}
	return true
}

func truncateForLog(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
