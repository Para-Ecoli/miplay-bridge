package miplay

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

func rtspRequest(startLine, cseq string, headers map[string]string, body []byte) RTSPMessage {
	message := RTSPMessage{StartLine: startLine, Body: body}
	if cseq != "" {
		message.SetHeader("CSeq", cseq)
	}
	for name, value := range headers {
		message.SetHeader(name, value)
	}
	return message
}

func TestEncodeRTSPHandlesContentLength(t *testing.T) {
	body := []byte("wfd_video_formats: none\r\n")
	message := RTSPMessage{StartLine: "GET_PARAMETER rtsp://host/wfd1.0 RTSP/1.0", Body: body}
	message.SetHeader("CSeq", "2")
	message.SetHeader("Content-Length", "999") // must be rewritten to the real length
	encoded, err := EncodeRTSP(message)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "CSeq:2\r\n") {
		t.Fatalf("encoded = %q, want the captured no-space header style", encoded)
	}
	if !strings.Contains(string(encoded), "Content-Length:"+strconv.Itoa(len(body))+"\r\n") {
		t.Fatalf("encoded = %q, want a corrected Content-Length", encoded)
	}
	if !bytes.HasSuffix(encoded, body) {
		t.Fatal("the body must be appended")
	}

	// A missing Content-Length is added automatically when a body exists.
	bare := RTSPMessage{StartLine: "SET_PARAMETER rtsp://host/wfd1.0 RTSP/1.0", Body: []byte("x")}
	bare.SetHeader("CSeq", "3")
	encoded, err = EncodeRTSP(bare)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "Content-Length:1\r\n") {
		t.Fatalf("encoded = %q, want an automatic Content-Length", encoded)
	}
}

func TestEncodeRTSPRejectsHeaderInjection(t *testing.T) {
	injected := RTSPMessage{StartLine: "OPTIONS * RTSP/1.0\r\nInjected: yes"}
	if _, err := EncodeRTSP(injected); err == nil {
		t.Fatal("a start line with CRLF must be rejected")
	}
	header := RTSPMessage{StartLine: "OPTIONS * RTSP/1.0"}
	header.SetHeader("X", "a\r\nInjected: yes")
	if _, err := EncodeRTSP(header); err == nil {
		t.Fatal("a header value with CRLF must be rejected")
	}
}

func TestRTSPDecoderRoundTrip(t *testing.T) {
	first := RTSPMessage{StartLine: "OPTIONS * RTSP/1.0"}
	first.SetHeader("CSeq", "1")
	first.SetHeader("Require", "org.wfa.wfd1.0")
	firstWire, err := EncodeRTSP(first)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("wfd_audio_codecs: AAC 00000001 00\r\n")
	second := RTSPMessage{StartLine: "SET_PARAMETER rtsp://source/wfd1.0 RTSP/1.0", Body: body}
	second.SetHeader("CSeq", "3")
	secondWire, err := EncodeRTSP(second)
	if err != nil {
		t.Fatal(err)
	}
	stream := append(firstWire, secondWire...)

	decoder := NewRTSPDecoder()
	var messages []RTSPMessage
	for _, chunk := range [][]byte{stream[:10], stream[10:40], stream[40:]} {
		parsed, err := decoder.Feed(chunk)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, parsed...)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(messages))
	}
	if value, ok := messages[0].Header("requiRe"); !ok || value != "org.wfa.wfd1.0" {
		t.Fatalf("header lookup = %q, %v", value, ok)
	}
	if !bytes.Equal(messages[1].Body, body) {
		t.Fatalf("body = %q", messages[1].Body)
	}
}

func TestRTSPDecoderRejectsMalformedMessages(t *testing.T) {
	cases := []string{
		"OPTIONS * RTSP/1.0\r\nNoColonHere\r\n\r\n",
		"OPTIONS * RTSP/1.0\r\nCSeq: 1\r\ncseq: 2\r\n\r\n",
		"OPTIONS * RTSP/1.0\r\nContent-Length: nope\r\n\r\n",
	}
	for _, raw := range cases {
		decoder := NewRTSPDecoder()
		if _, err := decoder.Feed([]byte(raw)); err == nil {
			t.Fatalf("Feed(%q) must fail", raw)
		}
	}
	decoder := NewRTSPDecoder()
	if _, err := decoder.Feed([]byte("OPTIONS * RTSP/1.0\r\nX: \xff\xfe\r\n\r\n")); err == nil {
		t.Fatal("non-ASCII headers must be rejected")
	}
}

// driveSession walks the ladder up to (and including) the target phase.
func driveSession(t *testing.T, session *ReceiverRtspSession, upto RTSPPhase) {
	t.Helper()
	selectionBody := []byte("wfd_audio_codecs: AAC 00000001 00\r\nwfd_client_rtp_ports: RTP/AVP/TCP;interleaved mode=play\r\n")
	steps := []struct {
		message RTSPMessage
		phase   RTSPPhase
	}{
		{rtspRequest("OPTIONS * RTSP/1.0", "1", nil, nil), RTSPAwaitingOptionsAck},
		{rtspRequest("RTSP/1.0 200 OK", "1", nil, nil), RTSPAwaitingCapabilityQuery},
		{rtspRequest("GET_PARAMETER rtsp://source/wfd1.0 RTSP/1.0", "2", nil, nil), RTSPAwaitingSelectedParams},
		{rtspRequest("SET_PARAMETER rtsp://source/wfd1.0 RTSP/1.0", "3", nil, selectionBody), RTSPAwaitingSetupTrigger},
		{rtspRequest("SET_PARAMETER rtsp://source/wfd1.0 RTSP/1.0", "4", nil, []byte("wfd_trigger_method: SETUP\r\n")), RTSPAwaitingSetupAck},
		{rtspRequest("RTSP/1.0 200 OK", "2", map[string]string{"Session": "42"}, nil), RTSPAwaitingPlayAck},
		{rtspRequest("RTSP/1.0 200 OK", "3", nil, nil), RTSPAwaitingTimeOffset},
		{rtspRequest("TIME_OFFSET rtsp://source/wfd1.0/streamid=0 RTSP/1.0", "5", map[string]string{"TimeOffset": "1"}, nil), RTSPReady},
	}
	for _, step := range steps {
		if _, err := session.Process(step.message); err != nil {
			t.Fatalf("drive to %s: %v", step.phase, err)
		}
		if session.Phase() != step.phase {
			t.Fatalf("phase = %s, want %s", session.Phase(), step.phase)
		}
		if step.phase == upto {
			return
		}
	}
}

func TestReceiverRtspSessionLadder(t *testing.T) {
	session := NewReceiverRtspSession("192.168.1.20")
	if session.Phase() != RTSPAwaitingSourceOptions {
		t.Fatalf("initial phase = %s", session.Phase())
	}

	// 1. The source speaks first: its OPTIONS is mirrored.
	transition, err := session.Process(rtspRequest("OPTIONS * RTSP/1.0", "1", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(transition.Writes) != 2 {
		t.Fatalf("writes = %d, want the response plus the mirrored OPTIONS", len(transition.Writes))
	}
	if !strings.Contains(string(transition.Writes[1]), "Require:org.wfa.wfd1.0") {
		t.Fatalf("mirrored OPTIONS = %q", transition.Writes[1])
	}

	// 2. The source acknowledges the mirrored OPTIONS.
	transition, err = session.Process(rtspRequest("RTSP/1.0 200 OK", "1", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(transition.Writes) != 0 {
		t.Fatalf("writes = %d, want none", len(transition.Writes))
	}

	// 3. The source queries capabilities: AAC-only, interleaved TCP.
	transition, err = session.Process(rtspRequest("GET_PARAMETER rtsp://192.168.1.20/wfd1.0 RTSP/1.0", "2", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(transition.Writes) != 1 {
		t.Fatalf("writes = %d, want the capability body", len(transition.Writes))
	}
	capabilities := string(transition.Writes[0])
	for _, expected := range []string{"wfd_audio_codecs: AAC 00000001 00", "RTP/AVP/TCP", "Content-Type:text/parameters"} {
		if !strings.Contains(capabilities, expected) {
			t.Fatalf("capability body missing %q:\n%s", expected, capabilities)
		}
	}

	// 4. The source confirms the selection.
	selection := rtspRequest("SET_PARAMETER rtsp://192.168.1.20/wfd1.0 RTSP/1.0", "3", nil,
		[]byte("wfd_audio_codecs: AAC 00000001 00\r\nwfd_client_rtp_ports: RTP/AVP/TCP;interleaved mode=play\r\n"))
	if _, err := session.Process(selection); err != nil {
		t.Fatal(err)
	}

	// 5. The SETUP trigger: the receiver issues SETUP with interleaved 0-1.
	trigger := rtspRequest("SET_PARAMETER rtsp://192.168.1.20/wfd1.0 RTSP/1.0", "4", nil, []byte("wfd_trigger_method: SETUP\r\n"))
	transition, err = session.Process(trigger)
	if err != nil {
		t.Fatal(err)
	}
	if len(transition.Writes) != 2 {
		t.Fatalf("writes = %d, want the response plus SETUP", len(transition.Writes))
	}
	if !strings.Contains(string(transition.Writes[1]), "Transport:RTP/AVP/TCP;interleaved=0-1") {
		t.Fatalf("SETUP = %q", transition.Writes[1])
	}

	// 6. The SETUP response carries the session id; the receiver plays.
	setupAck := rtspRequest("RTSP/1.0 200 OK", "2", map[string]string{"Session": "1234567890;timeout=60"}, nil)
	transition, err = session.Process(setupAck)
	if err != nil {
		t.Fatal(err)
	}
	if len(transition.Writes) != 1 || !strings.Contains(string(transition.Writes[0]), "PLAY rtsp://192.168.1.20/wfd1.0/streamid=0") {
		t.Fatalf("writes = %q", transition.Writes)
	}
	if session.SessionID() != "1234567890" {
		t.Fatalf("session = %q", session.SessionID())
	}
	if !strings.Contains(string(transition.Writes[0]), "Session:1234567890") {
		t.Fatalf("PLAY = %q", transition.Writes[0])
	}

	// 7. PLAY acknowledged; the source sends TIME_OFFSET to finish the ladder.
	if _, err := session.Process(rtspRequest("RTSP/1.0 200 OK", "3", nil, nil)); err != nil {
		t.Fatal(err)
	}
	transition, err = session.Process(rtspRequest("TIME_OFFSET rtsp://192.168.1.20/wfd1.0/streamid=0 RTSP/1.0", "5", map[string]string{"TimeOffset": "125000"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !transition.Ready || session.Phase() != RTSPReady {
		t.Fatalf("transition = %+v, phase = %s", transition, session.Phase())
	}

	// 8. Keepalives keep flowing once ready.
	transition, err = session.Process(rtspRequest("OPTIONS * RTSP/1.0", "9", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !transition.Accepted || !transition.Ready || len(transition.Writes) != 1 {
		t.Fatalf("keepalive transition = %+v", transition)
	}
}

func TestReceiverRtspSessionRejectsUnexpectedMessages(t *testing.T) {
	// The ladder must start with the source OPTIONS.
	session := NewReceiverRtspSession("192.168.1.20")
	if _, err := session.Process(rtspRequest("GET_PARAMETER rtsp://source/wfd1.0 RTSP/1.0", "2", nil, nil)); err == nil {
		t.Fatal("a non-OPTIONS first message must be rejected")
	}
	// A wrong CSeq is a protocol violation.
	session = NewReceiverRtspSession("192.168.1.20")
	if _, err := session.Process(rtspRequest("OPTIONS * RTSP/1.0", "5", nil, nil)); err == nil {
		t.Fatal("a wrong CSeq must be rejected")
	}
	// A SETUP response without a Session header breaks the ladder.
	session = NewReceiverRtspSession("192.168.1.20")
	driveSession(t, session, RTSPAwaitingSetupAck)
	if _, err := session.Process(rtspRequest("RTSP/1.0 200 OK", "2", nil, nil)); err == nil {
		t.Fatal("a SETUP response without Session must be rejected")
	}
	// A non-decimal session id is a protocol violation.
	session = NewReceiverRtspSession("192.168.1.20")
	driveSession(t, session, RTSPAwaitingSetupAck)
	if _, err := session.Process(rtspRequest("RTSP/1.0 200 OK", "2", map[string]string{"Session": "abc"}, nil)); err == nil {
		t.Fatal("a non-decimal session id must be rejected")
	}
	// The selection must keep the AAC interleaved profile.
	session = NewReceiverRtspSession("192.168.1.20")
	driveSession(t, session, RTSPAwaitingSelectedParams)
	badSelection := rtspRequest("SET_PARAMETER rtsp://source/wfd1.0 RTSP/1.0", "3", nil, []byte("wfd_video_formats: none\r\n"))
	if _, err := session.Process(badSelection); err == nil {
		t.Fatal("a selection without the AAC profile must be rejected")
	}
	// The trigger must request SETUP.
	session = NewReceiverRtspSession("192.168.1.20")
	driveSession(t, session, RTSPAwaitingSetupTrigger)
	badTrigger := rtspRequest("SET_PARAMETER rtsp://source/wfd1.0 RTSP/1.0", "4", nil, []byte("wfd_trigger_method: PAUSE\r\n"))
	if _, err := session.Process(badTrigger); err == nil {
		t.Fatal("a non-SETUP trigger must be rejected")
	}
	// TIME_OFFSET must carry a numeric TimeOffset header.
	session = NewReceiverRtspSession("192.168.1.20")
	driveSession(t, session, RTSPAwaitingTimeOffset)
	if _, err := session.Process(rtspRequest("TIME_OFFSET rtsp://source/wfd1.0/streamid=0 RTSP/1.0", "5", nil, nil)); err == nil {
		t.Fatal("TIME_OFFSET without a TimeOffset header must be rejected")
	}
	// Unknown ready-state methods stop the session.
	session = NewReceiverRtspSession("192.168.1.20")
	driveSession(t, session, RTSPReady)
	if _, err := session.Process(rtspRequest("PLAY rtsp://source/wfd1.0/streamid=0 RTSP/1.0", "9", nil, nil)); err == nil {
		t.Fatal("an unknown ready-state method must be rejected")
	}
}
