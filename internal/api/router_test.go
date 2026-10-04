package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/miplay-bridge/miplay-bridge/internal/alsa"
	"github.com/miplay-bridge/miplay-bridge/internal/config"
	"github.com/miplay-bridge/miplay-bridge/internal/dlna"
	"github.com/miplay-bridge/miplay-bridge/internal/miplay"
	"github.com/miplay-bridge/miplay-bridge/internal/mpd"
	"github.com/miplay-bridge/miplay-bridge/internal/output"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// fakeKernel is a minimal in-process MPD server covering the command subset
// the HTTP handlers drive.
type fakeKernel struct {
	listener net.Listener

	mu       sync.Mutex
	commands []string
	state    string
	volume   int
	busy     bool
}

func newFakeKernel(t *testing.T) *fakeKernel {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	kernel := &fakeKernel{listener: listener, state: "stop", volume: 50}
	go kernel.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return kernel
}

func (k *fakeKernel) port() int {
	return k.listener.Addr().(*net.TCPAddr).Port
}

func (k *fakeKernel) serve() {
	for {
		connection, err := k.listener.Accept()
		if err != nil {
			return
		}
		go k.handle(connection)
	}
}

func (k *fakeKernel) handle(connection net.Conn) {
	defer connection.Close()
	fmt.Fprint(connection, "OK MPD 0.23.5\n")
	reader := bufio.NewReader(connection)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k.mu.Lock()
		k.commands = append(k.commands, line)
		state, volume, busy := k.state, k.volume, k.busy
		k.mu.Unlock()

		if busy {
			fmt.Fprint(connection, "ACK [50@0] {add} Resource busy\n")
			continue
		}

		fields := strings.Fields(line)
		switch fields[0] {
		case "status":
			fmt.Fprintf(connection, "volume: %d\nstate: %s\nelapsed: 3.500\nduration: 210.000\nOK\n", volume, state)
		case "currentsong":
			fmt.Fprint(connection, "file: http://127.0.0.1:8080/stream/1\nTitle: Test Track\nArtist: Test Artist\nAlbum: Test Album\nduration: 210.000\nOK\n")
		case "play":
			k.setState("play")
			fmt.Fprint(connection, "OK\n")
		case "pause":
			if len(fields) > 1 && fields[1] == "1" {
				k.setState("pause")
			} else {
				k.setState("play")
			}
			fmt.Fprint(connection, "OK\n")
		case "stop":
			k.setState("stop")
			fmt.Fprint(connection, "OK\n")
		case "setvol":
			if len(fields) > 1 {
				var parsed int
				_, _ = fmt.Sscanf(fields[1], "%d", &parsed)
				k.mu.Lock()
				k.volume = parsed
				k.mu.Unlock()
			}
			fmt.Fprint(connection, "OK\n")
		case "clear", "add", "seekcur", "next", "previous":
			fmt.Fprint(connection, "OK\n")
		default:
			fmt.Fprintf(connection, "ACK [5@0] {%s} unknown command\n", fields[0])
		}
	}
}

func (k *fakeKernel) setState(state string) {
	k.mu.Lock()
	k.state = state
	k.mu.Unlock()
}

func (k *fakeKernel) setBusy(busy bool) {
	k.mu.Lock()
	k.busy = busy
	k.mu.Unlock()
}

func (k *fakeKernel) recorded() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string{}, k.commands...)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type harness struct {
	kernel   *fakeKernel
	cfg      config.Config
	logger   *slog.Logger
	report   alsa.Report
	mixer    *alsa.Mixer
	engine   *output.Engine
	receiver *miplay.Receiver
	renderer *dlna.Renderer
	tone     *ToneProvider
}

// newHarness assembles the real engine over a fake kernel with an
// uninitialised mixer, plus a real MiPlay receiver, DLNA renderer and tone
// provider. None of them need hardware or the network for these tests.
func newHarness(t *testing.T, adjust ...func(*config.Config)) *harness {
	t.Helper()
	kernel := newFakeKernel(t)
	cfg := config.Default()
	cfg.MPDHost = "127.0.0.1"
	cfg.MPDPort = kernel.port()
	cfg.DataDirectory = t.TempDir()
	for _, fn := range adjust {
		fn(&cfg)
	}
	logger := testLogger()
	mixer := alsa.New(cfg, logger)
	engine := output.New(cfg, logger, mpd.NewManager(cfg, logger), mixer)
	return &harness{
		kernel: kernel,
		cfg:    cfg,
		logger: logger,
		report: alsa.Report{
			SoundDirectory: "/dev/snd",
			Present:        true,
			Card:           0,
			PCMDevice:      0,
			HardwareDevice: "hw:0,0",
			ControlAccess:  alsa.AccessOK,
			PCMAccess:      alsa.AccessOK,
		},
		mixer:    mixer,
		engine:   engine,
		receiver: miplay.NewReceiver(cfg, logger, miplay.Hooks{}),
		renderer: dlna.NewRenderer(cfg, logger, engine, Version),
		tone:     NewToneProvider(cfg),
	}
}

// routes builds the handler tree with optional component overrides.
func (h *harness) routes(adjust ...func(*Options)) http.Handler {
	options := Options{
		Config: h.cfg,
		Logger: h.logger,
		Report: h.report,
		Mixer:  h.mixer,
		Engine: h.engine,
		MiPlay: h.receiver,
		DLNA:   h.renderer,
		Tone:   h.tone,
	}
	for _, fn := range adjust {
		fn(&options)
	}
	return New(options).Routes()
}

// do performs one JSON request against the handler.
func do(t *testing.T, handler http.Handler, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return payload
}

func lastCommands(commands []string, count int) []string {
	if len(commands) < count {
		return commands
	}
	return commands[len(commands)-count:]
}

// mutatingCommands drops the read-only status probes so ordering assertions
// see only the commands that actually change playback state.
func mutatingCommands(commands []string) []string {
	result := make([]string, 0, len(commands))
	for _, command := range commands {
		head := command
		if index := strings.IndexByte(command, ' '); index >= 0 {
			head = command[:index]
		}
		switch head {
		case "status", "currentsong":
			continue
		}
		result = append(result, command)
	}
	return result
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func containsString(values []any, want string) bool {
	for _, value := range values {
		if text, ok := value.(string); ok && text == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// health and auth
// ---------------------------------------------------------------------------

func TestHealthzDegradedAndUnhealthy(t *testing.T) {
	h := newHarness(t)
	handler := h.routes()

	recorder := do(t, handler, "GET", "/healthz", nil, nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	payload := decodeJSON(t, recorder)
	if payload["status"] != "degraded" {
		t.Fatalf("status = %v", payload["status"])
	}
	reasons, _ := payload["reasons"].([]any)
	if !containsString(reasons, "playback kernel is not running") || !containsString(reasons, "mixer is not initialised") {
		t.Fatalf("reasons = %v", payload["reasons"])
	}
	if payload["auto_mute_fixed"] != false {
		t.Fatalf("auto_mute_fixed = %v", payload["auto_mute_fixed"])
	}

	// /api/healthz mirrors /healthz.
	if recorder := do(t, handler, "GET", "/api/healthz", nil, nil); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}

	// A missing sound card is the hard failure.
	unhealthy := newHarness(t)
	unhealthy.report.Present = false
	recorder = do(t, unhealthy.routes(), "GET", "/healthz", nil, nil)
	payload = decodeJSON(t, recorder)
	if payload["status"] != "unhealthy" {
		t.Fatalf("status = %v", payload["status"])
	}
	reasons, _ = payload["reasons"].([]any)
	if !containsString(reasons, "sound card is not available") {
		t.Fatalf("reasons = %v", payload["reasons"])
	}

	// Health probes must stay unauthenticated even with a token configured.
	guarded := newHarness(t, func(cfg *config.Config) { cfg.Token = "sekret" })
	if recorder := do(t, guarded.routes(), "GET", "/healthz", nil, nil); recorder.Code == http.StatusUnauthorized {
		t.Fatal("health probes must never require a token")
	}
}

func TestGuardEnforcement(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.Token = "sekret" })
	handler := h.routes()

	recorder := do(t, handler, "GET", "/api/status", nil, nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status without a token = %d", recorder.Code)
	}
	if payload := decodeJSON(t, recorder); payload["code"] != "unauthorized" {
		t.Fatalf("payload = %v", payload)
	}
	if recorder := do(t, handler, "GET", "/api/status", nil, map[string]string{"x-bridge-token": "nope"}); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status with a wrong token = %d", recorder.Code)
	}

	for name, headers := range map[string]map[string]string{
		"x-bridge-token":       {"x-bridge-token": "sekret"},
		"x-local-output-token": {"x-local-output-token": "sekret"},
		"bearer lower-case":    {"Authorization": "bearer sekret"},
		"bearer canonical":     {"Authorization": "Bearer sekret"},
	} {
		if recorder := do(t, handler, "GET", "/api/status", nil, headers); recorder.Code != http.StatusOK {
			t.Fatalf("status with %s = %d", name, recorder.Code)
		}
	}

	// The console page stays public; its data operations are guarded.
	if recorder := do(t, handler, "GET", "/", nil, nil); recorder.Code != http.StatusOK {
		t.Fatalf("console status = %d", recorder.Code)
	}

	// Without a configured token every endpoint is open.
	open := newHarness(t)
	if recorder := do(t, open.routes(), "GET", "/api/status", nil, nil); recorder.Code != http.StatusOK {
		t.Fatalf("tokenless status = %d", recorder.Code)
	}
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func TestStatusPayload(t *testing.T) {
	h := newHarness(t)
	handler := h.routes()

	recorder := do(t, handler, "GET", "/api/status", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	payload := decodeJSON(t, recorder)
	if payload["success"] != true || payload["active_source"] != "idle" {
		t.Fatalf("payload = %v", payload)
	}
	playback, _ := payload["playback"].(map[string]any)
	if playback["state"] != "stop" || playback["title"] != "Test Track" || playback["volume"] != float64(50) {
		t.Fatalf("playback = %v", playback)
	}
	miplaySlice, _ := payload["miplay"].(map[string]any)
	if miplaySlice["enabled"] != true {
		t.Fatalf("miplay = %v", miplaySlice)
	}
	dlnaSlice, _ := payload["dlna"].(map[string]any)
	if dlnaSlice["enabled"] != true || dlnaSlice["friendly_name"] != h.cfg.DLNAFriendlyName {
		t.Fatalf("dlna = %v", dlnaSlice)
	}

	// Claiming the slot for DLNA is reflected immediately.
	if err := h.engine.ClaimDLNA(); err != nil {
		t.Fatal(err)
	}
	payload = decodeJSON(t, do(t, handler, "GET", "/api/status", nil, nil))
	if payload["active_source"] != "dlna" {
		t.Fatalf("active_source = %v", payload["active_source"])
	}

	// Disabled features report enabled:false slices.
	disabled := h.routes(func(options *Options) { options.MiPlay = nil; options.DLNA = nil })
	payload = decodeJSON(t, do(t, disabled, "GET", "/api/status", nil, nil))
	if slice, _ := payload["miplay"].(map[string]any); slice["enabled"] != false {
		t.Fatalf("miplay = %v", payload["miplay"])
	}
	if slice, _ := payload["dlna"].(map[string]any); slice["enabled"] != false {
		t.Fatalf("dlna = %v", payload["dlna"])
	}
}

// ---------------------------------------------------------------------------
// playback and control
// ---------------------------------------------------------------------------

func TestPlayEndpointDrivesTheKernel(t *testing.T) {
	h := newHarness(t)
	handler := h.routes()

	recorder := do(t, handler, "POST", "/api/play", map[string]any{
		"url":    "http://host/song.flac",
		"volume": 42,
	}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	payload := decodeJSON(t, recorder)
	if payload["success"] != true {
		t.Fatalf("payload = %v", payload)
	}
	if playback, _ := payload["playback"].(map[string]any); playback["state"] != "play" {
		t.Fatalf("playback = %v", payload["playback"])
	}
	want := []string{"clear", "add http://host/song.flac", "setvol 42", "play"}
	if got := lastCommands(mutatingCommands(h.kernel.recorded()), len(want)); !equalStrings(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}

	// start_paused leaves the queue paused right after play.
	recorder = do(t, handler, "POST", "/api/play", map[string]any{
		"url":          "http://host/next.flac",
		"start_paused": true,
	}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	want = []string{"clear", "add http://host/next.flac", "play", "pause 1"}
	if got := lastCommands(mutatingCommands(h.kernel.recorded()), len(want)); !equalStrings(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}

	// A start position maps onto seekcur.
	recorder = do(t, handler, "POST", "/api/play", map[string]any{
		"url":      "http://host/third.flac",
		"position": 12.5,
	}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	want = []string{"clear", "add http://host/third.flac", "play", "seekcur 12.500"}
	if got := lastCommands(mutatingCommands(h.kernel.recorded()), len(want)); !equalStrings(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

func TestPlayEndpointValidation(t *testing.T) {
	h := newHarness(t)
	handler := h.routes()

	if recorder := do(t, handler, "POST", "/api/play", map[string]any{}, nil); recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing url status = %d", recorder.Code)
	}
	if recorder := do(t, handler, "POST", "/api/play", map[string]any{"url": "ftp://host/a"}, nil); recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad scheme status = %d", recorder.Code)
	}
	if recorder := do(t, handler, "POST", "/api/play", map[string]any{"url": "file:///etc/passwd"}, nil); recorder.Code != http.StatusBadRequest {
		t.Fatalf("outside-library status = %d", recorder.Code)
	}
	// Unknown JSON fields are rejected.
	if recorder := do(t, handler, "POST", "/api/play", map[string]any{"url": "http://host/a", "bogus": 1}, nil); recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", recorder.Code)
	}
}

func TestPlayEndpointDeviceBusy(t *testing.T) {
	// A DLNA claim refuses the API play with 409 device_busy.
	h := newHarness(t)
	if err := h.engine.ClaimDLNA(); err != nil {
		t.Fatal(err)
	}
	recorder := do(t, h.routes(), "POST", "/api/play", map[string]any{"url": "http://host/song.flac"}, nil)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if payload := decodeJSON(t, recorder); payload["code"] != "device_busy" {
		t.Fatalf("payload = %v", payload)
	}

	// A busy ALSA device inside the kernel maps onto the same 409, and the
	// API claim is rolled back.
	busy := newHarness(t)
	busy.kernel.setBusy(true)
	recorder = do(t, busy.routes(), "POST", "/api/play", map[string]any{"url": "http://host/song.flac"}, nil)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if payload := decodeJSON(t, recorder); payload["code"] != "device_busy" {
		t.Fatalf("payload = %v", payload)
	}
	if busy.engine.ActiveSource() != output.SourceIdle {
		t.Fatalf("active = %q, want the claim rolled back", busy.engine.ActiveSource())
	}
}

func TestControlEndpoint(t *testing.T) {
	h := newHarness(t)
	handler := h.routes()

	cases := []struct {
		action string
		want   string
	}{
		{"play", "play"},
		{"pause", "pause 1"},
		{"resume", "pause 0"},
		{"next", "next"},
		{"prev", "previous"},
		{"stop", "stop"},
	}
	for _, testCase := range cases {
		recorder := do(t, handler, "POST", "/api/control", map[string]any{"action": testCase.action}, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", testCase.action, recorder.Code, recorder.Body.String())
		}
		if got := lastCommands(mutatingCommands(h.kernel.recorded()), 1); !equalStrings(got, []string{testCase.want}) {
			t.Fatalf("%s command = %v, want %q", testCase.action, got, testCase.want)
		}
	}

	// Toggle pauses a playing kernel and resumes a paused one.
	if recorder := do(t, handler, "POST", "/api/control", map[string]any{"action": "play"}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("play status = %d", recorder.Code)
	}
	if recorder := do(t, handler, "POST", "/api/control", map[string]any{"action": "toggle"}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("toggle status = %d", recorder.Code)
	}
	if got := lastCommands(mutatingCommands(h.kernel.recorded()), 1); !equalStrings(got, []string{"pause 1"}) {
		t.Fatalf("toggle command = %v, want pause 1", got)
	}
	if recorder := do(t, handler, "POST", "/api/control", map[string]any{"action": "toggle"}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("toggle status = %d", recorder.Code)
	}
	if got := lastCommands(mutatingCommands(h.kernel.recorded()), 1); !equalStrings(got, []string{"pause 0"}) {
		t.Fatalf("second toggle command = %v, want pause 0", got)
	}

	// Seek accepts value and position, rejects negatives.
	if recorder := do(t, handler, "POST", "/api/control", map[string]any{"action": "seek", "value": 30}, nil); recorder.Code != http.StatusOK {
		t.Fatalf("seek status = %d", recorder.Code)
	}
	if got := lastCommands(mutatingCommands(h.kernel.recorded()), 1); !equalStrings(got, []string{"seekcur 30.000"}) {
		t.Fatalf("seek command = %v", got)
	}
	if recorder := do(t, handler, "POST", "/api/control", map[string]any{"action": "seek", "position": -1}, nil); recorder.Code != http.StatusBadRequest {
		t.Fatalf("negative seek status = %d", recorder.Code)
	}

	// Unknown actions are rejected with 400.
	recorder := do(t, handler, "POST", "/api/control", map[string]any{"action": "frobnicate"}, nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown action status = %d", recorder.Code)
	}
	if payload := decodeJSON(t, recorder); payload["code"] != "invalid_request" {
		t.Fatalf("payload = %v", payload)
	}
}

func TestControlStopAll(t *testing.T) {
	h := newHarness(t)
	handler := h.routes()

	if err := h.engine.ClaimDLNA(); err != nil {
		t.Fatal(err)
	}
	recorder := do(t, handler, "POST", "/api/control", map[string]any{"action": "stop_all"}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if h.engine.ActiveSource() != output.SourceIdle {
		t.Fatalf("active = %q, want idle after stop_all", h.engine.ActiveSource())
	}
	commands := mutatingCommands(h.kernel.recorded())
	hasStop, hasClear := false, false
	for _, command := range commands {
		switch command {
		case "stop":
			hasStop = true
		case "clear":
			hasClear = true
		}
	}
	if !hasStop || !hasClear {
		t.Fatalf("commands = %v, want both stop and clear", commands)
	}
}

// ---------------------------------------------------------------------------
// volume
// ---------------------------------------------------------------------------

func TestVolumeEndpoint(t *testing.T) {
	h := newHarness(t)
	handler := h.routes()

	recorder := do(t, handler, "POST", "/api/volume", map[string]any{"volume": 55}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := lastCommands(mutatingCommands(h.kernel.recorded()), 1); !equalStrings(got, []string{"setvol 55"}) {
		t.Fatalf("command = %v, want setvol 55", got)
	}

	if recorder := do(t, handler, "POST", "/api/volume", map[string]any{"volume": 150}, nil); recorder.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range status = %d", recorder.Code)
	}
	if recorder := do(t, handler, "POST", "/api/volume", map[string]any{"volume": -1}, nil); recorder.Code != http.StatusBadRequest {
		t.Fatalf("negative status = %d", recorder.Code)
	}
	if recorder := do(t, handler, "POST", "/api/volume", map[string]any{}, nil); recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty status = %d", recorder.Code)
	}

	// Mute needs an initialised hardware mixer.
	recorder = do(t, handler, "POST", "/api/volume", map[string]any{"mute": true}, nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("mute status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if payload := decodeJSON(t, recorder); payload["code"] != "mixer_unavailable" {
		t.Fatalf("payload = %v", payload)
	}

	// Without the kernel the endpoint reports mpd_unavailable.
	disabled := h.routes(func(options *Options) { options.Engine = nil })
	recorder = do(t, disabled, "POST", "/api/volume", map[string]any{"volume": 10}, nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
	if payload := decodeJSON(t, recorder); payload["code"] != "mpd_unavailable" {
		t.Fatalf("payload = %v", payload)
	}
}

// ---------------------------------------------------------------------------
// disconnect endpoints
// ---------------------------------------------------------------------------

func TestDisconnectEndpoints(t *testing.T) {
	h := newHarness(t)
	handler := h.routes()

	// MiPlay: a receiver without an active cast reports was_connected false.
	recorder := do(t, handler, "POST", "/api/miplay/disconnect", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	payload := decodeJSON(t, recorder)
	if payload["success"] != true || payload["was_connected"] != false {
		t.Fatalf("payload = %v", payload)
	}

	// DLNA: an idle renderer reports was_active false.
	recorder = do(t, handler, "POST", "/api/dlna/disconnect", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	payload = decodeJSON(t, recorder)
	if payload["success"] != true || payload["was_active"] != false {
		t.Fatalf("payload = %v", payload)
	}

	// An active DLNA claim is reported and cleared, freeing the slot.
	if err := h.engine.ClaimDLNA(); err != nil {
		t.Fatal(err)
	}
	recorder = do(t, handler, "POST", "/api/dlna/disconnect", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	payload = decodeJSON(t, recorder)
	if payload["success"] != true || payload["was_active"] != true {
		t.Fatalf("payload = %v", payload)
	}
	if h.engine.ActiveSource() != output.SourceIdle {
		t.Fatalf("active = %q, want idle after the disconnect", h.engine.ActiveSource())
	}

	// Disabled features answer idempotently.
	disabled := h.routes(func(options *Options) { options.MiPlay = nil; options.DLNA = nil })
	payload = decodeJSON(t, do(t, disabled, "POST", "/api/miplay/disconnect", nil, nil))
	if payload["success"] != true || payload["was_connected"] != false {
		t.Fatalf("payload = %v", payload)
	}
	payload = decodeJSON(t, do(t, disabled, "POST", "/api/dlna/disconnect", nil, nil))
	if payload["success"] != true || payload["was_active"] != false {
		t.Fatalf("payload = %v", payload)
	}
}

// ---------------------------------------------------------------------------
// test tone
// ---------------------------------------------------------------------------

func TestTestPlayEndpointAndToneServing(t *testing.T) {
	h := newHarness(t)
	handler := h.routes()

	recorder := do(t, handler, "POST", "/api/test-play", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	payload := decodeJSON(t, recorder)
	if payload["success"] != true {
		t.Fatalf("payload = %v", payload)
	}
	toneURL, _ := payload["url"].(string)
	if !strings.Contains(toneURL, "/tone/") {
		t.Fatalf("url = %q", toneURL)
	}
	if int(payload["seconds"].(float64)) != h.cfg.TestToneSeconds {
		t.Fatalf("seconds = %v", payload["seconds"])
	}
	if int(payload["frequency_hz"].(float64)) != h.cfg.TestToneFrequencyHz {
		t.Fatalf("frequency = %v", payload["frequency_hz"])
	}
	if playback, _ := payload["playback"].(map[string]any); playback["state"] != "play" {
		t.Fatalf("playback = %v", payload["playback"])
	}

	// The kernel received the tone URL over the production path.
	commands := mutatingCommands(h.kernel.recorded())
	if len(commands) < 2 || !strings.HasPrefix(commands[len(commands)-2], "add http://") || !strings.HasSuffix(commands[len(commands)-2], "/tone.wav") {
		t.Fatalf("commands = %v", commands)
	}

	// The advertised URL serves the generated WAV over the same handler.
	parsed, err := url.Parse(toneURL)
	if err != nil {
		t.Fatal(err)
	}
	toneRecorder := do(t, handler, "GET", parsed.Path, nil, nil)
	if toneRecorder.Code != http.StatusOK {
		t.Fatalf("tone status = %d", toneRecorder.Code)
	}
	if contentType := toneRecorder.Header().Get("Content-Type"); contentType != "audio/wav" {
		t.Fatalf("content type = %q", contentType)
	}
	if cacheControl := toneRecorder.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("cache control = %q", cacheControl)
	}
	audio := toneRecorder.Body.Bytes()
	if len(audio) < 44 || string(audio[:4]) != "RIFF" || string(audio[8:12]) != "WAVE" {
		t.Fatalf("tone is not a WAV: %d bytes", len(audio))
	}

	// Unknown tokens are 404.
	if recorder := do(t, handler, "GET", "/tone/deadbeef/tone.wav", nil, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown token status = %d", recorder.Code)
	}

	// Contention: a DLNA claim refuses the test tone.
	busy := newHarness(t)
	if err := busy.engine.ClaimDLNA(); err != nil {
		t.Fatal(err)
	}
	recorder = do(t, busy.routes(), "POST", "/api/test-play", nil, nil)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if payload := decodeJSON(t, recorder); payload["code"] != "device_busy" {
		t.Fatalf("payload = %v", payload)
	}

	// Without a tone provider the endpoint is unavailable.
	recorder = do(t, h.routes(func(options *Options) { options.Tone = nil }), "POST", "/api/test-play", nil, nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
}

// ---------------------------------------------------------------------------
// DLNA paths and the embedded console
// ---------------------------------------------------------------------------

func TestDLNAMountedPaths(t *testing.T) {
	h := newHarness(t)
	handler := h.routes()

	// SCPD documents are served.
	recorder := do(t, handler, "GET", "/dlna/scpd/avtransport.xml", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("scpd status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "<name>SetAVTransportURI</name>") {
		t.Fatalf("scpd body = %s", recorder.Body.String())
	}

	// A RenderingControl GetVolume SOAP round trip against the real engine.
	envelope := `<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:GetVolume xmlns:u="urn:schemas-upnp-org:service:RenderingControl:1"><InstanceID>0</InstanceID><Channel>Master</Channel></u:GetVolume></s:Body></s:Envelope>`
	soapRequest := httptest.NewRequest("POST", "/dlna/control/renderingcontrol", strings.NewReader(envelope))
	soapRequest.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:RenderingControl:1#GetVolume"`)
	soapRequest.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	soapRecorder := httptest.NewRecorder()
	handler.ServeHTTP(soapRecorder, soapRequest)
	if soapRecorder.Code != http.StatusOK {
		t.Fatalf("soap status = %d, body = %s", soapRecorder.Code, soapRecorder.Body.String())
	}
	if !strings.Contains(soapRecorder.Body.String(), "<CurrentVolume>") {
		t.Fatalf("soap body = %s", soapRecorder.Body.String())
	}

	// GENA subscribe and unsubscribe round trip.
	subscribe := httptest.NewRequest("SUBSCRIBE", "/dlna/event/renderingcontrol", nil)
	subscribe.Header.Set("CALLBACK", "<http://127.0.0.1:1/notify>")
	subscribe.Header.Set("NT", "upnp:event")
	subscribe.Header.Set("TIMEOUT", "Second-300")
	subscribeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(subscribeRecorder, subscribe)
	if subscribeRecorder.Code != http.StatusOK {
		t.Fatalf("subscribe status = %d", subscribeRecorder.Code)
	}
	sid := subscribeRecorder.Header().Get("SID")
	if !strings.HasPrefix(sid, "uuid:") {
		t.Fatalf("SID = %q", sid)
	}
	if timeout := subscribeRecorder.Header().Get("TIMEOUT"); timeout != "Second-300" {
		t.Fatalf("TIMEOUT = %q", timeout)
	}

	unsubscribe := httptest.NewRequest("UNSUBSCRIBE", "/dlna/event/renderingcontrol", nil)
	unsubscribe.Header.Set("SID", sid)
	unsubscribeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(unsubscribeRecorder, unsubscribe)
	if unsubscribeRecorder.Code != http.StatusOK {
		t.Fatalf("unsubscribe status = %d", unsubscribeRecorder.Code)
	}

	// The device description needs a started renderer (no SSDP in tests).
	if recorder := do(t, handler, "GET", "/dlna/description.xml", nil, nil); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("description status = %d", recorder.Code)
	}
}

func TestConsolePageServed(t *testing.T) {
	h := newHarness(t)
	handler := h.routes()

	recorder := do(t, handler, "GET", "/", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
		t.Fatalf("content type = %q", contentType)
	}
	body := recorder.Body.String()
	for _, expected := range []string{"妙播桥", "清除妙播连接", "清除 DLNA 连接", "测试声卡"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("console page missing %q", expected)
		}
	}

	// The alias path works and unknown paths fall through to a 404.
	if recorder := do(t, handler, "GET", "/index.html", nil, nil); recorder.Code != http.StatusOK {
		t.Fatalf("alias status = %d", recorder.Code)
	}
	if recorder := do(t, handler, "GET", "/nope", nil, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d", recorder.Code)
	}
}

func TestCORSPreflight(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.Token = "sekret" })
	handler := h.routes()

	request := httptest.NewRequest("OPTIONS", "/api/status", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	if origin := recorder.Header().Get("Access-Control-Allow-Origin"); origin != "*" {
		t.Fatalf("allow origin = %q", origin)
	}
	if methods := recorder.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(methods, "SUBSCRIBE") {
		t.Fatalf("allow methods = %q", methods)
	}
}

// ---------------------------------------------------------------------------
// resolvePlayURI
// ---------------------------------------------------------------------------

func TestResolvePlayURI(t *testing.T) {
	cases := []struct {
		raw            string
		musicDirectory string
		want           string
		ok             bool
	}{
		{"http://host/stream.flac", "/music", "http://host/stream.flac", true},
		{"https://host/a?token=1", "/music", "https://host/a?token=1", true},
		{"http:///nohost", "/music", "", false},
		{"ftp://host/a", "/music", "", false},
		{"file:///music/a.mp3", "/music", "a.mp3", true},
		{"file:///etc/passwd", "/music", "", false},
		{"/music/sub/a.mp3", "/music", "sub/a.mp3", true},
		{"/music", "/music", "", false},
		{"/etc/passwd", "/music", "", false},
		{"sub/a.mp3", "/music", "sub/a.mp3", true},
		{"sub/../a.mp3", "/music", "a.mp3", true},
		{"../escape.mp3", "/music", "escape.mp3", true},
		{"", "/music", "", false},
		{"   ", "/music", "", false},
		{"/music/a.mp3", "", "", false},
	}
	for _, testCase := range cases {
		got, err := resolvePlayURI(testCase.raw, testCase.musicDirectory)
		if !testCase.ok {
			if err == nil {
				t.Fatalf("resolvePlayURI(%q, %q) = %q, want an error", testCase.raw, testCase.musicDirectory, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("resolvePlayURI(%q, %q): %v", testCase.raw, testCase.musicDirectory, err)
		}
		if got != testCase.want {
			t.Fatalf("resolvePlayURI(%q, %q) = %q, want %q", testCase.raw, testCase.musicDirectory, got, testCase.want)
		}
	}
}
