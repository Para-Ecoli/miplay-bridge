// Command miplay-bridge is the MiPlay Bridge (妙播桥) MiPlay + DLNA local audio bridge.
//
// Responsibilities, in startup order:
//
//  1. probe the host sound card through /dev/snd (NEVER /proc/asound, which is
//     not readable inside the container);
//  2. force `Auto-Mute Mode = Disabled` and unmute Master/Headphone/Speaker,
//     then verify the result — without this the whole feature is silent while
//     every API answers "ok";
//  3. render /etc/mpd.conf and supervise the MPD playback kernel (serves the
//     DLNA session and every URL submitted through the HTTP API);
//  4. bring up the sound-card arbitration engine, the MiPlay receiver (mDNS
//     advertising, binary control channel, ffmpeg|aplay realtime pipeline)
//     and the DLNA MediaRenderer (SSDP + SOAP + GENA);
//  5. serve the HTTP API, the DLNA device paths and the
//     embedded console on port 8092.
//
// MPD is GPL-2.0 third-party software executed as an independent process; this
// program neither links nor modifies it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/miplay-bridge/miplay-bridge/internal/alsa"
	"github.com/miplay-bridge/miplay-bridge/internal/api"
	"github.com/miplay-bridge/miplay-bridge/internal/config"
	"github.com/miplay-bridge/miplay-bridge/internal/dlna"
	"github.com/miplay-bridge/miplay-bridge/internal/logging"
	"github.com/miplay-bridge/miplay-bridge/internal/miplay"
	"github.com/miplay-bridge/miplay-bridge/internal/mpd"
	"github.com/miplay-bridge/miplay-bridge/internal/output"
)

// Exit codes are stable so the compose restart policy and the operator can
// tell the failure classes apart.
const (
	exitOK               = 0
	exitConfig           = 2
	exitSoundCardMissing = 3
	exitMixerNotReady    = 4
	exitKernelFailed     = 5
	exitReceiverFailed   = 6
)

func main() {
	// `miplay-bridge healthcheck` is what the container healthcheck runs.
	if len(os.Args) > 1 && strings.EqualFold(os.Args[1], "healthcheck") {
		os.Exit(runHealthcheck())
	}

	showVersion := flag.Bool("version", false, "print the bridge version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("miplay-bridge " + api.Version)
		return
	}

	if err := run(); err != nil {
		// run() already logged the operator-facing detail.
		os.Exit(exitCodeFor(err))
	}
}

// exitError carries the process exit code alongside the error.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func exitCodeFor(err error) int {
	var coded *exitError
	if errors.As(err, &coded) {
		return coded.code
	}
	return 1
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid configuration:", err)
		return &exitError{code: exitConfig, err: err}
	}
	logger := logging.New(cfg.LogLevel)
	slog.SetDefault(logger)

	logger.Info("miplay-bridge starting",
		"version", api.Version,
		"port", cfg.Port,
		"alsa_device", cfg.ALSAHardwareDevice(),
		"mpd", cfg.MPDAddress(),
		"music_directory", cfg.MPDMusicDirectory,
		"miplay_enabled", cfg.MiPlayEnabled,
		"dlna_enabled", cfg.DLNAEnabled,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Step 1: sound card self-check.
	mixer := alsa.New(cfg, logger)
	report, probeErr := alsa.Probe(ctx, cfg, mixer, logger)
	if probeErr != nil {
		logger.Error("sound card self-check failed: this container does not apply to this host",
			"error", probeErr,
			"hint", "if the host has no sound card, do not enable deploy/docker-compose.miplay-bridge.yml",
		)
		return &exitError{code: exitSoundCardMissing, err: probeErr}
	}
	logger.Info("sound card ready",
		"device", report.HardwareDevice,
		"pcm_access", report.PCMAccess,
		"control_access", report.ControlAccess,
		"controls", strings.Join(report.ControlNames, ", "),
	)

	// Step 2 (the decisive step): mixer initialisation.
	mixerState, mixerErr := mixer.Init(ctx)
	if mixerErr != nil {
		logger.Error("mixer initialisation failed; refusing to start the playback kernel to avoid a silent-but-green container",
			"error", mixerErr,
			"auto_mute_state", mixerState.AutoMuteState,
			"hint", "check `amixer -c "+fmt.Sprint(cfg.Card)+" scontrols` on the host and set ALSA_* control names to match",
		)
		return &exitError{code: exitMixerNotReady, err: mixerErr}
	}
	if !mixerState.AutoMuteFixed {
		logger.Warn("Auto-Mute Mode was not disabled; if there is no sound, this is the first thing to check",
			"auto_mute_state", mixerState.AutoMuteState)
	}

	// Step 3: MPD playback kernel.
	manager := mpd.NewManager(cfg, logger)
	if err := manager.LoadTemplate(); err != nil {
		logger.Error("could not load the MPD configuration template", "error", err)
		return &exitError{code: exitKernelFailed, err: err}
	}
	manager.SetMixerControl(mixerControlFor(report, cfg, mixerState.EffectiveControl))
	if err := manager.Start(ctx); err != nil {
		logger.Error("could not start the MPD playback kernel", "error", err)
		return &exitError{code: exitKernelFailed, err: err}
	}
	manager.Supervise(ctx)

	// Step 3b: sound-card arbitration engine. Every writer (MiPlay, DLNA,
	// API) goes through the engine, so the exclusive ALSA device is never
	// claimed twice.
	engine := output.New(cfg, logger, manager, mixer)
	engine.Supervise(ctx)

	// Apply the configured default volume through the engine so the hardware
	// mixer and the kernel start from the same predictable level.
	if _, err := engine.SetVolume(ctx, cfg.DefaultVolume); err != nil {
		logger.Warn("could not apply the default volume", "error", err, "volume", cfg.DefaultVolume)
	}

	// Step 4: MiPlay receiver (mDNS + control channel + realtime pipeline).
	var receiver *miplay.Receiver
	if cfg.MiPlayEnabled {
		receiver = miplay.NewReceiver(cfg, logger, miplayHooks(engine, logger))
		if err := receiver.Start(ctx); err != nil {
			logger.Error("could not start the MiPlay receiver", "error", err)
			_ = manager.Stop(context.Background())
			return &exitError{code: exitReceiverFailed, err: err}
		}
	} else {
		logger.Info("miplay receiver disabled by configuration")
	}

	// Step 4b: DLNA MediaRenderer.
	var renderer *dlna.Renderer
	if cfg.DLNAEnabled {
		renderer = dlna.NewRenderer(cfg, logger, engine, api.Version)
		if err := renderer.Start(ctx); err != nil {
			logger.Error("could not start the DLNA renderer", "error", err)
			if receiver != nil {
				receiver.Shutdown()
			}
			_ = manager.Stop(context.Background())
			return &exitError{code: exitReceiverFailed, err: err}
		}
	} else {
		logger.Info("dlna renderer disabled by configuration")
	}

	// Step 5: HTTP API + embedded console + DLNA paths.
	tone := api.NewToneProvider(cfg)
	server := api.New(api.Options{
		Config: cfg,
		Logger: logger,
		Report: report,
		Mixer:  mixer,
		Engine: engine,
		MiPlay: receiver,
		DLNA:   renderer,
		Tone:   tone,
	})
	httpServer := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           server.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http api listening", "address", cfg.Addr())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			logger.Error("http api failed", "error", err)
			if renderer != nil {
				renderer.Shutdown()
			}
			if receiver != nil {
				receiver.Shutdown()
			}
			_ = manager.Stop(context.Background())
			return &exitError{code: exitKernelFailed, err: err}
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown was not clean", "error", err)
	}
	if renderer != nil {
		renderer.Shutdown()
	}
	if receiver != nil {
		receiver.Shutdown()
	}
	if err := manager.Stop(shutdownCtx); err != nil {
		logger.Warn("could not stop the playback kernel cleanly", "error", err)
	}
	logger.Info("miplay-bridge stopped")
	return nil
}

// miplayHooks connect the MiPlay receiver to the arbitration engine. The
// engine is the single authority on who owns the sound card; the receiver
// only ever asks.
func miplayHooks(engine *output.Engine, logger *slog.Logger) miplay.Hooks {
	return miplay.Hooks{
		Claim: func() error {
			if err := engine.Claim(output.SourceMiPlay); err != nil {
				return fmt.Errorf("%w: %v", miplay.ErrDeviceBusy, err)
			}
			return nil
		},
		Release: func() {
			engine.Release(output.SourceMiPlay)
		},
		SetVolume: func(percent int) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := engine.SetVolume(ctx, percent); err != nil {
				logger.Warn("could not mirror the miplay volume onto the hardware mixer", "error", err, "volume", percent)
			}
		},
	}
}

// mixerControlFor decides whether MPD may use a hardware mixer. MPD refuses to
// start when a configured hardware mixer control does not exist, so an absent
// control degrades to MPD's software mixer instead of a crash loop.
func mixerControlFor(report alsa.Report, cfg config.Config, effectiveControl string) string {
	if strings.TrimSpace(cfg.MPDMixerControl) != "" {
		for _, control := range report.ControlNames {
			if control == cfg.MPDMixerControl {
				return cfg.MPDMixerControl
			}
		}
	}
	if effectiveControl != "" {
		for _, control := range report.ControlNames {
			if control == effectiveControl {
				return effectiveControl
			}
		}
	}
	return ""
}

// runHealthcheck probes the local HTTP API. It is used by the container
// healthcheck and returns a process exit code.
func runHealthcheck() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: invalid configuration:", err)
		return 1
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.Port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: request failed:", err)
		return 1
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: unhealthy status", response.StatusCode)
		return 1
	}
	return 0
}
