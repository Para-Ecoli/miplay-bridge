package miplay

import (
	"crypto/hmac"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
)

// ControlPhase is the control-session lifecycle state.
type ControlPhase string

// The recovered phase ladder: after the legacy challenge the session is READY
// for business commands and OPEN moves it to OPENED once the phone hands over
// its reverse-WFD endpoint.
const (
	PhaseCreated      ControlPhase = "created"
	PhaseAwaitingAuth ControlPhase = "awaiting_auth"
	PhaseReady        ControlPhase = "ready"
	PhaseOpened       ControlPhase = "opened"
	PhaseStopped      ControlPhase = "stopped"
)

// controlSourceVersion is the receiver version string modern phones probe
// for.
const controlSourceVersion = "2.1.5091615"

// ControlResult is the outcome of processing one inbound control frame.
type ControlResult struct {
	// Accepted is false when the frame violated the session contract and the
	// connection must be closed.
	Accepted bool
	// Writes are the frames to send back, in order.
	Writes [][]byte
	// Reason explains the outcome for logs.
	Reason string
	// OpenRequest is set when the frame was the OPEN handover.
	OpenRequest *OpenDeviceRequest
	// VolumeSet reports that the frame carried an accepted SET_VOLUME value.
	VolumeSet bool
	// Volume is the accepted volume percentage (0-100).
	Volume int
	// PauseRequested / ResumeRequested / CloseRequested mirror the transport
	// commands so the receiver can act on the realtime pipeline.
	PauseRequested  bool
	ResumeRequested bool
	CloseRequested  bool
}

// TraceEntry is one control frame for the last-session diagnostics.
type TraceEntry struct {
	Command      string `json:"command"`
	Sequence     int    `json:"sequence"`
	PayloadBytes int    `json:"payload_bytes"`
}

// LegacyReceiverSession is the receiver side of one 8899 control connection.
//
// The session runs on a single goroutine; every mutating entry point is
// serialised through mu, and the outbound write callback is invoked while the
// lock is held so the SafetyData CBC state and the wire order can never
// diverge.
type LegacyReceiverSession struct {
	mu sync.Mutex

	challenge         []byte
	challengeSequence uint16
	friendlyName      string
	volume            int
	// volumeProvider, when wired, reports the live hardware volume. The
	// session answers GET_VOLUME with it so a level the console or the phone
	// changed after the connection was established is reported truthfully.
	// Invoked with the session lock held.
	volumeProvider func() (int, bool)

	phase             ControlPhase
	authenticated     bool
	sourceVersion     string
	sourceName        string
	mediaInfo         map[string]string
	setPlaySourceSeen bool
	mediaStarted      bool
	notificationSeq   uint16

	localHost string
	localPort int
	peerHost  string
	peerPort  int
	safety    *ModernSafetyReceiver

	trace []TraceEntry
}

// NewLegacyReceiverSession validates the challenge and prepares the session.
func NewLegacyReceiverSession(challenge []byte, challengeSequence uint16, friendlyName string,
	volume int, localHost string, localPort int, peerHost string, peerPort int) (*LegacyReceiverSession, error) {
	if len(challenge) < 12 || len(challenge) > 17 {
		return nil, fmt.Errorf("legacy challenge must be 12 to 17 ASCII digits")
	}
	for _, character := range challenge {
		if character < '0' || character > '9' {
			return nil, fmt.Errorf("legacy challenge must be 12 to 17 ASCII digits")
		}
	}
	if volume < 0 || volume > 100 {
		return nil, fmt.Errorf("legacy volume must be 0 to 100")
	}
	return &LegacyReceiverSession{
		challenge:         append([]byte(nil), challenge...),
		challengeSequence: challengeSequence,
		friendlyName:      friendlyName,
		volume:            volume,
		phase:             PhaseCreated,
		notificationSeq:   1,
		localHost:         localHost,
		localPort:         localPort,
		peerHost:          peerHost,
		peerPort:          peerPort,
		mediaInfo:         map[string]string{},
		trace:             make([]TraceEntry, 0, 64),
	}, nil
}

// Phase reports the current lifecycle phase.
func (s *LegacyReceiverSession) Phase() ControlPhase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

// Authenticated reports whether the legacy challenge was answered.
func (s *LegacyReceiverSession) Authenticated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authenticated
}

// SourceName is the phone-reported device name, once known.
func (s *LegacyReceiverSession) SourceName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sourceName
}

// MediaInfo returns the last metadata reported by the source, or nil.
func (s *LegacyReceiverSession) MediaInfo() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.mediaInfo) == 0 {
		return nil
	}
	snapshot := make(map[string]string, len(s.mediaInfo))
	for key, value := range s.mediaInfo {
		snapshot[key] = value
	}
	return snapshot
}

// Volume reports the session's own volume copy (0-100).
func (s *LegacyReceiverSession) Volume() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.volume
}

// SetVolumeProvider wires the live hardware-volume reader used to answer
// GET_VOLUME. A nil provider keeps the session's own copy authoritative.
func (s *LegacyReceiverSession) SetVolumeProvider(provider func() (int, bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volumeProvider = provider
}

// SafetyDiagnostics reports the encrypted-channel state, when active.
func (s *LegacyReceiverSession) SafetyDiagnostics() *SafetyDiagnostics {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.safety == nil {
		return nil
	}
	diagnostics := s.safety.Diagnostics()
	return &diagnostics
}

// Trace returns a copy of the recent frame trace.
func (s *LegacyReceiverSession) Trace() []TraceEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TraceEntry(nil), s.trace...)
}

// Start emits the opening legacy challenge.
func (s *LegacyReceiverSession) Start(write func([][]byte) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != PhaseCreated {
		return fmt.Errorf("control session already started")
	}
	s.phase = PhaseAwaitingAuth
	return write([][]byte{EncodeCommand(CmdLegacyChallenge, s.challengeSequence, s.challenge)})
}

// Step processes one inbound frame and writes the replies while holding the
// session lock, keeping CBC state and wire order aligned.
func (s *LegacyReceiverSession) Step(frame CommandFrame, write func([][]byte) error) (ControlResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record(frame)
	result, err := s.process(frame)
	if err != nil {
		return result, err
	}
	if result.Accepted && len(result.Writes) > 0 {
		if writeErr := write(result.Writes); writeErr != nil {
			return result, writeErr
		}
	}
	return result, nil
}

// MediaStarted sends the two NOTIFY frames once the first media frame
// arrived, exactly once per session.
func (s *LegacyReceiverSession) MediaStarted(write func([][]byte) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != PhaseOpened || s.mediaStarted {
		return nil
	}
	s.mediaStarted = true
	writes := [][]byte{
		s.notify("first-audiopcm", 1),
		s.notify("state", 2),
	}
	if s.safety != nil {
		wrapped, err := s.safety.WrapWrites(writes)
		if err != nil {
			return err
		}
		writes = wrapped
	}
	return write(writes)
}

func (s *LegacyReceiverSession) notify(label string, value byte) []byte {
	sequence := s.notificationSeq
	s.notificationSeq = (sequence + 1) & 0xFFFF
	return EncodeCommand(CmdNotify, sequence, EncodeNotifyScalar(label, value))
}

func (s *LegacyReceiverSession) record(frame CommandFrame) {
	s.trace = append(s.trace, TraceEntry{
		Command:      frame.Command.String(),
		Sequence:     int(frame.Sequence),
		PayloadBytes: len(frame.Payload),
	})
	if len(s.trace) > 64 {
		s.trace = s.trace[len(s.trace)-64:]
	}
}

func (s *LegacyReceiverSession) process(frame CommandFrame) (ControlResult, error) {
	if s.phase == PhaseStopped {
		return s.stop("control session is stopped")
	}
	switch frame.Command {
	case CmdSafetyInfo:
		return s.startSafety(frame)
	case CmdSafetyAuth, CmdSafetyAuthAck:
		return s.processSafety(frame)
	case CmdSafetyInfoAck:
		return s.stop("unexpected SafetyInfo acknowledgement")
	case CmdSourceVersion:
		return s.sourceVersionStep(frame)
	case CmdLegacyChallengeAck:
		return s.authenticate(frame)
	}
	if !s.authenticated {
		return s.stop("authentication must complete before business commands")
	}
	if s.phase != PhaseReady && s.phase != PhaseOpened {
		return s.stop("business command arrived in an invalid phase")
	}
	if s.safety != nil {
		decoded, err := s.safety.Process(frame)
		if err != nil {
			return s.stop(err.Error())
		}
		if decoded.plaintext == nil {
			// Intermediate mutual-auth frames still carry their own writes,
			// which the safety layer already encrypted.
			return ControlResult{Accepted: true, Writes: decoded.writes, Reason: s.safety.phase}, nil
		}
		result, err := s.processBusiness(*decoded.plaintext)
		if err != nil {
			return s.stop(err.Error())
		}
		if result.Accepted && len(result.Writes) > 0 {
			wrapped, wrapErr := s.safety.WrapWrites(result.Writes)
			if wrapErr != nil {
				return s.stop(wrapErr.Error())
			}
			result.Writes = wrapped
		}
		return result, nil
	}
	result, err := s.processBusiness(frame)
	if err != nil {
		return s.stop(err.Error())
	}
	return result, nil
}

func (s *LegacyReceiverSession) startSafety(frame CommandFrame) (ControlResult, error) {
	if !s.authenticated {
		return s.stop("SafetyInfo arrived before legacy authentication")
	}
	if s.safety != nil {
		return s.stop("duplicate SafetyInfo offer")
	}
	if s.localHost == "" || s.peerHost == "" {
		return s.stop("modern Safety protocol requires TCP endpoint context")
	}
	safety, err := NewModernSafetyReceiver(s.localHost, s.localPort, s.peerHost, s.peerPort)
	if err != nil {
		return s.stop(err.Error())
	}
	result, err := safety.AcceptInfo(frame)
	if err != nil {
		return s.stop(err.Error())
	}
	s.safety = safety
	return ControlResult{Accepted: true, Writes: result.writes, Reason: "SafetyInfo negotiated"}, nil
}

func (s *LegacyReceiverSession) processSafety(frame CommandFrame) (ControlResult, error) {
	if s.safety == nil {
		return s.stop("SafetyAuth arrived before SafetyInfo")
	}
	result, err := s.safety.Process(frame)
	if err != nil {
		return s.stop(err.Error())
	}
	if result.plaintext == nil {
		return ControlResult{Accepted: true, Writes: result.writes, Reason: s.safety.phase}, nil
	}
	business, err := s.processBusiness(*result.plaintext)
	if err != nil {
		return s.stop(err.Error())
	}
	if business.Accepted && len(business.Writes) > 0 {
		wrapped, wrapErr := s.safety.WrapWrites(business.Writes)
		if wrapErr != nil {
			return s.stop(wrapErr.Error())
		}
		business.Writes = wrapped
	}
	return business, nil
}

func (s *LegacyReceiverSession) sourceVersionStep(frame CommandFrame) (ControlResult, error) {
	if s.sourceVersion != "" {
		return s.stop("duplicate source version")
	}
	if len(frame.Payload) == 0 || frame.Payload[len(frame.Payload)-1] != 0 {
		return s.stop("source version is not NUL terminated")
	}
	value := frame.Payload[:len(frame.Payload)-1]
	if !isASCII(string(value)) {
		return s.stop("source version is not ASCII")
	}
	s.sourceVersion = string(value)
	reply := append([]byte(controlSourceVersion), 0)
	return s.ok([][]byte{EncodeCommand(CmdSourceVersionAck, frame.Sequence, reply)}, "source version acknowledged"), nil
}

func (s *LegacyReceiverSession) authenticate(frame CommandFrame) (ControlResult, error) {
	if s.authenticated {
		return s.stop("duplicate legacy authentication")
	}
	expected := LegacyChallengeResponse(s.challenge)
	if frame.Sequence != s.challengeSequence || !hmac.Equal(frame.Payload, expected) {
		return s.stop("legacy authentication failed")
	}
	s.authenticated = true
	s.phase = PhaseReady
	return s.ok(nil, "legacy authentication verified"), nil
}

func (s *LegacyReceiverSession) processBusiness(frame CommandFrame) (ControlResult, error) {
	switch frame.Command {
	case CmdGetDeviceInfo:
		if len(frame.Payload) != 0 {
			return s.stop("getDeviceInfo payload must be empty")
		}
		payload, err := EncodeDeviceInfo([]DeviceInfoField{
			{Name: "name", Value: s.friendlyName},
			{Name: "model", Value: "miplay.bridge"},
			{Name: "support", Value: "audio"},
			{Name: "mirrorMode", Value: "2"},
		})
		if err != nil {
			return s.stop(err.Error())
		}
		return s.ack(CmdGetDeviceInfoAck, frame.Sequence, payload), nil

	case CmdSetLocalDeviceInfo:
		var payload map[string]any
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			return s.stop("setLocalDeviceInfo payload is not JSON")
		}
		if sourceName, ok := payload["sourceName"].(string); ok && sourceName != "" {
			if len(sourceName) > 120 {
				sourceName = sourceName[:120]
			}
			s.sourceName = sourceName
		}
		return s.ack(CmdSetLocalDeviceInfoAck, frame.Sequence, nil), nil

	case CmdSourceCapabilityUpdate:
		// Current MIUI sources do not wait for an acknowledgement; this is a
		// receive-only compatibility boundary.
		return s.ok(nil, "source capability update observed"), nil

	case CmdGetMirrorMode:
		return s.emptyQueryAck(frame, CmdGetMirrorModeAck, EncodeScalar(2))

	case CmdGetVolume:
		// Answer with the current volume no matter what the inbound payload
		// carries: the request is a pure query (the reply is the payload), so
		// the request shape decides nothing - a modern source may wrap it in a
		// SafetyData blob - and a volume query must never end a session. A
		// wired provider answers the live hardware level, so the reply tracks
		// console-side changes too and the phone's slider never springs back
		// to a stale value.
		return s.ack(CmdGetVolumeAck, frame.Sequence, EncodeScalar(uint32(s.currentVolume()))), nil

	case CmdSetVolume:
		// The payload arrives in one of two recovered shapes: the bare
		// four-byte big-endian integer of earlier firmware, or the five-byte
		// 0x00-tagged scalar modern MIUI sends (the EncodeScalar shape every
		// other volume message uses). A modern source additionally wraps the
		// command in a SafetyData blob; through an active safety channel the
		// blob is decrypted before this handler runs, and without one
		// decodeVolumePayload probes it directly.
		//
		// A malformed or undecryptable volume command is acknowledged and
		// ignored, never fatal: the CBC IV takes a moment to settle on the
		// encrypted channel, and tearing the session down every time the
		// slider moves leaves the phone in a reconnect loop.
		if volume, ok := s.decodeVolumePayload(frame.Payload); ok {
			s.volume = volume
			result := s.ack(CmdSetVolumeAck, frame.Sequence, nil)
			result.VolumeSet = true
			result.Volume = volume
			return result, nil
		}
		return s.ack(CmdSetVolumeAck, frame.Sequence, nil), nil

	case CmdGetState:
		state := uint32(3)
		if s.mediaStarted {
			state = 2
		}
		return s.emptyQueryAck(frame, CmdGetStateAck, EncodeScalar(state))

	case CmdGetMediaInfo:
		return s.emptyQueryAck(frame, CmdGetMediaInfoAck, []byte{})

	case CmdHeartbeat:
		return s.emptyQueryAck(frame, CmdHeartbeatAck, []byte{})

	case CmdGetPosition:
		// Defensive extension over the reference table: answer the tolerant
		// scalar instead of tearing the session down on a stray query.
		return s.emptyQueryAck(frame, CmdGetPositionAck, EncodeScalar(0))

	case CmdSetPlaySource:
		var payload map[string]any
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			return s.stop("setPlaySource payload is not JSON")
		}
		s.setPlaySourceSeen = true
		return s.ok(nil, "play source observed"), nil

	case CmdOpen:
		if !s.setPlaySourceSeen && s.safety == nil {
			return s.stop("Open arrived before setPlaySource")
		}
		request, err := ParseOpenDeviceRequest(frame.Payload, s.safety != nil)
		if err != nil {
			return s.stop(err.Error())
		}
		s.phase = PhaseOpened
		return ControlResult{Accepted: true, Reason: "WFD source endpoint accepted", OpenRequest: &request}, nil

	case CmdSetMediaInfo:
		var payload map[string]any
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			return s.stop("setMediaInfo payload is not JSON")
		}
		s.captureMediaInfo(payload)
		return s.ack(CmdSetMediaInfoAck, frame.Sequence, nil), nil

	case CmdPause:
		result := s.ack(CmdPauseAck, frame.Sequence, nil)
		result.PauseRequested = true
		return result, nil

	case CmdResume:
		result := s.ack(CmdResumeAck, frame.Sequence, nil)
		result.ResumeRequested = true
		return result, nil

	case CmdClose:
		s.phase = PhaseStopped
		result := s.ok(nil, "source closed the session")
		result.CloseRequested = true
		return result, nil
	}
	return s.stop(fmt.Sprintf("unsupported legacy command %s", frame.Command))
}

// captureMediaInfo best-effort extracts title/artist/album from the metadata
// payload; stray shapes never fail the session because metadata is cosmetic.
func (s *LegacyReceiverSession) captureMediaInfo(payload map[string]any) {
	sources := []map[string]any{payload}
	for _, key := range []string{"mediaInfo", "media_info"} {
		if nested, ok := payload[key].(map[string]any); ok {
			sources = append(sources, nested)
		}
	}
	for _, source := range sources {
		for _, key := range []string{"title", "artist", "album"} {
			value, ok := source[key].(string)
			if !ok || value == "" {
				continue
			}
			if _, done := s.mediaInfo[key]; !done {
				s.mediaInfo[key] = truncateForLog(value, 120)
			}
		}
	}
}

// decodeVolumePayload reads a SET_VOLUME payload in any of the recovered
// shapes: a bare scalar, or a SafetyData blob wrapping a scalar. The blob
// probe re-derives the protocol key material from the TCP endpoints and tries
// the two recovered IV candidates in the same order as
// NewModernSafetyReceiver. It runs on a fresh cipher and never mutates the
// live safety channel, so a blob the channel itself will handle later is not
// disturbed, and a probe miss just leaves the command ignored.
func (s *LegacyReceiverSession) decodeVolumePayload(payload []byte) (int, bool) {
	if volume, ok := decodeVolumeScalar(payload); ok {
		return volume, true
	}
	if s.localHost == "" || s.peerHost == "" {
		return 0, false
	}
	authKey := deriveType1AuthKey(s.localHost, s.localPort, s.peerHost, s.peerPort)
	key := authKey[:16]
	for _, iv := range [][]byte{authKey[16:], key} {
		cipher, err := newSafetyCipher(key, iv)
		if err != nil {
			continue
		}
		plaintext, err := cipher.Decrypt(payload)
		if err != nil {
			continue
		}
		if volume, ok := decodeVolumeScalar(plaintext); ok {
			return volume, true
		}
	}
	return 0, false
}

// decodeVolumeScalar reads a volume in 0..100 from either recovered scalar
// shape: a four-byte big-endian integer, or the five-byte 0x00-tagged scalar.
// Out-of-range values are rejected so a malformed frame is skipped instead of
// clamping the mixer.
func decodeVolumeScalar(payload []byte) (int, bool) {
	switch {
	case len(payload) == 4:
	case len(payload) == 5 && payload[0] == 0:
		payload = payload[1:]
	default:
		return 0, false
	}
	value := binary.BigEndian.Uint32(payload)
	if value > 100 {
		return 0, false
	}
	return int(value), true
}

// currentVolume resolves the level GET_VOLUME answers with: the live hardware
// level when a provider is wired and reports a sane value, else the session's
// own copy. The caller must hold mu.
func (s *LegacyReceiverSession) currentVolume() int {
	if s.volumeProvider != nil {
		if volume, ok := s.volumeProvider(); ok && volume >= 0 && volume <= 100 {
			return volume
		}
	}
	return s.volume
}

func (s *LegacyReceiverSession) emptyQueryAck(frame CommandFrame, command Command, payload []byte) (ControlResult, error) {
	if len(frame.Payload) != 0 {
		return s.stop(fmt.Sprintf("command %s payload must be empty", frame.Command))
	}
	return s.ack(command, frame.Sequence, payload), nil
}

func (s *LegacyReceiverSession) ack(command Command, sequence uint16, payload []byte) ControlResult {
	return s.ok([][]byte{EncodeCommand(command, sequence, payload)}, "acknowledged")
}

func (s *LegacyReceiverSession) ok(writes [][]byte, reason string) ControlResult {
	return ControlResult{Accepted: true, Writes: writes, Reason: reason}
}

func (s *LegacyReceiverSession) stop(reason string) (ControlResult, error) {
	s.phase = PhaseStopped
	return ControlResult{Accepted: false, Reason: reason}, nil
}
