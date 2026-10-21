// Package output is the sound-card arbitration engine.
//
// One exclusive ALSA device (no dmix) is shared by three writers: the MiPlay
// realtime pipeline (ffmpeg|aplay), the MPD playback kernel serving DLNA and
// the HTTP API, and the operator console. The engine tracks which source
// currently owns the card, serialises the hand-offs and exposes one typed
// playback surface so no caller has to talk to MPD directly.
package output

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
	"github.com/miplay-bridge/miplay-bridge/internal/mpd"
)

// Source identifies who owns the sound card right now.
type Source string

// The arbitration states. "api" covers /api/play and the test tone, "dlna"
// covers every URL loaded by an UPnP controller.
const (
	SourceIdle   Source = "idle"
	SourceMiPlay Source = "miplay"
	SourceDLNA   Source = "dlna"
	SourceAPI    Source = "api"
)

// ErrSourceBusy is returned when a claim collides with the current owner.
// The HTTP layer maps it onto 409 device_busy, the DLNA layer onto a UPnP
// "transition not available" fault.
var ErrSourceBusy = errors.New("the sound card is held by another source")

// ErrNoVolumeControl reports that the software volume surface is unavailable
// because the playback kernel is not running. The DLNA layer degrades to its
// fixed full-scale answers on this error. Protocol volume never depends on an
// ALSA control: the hardware mixer belongs to the console.
var ErrNoVolumeControl = mpd.ErrUnavailable

// PlaybackInfo is the unified playback snapshot.
type PlaybackInfo struct {
	// State is the kernel transport state: play, pause, stop or unknown.
	State string `json:"state"`
	// Elapsed / Duration are seconds of the current track.
	Elapsed  float64 `json:"elapsed"`
	Duration float64 `json:"duration"`
	// URI / Title / Artist / Album describe the current queue entry.
	URI    string `json:"uri,omitempty"`
	Title  string `json:"title,omitempty"`
	Artist string `json:"artist,omitempty"`
	Album  string `json:"album,omitempty"`
	// Error carries a kernel access problem without failing the read.
	Error string `json:"error,omitempty"`
}

// Player is the playback surface the DLNA renderer drives. *Engine implements
// it; declaring it here also keeps the renderer unit-testable with a fake.
// The volume methods drive the playback kernel's software gain; they never
// touch the ALSA mixer, which the console owns exclusively.
type Player interface {
	ClaimDLNA() error
	ReleaseDLNA()
	ActiveSource() Source
	Load(ctx context.Context, uri string) error
	Clear(ctx context.Context) error
	Play(ctx context.Context) error
	Pause(ctx context.Context) error
	Stop(ctx context.Context) error
	Seek(ctx context.Context, seconds float64) error
	Info(ctx context.Context) PlaybackInfo
	SetVolume(ctx context.Context, percent int) (int, error)
	SetMute(ctx context.Context, muted bool) (bool, error)
	Volume(ctx context.Context) (int, bool, error)
}

// Engine arbitrates the sound card between the MiPlay pipeline and the MPD
// kernel and proxies the kernel surface.
type Engine struct {
	cfg     config.Config
	logger  *slog.Logger
	manager *mpd.Manager
	client  *mpd.Client

	// volumeMu guards the playback kernel's software gain state. Protocol
	// volume is an upstream gain only: the hardware mixer is never read or
	// written here, so the console's ALSA controls stay independent.
	volumeMu sync.Mutex
	volume   int
	muted    bool

	mu     sync.Mutex
	active Source
	// stopStreak counts consecutive monitor ticks that observed a stopped
	// kernel while dlna/api held the slot. Two in a row release it: one lone
	// "stop" is a normal step inside Clear -> Add -> Play.
	stopStreak int
}

// New assembles the engine around an already running playback kernel.
func New(cfg config.Config, logger *slog.Logger, manager *mpd.Manager) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	engine := &Engine{
		cfg:     cfg,
		logger:  logger,
		manager: manager,
		active:  SourceIdle,
		volume:  100,
	}
	if manager != nil {
		engine.client = manager.Client()
	}
	return engine
}

// Client exposes the raw MPD client for the read-only status paths that keep
// the original local-output response shapes.
func (e *Engine) Client() *mpd.Client { return e.client }

// Manager exposes the supervised playback kernel for /healthz reporting.
func (e *Engine) Manager() *mpd.Manager { return e.manager }

// ---------------------------------------------------------------------------
// Protocol volume: software gain inside the playback kernel
// ---------------------------------------------------------------------------
// SetVolume sets the playback kernel's software gain and reports the level in
// effect. The hardware mixer is deliberately untouched: protocol volume is an
// upstream gain, while the console owns every ALSA control.
func (e *Engine) SetVolume(ctx context.Context, percent int) (int, error) {
	percent = clampPercent(percent)
	if e.client == nil {
		return 0, ErrNoVolumeControl
	}
	e.volumeMu.Lock()
	defer e.volumeMu.Unlock()
	if e.muted {
		// Keep the requested level for the next unmute; the kernel stays at 0.
		e.volume = percent
		return percent, nil
	}
	if err := e.client.SetVolume(ctx, percent); err != nil {
		return 0, err
	}
	e.volume = percent
	return percent, nil
}

// SetMute applies a software mute by driving the kernel's gain to zero and
// restores the previous level on unmute. No ALSA switch is involved.
func (e *Engine) SetMute(ctx context.Context, muted bool) (bool, error) {
	if e.client == nil {
		return false, ErrNoVolumeControl
	}
	e.volumeMu.Lock()
	defer e.volumeMu.Unlock()
	if e.muted == muted {
		return muted, nil
	}
	target := e.volume
	if muted {
		target = 0
	}
	if err := e.client.SetVolume(ctx, target); err != nil {
		return false, err
	}
	e.muted = muted
	return muted, nil
}

// Volume reports the kernel's software gain and mute state. While muted the
// remembered level is returned (not the zero the kernel sees) so a controller
// can display and restore it.
func (e *Engine) Volume(ctx context.Context) (int, bool, error) {
	if e.client == nil {
		return 0, false, ErrNoVolumeControl
	}
	e.volumeMu.Lock()
	defer e.volumeMu.Unlock()
	if e.muted {
		return e.volume, true, nil
	}
	live, ok, err := e.client.Volume(ctx)
	if err != nil {
		return 0, false, err
	}
	if !ok {
		return e.volume, false, nil
	}
	e.volume = live
	return live, false, nil
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

// ---------------------------------------------------------------------------
// Arbitration
// ---------------------------------------------------------------------------

// Claim reserves the card for one source. MiPlay additionally verifies that
// the kernel is not mid-stream, covering a lost release.
func (e *Engine) Claim(source Source) error {
	if source == SourceIdle {
		return errors.New("the idle source cannot claim the sound card")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active == source {
		return nil
	}
	if e.active != SourceIdle {
		return fmt.Errorf("%w: %s is active", ErrSourceBusy, e.active)
	}
	if source == SourceMiPlay {
		if state, err := e.kernelState(); err == nil && (state == "play" || state == "pause") {
			return fmt.Errorf("%w: the playback kernel is still streaming", ErrSourceBusy)
		}
	}
	e.active = source
	e.stopStreak = 0
	e.logger.Info("sound card claimed", "source", source)
	return nil
}

// Release frees the slot when the given source still owns it. Idempotent.
func (e *Engine) Release(source Source) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.releaseLocked(source)
}

// ReleaseSource forces the slot back to idle regardless of the owner and
// reports who held it. It is the safety net behind stop_all and the console.
func (e *Engine) ReleaseSource() Source {
	e.mu.Lock()
	defer e.mu.Unlock()
	previous := e.active
	e.releaseLocked(previous)
	return previous
}

func (e *Engine) releaseLocked(source Source) {
	if source == SourceIdle || e.active != source {
		return
	}
	e.active = SourceIdle
	e.stopStreak = 0
	e.logger.Info("sound card released", "source", source)
}

// ActiveSource reports the current owner for /api/status.
func (e *Engine) ActiveSource() Source {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.active
}

// ClaimDLNA / ReleaseDLNA are the Player interface spellings of Claim/Release.
func (e *Engine) ClaimDLNA() error { return e.Claim(SourceDLNA) }
func (e *Engine) ReleaseDLNA()     { e.Release(SourceDLNA) }

// ---------------------------------------------------------------------------
// Kernel surface (Player implementation + API primitives)
// ---------------------------------------------------------------------------

// Load prepares the queue with exactly one URI, stopped.
func (e *Engine) Load(ctx context.Context, uri string) error {
	if err := e.client.Clear(ctx); err != nil {
		return err
	}
	return e.client.Add(ctx, uri)
}

// Clear empties the queue; playback of a cleared queue stops.
func (e *Engine) Clear(ctx context.Context) error {
	if e.client == nil {
		return mpd.ErrUnavailable
	}
	return e.client.Clear(ctx)
}

// Play starts (or resumes) kernel playback.
func (e *Engine) Play(ctx context.Context) error {
	if e.client == nil {
		return mpd.ErrUnavailable
	}
	return e.client.Play(ctx)
}

// Pause pauses kernel playback.
func (e *Engine) Pause(ctx context.Context) error {
	if e.client == nil {
		return mpd.ErrUnavailable
	}
	return e.client.Pause(ctx, true)
}

// Stop halts the kernel and ends whatever dlna/api claim was active: a
// stopped kernel releases the ALSA device by itself.
func (e *Engine) Stop(ctx context.Context) error {
	if e.client == nil {
		return mpd.ErrUnavailable
	}
	if err := e.client.Stop(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	e.releaseLocked(SourceDLNA)
	e.releaseLocked(SourceAPI)
	e.mu.Unlock()
	return nil
}

// Seek moves within the current queue entry.
func (e *Engine) Seek(ctx context.Context, seconds float64) error {
	if e.client == nil {
		return mpd.ErrUnavailable
	}
	if seconds < 0 {
		seconds = 0
	}
	return e.client.SeekCur(ctx, time.Duration(seconds*float64(time.Second)))
}

// Info reads one unified playback snapshot. Failures are reported inside the
// struct so polling callers never need error branches for cosmetic reads.
func (e *Engine) Info(ctx context.Context) PlaybackInfo {
	info := PlaybackInfo{State: "unknown"}
	if e.client == nil {
		info.Error = "playback kernel is unavailable"
		return info
	}
	status, err := e.client.Status(ctx)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.State = status["state"]
	info.Elapsed = atofOr(status["elapsed"], 0)
	info.Duration = atofOr(status["duration"], 0)
	if song, songErr := e.client.CurrentSong(ctx); songErr == nil && song != nil {
		info.URI = song["file"]
		info.Title = song["Title"]
		info.Artist = song["Artist"]
		info.Album = song["Album"]
		if info.Duration == 0 {
			info.Duration = atofOr(song["duration"], 0)
		}
	}
	return info
}

// Supervise watches for a kernel that stopped on its own (natural end of a
// stream) while dlna/api still held the slot, and releases it.
func (e *Engine) Supervise(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			e.mu.Lock()
			source := e.active
			e.mu.Unlock()
			if source != SourceDLNA && source != SourceAPI {
				continue
			}
			state, err := e.kernelState()
			if err != nil {
				continue
			}
			e.mu.Lock()
			if e.active == source && state == "stop" {
				e.stopStreak++
				if e.stopStreak >= 2 {
					e.logger.Info("playback kernel stopped on its own; releasing the slot", "source", source)
					e.releaseLocked(source)
				}
			} else {
				e.stopStreak = 0
			}
			e.mu.Unlock()
		}
	}()
}

// kernelState reads the current transport state with a short timeout; it is
// called while the arbitration mutex is held, so it must never block long.
func (e *Engine) kernelState() (string, error) {
	if e.client == nil {
		return "", mpd.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status, err := e.client.Status(ctx)
	if err != nil {
		return "", err
	}
	return status["state"], nil
}

func atofOr(raw string, fallback float64) float64 {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return fallback
	}
	return value
}
