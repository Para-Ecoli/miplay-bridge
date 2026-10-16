// Package alsatest provides a scripted amixer double for tests outside the
// alsa package, whose own fake is unexported. It emulates the alsa-utils
// subset the bridge drives: scontrols, sget and sset.
package alsatest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Fake is a scripted amixer replacement. Build it with New; every method is
// safe for concurrent use.
type Fake struct {
	mu       sync.Mutex
	autoMute string
	controls []string
	switches map[string]string
	percent  map[string]int
}

// New builds a fake card: Master/Headphone/Speaker plus Auto-Mute Mode, all
// switches off (unmuted on demand), Master at 70% and the outputs at 65%, with
// Auto-Mute still enabled - exactly the state a real card starts in.
func New() *Fake {
	return &Fake{
		autoMute: "Enabled",
		controls: []string{"Master", "Headphone", "Speaker", "Auto-Mute Mode"},
		switches: map[string]string{"Master": "off", "Headphone": "off", "Speaker": "off"},
		percent:  map[string]int{"Master": 70, "Headphone": 65, "Speaker": 65},
	}
}

// Run is the Runner the mixer should be built with.
func (f *Fake) Run(_ context.Context, name string, args ...string) (string, string, error) {
	if name != "amixer" {
		return "", "", fmt.Errorf("unexpected binary %q", name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Drop the "-c <card>" prefix.
	if len(args) >= 2 && args[0] == "-c" {
		args = args[2:]
	}
	if len(args) == 0 {
		return "", "", errors.New("no arguments")
	}
	switch args[0] {
	case "scontrols":
		var builder strings.Builder
		for _, control := range f.controls {
			fmt.Fprintf(&builder, "Simple mixer control '%s',0\n", control)
		}
		return builder.String(), "", nil
	case "sget":
		control := args[1]
		if control == "Auto-Mute Mode" {
			return fmt.Sprintf("Simple mixer control 'Auto-Mute Mode',0\n  Items: 'Disabled' 'Enabled'\n  Item0: '%s'\n", f.autoMute), "", nil
		}
		return fmt.Sprintf("Simple mixer control '%s',0\n  Front Left: Playback %d [%d%%] [0.00dB] [%s]\n", control, f.percent[control], f.percent[control], f.switches[control]), "", nil
	case "sset":
		control := args[1]
		if control == "Auto-Mute Mode" {
			if len(args) > 2 {
				f.autoMute = args[2]
			}
			return "", "", nil
		}
		for _, value := range args[2:] {
			if value == "unmute" {
				f.switches[control] = "on"
			}
			if value == "mute" {
				f.switches[control] = "off"
			}
			if strings.HasSuffix(value, "%") {
				var percent int
				_, _ = fmt.Sscanf(strings.TrimSuffix(value, "%"), "%d", &percent)
				f.percent[control] = percent
			}
		}
		return "", "", nil
	default:
		return "", "", fmt.Errorf("unsupported amixer command %q", args[0])
	}
}

// SetPercent simulates an out-of-band hardware change to one control.
func (f *Fake) SetPercent(control string, percent int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.percent[control] = percent
}

// PercentOf reads the percent one control currently holds.
func (f *Fake) PercentOf(control string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.percent[control]
}

// Switch reads the on/off state one control currently holds.
func (f *Fake) Switch(control string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.switches[control]
}

// Item reads the Auto-Mute Mode value.
func (f *Fake) Item() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.autoMute
}
