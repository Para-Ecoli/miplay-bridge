package dlna

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
	"github.com/miplay-bridge/miplay-bridge/internal/output"
)

// fakeVolumePlayer is a scripted output.Player for the RenderingControl
// volume paths: reads answer the live level, writes are recorded, and either
// surface can degrade to the ErrNoVolumeControl contract.
type fakeVolumePlayer struct {
	mu         sync.Mutex
	volume     int
	muted      bool
	noControl  bool
	failVolume bool
	setVolumes []int
	setMutes   []bool
}

func (p *fakeVolumePlayer) ClaimDLNA() error                    { return nil }
func (p *fakeVolumePlayer) ReleaseDLNA()                        {}
func (p *fakeVolumePlayer) ActiveSource() output.Source         { return output.SourceDLNA }
func (p *fakeVolumePlayer) Load(context.Context, string) error  { return nil }
func (p *fakeVolumePlayer) Clear(context.Context) error         { return nil }
func (p *fakeVolumePlayer) Play(context.Context) error          { return nil }
func (p *fakeVolumePlayer) Pause(context.Context) error         { return nil }
func (p *fakeVolumePlayer) Stop(context.Context) error          { return nil }
func (p *fakeVolumePlayer) Seek(context.Context, float64) error { return nil }
func (p *fakeVolumePlayer) Info(context.Context) output.PlaybackInfo {
	return output.PlaybackInfo{State: "stop"}
}

func (p *fakeVolumePlayer) SetVolume(_ context.Context, percent int) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.noControl {
		return 0, output.ErrNoVolumeControl
	}
	if p.failVolume {
		return 0, errors.New("amixer exploded")
	}
	p.setVolumes = append(p.setVolumes, percent)
	p.volume = percent
	return percent, nil
}

func (p *fakeVolumePlayer) SetMute(_ context.Context, muted bool) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.noControl {
		return false, output.ErrNoVolumeControl
	}
	if p.failVolume {
		return false, errors.New("amixer exploded")
	}
	p.setMutes = append(p.setMutes, muted)
	p.muted = muted
	return muted, nil
}

func (p *fakeVolumePlayer) Volume(context.Context) (int, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.noControl {
		return 0, false, output.ErrNoVolumeControl
	}
	if p.failVolume {
		return 0, false, errors.New("amixer exploded")
	}
	return p.volume, p.muted, nil
}

func (p *fakeVolumePlayer) volumes() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int{}, p.setVolumes...)
}

func (p *fakeVolumePlayer) mutes() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool{}, p.setMutes...)
}

// renderingControlRequest drives one RenderingControl SOAP action through the
// renderer handler exactly as a control point would.
func renderingControlRequest(t *testing.T, handler http.Handler, action, inner string) *httptest.ResponseRecorder {
	t.Helper()
	envelope := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:%s xmlns:u="urn:schemas-upnp-org:service:RenderingControl:1">%s</u:%s></s:Body></s:Envelope>`, action, inner, action)
	request := httptest.NewRequest("POST", "/dlna/control/renderingcontrol", strings.NewReader(envelope))
	request.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:RenderingControl:1#`+action+`"`)
	request.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func newVolumeRenderer(t *testing.T, player *fakeVolumePlayer) (*Renderer, http.Handler) {
	t.Helper()
	renderer := NewRenderer(config.Default(), testLogger(), player, "test")
	t.Cleanup(renderer.Shutdown)
	return renderer, renderer.Handler()
}

// ---------------------------------------------------------------------------
// Get/SetVolume and Get/SetMute
// ---------------------------------------------------------------------------

func TestRenderingControlVolumeRoundTrip(t *testing.T) {
	player := &fakeVolumePlayer{volume: 42}
	_, handler := newVolumeRenderer(t, player)

	recorder := renderingControlRequest(t, handler, "GetVolume", "<InstanceID>0</InstanceID><Channel>Master</Channel>")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GetVolume status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "<CurrentVolume>42</CurrentVolume>") {
		t.Fatalf("GetVolume body = %s", recorder.Body.String())
	}

	recorder = renderingControlRequest(t, handler, "SetVolume",
		"<InstanceID>0</InstanceID><Channel>Master</Channel><DesiredVolume>33</DesiredVolume>")
	if recorder.Code != http.StatusOK {
		t.Fatalf("SetVolume status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := player.volumes(); len(got) != 1 || got[0] != 33 {
		t.Fatalf("written volumes = %v, want [33]", got)
	}

	// A later read sees the live level (one shared hardware control).
	recorder = renderingControlRequest(t, handler, "GetVolume", "<InstanceID>0</InstanceID><Channel>Master</Channel>")
	if !strings.Contains(recorder.Body.String(), "<CurrentVolume>33</CurrentVolume>") {
		t.Fatalf("read-back body = %s", recorder.Body.String())
	}
}

func TestRenderingControlMuteRoundTrip(t *testing.T) {
	player := &fakeVolumePlayer{volume: 42}
	_, handler := newVolumeRenderer(t, player)

	recorder := renderingControlRequest(t, handler, "SetMute",
		"<InstanceID>0</InstanceID><Channel>Master</Channel><DesiredMute>1</DesiredMute>")
	if recorder.Code != http.StatusOK {
		t.Fatalf("SetMute status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := player.mutes(); len(got) != 1 || !got[0] {
		t.Fatalf("written mutes = %v, want [true]", got)
	}
	recorder = renderingControlRequest(t, handler, "GetMute", "<InstanceID>0</InstanceID><Channel>Master</Channel>")
	if !strings.Contains(recorder.Body.String(), "<CurrentMute>1</CurrentMute>") {
		t.Fatalf("GetMute body = %s", recorder.Body.String())
	}

	if recorder := renderingControlRequest(t, handler, "SetMute",
		"<InstanceID>0</InstanceID><Channel>Master</Channel><DesiredMute>0</DesiredMute>"); recorder.Code != http.StatusOK {
		t.Fatalf("unmute status = %d", recorder.Code)
	}
	if got := player.mutes(); len(got) != 2 || got[1] {
		t.Fatalf("written mutes = %v, want [true false]", got)
	}
}

func TestRenderingControlVolumeValidation(t *testing.T) {
	player := &fakeVolumePlayer{volume: 42}
	_, handler := newVolumeRenderer(t, player)

	cases := []struct {
		name   string
		action string
		inner  string
	}{
		{"volume not a number", "SetVolume", "<InstanceID>0</InstanceID><DesiredVolume>abc</DesiredVolume>"},
		{"volume over range", "SetVolume", "<InstanceID>0</InstanceID><DesiredVolume>101</DesiredVolume>"},
		{"volume below range", "SetVolume", "<InstanceID>0</InstanceID><DesiredVolume>-1</DesiredVolume>"},
		{"unknown channel", "GetVolume", "<InstanceID>0</InstanceID><Channel>FrontLeft</Channel>"},
		{"bad instance", "GetVolume", "<InstanceID>1</InstanceID>"},
		{"mute not a flag", "SetMute", "<InstanceID>0</InstanceID><DesiredMute>yes</DesiredMute>"},
	}
	for _, testCase := range cases {
		recorder := renderingControlRequest(t, handler, testCase.action, testCase.inner)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("%s status = %d, body = %s", testCase.name, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "<errorCode>") {
			t.Fatalf("%s must produce a SOAP fault, body = %s", testCase.name, recorder.Body.String())
		}
	}
	if got := player.volumes(); len(got) != 0 {
		t.Fatalf("invalid requests must not reach the mixer: %v", got)
	}
}

func TestRenderingControlVolumeRuntimeFailureIsAFault(t *testing.T) {
	player := &fakeVolumePlayer{failVolume: true}
	_, handler := newVolumeRenderer(t, player)

	recorder := renderingControlRequest(t, handler, "SetVolume",
		"<InstanceID>0</InstanceID><DesiredVolume>50</DesiredVolume>")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "<errorCode>501</errorCode>") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestRenderingControlDegradesWithoutAVolumeControl(t *testing.T) {
	player := &fakeVolumePlayer{noControl: true}
	_, handler := newVolumeRenderer(t, player)

	// Reads keep the documented full-scale profile.
	recorder := renderingControlRequest(t, handler, "GetVolume", "<InstanceID>0</InstanceID><Channel>Master</Channel>")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "<CurrentVolume>100</CurrentVolume>") {
		t.Fatalf("GetVolume body = %s", recorder.Body.String())
	}
	recorder = renderingControlRequest(t, handler, "GetMute", "<InstanceID>0</InstanceID><Channel>Master</Channel>")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "<CurrentMute>0</CurrentMute>") {
		t.Fatalf("GetMute body = %s", recorder.Body.String())
	}

	// Writes are acknowledged and ignored: the control point panel stays
	// usable and the session never errors out.
	if recorder := renderingControlRequest(t, handler, "SetVolume",
		"<InstanceID>0</InstanceID><DesiredVolume>50</DesiredVolume>"); recorder.Code != http.StatusOK {
		t.Fatalf("SetVolume status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder := renderingControlRequest(t, handler, "SetMute",
		"<InstanceID>0</InstanceID><DesiredMute>1</DesiredMute>"); recorder.Code != http.StatusOK {
		t.Fatalf("SetMute status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := player.volumes(); len(got) != 0 {
		t.Fatalf("degraded writes must stay uncounted: %v", got)
	}
}

// ---------------------------------------------------------------------------
// GENA events
// ---------------------------------------------------------------------------

func TestRenderingControlInitialEventCarriesTheLiveVolume(t *testing.T) {
	records := make(chan genaNotifyRecord, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		records <- genaNotifyRecord{
			method: request.Method,
			sid:    request.Header.Get("SID"),
			body:   string(body),
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	player := &fakeVolumePlayer{volume: 55, muted: true}
	_, handler := newVolumeRenderer(t, player)

	subscribe := httptest.NewRequest("SUBSCRIBE", "/dlna/event/renderingcontrol", nil)
	subscribe.Header.Set("CALLBACK", "<"+server.URL+"/notify>")
	subscribe.Header.Set("NT", "upnp:event")
	subscribe.Header.Set("TIMEOUT", "Second-300")
	subscribeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(subscribeRecorder, subscribe)
	if subscribeRecorder.Code != http.StatusOK {
		t.Fatalf("subscribe status = %d", subscribeRecorder.Code)
	}

	// The initial NOTIFY carries the live hardware level instead of the old
	// hard-coded full-scale snapshot.
	record := awaitNotify(t, records)
	if !strings.Contains(record.body, "val=&#34;55&#34;") || !strings.Contains(record.body, "Mute channel=&#34;Master&#34; val=&#34;1&#34;") {
		t.Fatalf("initial event body = %s", record.body)
	}
}

func TestRenderingControlSetVolumeBroadcastsLastChange(t *testing.T) {
	records := make(chan genaNotifyRecord, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		records <- genaNotifyRecord{method: request.Method, body: string(body)}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	player := &fakeVolumePlayer{volume: 42}
	_, handler := newVolumeRenderer(t, player)

	subscribe := httptest.NewRequest("SUBSCRIBE", "/dlna/event/renderingcontrol", nil)
	subscribe.Header.Set("CALLBACK", "<"+server.URL+"/notify>")
	subscribe.Header.Set("NT", "upnp:event")
	subscribe.Header.Set("TIMEOUT", "Second-300")
	subscribeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(subscribeRecorder, subscribe)
	if subscribeRecorder.Code != http.StatusOK {
		t.Fatalf("subscribe status = %d", subscribeRecorder.Code)
	}
	awaitNotify(t, records) // the initial snapshot

	if recorder := renderingControlRequest(t, handler, "SetVolume",
		"<InstanceID>0</InstanceID><DesiredVolume>33</DesiredVolume>"); recorder.Code != http.StatusOK {
		t.Fatalf("SetVolume status = %d", recorder.Code)
	}
	// The write broadcasts the new level to the subscriber.
	notification := awaitNotify(t, records)
	if !strings.Contains(notification.body, "val=&#34;33&#34;") {
		t.Fatalf("LastChange body = %s", notification.body)
	}
}
