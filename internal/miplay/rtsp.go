package miplay

import (
	"fmt"
	"strconv"
	"strings"
)

// maxRTSPMessage bounds one RTSP message during the WFD handshake.
const maxRTSPMessage = 256 * 1024

// rtspHeader is one ordered RTSP header.
type rtspHeader struct {
	Name  string
	Value string
}

// RTSPMessage is one decoded RTSP message (request or response).
type RTSPMessage struct {
	StartLine string
	Headers   []rtspHeader
	Body      []byte
}

// Header performs a case-insensitive lookup.
func (m *RTSPMessage) Header(name string) (string, bool) {
	for _, header := range m.Headers {
		if strings.EqualFold(header.Name, name) {
			return header.Value, true
		}
	}
	return "", false
}

// SetHeader appends a header.
func (m *RTSPMessage) SetHeader(name, value string) {
	m.Headers = append(m.Headers, rtspHeader{Name: name, Value: value})
}

// EncodeRTSP serialises a message with the captured "name:value" style (no
// space after the colon) and automatic Content-Length handling.
func EncodeRTSP(message RTSPMessage) ([]byte, error) {
	if strings.ContainsAny(message.StartLine, "\r\n") {
		return nil, protocolErrorf("invalid RTSP start line")
	}
	lines := []string{message.StartLine}
	sawLength := false
	for _, header := range message.Headers {
		if strings.ContainsAny(header.Name+header.Value, "\r\n") {
			return nil, protocolErrorf("invalid RTSP header")
		}
		if strings.EqualFold(header.Name, "Content-Length") {
			sawLength = true
			lines = append(lines, header.Name+":"+strconv.Itoa(len(message.Body)))
			continue
		}
		lines = append(lines, header.Name+":"+header.Value)
	}
	if len(message.Body) > 0 && !sawLength {
		lines = append(lines, "Content-Length:"+strconv.Itoa(len(message.Body)))
	}
	encoded := []byte(strings.Join(lines, "\r\n") + "\r\n\r\n")
	return append(encoded, message.Body...), nil
}

// RTSPDecoder incrementally splits a TCP stream into RTSP messages.
type RTSPDecoder struct {
	buffer []byte
}

// NewRTSPDecoder returns an empty decoder.
func NewRTSPDecoder() *RTSPDecoder { return &RTSPDecoder{buffer: make([]byte, 0, 4096)} }

// Feed appends data and returns every complete message.
func (d *RTSPDecoder) Feed(data []byte) ([]RTSPMessage, error) {
	d.buffer = append(d.buffer, data...)
	if len(d.buffer) > maxRTSPMessage {
		d.buffer = d.buffer[:0]
		return nil, protocolErrorf("RTSP input exceeds the safety limit")
	}
	messages := []RTSPMessage{}
	for {
		headerEnd := indexOfDoubleCRLF(d.buffer)
		if headerEnd < 0 {
			break
		}
		head, err := decodeASCII(d.buffer[:headerEnd])
		if err != nil {
			d.buffer = d.buffer[:0]
			return nil, protocolErrorf("RTSP headers are not ASCII")
		}
		lines := strings.Split(head, "\r\n")
		if len(lines) == 0 || lines[0] == "" {
			d.buffer = d.buffer[:0]
			return nil, protocolErrorf("RTSP start line is empty")
		}
		headers := make([]rtspHeader, 0, len(lines)-1)
		for _, line := range lines[1:] {
			name, value, found := strings.Cut(line, ":")
			if !found {
				d.buffer = d.buffer[:0]
				return nil, protocolErrorf("malformed RTSP header %q", truncateForLog(line, 80))
			}
			name = strings.TrimSpace(name)
			if name == "" {
				d.buffer = d.buffer[:0]
				return nil, protocolErrorf("empty RTSP header name")
			}
			for _, existing := range headers {
				if strings.EqualFold(existing.Name, name) {
					d.buffer = d.buffer[:0]
					return nil, protocolErrorf("duplicate RTSP header %q", name)
				}
			}
			headers = append(headers, rtspHeader{Name: name, Value: strings.TrimSpace(value)})
		}
		lengthText := "0"
		for _, header := range headers {
			if strings.EqualFold(header.Name, "Content-Length") {
				lengthText = header.Value
				break
			}
		}
		bodyLength, err := strconv.Atoi(lengthText)
		if err != nil {
			d.buffer = d.buffer[:0]
			return nil, protocolErrorf("invalid RTSP Content-Length %q", lengthText)
		}
		if bodyLength < 0 || bodyLength > maxRTSPMessage {
			d.buffer = d.buffer[:0]
			return nil, protocolErrorf("RTSP body exceeds the safety limit")
		}
		messageLength := headerEnd + 4 + bodyLength
		if len(d.buffer) < messageLength {
			break
		}
		body := make([]byte, bodyLength)
		copy(body, d.buffer[headerEnd+4:messageLength])
		d.buffer = append(d.buffer[:0], d.buffer[messageLength:]...)
		// The decoded "Content-Length: N" header stays part of the message.
		messages = append(messages, RTSPMessage{StartLine: lines[0], Headers: headers, Body: body})
	}
	return messages, nil
}

func indexOfDoubleCRLF(data []byte) int {
	return strings.Index(string(data), "\r\n\r\n")
}

func decodeASCII(data []byte) (string, error) {
	for _, value := range data {
		if value > 0x7F {
			return "", protocolErrorf("non-ASCII byte 0x%02x", value)
		}
	}
	return string(data), nil
}

// RTSPPhase is the reverse-WFD handshake ladder.
type RTSPPhase string

// The recovered sequence: the source speaks first; the receiver mirrors
// OPTIONS, publishes AAC-only capabilities, confirms the selection, triggers
// SETUP, and completes with PLAY and TIME_OFFSET.
const (
	RTSPAwaitingSourceOptions   RTSPPhase = "awaiting_source_options"
	RTSPAwaitingOptionsAck      RTSPPhase = "awaiting_options_ack"
	RTSPAwaitingCapabilityQuery RTSPPhase = "awaiting_capability_query"
	RTSPAwaitingSelectedParams  RTSPPhase = "awaiting_selected_parameters"
	RTSPAwaitingSetupTrigger    RTSPPhase = "awaiting_setup_trigger"
	RTSPAwaitingSetupAck        RTSPPhase = "awaiting_setup_ack"
	RTSPAwaitingPlayAck         RTSPPhase = "awaiting_play_ack"
	RTSPAwaitingTimeOffset      RTSPPhase = "awaiting_time_offset"
	RTSPReady                   RTSPPhase = "ready"
	RTSPStopped                 RTSPPhase = "stopped"
)

// RTSPTransition is the outcome of processing one RTSP message.
type RTSPTransition struct {
	Accepted bool
	Writes   [][]byte
	Reason   string
	Ready    bool
}

// receiverCapabilities is the AAC-only interleaved-TCP capability body.
var receiverCapabilities = []byte(
	"wfd_video_formats: none\r\n" +
		"wfd_audio_codecs: AAC 00000001 00\r\n" +
		"wfd_client_rtp_ports: RTP/AVP/TCP;interleaved mode=play\r\n" +
		"wfd_tcp_enable: 1\r\n")

// ReceiverRtspSession drives the receiver half of the reverse WFD session.
type ReceiverRtspSession struct {
	sourceAddress string
	phase         RTSPPhase
	sessionID     string
	timeOffsetUS  int64
	timerEndpoint string
}

// NewReceiverRtspSession creates the session for one source.
func NewReceiverRtspSession(sourceAddress string) *ReceiverRtspSession {
	return &ReceiverRtspSession{
		sourceAddress: sourceAddress,
		phase:         RTSPAwaitingSourceOptions,
		timeOffsetUS:  -1,
	}
}

// Phase reports the current ladder position.
func (s *ReceiverRtspSession) Phase() RTSPPhase { return s.phase }

// SessionID reports the negotiated session id, once known.
func (s *ReceiverRtspSession) SessionID() string { return s.sessionID }

// Process advances the ladder. The returned transition carries the messages
// to write; an error means the session must stop.
func (s *ReceiverRtspSession) Process(message RTSPMessage) (RTSPTransition, error) {
	if s.phase == RTSPStopped {
		return RTSPTransition{}, protocolErrorf("RTSP session is stopped")
	}
	switch s.phase {
	case RTSPAwaitingSourceOptions:
		return s.sourceOptions(message)
	case RTSPAwaitingOptionsAck:
		return s.optionsAck(message)
	case RTSPAwaitingCapabilityQuery:
		return s.capabilityQuery(message)
	case RTSPAwaitingSelectedParams:
		return s.selectedParameters(message)
	case RTSPAwaitingSetupTrigger:
		return s.setupTrigger(message)
	case RTSPAwaitingSetupAck:
		return s.setupAck(message)
	case RTSPAwaitingPlayAck:
		return s.playAck(message)
	case RTSPAwaitingTimeOffset:
		return s.timeOffset(message)
	case RTSPReady:
		return s.readyRequest(message)
	}
	return RTSPTransition{}, protocolErrorf("unsupported RTSP phase %s", s.phase)
}

func (s *ReceiverRtspSession) sourceOptions(message RTSPMessage) (RTSPTransition, error) {
	if err := requireRequest(message, "OPTIONS", 1); err != nil {
		return RTSPTransition{}, err
	}
	// The timer endpoint is not needed by this receiver; record it for
	// diagnostics but never fail the handshake over it.
	if timer, ok := message.Header("wfd_timer_server_port"); ok {
		s.timerEndpoint = timer
	}
	response := s.response(1, nil)
	receiverOptions := RTSPMessage{StartLine: "OPTIONS * RTSP/1.0"}
	receiverOptions.SetHeader("CSeq", "1")
	receiverOptions.SetHeader("Require", "org.wfa.wfd1.0")
	receiverOptions.SetHeader("lib_version", "audio-speaker-mico-cloud "+controlSourceVersion)
	s.phase = RTSPAwaitingOptionsAck
	return s.ok([]RTSPMessage{response, receiverOptions}, "bidirectional OPTIONS started", false)
}

func (s *ReceiverRtspSession) optionsAck(message RTSPMessage) (RTSPTransition, error) {
	if err := requireResponse(message, 1); err != nil {
		return RTSPTransition{}, err
	}
	s.phase = RTSPAwaitingCapabilityQuery
	return s.ok(nil, "receiver OPTIONS acknowledged", false)
}

func (s *ReceiverRtspSession) capabilityQuery(message RTSPMessage) (RTSPTransition, error) {
	if err := requireRequest(message, "GET_PARAMETER", 2); err != nil {
		return RTSPTransition{}, err
	}
	s.phase = RTSPAwaitingSelectedParams
	return s.ok([]RTSPMessage{s.response(2, receiverCapabilities)}, "AAC-only receiver capabilities returned", false)
}

func (s *ReceiverRtspSession) selectedParameters(message RTSPMessage) (RTSPTransition, error) {
	if err := requireRequest(message, "SET_PARAMETER", 3); err != nil {
		return RTSPTransition{}, err
	}
	body := string(message.Body)
	if !strings.Contains(body, "wfd_audio_codecs: AAC 00000001 00") || !strings.Contains(body, "RTP/AVP/TCP") {
		return RTSPTransition{}, protocolErrorf("source did not select the AAC interleaved profile")
	}
	s.phase = RTSPAwaitingSetupTrigger
	return s.ok([]RTSPMessage{s.response(3, nil)}, "selected parameters accepted", false)
}

func (s *ReceiverRtspSession) setupTrigger(message RTSPMessage) (RTSPTransition, error) {
	if err := requireRequest(message, "SET_PARAMETER", 4); err != nil {
		return RTSPTransition{}, err
	}
	if !strings.Contains(string(message.Body), "wfd_trigger_method: SETUP") {
		return RTSPTransition{}, protocolErrorf("SET_PARAMETER did not contain the SETUP trigger")
	}
	setup := RTSPMessage{StartLine: "SETUP " + s.streamTarget() + " RTSP/1.0"}
	setup.SetHeader("CSeq", "2")
	setup.SetHeader("Transport", "RTP/AVP/TCP;interleaved=0-1")
	s.phase = RTSPAwaitingSetupAck
	return s.ok([]RTSPMessage{s.response(4, nil), setup}, "SETUP requested", false)
}

func (s *ReceiverRtspSession) setupAck(message RTSPMessage) (RTSPTransition, error) {
	if err := requireResponse(message, 2); err != nil {
		return RTSPTransition{}, err
	}
	sessionHeader, ok := message.Header("Session")
	if !ok {
		return RTSPTransition{}, protocolErrorf("SETUP response omitted Session")
	}
	sessionID := strings.TrimSpace(strings.SplitN(sessionHeader, ";", 2)[0])
	if sessionID == "" {
		return RTSPTransition{}, protocolErrorf("SETUP Session is empty")
	}
	for _, character := range sessionID {
		if character < '0' || character > '9' {
			return RTSPTransition{}, protocolErrorf("SETUP Session is not decimal")
		}
	}
	s.sessionID = sessionID
	play := RTSPMessage{StartLine: "PLAY " + s.streamTarget() + " RTSP/1.0"}
	play.SetHeader("CSeq", "3")
	play.SetHeader("Session", sessionID)
	s.phase = RTSPAwaitingPlayAck
	return s.ok([]RTSPMessage{play}, "PLAY requested", false)
}

func (s *ReceiverRtspSession) playAck(message RTSPMessage) (RTSPTransition, error) {
	if err := requireResponse(message, 3); err != nil {
		return RTSPTransition{}, err
	}
	s.phase = RTSPAwaitingTimeOffset
	return s.ok(nil, "PLAY acknowledged", false)
}

func (s *ReceiverRtspSession) timeOffset(message RTSPMessage) (RTSPTransition, error) {
	if err := requireRequest(message, "TIME_OFFSET", 5); err != nil {
		return RTSPTransition{}, err
	}
	value, ok := message.Header("TimeOffset")
	if !ok {
		return RTSPTransition{}, protocolErrorf("TIME_OFFSET omitted a numeric TimeOffset")
	}
	offset, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return RTSPTransition{}, protocolErrorf("TIME_OFFSET omitted a numeric TimeOffset")
	}
	s.timeOffsetUS = offset
	s.phase = RTSPReady
	return s.ok([]RTSPMessage{s.response(5, nil)}, "WFD session ready", true)
}

func (s *ReceiverRtspSession) readyRequest(message RTSPMessage) (RTSPTransition, error) {
	method := strings.SplitN(message.StartLine, " ", 2)[0]
	cseq := 0
	if value, ok := message.Header("CSeq"); ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			cseq = parsed
		}
	}
	switch method {
	case "OPTIONS", "GET_PARAMETER", "SET_PARAMETER":
		return s.ok([]RTSPMessage{s.response(cseq, nil)}, method+" keepalive acknowledged", true)
	case "VIDEO_LATENCY":
		return s.ok(nil, "VIDEO_LATENCY observed", true)
	}
	return RTSPTransition{}, protocolErrorf("unsupported ready-state RTSP method %s", method)
}

func (s *ReceiverRtspSession) streamTarget() string {
	return fmt.Sprintf("rtsp://%s/wfd1.0/streamid=0", s.sourceAddress)
}

func (s *ReceiverRtspSession) response(cseq int, body []byte) RTSPMessage {
	message := RTSPMessage{StartLine: "RTSP/1.0 200 OK", Body: body}
	message.SetHeader("CSeq", strconv.Itoa(cseq))
	if len(body) > 0 {
		message.SetHeader("Content-Type", "text/parameters")
	}
	if s.sessionID != "" && cseq != 1 && cseq != 2 {
		message.SetHeader("Session", s.sessionID)
	}
	return message
}

func (s *ReceiverRtspSession) ok(messages []RTSPMessage, reason string, ready bool) (RTSPTransition, error) {
	writes := make([][]byte, 0, len(messages))
	for _, message := range messages {
		encoded, err := EncodeRTSP(message)
		if err != nil {
			return RTSPTransition{}, err
		}
		writes = append(writes, encoded)
	}
	return RTSPTransition{Accepted: true, Writes: writes, Reason: reason, Ready: ready}, nil
}

func requireRequest(message RTSPMessage, method string, cseq int) error {
	if !strings.HasPrefix(message.StartLine, method+" ") {
		return protocolErrorf("expected %s, got %q", method, truncateForLog(message.StartLine, 80))
	}
	return requireCSeq(message, cseq)
}

func requireResponse(message RTSPMessage, cseq int) error {
	if message.StartLine != "RTSP/1.0 200 OK" {
		return protocolErrorf("expected RTSP 200 OK, got %q", truncateForLog(message.StartLine, 80))
	}
	return requireCSeq(message, cseq)
}

func requireCSeq(message RTSPMessage, cseq int) error {
	value, ok := message.Header("CSeq")
	if !ok {
		return protocolErrorf("RTSP message omitted numeric CSeq")
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed != cseq {
		return protocolErrorf("expected RTSP CSeq %d, got %q", cseq, value)
	}
	return nil
}
