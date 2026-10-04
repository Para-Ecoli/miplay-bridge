package dlna

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestParseSOAPAction(t *testing.T) {
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{`"urn:schemas-upnp-org:service:AVTransport:1#Play"`, "Play", true},
		{"urn:schemas-upnp-org:service:RenderingControl:1#SetVolume", "SetVolume", true},
		{"Play", "Play", true},
		{"", "", false},
		{`"   "`, "", false},
	}
	for _, testCase := range cases {
		request := httptest.NewRequest("POST", "/dlna/control/avtransport", nil)
		if testCase.header != "" {
			request.Header.Set("SOAPAction", testCase.header)
		}
		got, ok := parseSOAPAction(request)
		if got != testCase.want || ok != testCase.ok {
			t.Fatalf("parseSOAPAction(%q) = %q, %v; want %q, %v", testCase.header, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestParseActionArgs(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:Play xmlns:u="urn:schemas-upnp-org:service:AVTransport:1"><InstanceID>0</InstanceID><Speed>1</Speed></u:Play></s:Body></s:Envelope>`)
	action, args, err := parseActionArgs(body)
	if err != nil {
		t.Fatal(err)
	}
	if action != "Play" {
		t.Fatalf("action = %q", action)
	}
	if args["InstanceID"] != "0" || args["Speed"] != "1" {
		t.Fatalf("args = %v", args)
	}
}

func TestParseActionArgsDecodesEscapedMetadata(t *testing.T) {
	body := []byte(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:SetAVTransportURI xmlns:u="urn:schemas-upnp-org:service:AVTransport:1"><InstanceID>0</InstanceID><CurrentURI>http://host/song.flac</CurrentURI><CurrentURIMetaData>&lt;DIDL-Lite&gt;&lt;item&gt;&lt;dc:title&gt;Song&lt;/dc:title&gt;&lt;/item&gt;&lt;/DIDL-Lite&gt;</CurrentURIMetaData></u:SetAVTransportURI></s:Body></s:Envelope>`)
	action, args, err := parseActionArgs(body)
	if err != nil {
		t.Fatal(err)
	}
	if action != "SetAVTransportURI" {
		t.Fatalf("action = %q", action)
	}
	if !strings.Contains(args["CurrentURIMetaData"], "<DIDL-Lite>") {
		t.Fatalf("metadata = %q, want the decoded DIDL-Lite", args["CurrentURIMetaData"])
	}
	if args["CurrentURI"] != "http://host/song.flac" {
		t.Fatalf("args = %v", args)
	}
}

func TestParseActionArgsConcatenatesUnescapedNestedXML(t *testing.T) {
	body := []byte(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:SetAVTransportURI xmlns:u="urn:schemas-upnp-org:service:AVTransport:1"><CurrentURIMetaData><DIDL-Lite><dc:title>Plain</dc:title></DIDL-Lite></CurrentURIMetaData></u:SetAVTransportURI></s:Body></s:Envelope>`)
	_, args, err := parseActionArgs(body)
	if err != nil {
		t.Fatal(err)
	}
	// Unescaped nested XML contributes its text content.
	if args["CurrentURIMetaData"] != "Plain" {
		t.Fatalf("metadata = %q, want the nested text", args["CurrentURIMetaData"])
	}
}

func TestParseActionArgsRejectsMalformedBodies(t *testing.T) {
	if _, _, err := parseActionArgs([]byte("not xml at all")); err == nil {
		t.Fatal("invalid XML must be rejected")
	}
	envelope := []byte(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body></s:Body></s:Envelope>`)
	if _, _, err := parseActionArgs(envelope); err == nil {
		t.Fatal("an envelope without an action element must be rejected")
	}
}

func TestWriteSOAPResponse(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeSOAPResponse(recorder, "urn:schemas-upnp-org:service:RenderingControl:1", "GetVolume", []soapArgument{
		{Name: "CurrentVolume", Value: "42"},
	})
	if recorder.Code != 200 {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `<u:GetVolumeResponse xmlns:u="urn:schemas-upnp-org:service:RenderingControl:1">`) {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(body, "<CurrentVolume>42</CurrentVolume>") {
		t.Fatalf("body = %s", body)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != `text/xml; charset="utf-8"` {
		t.Fatalf("content type = %q", contentType)
	}
}

func TestWriteSOAPFault(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeSOAPFault(recorder, errDeviceBusy)
	if recorder.Code != 500 {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "<errorCode>701</errorCode>") {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(body, "sound card is held by another source") {
		t.Fatalf("body = %s", body)
	}
}

func TestFormatAndParseUPnPTime(t *testing.T) {
	formatted := map[float64]string{
		0:      "0:00:00",
		59.4:   "0:00:59",
		60:     "0:01:00",
		3661.6: "1:01:02",
		-5:     "0:00:00",
	}
	for seconds, want := range formatted {
		if got := formatUPnPTime(seconds); got != want {
			t.Fatalf("formatUPnPTime(%v) = %q, want %q", seconds, got, want)
		}
	}

	parsed := map[string]float64{
		"1:01:02":    3662,
		"0:00:03.5":  3.5,
		"12.5":       12.5,
		"0:00:00":    0,
		" 1:00:00 ":  3600,
		"0:00:59.25": 59.25,
	}
	for value, want := range parsed {
		got, err := parseUPnPTime(value)
		if err != nil {
			t.Fatalf("parseUPnPTime(%q): %v", value, err)
		}
		if got != want {
			t.Fatalf("parseUPnPTime(%q) = %v, want %v", value, got, want)
		}
	}

	for _, invalid := range []string{"", "1:2", "1:61:00", "a:00:00", "-1", "0:00:60", "0:00:xx"} {
		if _, err := parseUPnPTime(invalid); err == nil {
			t.Fatalf("parseUPnPTime(%q) must fail", invalid)
		}
	}
}

func TestParseDIDL(t *testing.T) {
	metadata := `<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/"><item><dc:title>安静</dc:title><upnp:artist>周杰伦</upnp:artist><upnp:album>范特西</upnp:album></item></DIDL-Lite>`
	title, artist, album := parseDIDL(metadata)
	if title != "安静" || artist != "周杰伦" || album != "范特西" {
		t.Fatalf("parsed = %q / %q / %q", title, artist, album)
	}

	if title, artist, album := parseDIDL(""); title != "" || artist != "" || album != "" {
		t.Fatalf("empty metadata = %q / %q / %q", title, artist, album)
	}
	if title, artist, album := parseDIDL("not xml"); title != "" || artist != "" || album != "" {
		t.Fatalf("invalid metadata must degrade to empty fields, got %q / %q / %q", title, artist, album)
	}

	long := strings.Repeat("a", 300)
	metadata = "<DIDL-Lite><item><dc:title>" + long + "</dc:title></item></DIDL-Lite>"
	title, _, _ = parseDIDL(metadata)
	if len([]rune(title)) <= 200 {
		t.Fatalf("long titles must stay truncated to 200 characters, got %d", len(title))
	}
}
