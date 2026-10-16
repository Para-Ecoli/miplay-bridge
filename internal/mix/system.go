// System volume surface: the single designated ALSA control the MiPlay and
// DLNA protocols drive with 0-100 percentages.
//
// Both protocol surfaces only exchange percentages, so they need one bound
// hardware control and one shared scale. The scale is the raw ALSA value
// (amixer's default linear percentage), written through the same
// `amixer -R sset` path the console uses, so every writer and reader agrees
// without the -M dB mapping.
package mix

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
)

// ErrNoVolumeControl is returned when no system volume control is bound or
// the bound control vanished from the card; protocol layers degrade to their
// fixed full-scale answers and ignore writes.
var ErrNoVolumeControl = errors.New("未绑定系统音量控件")

// SystemState is the read-back snapshot of the designated control.
type SystemState struct {
	Control   string `json:"control"`
	Raw       int    `json:"raw"`
	Percent   int    `json:"percent"`
	Muted     bool   `json:"muted"`
	HasVolume bool   `json:"hasVolume"`
	HasSwitch bool   `json:"hasSwitch"`
}

// volumeControlCandidates is the auto-detection order, verified against the
// target hardware. The first control of the card that carries a playback
// volume is the final fallback.
var volumeControlCandidates = []string{
	"Headphone", "Master", "Speaker", "PCM", "Playback",
	"Line Out", "Lineout", "DAC", "Front", "Digital",
}

// ResolveSystemControl picks the designated control: the explicitly
// preferred name when it exists and is usable, otherwise the first candidate
// with a playback volume, otherwise the first such control of the card.
// An empty result means the protocol volume degrades.
func ResolveSystemControl(state *State, preferred string) string {
	if state == nil {
		return ""
	}
	if preferred = strings.TrimSpace(preferred); preferred != "" {
		if control := findControlFold(state, preferred); usableVolumeControl(control) {
			return control.Name
		}
		return ""
	}
	for _, name := range volumeControlCandidates {
		if control := findControlFold(state, name); usableVolumeControl(control) {
			return control.Name
		}
	}
	for i := range state.Controls {
		if usableVolumeControl(&state.Controls[i]) {
			return state.Controls[i].Name
		}
	}
	return ""
}

// usableVolumeControl keeps a control eligible for the designation: index 0
// only (operating a duplicate name by name alone is ambiguous) and a real
// playback volume.
func usableVolumeControl(control *Control) bool {
	return control != nil && control.Index == 0 &&
		control.PlaybackLimit != nil && control.VolumeChannelCount("playback") > 0
}

func findControlFold(state *State, name string) *Control {
	if control := FindControl(state, name); control != nil {
		return control
	}
	for i := range state.Controls {
		if strings.EqualFold(state.Controls[i].Name, name) {
			return &state.Controls[i]
		}
	}
	return nil
}

// SetSystemControl binds (or clears, with an empty name) the designated
// control.
func (c *Controller) SetSystemControl(name string) {
	c.mu.Lock()
	c.systemControl = strings.TrimSpace(name)
	c.mu.Unlock()
}

// SystemControl reports the bound control name; empty means unbound.
func (c *Controller) SystemControl() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.systemControl
}

// SystemVolume reads the designated control with a targeted sget. Every read
// is live: nothing is cached, so the console, the phone slider and the DLNA
// controller can never observe a stale level.
func (c *Controller) SystemVolume(ctx context.Context) (SystemState, error) {
	name := c.SystemControl()
	if name == "" {
		return SystemState{}, ErrNoVolumeControl
	}
	control, err := c.ControlState(ctx, name)
	if err != nil {
		return SystemState{}, err
	}
	state, ok := systemStateFromControl(control)
	if !ok {
		return SystemState{}, ErrNoVolumeControl
	}
	return state, nil
}

// SetSystemPercent converts the percentage to a raw value on the shared
// scale, writes every playback channel with one -R sset and returns the
// read-back snapshot. A control that carries a switch is unmuted in the same
// operation; a failing unmute degrades to "volume only" instead of failing
// the write.
func (c *Controller) SetSystemPercent(ctx context.Context, percent int) (SystemState, error) {
	name := c.SystemControl()
	if name == "" {
		return SystemState{}, ErrNoVolumeControl
	}
	percent = clampPercent(percent)
	control, err := c.ControlState(ctx, name)
	if err != nil {
		return SystemState{}, err
	}
	if !usableVolumeControl(control) {
		return SystemState{}, ErrNoVolumeControl
	}
	channels := control.VolumeChannelCount("playback")
	raw := rawFromPercent(percent, *control.PlaybackLimit)
	values := make([]string, channels)
	for i := range values {
		values[i] = strconv.Itoa(raw)
	}
	if _, err := c.exec(ctx, "-R", "sset", name, "playback", strings.Join(values, ",")); err != nil {
		return SystemState{}, err
	}
	if controlHasSwitch(control) {
		if _, err := c.exec(ctx, "sset", name, "playback", "unmute"); err != nil {
			c.logger.Debug("mixer unmute alongside the volume write failed", "control", name, "error", err)
		}
	}
	return c.SystemVolume(ctx)
}

// SetSystemMute drives the playback switch of the designated control. A
// control without a switch accepts the call as a no-op so a controller is
// never left with a failing panel.
func (c *Controller) SetSystemMute(ctx context.Context, muted bool) error {
	name := c.SystemControl()
	if name == "" {
		return ErrNoVolumeControl
	}
	control, err := c.ControlState(ctx, name)
	if err != nil {
		return err
	}
	if !controlHasSwitch(control) {
		return nil
	}
	action := "unmute"
	if muted {
		action = "mute"
	}
	_, err = c.exec(ctx, "sset", name, "playback", action)
	return err
}

// systemStateFromControl reduces a control to the single exchange level: the
// first volume channel drives the percentage, the first switch channel the
// mute flag.
func systemStateFromControl(control *Control) (SystemState, bool) {
	state := SystemState{Control: control.Name}
	found := false
	for i := range control.Channels {
		value := control.Channels[i].Playback
		if value == nil {
			continue
		}
		if value.HasVolume && !found && control.PlaybackLimit != nil {
			state.Raw = value.Raw
			state.Percent = percentFromRaw(value.Raw, *control.PlaybackLimit)
			state.HasVolume = true
			found = true
		}
		if value.HasSwitch && !state.HasSwitch {
			state.HasSwitch = true
			state.Muted = !value.On
		}
	}
	return state, found
}

func controlHasSwitch(control *Control) bool {
	for i := range control.Channels {
		if value := control.Channels[i].Playback; value != nil && value.HasSwitch {
			return true
		}
	}
	return false
}

// percentFromRaw inverts rawFromPercent on the same linear raw scale amixer
// uses by default.
func percentFromRaw(raw int, limit Limit) int {
	span := limit.Max - limit.Min
	if span <= 0 {
		return 100
	}
	return clampPercent(int(math.Round(float64(raw-limit.Min) * 100 / float64(span))))
}

// rawFromPercent converts a 0-100 percentage to the raw hardware value:
// raw = min + round(pct x span / 100).
func rawFromPercent(percent int, limit Limit) int {
	span := limit.Max - limit.Min
	if span <= 0 {
		return limit.Min
	}
	return limit.Min + int(math.Round(float64(percent)*float64(span)/100))
}

func clampPercent(percent int) int {
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
}
