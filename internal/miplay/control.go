package miplay

import (
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
	// VolumeSet reports a new SET_VOLUME value.
	VolumeSet bool
	// Volume is the accepted volume percentage.
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

	// onWarn is an optional diagnostic sink (set by the receiver). Kept as a plain
	// func so this package stays free of logging dependencies.
	onWarn func(string)

	challenge         []byte
	challengeSequence uint16
	friendlyName      string
	volume            int

	phase           ControlPhase
	authenticated   bool
	sourceVersion   string
	sourceName      string
	mediaInfo       map[string]string
	durationSeconds float64 // 手机上报的曲目时长（秒）；0 = 未知
	coverURL        string  // 手机上报的封面 URL；"" = 无
	// trackVersion 是曲目身份代次：标题一变就 +1。接收端据此把「本次投送的
	// 已播放时长」归零，否则同一次投送里换歌会继承上一首的进度。
	trackVersion      int
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
		return nil, fmt.Errorf("volume out of range")
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

// DurationSeconds reports the track length the source announced, or 0 when the
// source never sent one.
func (s *LegacyReceiverSession) DurationSeconds() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.durationSeconds
}

// CoverURL reports the cover-art URL the source announced, or "".
func (s *LegacyReceiverSession) CoverURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.coverURL
}

// TrackVersion changes whenever the reported track identity changes, so the
// receiver can restart its elapsed clock for the next song.
func (s *LegacyReceiverSession) TrackVersion() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trackVersion
}

// Volume reports the current protocol volume.
func (s *LegacyReceiverSession) Volume() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.volume
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
		// 10-04 实测：这条命令的载荷是**明文 JSON**（0x0058 的 hex_head 直接是
		// {"sourceName":"R…），只有 SET_VOLUME 那类才包 SafetyData 信封。
		// 上游原本的 json.Unmarshal 是对的——曾经在这里多套了一层 Safety 解密，
		// 反而把明文当成密文解不开、白白丢掉全部元数据。
		var info map[string]any
		if err := json.Unmarshal(frame.Payload, &info); err != nil {
			// Malformed metadata must not kill a working cast.
			s.loggerWarn(fmt.Sprintf("setLocalDeviceInfo payload is not JSON (%d bytes hex_head=%s); ignoring metadata",
				len(frame.Payload), hex.EncodeToString(truncateBytes(frame.Payload, 16))))
			return s.ack(CmdSetLocalDeviceInfoAck, frame.Sequence, nil), nil
		}
		if sourceName, ok := info["sourceName"].(string); ok && sourceName != "" {
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
		// 音量查询的上行载荷不参与业务（响应由我们下行）；不因载荷形态（可能是
		// SafetyData 信封）而掐会话——与 SET_VOLUME 同一口径。
		return s.ack(CmdGetVolumeAck, frame.Sequence, EncodeScalar(uint32(s.volume))), nil

	case CmdSetVolume:
		// MIUI 会把业务命令也放进 SafetyData（AES-128-CBC 状态化信封）。10-04 真机实测
		// 该命令 25 字节：00 07 01 e0 0c | 4 字节 CRC | 16 字节密文，字段与作者自己的
		// safetyCipher 头完全吻合（headerLength=7 / version=1 / flags=0xE0 / padding=12）。
		// 原实现只认 4 字节明文，于是每条音量命令都被判协议违规并 stop，手机侧表现为
		// 音量条拖动无响应。
		//
		// 注意：解密失败绝不能 stop()。上一版把错误处理写成 stop()，结果每次调音量就
		// 断开会话、手机反复重连（主人看到的「一调音量就崩溃」）。IV 状态尚未锁定时
		// 解不开是暂时的，忽略这条命令、等后续命令重新对齐状态即可。
		if len(frame.Payload) == 4 {
			// 明文形态（无 Safety 通道的老固件）
			if volume, ok := decodeScalarVolume(frame.Payload); ok {
				s.volume = volume
				result := s.ack(CmdSetVolumeAck, frame.Sequence, nil)
				result.VolumeSet = true
				result.Volume = volume
				return result, nil
			}
		}
		if s.safety != nil {
			if volume, ok := s.decryptVolume(frame.Payload); ok {
				s.volume = volume
				result := s.ack(CmdSetVolumeAck, frame.Sequence, nil)
				result.VolumeSet = true
				result.Volume = volume
				return result, nil
			}
			diag := s.safety.Diagnostics()
			s.loggerWarn(fmt.Sprintf("setVolume safety decrypt unavailable (phase=%q mutual_auth=%v in_iv=%q in_key=%q payload=%d); ignoring command",
				diag.Phase, diag.MutualAuthComplete, diag.InboundIVMode, diag.AuthKeyMode, len(frame.Payload)))
			// 仍然回 ACK：让手机知道 NAS 在线，不要重连风暴。音量维持上一值。
			return s.ack(CmdSetVolumeAck, frame.Sequence, nil), nil
		}
		s.loggerWarn(fmt.Sprintf("setVolume arrived without a Safety channel (payload=%d bytes); ignoring command", len(frame.Payload)))
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
		// 保持严格：这条命令是 OPEN 握手的前置状态，解析不了就无法确认播放源已就绪
		// （与 setLocalDeviceInfo / setMediaInfo 那类装饰性元数据不同，不适用容忍式）。
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
		// 同 setLocalDeviceInfo：元数据出错不得掐会话（载荷形态随手机版本变化）。
		var payload map[string]any
		if err := json.Unmarshal(frame.Payload, &payload); err != nil {
			s.loggerWarn(fmt.Sprintf("setMediaInfo payload is not JSON (%d bytes hex_head=%s); ignoring metadata",
				len(frame.Payload), hex.EncodeToString(truncateBytes(frame.Payload, 16))))
			return s.ack(CmdSetMediaInfoAck, frame.Sequence, nil), nil
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
		// 10-05 真机取证：MIUI 发的是 mTitle/mArtist/mAlbum（带 m 前缀），
		// 上游只认 title/artist/album，所以歌名一直收得到却解析不出来。
		// 两种拼写都试，老版本客户端可能不带前缀。
		for _, key := range []string{"title", "artist", "album"} {
			value := ""
			for _, field := range []string{"m" + strings.ToUpper(key[:1]) + key[1:], key} {
				if v, ok := source[field].(string); ok && v != "" {
					value = v
					break
				}
			}
			if value == "" {
				continue
			}
			// 逐字段更新（新值非空才覆盖）：同一次投送里手机会换歌，只在首次
			// 写入会让控制台永远停播第一首的歌名。标题一变即视为新曲目。
			value = truncateForLog(value, 120)
			if s.mediaInfo[key] == value {
				continue
			}
			s.mediaInfo[key] = value
			if key == "title" {
				s.trackVersion++
				// 新曲目的时长/封面必须一起作废，否则这一首没带元数据时会
				// 沿用上一首的长度，进度条直接失真。
				s.durationSeconds = 0
				s.coverURL = ""
			}
		}
	}
	// 第二遍专门取时长与封面：它们同样带 m 前缀（真机实测 mDuration=270800ms、
	// mCoverUrl 存在），但不进 mediaInfo —— 那是给歌名用的字符串表。单独一遍
	// 是为了避免「时长先于标题读到」时被上面的新曲目重置清掉。
	for _, source := range sources {
		if s.durationSeconds == 0 {
			for _, field := range []string{"mDuration", "durationMs", "duration_ms"} {
				if seconds, ok := mediaDurationSeconds(source[field]); ok {
					s.durationSeconds = seconds
					break
				}
			}
		}
		if s.coverURL == "" {
			for _, field := range []string{"mCoverUrl", "coverUrl", "cover_url"} {
				value, ok := source[field].(string)
				if !ok {
					continue
				}
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					s.coverURL = truncateForLog(trimmed, 512)
					break
				}
			}
		}
	}
}

// mediaDurationSeconds normalises the source-reported track length.
//
// Measured on the target phone: mDuration is milliseconds (270800 -> 4m31s), so
// that is the only unit accepted. The value must also land inside a plausible
// window; anything else yields ok=false and the console says "duration unknown"
// instead of drawing a progress bar from a bogus number.
func mediaDurationSeconds(value any) (float64, bool) {
	var milliseconds float64
	switch typed := value.(type) {
	case float64:
		milliseconds = typed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, false
		}
		milliseconds = parsed
	default:
		return 0, false
	}
	if milliseconds < 1000 || milliseconds > 24*60*60*1000 {
		return 0, false
	}
	return milliseconds / 1000, true
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
