package mix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
)

// ---------------------------------------------------------------------------
// scripted amixer
// ---------------------------------------------------------------------------

// fakeAmixer records every invocation and answers from a script keyed by the
// arguments after the "-c <card>" prefix.
type fakeAmixer struct {
	mu       sync.Mutex
	commands []string
	replies  map[string]string
	failures map[string]error
}

func newFakeAmixer() *fakeAmixer {
	return &fakeAmixer{replies: map[string]string{}, failures: map[string]error{}}
}

// reply scripts the stdout for one argument tail (e.g. "scontents").
func (f *fakeAmixer) reply(key, output string) *fakeAmixer {
	f.replies[key] = output
	return f
}

// fail scripts an error for one argument tail.
func (f *fakeAmixer) fail(key string, err error) *fakeAmixer {
	f.failures[key] = err
	return f
}

func (f *fakeAmixer) run(_ context.Context, binary string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.commands = append(f.commands, strings.Join(append([]string{binary}, args...), " "))
	f.mu.Unlock()
	key := strings.Join(args[2:], " ")
	if err := f.failures[key]; err != nil {
		return nil, err
	}
	return []byte(f.replies[key]), nil
}

func (f *fakeAmixer) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.commands...)
}

func testController(t *testing.T, card int, run Runner) *Controller {
	t.Helper()
	cfg := config.Default()
	cfg.Card = card
	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).WithRunner(run)
}

// sContentsFixture is a representative card: a volume+switch playback
// control, a volume+switch capture control, a switch-only control and an
// exclusive capture source group.
const sContentsFixture = `Simple mixer control 'Master',0
  Capabilities: pvolume pswitch pswitch-joined
  Playback channels: Front Left - Front Right
  Limits: Playback 0 - 64
  Front Left: Playback 48 [75%] [-16.00dB] [on]
  Front Right: Playback 48 [75%] [-16.00dB] [on]
Simple mixer control 'Capture',0
  Capabilities: cvolume cswitch cswitch-joined
  Capture channels: Front Left - Front Right
  Limits: Capture 0 - 30
  Front Left: Capture 24 [80%] [12.00dB] [on]
  Front Right: Capture 24 [80%] [12.00dB] [on]
Simple mixer control 'Mono',0
  Capabilities: pswitch
  Playback channels: Mono
  Mono: Playback [off]
Simple mixer control 'Input Source',0
  Capabilities: cswitch cswitch-exclusive cswitch-joined
  Capture exclusive group: 0
  Capture channels: Front Left - Front Right
  Front Left: Capture [on]
  Front Right: Capture [off]
`

const cardInfoFixture = `Card hw:0 'PCH'/'HDA Intel PCH at 0xf7f10000 irq 32'
  Mixer name : 'Realtek ALC269VC'
  Components : 'HDA:10ec0269,10431182,00100200'
  Controls      : 25
  Simple ctrls  : 12
`

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

func TestStateParsesSContentsAndInfo(t *testing.T) {
	fake := newFakeAmixer().reply("scontents", sContentsFixture).reply("info", cardInfoFixture)
	controller := testController(t, 0, fake.run)

	state, err := controller.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Card.ID != "PCH" || state.Card.LongName != "HDA Intel PCH at 0xf7f10000 irq 32" {
		t.Fatalf("card identity = %q / %q", state.Card.ID, state.Card.LongName)
	}
	if state.Card.MixerName != "Realtek ALC269VC" || state.Card.Components != "HDA:10ec0269,10431182,00100200" {
		t.Fatalf("mixer metadata = %q / %q", state.Card.MixerName, state.Card.Components)
	}
	if state.Card.Controls != 25 || state.Card.SimpleControls != 12 {
		t.Fatalf("control counts = %d / %d", state.Card.Controls, state.Card.SimpleControls)
	}

	if len(state.Controls) != 4 {
		t.Fatalf("controls = %d, want 4", len(state.Controls))
	}
	master := FindControl(state, "Master")
	if master == nil {
		t.Fatal("Master must be parsed")
	}
	if master.Index != 0 || master.ExclusiveGroup != -1 {
		t.Fatalf("Master index/group = %d/%d", master.Index, master.ExclusiveGroup)
	}
	if got := strings.Join(master.Capabilities, " "); got != "pvolume pswitch pswitch-joined" {
		t.Fatalf("Master capabilities = %q", got)
	}
	if got := strings.Join(master.PlaybackChannels, " - "); got != "Front Left - Front Right" {
		t.Fatalf("Master channels = %q", got)
	}
	if master.PlaybackLimit == nil || master.PlaybackLimit.Min != 0 || master.PlaybackLimit.Max != 64 {
		t.Fatalf("Master limit = %+v", master.PlaybackLimit)
	}
	if master.LimitFor("capture") != nil {
		t.Fatal("Master must have no capture limit")
	}
	if got := master.VolumeChannelCount("playback"); got != 2 {
		t.Fatalf("Master playback volume channels = %d", got)
	}
	left := master.Channels[0].Playback
	if left == nil || !left.HasVolume || left.Raw != 48 || left.Percent != 75 {
		t.Fatalf("Master left = %+v", left)
	}
	if !left.HasDB || left.DB != -16 || !left.HasSwitch || !left.On {
		t.Fatalf("Master left switch/db = %+v", left)
	}

	capture := FindControl(state, "Capture")
	if capture == nil || capture.CaptureLimit == nil || capture.CaptureLimit.Max != 30 {
		t.Fatalf("Capture limit = %+v", capture)
	}
	if capture.LimitFor("capture") == nil || capture.LimitFor("playback") != nil {
		t.Fatal("Capture must expose exactly the capture limit")
	}
	if got := capture.VolumeChannelCount("capture"); got != 2 {
		t.Fatalf("Capture volume channels = %d", got)
	}
	if value := capture.Channels[0].Capture; value == nil || !value.HasVolume || value.Raw != 24 || value.Percent != 80 {
		t.Fatalf("Capture left = %+v", value)
	}

	mono := FindControl(state, "Mono")
	if mono == nil {
		t.Fatal("Mono must be parsed")
	}
	playback := mono.Channels[0].Playback
	if playback == nil || playback.HasVolume || !playback.HasSwitch || playback.On {
		t.Fatalf("Mono playback = %+v, want a switch-only off value", playback)
	}
	if got := mono.VolumeChannelCount("playback"); got != 0 {
		t.Fatalf("Mono volume channels = %d", got)
	}

	source := FindControl(state, "Input Source")
	if source == nil || source.ExclusiveGroup != 0 {
		t.Fatalf("Input Source group = %+v", source)
	}
	if !source.Channels[0].Capture.On || source.Channels[1].Capture.On {
		t.Fatalf("Input Source channels = %+v", source.Channels)
	}

	if got := fake.recorded(); len(got) != 2 || got[0] != "amixer -c 0 scontents" || got[1] != "amixer -c 0 info" {
		t.Fatalf("commands = %v", got)
	}
}

func TestStateDegradesWhenInfoFails(t *testing.T) {
	fake := newFakeAmixer().reply("scontents", sContentsFixture).fail("info", errors.New("no info"))
	controller := testController(t, 0, fake.run)

	state, err := controller.State(context.Background())
	if err != nil {
		t.Fatalf("a failing amixer info must not fail the state: %v", err)
	}
	if state.Card.Card != 0 || state.Card.ID != "" || len(state.Controls) != 4 {
		t.Fatalf("degraded state = %+v", state)
	}
}

func TestStateConsultsTheConfiguredCard(t *testing.T) {
	fake := newFakeAmixer().reply("scontents", "")
	controller := testController(t, 2, fake.run)

	if controller.Card() != 2 {
		t.Fatalf("Card() = %d", controller.Card())
	}
	if _, err := controller.State(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fake.recorded(); len(got) != 2 || got[0] != "amixer -c 2 scontents" || got[1] != "amixer -c 2 info" {
		t.Fatalf("commands = %v", got)
	}
}

func TestStatePropagatesAmixerFailures(t *testing.T) {
	fake := newFakeAmixer().fail("scontents", errors.New("amixer: card not found"))
	controller := testController(t, 0, fake.run)

	if _, err := controller.State(context.Background()); err == nil || !strings.Contains(err.Error(), "card not found") {
		t.Fatalf("State = %v, want the amixer failure", err)
	}
}

func TestIsUnavailableDetectsAMissingBinary(t *testing.T) {
	fake := newFakeAmixer().fail("scontents", fmt.Errorf("amixer missing: %w", exec.ErrNotFound))
	controller := testController(t, 0, fake.run)

	if _, err := controller.State(context.Background()); err == nil || !IsUnavailable(err) {
		t.Fatalf("IsUnavailable(%v) = false, want true", err)
	}
	if IsUnavailable(errors.New("control level error")) {
		t.Fatal("a control-level error must not read as unavailable")
	}
}

// ---------------------------------------------------------------------------
// SetVolume / SetSwitch
// ---------------------------------------------------------------------------

func TestSetVolumeBuildsTheRawWriteCommand(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		values []int
		want   string
	}{
		{"Headphone", "playback", []int{42}, "amixer -c 0 -R sset Headphone playback 42"},
		{"Headphone", "playback", []int{12, 24}, "amixer -c 0 -R sset Headphone playback 12,24"},
		{"Capture", "capture", []int{24}, "amixer -c 0 -R sset Capture capture 24"},
		{"Headphone", "bogus", []int{7}, "amixer -c 0 -R sset Headphone playback 7"},
	}
	for _, testCase := range cases {
		fake := newFakeAmixer()
		controller := testController(t, 0, fake.run)
		if err := controller.SetVolume(context.Background(), testCase.name, testCase.stream, testCase.values); err != nil {
			t.Fatalf("SetVolume(%s, %s, %v): %v", testCase.name, testCase.stream, testCase.values, err)
		}
		if got := fake.recorded(); len(got) != 1 || got[0] != testCase.want {
			t.Fatalf("commands = %v, want %q", got, testCase.want)
		}
	}
}

func TestSetVolumeRejectsBadInput(t *testing.T) {
	fake := newFakeAmixer()
	controller := testController(t, 0, fake.run)
	ctx := context.Background()

	cases := []struct {
		name   string
		values []int
		want   string
	}{
		{"", []int{1}, "控件名不能为空"},
		{"-h", []int{1}, "非法控件名"},
		{"Head\x01phone", []int{1}, "非法控件名"},
		{"Headphone", nil, "未指定音量值"},
	}
	for _, testCase := range cases {
		if err := controller.SetVolume(ctx, testCase.name, "playback", testCase.values); err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("SetVolume(%q, %v) = %v, want %q", testCase.name, testCase.values, err, testCase.want)
		}
	}
	if got := fake.recorded(); len(got) != 0 {
		t.Fatalf("rejected writes must not reach amixer: %v", got)
	}
}

func TestSetVolumePropagatesWriteFailures(t *testing.T) {
	fake := newFakeAmixer().fail("-R sset Headphone playback 42", errors.New("amixer: Invalid command"))
	controller := testController(t, 0, fake.run)

	err := controller.SetVolume(context.Background(), "Headphone", "playback", []int{42})
	if err == nil || !strings.Contains(err.Error(), "Invalid command") {
		t.Fatalf("SetVolume = %v, want the amixer failure", err)
	}
}

func TestSetSwitchMapsActionsOntoAmixerWords(t *testing.T) {
	cases := []struct {
		stream string
		action string
		want   string
	}{
		{"playback", "on", "amixer -c 0 sset Headphone playback unmute"},
		{"playback", "off", "amixer -c 0 sset Headphone playback mute"},
		{"playback", "toggle", "amixer -c 0 sset Headphone playback toggle"},
		{"playback", "", "amixer -c 0 sset Headphone playback toggle"},
		{"capture", "on", "amixer -c 0 sset Headphone capture cap"},
		{"capture", "off", "amixer -c 0 sset Headphone capture nocap"},
		{"capture", "toggle", "amixer -c 0 sset Headphone capture toggle"},
		{"bogus", "on", "amixer -c 0 sset Headphone playback unmute"},
	}
	for _, testCase := range cases {
		fake := newFakeAmixer()
		controller := testController(t, 0, fake.run)
		if err := controller.SetSwitch(context.Background(), "Headphone", testCase.stream, testCase.action); err != nil {
			t.Fatalf("SetSwitch(%s, %q): %v", testCase.stream, testCase.action, err)
		}
		if got := fake.recorded(); len(got) != 1 || got[0] != testCase.want {
			t.Fatalf("commands = %v, want %q", got, testCase.want)
		}
	}
}

func TestSetSwitchRejectsIllegalNames(t *testing.T) {
	controller := testController(t, 0, newFakeAmixer().run)
	if err := controller.SetSwitch(context.Background(), "-x", "playback", "on"); err == nil || !strings.Contains(err.Error(), "非法控件名") {
		t.Fatalf("SetSwitch(-x) = %v", err)
	}
}

// ---------------------------------------------------------------------------
// ControlState
// ---------------------------------------------------------------------------

func TestControlStateReadsATargetedControl(t *testing.T) {
	fake := newFakeAmixer().reply("sget Headphone", `Simple mixer control 'Headphone',0
  Capabilities: pvolume pswitch pswitch-joined
  Playback channels: Front Left - Front Right
  Limits: Playback 0 - 64
  Front Left: Playback 20 [31%] [-44.00dB] [on]
  Front Right: Playback 20 [31%] [-44.00dB] [on]
`)
	controller := testController(t, 0, fake.run)

	control, err := controller.ControlState(context.Background(), "Headphone")
	if err != nil {
		t.Fatal(err)
	}
	if control.Name != "Headphone" || control.Index != 0 {
		t.Fatalf("control = %+v", control)
	}
	if control.Channels[0].Playback == nil || control.Channels[0].Playback.Raw != 20 {
		t.Fatalf("channels = %+v", control.Channels)
	}
	if got := fake.recorded(); len(got) != 1 || got[0] != "amixer -c 0 sget Headphone" {
		t.Fatalf("commands = %v", got)
	}
}

func TestControlStateRejectsDuplicateIndices(t *testing.T) {
	fake := newFakeAmixer().reply("sget Master", `Simple mixer control 'Master',1
  Capabilities: pvolume
  Playback channels: Front Left - Front Right
  Limits: Playback 0 - 64
  Front Left: Playback 48 [75%] [-16.00dB]
  Front Right: Playback 48 [75%] [-16.00dB]
`)
	controller := testController(t, 0, fake.run)

	_, err := controller.ControlState(context.Background(), "Master")
	if err == nil || !strings.Contains(err.Error(), "index 为 1") || !strings.Contains(err.Error(), "无法按名操作") {
		t.Fatalf("ControlState(Master index 1) = %v", err)
	}
}

func TestControlStateReportsUnknownControls(t *testing.T) {
	fake := newFakeAmixer().reply("sget Ghost", "")
	controller := testController(t, 0, fake.run)

	_, err := controller.ControlState(context.Background(), "Ghost")
	if err == nil || !strings.Contains(err.Error(), "未找到控件 Ghost") {
		t.Fatalf("ControlState(Ghost) = %v", err)
	}
}

// ---------------------------------------------------------------------------
// exec
// ---------------------------------------------------------------------------

func TestExecTimesOutWedgedCommands(t *testing.T) {
	controller := testController(t, 0, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	controller.timeout = 50 * time.Millisecond

	_, err := controller.State(context.Background())
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("State with a wedged amixer = %v, want a timeout error", err)
	}
}

func TestWithRunnerIgnoresNil(t *testing.T) {
	cfg := config.Default()
	controller := New(cfg, nil)
	if controller.WithRunner(nil) == nil || controller.Card() != cfg.Card {
		t.Fatal("WithRunner(nil) must keep the controller usable")
	}
}
