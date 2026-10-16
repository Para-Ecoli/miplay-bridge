package mix

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// headphoneBlock renders a targeted sget reply for Headphone on a 0-64 raw
// scale with a playback switch, mirroring a real stereo headphone control.
func headphoneBlock(raw int) string {
	return fmt.Sprintf(`Simple mixer control 'Headphone',0
  Capabilities: pvolume pswitch pswitch-joined
  Playback channels: Front Left - Front Right
  Limits: Playback 0 - 64
  Front Left: Playback %d [%d%%] [-%.2fdB] [on]
  Front Right: Playback %d [%d%%] [-%.2fdB] [on]
`, raw, percentFromRaw(raw, Limit{Min: 0, Max: 64}), 64-float64(raw), raw, percentFromRaw(raw, Limit{Min: 0, Max: 64}), 64-float64(raw))
}

// volumeOnlyBlock renders a control without a switch.
func volumeOnlyBlock(raw int) string {
	return fmt.Sprintf(`Simple mixer control 'Headphone',0
  Capabilities: pvolume
  Playback channels: Front Left - Front Right
  Limits: Playback 0 - 64
  Front Left: Playback %d [%d%%] [-%.2fdB]
  Front Right: Playback %d [%d%%] [-%.2fdB]
`, raw, percentFromRaw(raw, Limit{Min: 0, Max: 64}), 64-float64(raw), raw, percentFromRaw(raw, Limit{Min: 0, Max: 64}), 64-float64(raw))
}

// switchOnlyBlock renders a control with a switch but no volume.
func switchOnlyBlock() string {
	return `Simple mixer control 'Mono',0
  Capabilities: pswitch
  Playback channels: Mono
  Mono: Playback [off]
`
}

func usableControl(name string, index int) Control {
	return Control{
		Name:           name,
		Index:          index,
		ExclusiveGroup: -1,
		PlaybackLimit:  &Limit{Min: 0, Max: 64},
		Channels: []Channel{
			{Name: "Front Left", Playback: &ValueInfo{Raw: 32, HasVolume: true}},
			{Name: "Front Right", Playback: &ValueInfo{Raw: 32, HasVolume: true}},
		},
	}
}

// ---------------------------------------------------------------------------
// ResolveSystemControl
// ---------------------------------------------------------------------------

func TestResolveSystemControl(t *testing.T) {
	state := &State{Controls: []Control{
		{Name: "Master", Index: 0, ExclusiveGroup: -1,
			Channels: []Channel{{Name: "Mono", Playback: &ValueInfo{HasSwitch: true}}}},
		usableControl("PCM", 0),
		usableControl("Headphone", 0),
	}}

	// The candidate order wins over the card order.
	if got := ResolveSystemControl(state, ""); got != "Headphone" {
		t.Fatalf("auto-detection = %q, want Headphone", got)
	}
	// An explicit preference is honoured case-insensitively.
	if got := ResolveSystemControl(state, "pcm"); got != "PCM" {
		t.Fatalf("preferred pcm = %q, want PCM", got)
	}
	// An explicit preference that cannot carry volume degrades instead of
	// silently falling back to another control.
	if got := ResolveSystemControl(state, "Master"); got != "" {
		t.Fatalf("preferred Master (switch-only) = %q, want an empty binding", got)
	}
	if got := ResolveSystemControl(state, "Ghost"); got != "" {
		t.Fatalf("preferred Ghost = %q, want an empty binding", got)
	}
	// Without a candidate hit the first usable control of the card is bound.
	fallback := &State{Controls: []Control{usableControl("Aux", 0), usableControl("CD", 0)}}
	if got := ResolveSystemControl(fallback, ""); got != "Aux" {
		t.Fatalf("fallback = %q, want Aux", got)
	}
	// Duplicate names (index != 0) are never bound.
	duplicate := &State{Controls: []Control{usableControl("Headphone", 1)}}
	if got := ResolveSystemControl(duplicate, ""); got != "" {
		t.Fatalf("duplicate index = %q, want an empty binding", got)
	}
	if got := ResolveSystemControl(nil, ""); got != "" {
		t.Fatalf("nil state = %q, want an empty binding", got)
	}
}

// ---------------------------------------------------------------------------
// percentage <-> raw scale
// ---------------------------------------------------------------------------

func TestPercentRawConversion(t *testing.T) {
	stereo := Limit{Min: 0, Max: 64}
	if got := rawFromPercent(50, stereo); got != 32 {
		t.Fatalf("rawFromPercent(50) = %d, want 32", got)
	}
	if got := rawFromPercent(100, stereo); got != 64 {
		t.Fatalf("rawFromPercent(100) = %d, want 64", got)
	}
	if got := percentFromRaw(32, stereo); got != 50 {
		t.Fatalf("percentFromRaw(32) = %d, want 50", got)
	}
	if got := percentFromRaw(64, stereo); got != 100 {
		t.Fatalf("percentFromRaw(64) = %d, want 100", got)
	}

	// Signed dB-style ranges stay symmetric.
	signed := Limit{Min: -32, Max: 0}
	if got := rawFromPercent(50, signed); got != -16 {
		t.Fatalf("rawFromPercent(50) on a signed range = %d, want -16", got)
	}
	if got := percentFromRaw(-16, signed); got != 50 {
		t.Fatalf("percentFromRaw(-16) on a signed range = %d, want 50", got)
	}

	// A degenerate range maps onto its single value.
	if got := rawFromPercent(50, Limit{Min: 7, Max: 7}); got != 7 {
		t.Fatalf("rawFromPercent on a zero span = %d, want 7", got)
	}
	if got := percentFromRaw(3, Limit{Min: 7, Max: 7}); got != 100 {
		t.Fatalf("percentFromRaw on a zero span = %d, want 100", got)
	}

	for input, want := range map[int]int{-5: 0, 0: 0, 37: 37, 100: 100, 150: 100} {
		if got := clampPercent(input); got != want {
			t.Fatalf("clampPercent(%d) = %d, want %d", input, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// SystemVolume / SetSystemPercent / SetSystemMute
// ---------------------------------------------------------------------------

func TestSystemVolumeRequiresABinding(t *testing.T) {
	controller := testController(t, 0, newFakeAmixer().run)
	ctx := context.Background()

	if _, err := controller.SystemVolume(ctx); !errors.Is(err, ErrNoVolumeControl) {
		t.Fatalf("SystemVolume unbound = %v, want ErrNoVolumeControl", err)
	}
	if _, err := controller.SetSystemPercent(ctx, 50); !errors.Is(err, ErrNoVolumeControl) {
		t.Fatalf("SetSystemPercent unbound = %v, want ErrNoVolumeControl", err)
	}
	if err := controller.SetSystemMute(ctx, true); !errors.Is(err, ErrNoVolumeControl) {
		t.Fatalf("SetSystemMute unbound = %v, want ErrNoVolumeControl", err)
	}
	if controller.SystemControl() != "" {
		t.Fatalf("SystemControl() = %q", controller.SystemControl())
	}
}

func TestSystemVolumeReadsTheBoundControlLive(t *testing.T) {
	fake := newFakeAmixer().reply("sget Headphone", headphoneBlock(32))
	controller := testController(t, 0, fake.run)
	controller.SetSystemControl("Headphone")
	if controller.SystemControl() != "Headphone" {
		t.Fatalf("SystemControl() = %q", controller.SystemControl())
	}

	state, err := controller.SystemVolume(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Control != "Headphone" || state.Raw != 32 || state.Percent != 50 {
		t.Fatalf("state = %+v", state)
	}
	if state.Muted || !state.HasVolume || !state.HasSwitch {
		t.Fatalf("flags = %+v", state)
	}
	// Every read is a targeted sget: nothing is cached.
	if got := fake.recorded(); len(got) != 1 || got[0] != "amixer -c 0 sget Headphone" {
		t.Fatalf("commands = %v", got)
	}
}

func TestSystemVolumeReportsAVanishedControl(t *testing.T) {
	fake := newFakeAmixer().reply("sget Headphone", "")
	controller := testController(t, 0, fake.run)
	controller.SetSystemControl("Headphone")

	if _, err := controller.SystemVolume(context.Background()); err == nil || !strings.Contains(err.Error(), "未找到控件") {
		t.Fatalf("SystemVolume = %v, want the vanished-control error", err)
	}
}

func TestSetSystemPercentWritesAllChannelsAndUnmutes(t *testing.T) {
	raw := 48
	var commands []string
	run := func(_ context.Context, binary string, args ...string) ([]byte, error) {
		key := strings.Join(args[2:], " ")
		commands = append(commands, strings.Join(append([]string{binary}, args...), " "))
		switch {
		case key == "sget Headphone":
			return []byte(headphoneBlock(raw)), nil
		case strings.HasPrefix(key, "-R sset Headphone playback "):
			values := strings.Split(strings.TrimPrefix(key, "-R sset Headphone playback "), ",")
			raw, _ = strconv.Atoi(values[0])
			return nil, nil
		default:
			return nil, nil
		}
	}
	controller := testController(t, 0, run)
	controller.SetSystemControl("Headphone")

	state, err := controller.SetSystemPercent(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if state.Raw != 32 || state.Percent != 50 {
		t.Fatalf("read-back state = %+v", state)
	}
	want := []string{
		"amixer -c 0 sget Headphone",
		"amixer -c 0 -R sset Headphone playback 32,32",
		"amixer -c 0 sset Headphone playback unmute",
		"amixer -c 0 sget Headphone",
	}
	if len(commands) != len(want) {
		t.Fatalf("commands = %v, want %v", commands, want)
	}
	for index := range want {
		if commands[index] != want[index] {
			t.Fatalf("commands[%d] = %q, want %q", index, commands[index], want[index])
		}
	}
}

func TestSetSystemPercentClampsOutOfRangeValues(t *testing.T) {
	raw := 48
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		key := strings.Join(args[2:], " ")
		if key == "sget Headphone" {
			return []byte(headphoneBlock(raw)), nil
		}
		if strings.HasPrefix(key, "-R sset Headphone playback ") {
			values := strings.Split(strings.TrimPrefix(key, "-R sset Headphone playback "), ",")
			raw, _ = strconv.Atoi(values[0])
		}
		return nil, nil
	}
	controller := testController(t, 0, run)
	controller.SetSystemControl("Headphone")

	state, err := controller.SetSystemPercent(context.Background(), 150)
	if err != nil {
		t.Fatal(err)
	}
	if state.Raw != 64 || state.Percent != 100 {
		t.Fatalf("clamped state = %+v", state)
	}
}

func TestSetSystemPercentDegradesToVolumeOnlyWhenUnmuteFails(t *testing.T) {
	fake := newFakeAmixer().
		reply("sget Headphone", headphoneBlock(32)).
		fail("sset Headphone playback unmute", errors.New("amixer: unmute rejected"))
	controller := testController(t, 0, fake.run)
	controller.SetSystemControl("Headphone")

	state, err := controller.SetSystemPercent(context.Background(), 50)
	if err != nil {
		t.Fatalf("a failing unmute must degrade to volume-only: %v", err)
	}
	if state.Raw != 32 {
		t.Fatalf("state = %+v", state)
	}
}

func TestSetSystemPercentSkipsTheUnmuteWithoutASwitch(t *testing.T) {
	fake := newFakeAmixer().reply("sget Headphone", volumeOnlyBlock(32))
	controller := testController(t, 0, fake.run)
	controller.SetSystemControl("Headphone")

	if _, err := controller.SetSystemPercent(context.Background(), 50); err != nil {
		t.Fatal(err)
	}
	for _, command := range fake.recorded() {
		if strings.Contains(command, "unmute") {
			t.Fatalf("a switch-less control must not be unmuted: %v", fake.recorded())
		}
	}
}

func TestSetSystemPercentDegradesOnUnusableControls(t *testing.T) {
	fake := newFakeAmixer().reply("sget Mono", switchOnlyBlock())
	controller := testController(t, 0, fake.run)
	controller.SetSystemControl("Mono")

	if _, err := controller.SetSystemPercent(context.Background(), 50); !errors.Is(err, ErrNoVolumeControl) {
		t.Fatalf("SetSystemPercent on a switch-only control = %v", err)
	}
}

func TestSetSystemMuteDrivesThePlaybackSwitch(t *testing.T) {
	fake := newFakeAmixer().reply("sget Headphone", headphoneBlock(32))
	controller := testController(t, 0, fake.run)
	controller.SetSystemControl("Headphone")
	ctx := context.Background()

	if err := controller.SetSystemMute(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := controller.SetSystemMute(ctx, false); err != nil {
		t.Fatal(err)
	}
	got := fake.recorded()
	want := []string{
		"amixer -c 0 sget Headphone",
		"amixer -c 0 sset Headphone playback mute",
		"amixer -c 0 sget Headphone",
		"amixer -c 0 sset Headphone playback unmute",
	}
	if len(got) != len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("commands[%d] = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestSetSystemMuteIsANoOpWithoutASwitch(t *testing.T) {
	fake := newFakeAmixer().reply("sget Headphone", volumeOnlyBlock(32))
	controller := testController(t, 0, fake.run)
	controller.SetSystemControl("Headphone")

	if err := controller.SetSystemMute(context.Background(), true); err != nil {
		t.Fatalf("SetSystemMute on a switch-less control = %v, want nil", err)
	}
	for _, command := range fake.recorded() {
		if strings.HasPrefix(command, "amixer -c 0 sset") {
			t.Fatalf("a switch-less control must not receive a write: %v", fake.recorded())
		}
	}
}
