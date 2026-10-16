package api

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
	"github.com/miplay-bridge/miplay-bridge/internal/mix"
)

// ---------------------------------------------------------------------------
// scripted amixer
// ---------------------------------------------------------------------------

// mixerCardInfo is the amixer info block of the scripted card.
const mixerCardInfo = `Card hw:0 'PCH'/'HDA Intel PCH at 0xf7f10000 irq 32'
  Mixer name : 'Realtek ALC269VC'
  Components : 'HDA:10ec0269,10431182,00100200'
  Controls      : 3
  Simple ctrls  : 3
`

// mixerScript is a scripted amixer whose scontents reply reflects the raw
// value of the last Headphone write, so the POST response snapshot (the
// "respond with the new state" contract) can be asserted end to end.
type mixerScript struct {
	mu       sync.Mutex
	commands []string
	raw      int
}

func (f *mixerScript) run(_ context.Context, binary string, args ...string) ([]byte, error) {
	key := strings.Join(args[2:], " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, strings.Join(append([]string{binary}, args...), " "))
	switch {
	case key == "scontents":
		return []byte(f.scontentsLocked()), nil
	case key == "info":
		return []byte(mixerCardInfo), nil
	case strings.HasPrefix(key, "-R sset Headphone playback "):
		first := strings.Split(strings.TrimPrefix(key, "-R sset Headphone playback "), ",")[0]
		if raw, err := strconv.Atoi(first); err == nil {
			f.raw = raw
		}
	}
	return nil, nil
}

// scontentsLocked renders the stereo Headphone control (0-64 scale), a
// switch-only Mono control and a duplicate 'Master',1 control.
func (f *mixerScript) scontentsLocked() string {
	return fmt.Sprintf(`Simple mixer control 'Headphone',0
  Capabilities: pvolume pswitch pswitch-joined
  Playback channels: Front Left - Front Right
  Limits: Playback 0 - 64
  Front Left: Playback %d [50%%] [-32.00dB] [on]
  Front Right: Playback %d [50%%] [-32.00dB] [on]
Simple mixer control 'Mono',0
  Capabilities: pswitch
  Playback channels: Mono
  Mono: Playback [off]
Simple mixer control 'Master',1
  Capabilities: pvolume
  Playback channels: Front Left - Front Right
  Limits: Playback 0 - 64
  Front Left: Playback 10 [15%%] [-48.00dB]
  Front Right: Playback 10 [15%%] [-48.00dB]
`, f.raw, f.raw)
}

func (f *mixerScript) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.commands...)
}

func newMixerTestController(t *testing.T, script *mixerScript) *mix.Controller {
	t.Helper()
	return mix.New(config.Default(), testLogger()).WithRunner(script.run)
}

func mixerRoutes(t *testing.T, controller *mix.Controller) http.Handler {
	t.Helper()
	h := newHarness(t)
	return h.routes(func(options *Options) { options.Mixer = controller })
}

func containsCommand(commands []string, want string) bool {
	for _, command := range commands {
		if command == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// GET /api/mixer
// ---------------------------------------------------------------------------

func TestMixerStateEndpoint(t *testing.T) {
	script := &mixerScript{raw: 48}
	handler := mixerRoutes(t, newMixerTestController(t, script))

	recorder := do(t, handler, "GET", "/api/mixer", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	payload := decodeJSON(t, recorder)
	if payload["success"] != true {
		t.Fatalf("payload = %v", payload)
	}
	mixerSlice, _ := payload["mixer"].(map[string]any)
	card, _ := mixerSlice["card"].(map[string]any)
	if card["id"] != "PCH" || card["mixerName"] != "Realtek ALC269VC" {
		t.Fatalf("card = %v", card)
	}
	controls, _ := mixerSlice["controls"].([]any)
	if len(controls) != 3 {
		t.Fatalf("controls = %v", controls)
	}
	headphone, _ := controls[0].(map[string]any)
	if headphone["name"] != "Headphone" {
		t.Fatalf("first control = %v", headphone)
	}
	limit, _ := headphone["playbackLimit"].(map[string]any)
	if limit["min"] != float64(0) || limit["max"] != float64(64) {
		t.Fatalf("limit = %v", limit)
	}
	channels, _ := headphone["channels"].([]any)
	if len(channels) != 2 {
		t.Fatalf("channels = %v", channels)
	}
	value, _ := channels[0].(map[string]any)["playback"].(map[string]any)
	if value["raw"] != float64(48) || value["percent"] != float64(50) || value["on"] != true {
		t.Fatalf("left channel = %v", value)
	}
	if commands := script.recorded(); !containsCommand(commands, "amixer -c 0 scontents") {
		t.Fatalf("commands = %v", commands)
	}
}

func TestMixerStateUnavailable(t *testing.T) {
	// A bridge without a wired mixer degrades to 503 for both verbs.
	handler := newHarness(t).routes()

	recorder := do(t, handler, "GET", "/api/mixer", nil, nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET status = %d", recorder.Code)
	}
	if payload := decodeJSON(t, recorder); payload["code"] != "mixer_unavailable" {
		t.Fatalf("payload = %v", payload)
	}

	recorder = do(t, handler, "POST", "/api/mixer", map[string]any{"name": "Headphone", "volume": []int{1}}, nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestMixerUnavailableWhenAmixerIsMissing(t *testing.T) {
	controller := mix.New(config.Default(), testLogger()).WithRunner(
		func(context.Context, string, ...string) ([]byte, error) {
			return nil, fmt.Errorf("amixer missing: %w", exec.ErrNotFound)
		})
	handler := mixerRoutes(t, controller)

	recorder := do(t, handler, "GET", "/api/mixer", nil, nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if payload := decodeJSON(t, recorder); payload["code"] != "mixer_unavailable" {
		t.Fatalf("payload = %v", payload)
	}

	recorder = do(t, handler, "POST", "/api/mixer", map[string]any{"name": "Headphone", "volume": []int{1}}, nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if payload := decodeJSON(t, recorder); payload["code"] != "mixer_unavailable" {
		t.Fatalf("payload = %v", payload)
	}
}

// ---------------------------------------------------------------------------
// POST /api/mixer
// ---------------------------------------------------------------------------

func TestMixerSetVolumeWritesAndReturnsTheSnapshot(t *testing.T) {
	script := &mixerScript{raw: 48}
	handler := mixerRoutes(t, newMixerTestController(t, script))

	// A single value is expanded to every volume channel (uniform control).
	recorder := do(t, handler, "POST", "/api/mixer", map[string]any{
		"name": "Headphone", "stream": "playback", "volume": []int{42},
	}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if commands := script.recorded(); !containsCommand(commands, "amixer -c 0 -R sset Headphone playback 42,42") {
		t.Fatalf("write command missing, commands = %v", commands)
	}

	// The response carries the post-write snapshot: one round trip realigns
	// the panel.
	payload := decodeJSON(t, recorder)
	controls, _ := payload["mixer"].(map[string]any)["controls"].([]any)
	channels, _ := controls[0].(map[string]any)["channels"].([]any)
	value, _ := channels[0].(map[string]any)["playback"].(map[string]any)
	if value["raw"] != float64(42) {
		t.Fatalf("snapshot after the write = %v", value)
	}

	// Explicit per-channel values pass through verbatim.
	recorder = do(t, handler, "POST", "/api/mixer", map[string]any{
		"name": "Headphone", "stream": "playback", "volume": []int{10, 20},
	}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if commands := script.recorded(); !containsCommand(commands, "amixer -c 0 -R sset Headphone playback 10,20") {
		t.Fatalf("per-channel write missing, commands = %v", commands)
	}

	// Out-of-range values are clamped to the control limits.
	recorder = do(t, handler, "POST", "/api/mixer", map[string]any{
		"name": "Headphone", "volume": []int{999},
	}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if commands := script.recorded(); !containsCommand(commands, "amixer -c 0 -R sset Headphone playback 64,64") {
		t.Fatalf("clamped write missing, commands = %v", commands)
	}
}

func TestMixerSetSwitch(t *testing.T) {
	cases := []struct {
		action string
		want   string
	}{
		{"on", "amixer -c 0 sset Headphone playback unmute"},
		{"off", "amixer -c 0 sset Headphone playback mute"},
		{"toggle", "amixer -c 0 sset Headphone playback toggle"},
	}
	for _, testCase := range cases {
		script := &mixerScript{raw: 48}
		handler := mixerRoutes(t, newMixerTestController(t, script))
		recorder := do(t, handler, "POST", "/api/mixer", map[string]any{
			"name": "Headphone", "stream": "playback", "switch": testCase.action,
		}, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("switch %s status = %d, body = %s", testCase.action, recorder.Code, recorder.Body.String())
		}
		if commands := script.recorded(); !containsCommand(commands, testCase.want) {
			t.Fatalf("switch %s commands = %v, want %q", testCase.action, commands, testCase.want)
		}
	}
}

func TestMixerSetValidation(t *testing.T) {
	script := &mixerScript{raw: 48}
	handler := mixerRoutes(t, newMixerTestController(t, script))

	cases := []struct {
		name      string
		body      any
		wantError string
	}{
		{"missing name", map[string]any{"volume": []int{1}}, "缺少控件名"},
		{"volume and switch", map[string]any{"name": "Headphone", "volume": []int{1}, "switch": "on"}, "volume 与 switch 必须二选一"},
		{"neither volume nor switch", map[string]any{"name": "Headphone"}, "volume 与 switch 必须二选一"},
		{"bad switch", map[string]any{"name": "Headphone", "switch": "blink"}, "switch 只能为 on / off / toggle"},
		{"bad stream", map[string]any{"name": "Headphone", "stream": "sideways", "volume": []int{1}}, "stream 只能为 playback 或 capture"},
		{"unknown control", map[string]any{"name": "Ghost", "volume": []int{1}}, "未找到控件 Ghost"},
		{"duplicate index", map[string]any{"name": "Master", "volume": []int{1}}, "无法按名操作"},
		{"no playback volume", map[string]any{"name": "Mono", "volume": []int{1}}, "不支持 playback 音量"},
		{"no capture volume", map[string]any{"name": "Headphone", "stream": "capture", "volume": []int{1}}, "不支持 capture 音量"},
		{"wrong value count", map[string]any{"name": "Headphone", "volume": []int{1, 2, 3}}, "需要 1 个或 2 个音量值"},
		{"unknown field", map[string]any{"name": "Headphone", "volume": []int{1}, "bogus": 1}, "JSON 解析失败"},
		{"malformed payload", map[string]any{"name": 123}, "JSON 解析失败"},
	}
	for _, testCase := range cases {
		recorder := do(t, handler, "POST", "/api/mixer", testCase.body, nil)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, body = %s", testCase.name, recorder.Code, recorder.Body.String())
		}
		payload := decodeJSON(t, recorder)
		if payload["code"] != "invalid_request" {
			t.Fatalf("%s payload = %v", testCase.name, payload)
		}
		message, _ := payload["error"].(string)
		if !strings.Contains(message, testCase.wantError) {
			t.Fatalf("%s error = %q, want %q", testCase.name, message, testCase.wantError)
		}
	}
}

func TestMixerEndpointsRequireTheToken(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.Token = "sekret" })
	script := &mixerScript{raw: 48}
	handler := h.routes(func(options *Options) { options.Mixer = newMixerTestController(t, script) })

	if recorder := do(t, handler, "GET", "/api/mixer", nil, nil); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless GET status = %d", recorder.Code)
	}
	if recorder := do(t, handler, "POST", "/api/mixer", map[string]any{"name": "Headphone", "volume": []int{1}}, nil); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless POST status = %d", recorder.Code)
	}

	headers := map[string]string{"x-bridge-token": "sekret"}
	if recorder := do(t, handler, "GET", "/api/mixer", nil, headers); recorder.Code != http.StatusOK {
		t.Fatalf("authorized GET status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder := do(t, handler, "POST", "/api/mixer", map[string]any{"name": "Headphone", "volume": []int{1}}, headers); recorder.Code != http.StatusOK {
		t.Fatalf("authorized POST status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}
