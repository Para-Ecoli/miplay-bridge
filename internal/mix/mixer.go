// Package mix drives the ALSA hardware mixer of the configured sound card
// through the amixer binary from alsa-utils.
//
// It is the in-process successor of the former standalone webmixer tool: the
// same scontents/info/sset model, the same parsing rules and the same
// raw-value write path (amixer -R sset), exposed as an injectable controller.
// The HTTP console, the MiPlay protocol and the DLNA renderer all drive one
// designated hardware control while MPD keeps mixer_type "null": the kernel
// never participates in volume, which structurally rules out the historical
// "two controls in series" attenuation bug.
package mix

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
)

// defaultCommandTimeout bounds every amixer invocation. The mixer mutex is
// held while a command runs, so a wedged amixer must not stall the HTTP
// console or the protocol volume paths forever.
const defaultCommandTimeout = 3 * time.Second

// ---------------------------------------------------------------- 数据模型
// 与 alsamixer（snd_mixer_selem 简单控件模型）一一对应。

// ValueInfo 描述某控件在单个声道、某个流方向（播放/录音）上的取值。
type ValueInfo struct {
	Raw       int     `json:"raw"`       // 原始硬件值
	HasVolume bool    `json:"hasVolume"` // 是否包含音量
	Percent   int     `json:"percent"`   // amixer 输出的百分比
	DB        float64 `json:"db"`        // amixer 输出的 dB 值
	HasDB     bool    `json:"hasDb"`
	On        bool    `json:"on"` // 开关状态
	HasSwitch bool    `json:"hasSwitch"`
}

// Channel 对应控件的一个声道（如 Front Left / Front Right / Mono）。
type Channel struct {
	Name     string     `json:"name"`
	Playback *ValueInfo `json:"playback,omitempty"`
	Capture  *ValueInfo `json:"capture,omitempty"`
}

// Limit 音量原始值范围。
type Limit struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// Control 一个 ALSA 简单控件（如 'Master'）。
type Control struct {
	Name             string    `json:"name"`
	Index            int       `json:"index"`
	Capabilities     []string  `json:"capabilities"`
	ExclusiveGroup   int       `json:"exclusiveGroup"` // -1 表示不在排他组中
	PlaybackChannels []string  `json:"playbackChannels,omitempty"`
	CaptureChannels  []string  `json:"captureChannels,omitempty"`
	PlaybackLimit    *Limit    `json:"playbackLimit,omitempty"`
	CaptureLimit     *Limit    `json:"captureLimit,omitempty"`
	Channels         []Channel `json:"channels"`
}

// CardInfo 声卡信息（来自 amixer info）。
type CardInfo struct {
	Card           int    `json:"card"`
	ID             string `json:"id"`        // 如 AudioPCI
	LongName       string `json:"longName"`  // 如 Ensoniq AudioPCI ENS1371 at 0x1000, irq 17
	MixerName      string `json:"mixerName"` // 如 Cirrus Logic CS4297A rev 3
	Components     string `json:"components"`
	Controls       int    `json:"controls"`
	SimpleControls int    `json:"simpleControls"`
}

// State 一次完整读取的结果。
type State struct {
	Card     CardInfo  `json:"card"`
	Controls []Control `json:"controls"`
}

// ---------------------------------------------------------------- amixer 调用

// Runner executes one amixer invocation and returns its stdout. It is
// injectable so tests can script the mixer without a sound card.
type Runner func(ctx context.Context, binary string, args ...string) ([]byte, error)

func defaultRunner(ctx context.Context, binary string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		command := strings.Join(args, " ")
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("amixer %s 失败: %w", command, err)
		}
		return nil, fmt.Errorf("amixer %s 失败: %s (%w)", command, msg, err)
	}
	return stdout.Bytes(), nil
}

// Controller reads and writes the simple mixer controls of one ALSA card.
// Every amixer invocation is serialised through an internal mutex, mirroring
// the single-owner model of the former webmixer server.
type Controller struct {
	card       int
	amixerPath string
	logger     *slog.Logger
	timeout    time.Duration
	run        Runner

	mu            sync.Mutex
	systemControl string
}

// New assembles the controller for the configured card (ALSA_CARD) using the
// configured amixer binary (AMIXER_PATH, default "amixer").
func New(cfg config.Config, logger *slog.Logger) *Controller {
	if logger == nil {
		logger = slog.Default()
	}
	path := strings.TrimSpace(cfg.AmixerPath)
	if path == "" {
		path = "amixer"
	}
	return &Controller{
		card:       cfg.Card,
		amixerPath: path,
		logger:     logger,
		timeout:    defaultCommandTimeout,
		run:        defaultRunner,
	}
}

// WithRunner replaces the amixer executor; tests pass a scripted fake.
func (c *Controller) WithRunner(run Runner) *Controller {
	if run != nil {
		c.run = run
	}
	return c
}

// Card reports the ALSA card index the controller drives.
func (c *Controller) Card() int { return c.card }

// State reads every simple control of the card (scontents) plus the card
// info block. A failing `amixer info` degrades to an empty info block: the
// controls are what every caller actually renders.
func (c *Controller) State(ctx context.Context) (*State, error) {
	out, err := c.exec(ctx, "scontents")
	if err != nil {
		return nil, err
	}
	state := &State{Card: CardInfo{Card: c.card}}
	if info, infoErr := c.exec(ctx, "info"); infoErr == nil {
		parseCardInfo(string(info), &state.Card)
	}
	controls, err := parseSContents(string(out))
	if err != nil {
		return nil, err
	}
	state.Controls = controls
	return state, nil
}

// SetVolume writes raw hardware values (one per channel, amixer -R
// semantics). The caller has already validated and clamped the values; a
// single value is expanded to every volume channel of the control.
func (c *Controller) SetVolume(ctx context.Context, name, stream string, values []int) error {
	if err := validControlName(name); err != nil {
		return err
	}
	if len(values) == 0 {
		return errors.New("未指定音量值")
	}
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.Itoa(v)
	}
	if stream != "capture" {
		stream = "playback"
	}
	// -R 强制原始值语义；显式指定流方向避免歧义。
	_, err := c.exec(ctx, "-R", "sset", name, stream, strings.Join(parts, ","))
	return err
}

// SetSwitch flips one switch: playback uses mute/unmute, capture uses
// nocap/cap, toggle works for both.
func (c *Controller) SetSwitch(ctx context.Context, name, stream, action string) error {
	if err := validControlName(name); err != nil {
		return err
	}
	if stream != "capture" {
		stream = "playback"
	}
	var value string
	switch stream {
	case "capture":
		switch action {
		case "on":
			value = "cap"
		case "off":
			value = "nocap"
		default:
			value = "toggle"
		}
	default:
		switch action {
		case "on":
			value = "unmute"
		case "off":
			value = "mute"
		default:
			value = "toggle"
		}
	}
	_, err := c.exec(ctx, "sset", name, stream, value)
	return err
}

// ControlState reads a single named control with a targeted `amixer sget`;
// the output uses the same block format as scontents. Controls with index
// other than 0 are rejected because operating a duplicate name by name alone
// is ambiguous.
func (c *Controller) ControlState(ctx context.Context, name string) (*Control, error) {
	if err := validControlName(name); err != nil {
		return nil, err
	}
	out, err := c.exec(ctx, "sget", name)
	if err != nil {
		return nil, err
	}
	controls, err := parseSContents(string(out))
	if err != nil {
		return nil, err
	}
	control := FindControl(&State{Controls: controls}, name)
	if control == nil {
		return nil, fmt.Errorf("未找到控件 %s", name)
	}
	if control.Index != 0 {
		return nil, fmt.Errorf("控件 %s 的 index 为 %d，无法按名操作", name, control.Index)
	}
	return control, nil
}

// exec runs one amixer command for the configured card and returns stdout.
// The mixer mutex is held for the whole invocation, serialising reads and
// writes across every caller (console, MiPlay, DLNA).
func (c *Controller) exec(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"-c", strconv.Itoa(c.card)}, args...)
	c.mu.Lock()
	defer c.mu.Unlock()
	timeout := c.timeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := c.run(execCtx, c.amixerPath, full...)
	if err != nil && execCtx.Err() != nil {
		return nil, fmt.Errorf("amixer %s 超时: %w", strings.Join(full, " "), execCtx.Err())
	}
	return out, err
}

// IsUnavailable reports whether the failure means amixer itself could not be
// launched (missing binary), as opposed to a control-level error.
func IsUnavailable(err error) bool {
	return errors.Is(err, exec.ErrNotFound)
}

// ---------------------------------------------------------------- 辅助

// validControlName rejects names that would be misread as amixer options or
// smuggle content into the command line.
func validControlName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("控件名不能为空")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("非法控件名 %q", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7F {
			return fmt.Errorf("非法控件名 %q", name)
		}
	}
	return nil
}

// FindControl 按名称查找控件。
func FindControl(state *State, name string) *Control {
	if state == nil {
		return nil
	}
	for i := range state.Controls {
		if state.Controls[i].Name == name {
			return &state.Controls[i]
		}
	}
	return nil
}

// VolumeChannelCount 返回指定流方向上带音量的声道数。
func (c *Control) VolumeChannelCount(stream string) int {
	n := 0
	for i := range c.Channels {
		v := c.Channels[i].Playback
		if stream == "capture" {
			v = c.Channels[i].Capture
		}
		if v != nil && v.HasVolume {
			n++
		}
	}
	return n
}

// LimitFor 返回指定流方向的音量范围，无该方向音量时为 nil。
func (c *Control) LimitFor(stream string) *Limit {
	if stream == "capture" {
		return c.CaptureLimit
	}
	return c.PlaybackLimit
}

// ---------------------------------------------------------------- 输出解析

var (
	reSControl = regexp.MustCompile(`^Simple mixer control '(.+)',(\d+)\s*$`)
	reLimit    = regexp.MustCompile(`^Limits: (Playback|Capture) (-?\d+) - (-?\d+)`)
	reExclGrp  = regexp.MustCompile(`^Capture exclusive group:\s*(\d+)`)
	reBool     = regexp.MustCompile(`^\[(on|off)\]$`)
	rePercent  = regexp.MustCompile(`^\[(-?\d+)%\]$`)
	reDB       = regexp.MustCompile(`^\[(-?[\d.]+)dB\]$`)

	reCard       = regexp.MustCompile(`^Card \S+ '([^']*)'/'([^']*)'`)
	reMixerName  = regexp.MustCompile(`Mixer name\s*: '([^']*)'`)
	reComponents = regexp.MustCompile(`Components\s*: '([^']*)'`)
	reControls   = regexp.MustCompile(`Controls\s*:\s*(\d+)`)
	reSimpleCtl  = regexp.MustCompile(`Simple ctrls\s*:\s*(\d+)`)
)

// parseSContents 解析 `amixer scontents`（或单控件 `sget`）输出。
func parseSContents(out string) ([]Control, error) {
	var controls []Control
	cur := -1

	for _, line := range strings.Split(out, "\n") {
		if m := reSControl.FindStringSubmatch(line); m != nil {
			idx, _ := strconv.Atoi(m[2])
			controls = append(controls, Control{
				Name:           m[1],
				Index:          idx,
				ExclusiveGroup: -1,
			})
			cur = len(controls) - 1
			continue
		}
		if cur < 0 {
			continue
		}
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		c := &controls[cur]

		switch {
		case strings.HasPrefix(t, "Capabilities:"):
			c.Capabilities = strings.Fields(strings.TrimSpace(strings.TrimPrefix(t, "Capabilities:")))
		case strings.HasPrefix(t, "Playback channels:"):
			c.PlaybackChannels = splitChannels(strings.TrimPrefix(t, "Playback channels:"))
		case strings.HasPrefix(t, "Capture channels:"):
			c.CaptureChannels = splitChannels(strings.TrimPrefix(t, "Capture channels:"))
		default:
			if m := reExclGrp.FindStringSubmatch(t); m != nil {
				g, _ := strconv.Atoi(m[1])
				c.ExclusiveGroup = g
				continue
			}
			if m := reLimit.FindStringSubmatch(t); m != nil {
				min, _ := strconv.Atoi(m[2])
				max, _ := strconv.Atoi(m[3])
				l := &Limit{Min: min, Max: max}
				if m[1] == "Playback" {
					c.PlaybackLimit = l
				} else {
					c.CaptureLimit = l
				}
				continue
			}
			// 声道取值行，如 "Front Left: Playback 36 [57%] [-40.50dB] [on] Capture [off]"
			if i := strings.Index(t, ":"); i > 0 {
				parseChannelLine(c, t[:i], strings.TrimSpace(t[i+1:]))
			}
		}
	}
	return controls, nil
}

func splitChannels(s string) []string {
	parts := strings.Split(strings.TrimSpace(s), " - ")
	res := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			res = append(res, p)
		}
	}
	return res
}

// parseChannelLine 解析单声道取值行，填充控制器的声道数据。
func parseChannelLine(c *Control, label, rest string) {
	if rest == "" {
		return
	}
	ch := c.channel(label)
	fields := strings.Fields(rest)

	for i := 0; i < len(fields); {
		stream := fields[i]
		if stream != "Playback" && stream != "Capture" {
			i++
			continue
		}
		i++
		v := &ValueInfo{}
		if i < len(fields) {
			if m := reBool.FindStringSubmatch(fields[i]); m != nil {
				// 仅开关，无音量，如 "Mono: Playback [off]"
				v.HasSwitch = true
				v.On = m[1] == "on"
				i++
			} else if n, err := strconv.Atoi(fields[i]); err == nil {
				// 音量原始值，其后跟可选的 [xx%] [x.xxdB] [on/off]
				v.HasVolume = true
				v.Raw = n
				i++
				for i < len(fields) {
					if m := rePercent.FindStringSubmatch(fields[i]); m != nil {
						v.Percent, _ = strconv.Atoi(m[1])
						i++
						continue
					}
					if m := reDB.FindStringSubmatch(fields[i]); m != nil {
						v.DB, _ = strconv.ParseFloat(m[1], 64)
						v.HasDB = true
						i++
						continue
					}
					if m := reBool.FindStringSubmatch(fields[i]); m != nil {
						v.HasSwitch = true
						v.On = m[1] == "on"
						i++
						continue
					}
					break
				}
			}
		}
		if stream == "Playback" {
			ch.Playback = v
		} else {
			ch.Capture = v
		}
	}
}

// channel 按声道名取（或新建）声道条目。
func (c *Control) channel(label string) *Channel {
	for i := range c.Channels {
		if c.Channels[i].Name == label {
			return &c.Channels[i]
		}
	}
	c.Channels = append(c.Channels, Channel{Name: label})
	return &c.Channels[len(c.Channels)-1]
}

// parseCardInfo 解析 `amixer info` 输出。
func parseCardInfo(out string, ci *CardInfo) {
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case reCard.MatchString(t):
			m := reCard.FindStringSubmatch(t)
			ci.ID, ci.LongName = m[1], m[2]
		case reMixerName.MatchString(t):
			ci.MixerName = reMixerName.FindStringSubmatch(t)[1]
		case reComponents.MatchString(t):
			ci.Components = reComponents.FindStringSubmatch(t)[1]
		case strings.HasPrefix(t, "Controls"):
			if m := reControls.FindStringSubmatch(t); m != nil {
				ci.Controls, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(t, "Simple ctrls"):
			if m := reSimpleCtl.FindStringSubmatch(t); m != nil {
				ci.SimpleControls, _ = strconv.Atoi(m[1])
			}
		}
	}
}
