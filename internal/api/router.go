// Package api exposes the miplay-bridge HTTP control plane.
//
// The endpoint shapes expose a stable REST contract and extend it with the
// MiPlay/DLNA cast clearing endpoints, the DLNA device paths and the embedded
// console, all on the same port (8092).
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/miplay-bridge/miplay-bridge/internal/alsa"
	"github.com/miplay-bridge/miplay-bridge/internal/config"
	"github.com/miplay-bridge/miplay-bridge/internal/dlna"
	"github.com/miplay-bridge/miplay-bridge/internal/miplay"
	"github.com/miplay-bridge/miplay-bridge/internal/mpd"
	"github.com/miplay-bridge/miplay-bridge/internal/output"
	"github.com/miplay-bridge/miplay-bridge/internal/webui"
)

// Version is reported by /healthz and /api/status.
var Version = "0.1.0"

// Options configures a Server. Engine may be nil in tests; the MiPlay and
// DLNA pointers are nil when the corresponding feature is disabled.
type Options struct {
	Config config.Config
	Logger *slog.Logger
	Report alsa.Report
	Mixer  *alsa.Mixer
	Engine *output.Engine
	MiPlay *miplay.Receiver
	DLNA   *dlna.Renderer
	Tone   *ToneProvider
}

// Server holds the HTTP dependencies.
type Server struct {
	cfg       config.Config
	logger    *slog.Logger
	report    alsa.Report
	mixer     *alsa.Mixer
	engine    *output.Engine
	miplay    *miplay.Receiver
	dlna      *dlna.Renderer
	client    *mpd.Client
	tone      *ToneProvider
	token     string
	startedAt time.Time
}

// New creates the HTTP server.
func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	server := &Server{
		cfg:       opts.Config,
		logger:    logger,
		report:    opts.Report,
		mixer:     opts.Mixer,
		engine:    opts.Engine,
		miplay:    opts.MiPlay,
		dlna:      opts.DLNA,
		tone:      opts.Tone,
		token:     strings.TrimSpace(opts.Config.Token),
		startedAt: time.Now().UTC(),
	}
	if opts.Engine != nil {
		server.client = opts.Engine.Client()
	}
	return server
}

// SetReport refreshes the cached sound-card probe result.
func (s *Server) SetReport(report alsa.Report) {
	s.report = report
}

// Routes builds the HTTP handler tree.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Health is deliberately unauthenticated: it is what the operator's
	// "test connection" action and the container healthcheck call.
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /api/healthz", s.healthz)

	mux.HandleFunc("GET /api/status", s.guard(s.status))
	mux.HandleFunc("GET /api/devices", s.guard(s.devices))
	mux.HandleFunc("POST /api/play", s.guard(s.play))
	mux.HandleFunc("POST /api/control", s.guard(s.control))
	mux.HandleFunc("POST /api/volume", s.guard(s.volume))
	mux.HandleFunc("POST /api/test-play", s.guard(s.testPlay))

	// One-click cast clearing for the embedded console and remote API callers.
	mux.HandleFunc("POST /api/miplay/disconnect", s.guard(s.miplayDisconnect))
	mux.HandleFunc("POST /api/dlna/disconnect", s.guard(s.dlnaDisconnect))

	// The generated test tone is fetched by MPD itself over loopback, so it
	// cannot carry the API token. Access is bounded by an unguessable path
	// segment plus a short time-to-live. The trailing ".wav" segment is kept
	// so the playback kernel can also probe the format from the URL.
	mux.HandleFunc("GET /tone/{token}/tone.wav", s.toneHandler)

	// The DLNA device endpoints (description, SCPD, SOAP control, GENA
	// eventing) are part of the UPnP contract: controllers cannot present a
	// bearer token, so these paths are open by design and guarded by their
	// own request shapes.
	if s.dlna != nil {
		mux.Handle("/dlna/", s.dlna.Handler())
	}

	// The embedded console page is public; every data operation it performs
	// goes through the token-guarded /api/* endpoints. The console owns only
	// the exact root path and its alias: a broader "GET /" pattern conflicts
	// with the all-methods "/dlna/" mount under the ServeMux precedence rules
	// (and panics at startup).
	mux.Handle("GET /{$}", webui.Handler())
	mux.Handle("GET /index.html", webui.Handler())

	return s.withCORS(s.withLogging(mux))
}

// guard enforces the optional shell token.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "invalid or missing bridge token", "unauthorized")
			return
		}
		next(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	candidate := strings.TrimSpace(r.Header.Get("x-bridge-token"))
	if candidate == "" {
		candidate = strings.TrimSpace(r.Header.Get("x-local-output-token"))
	}
	if candidate == "" {
		header := strings.TrimSpace(r.Header.Get("Authorization"))
		if len(header) > 7 && strings.EqualFold(header[:7], "Bearer ") {
			candidate = strings.TrimSpace(header[7:])
		}
	}
	if candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.token)) == 1
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		s.logger.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-bridge-token, x-local-output-token, X-Bridge-Username")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, SUBSCRIBE, UNSUBSCRIBE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// ---------------------------------------------------------------------------
// Response helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string, code string) {
	payload := map[string]any{"success": false, "error": message}
	if code != "" {
		payload["code"] = code
	}
	writeJSON(w, status, payload)
}

// writeKernelError maps playback failures onto structured HTTP errors.
// Device contention is a 409, not a 500: ALSA has no dmix on this hardware, so
// "resource busy" is an expected, actionable state rather than a server bug.
func (s *Server) writeKernelError(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, output.ErrSourceBusy):
		writeError(w, http.StatusConflict, "the sound card is held by another source: clear the MiPlay cast or the DLNA session first", "device_busy")
	case errors.Is(err, mpd.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "playback kernel is unavailable: "+err.Error(), "mpd_unavailable")
	case mpd.IsDeviceBusy(err):
		writeError(w, http.StatusConflict, "ALSA device is busy: the sound card is exclusive (no dmix) and only one stream can play at a time", "device_busy")
	case mpd.IsPermissionDenied(err):
		writeError(w, http.StatusForbidden, "playback kernel denied access to the resource: "+err.Error(), "permission_denied")
	case mpd.IsNotFound(err):
		writeError(w, http.StatusNotFound, "playback kernel could not find the requested resource: "+err.Error(), "not_found")
	default:
		writeError(w, http.StatusBadGateway, "playback kernel error: "+err.Error(), "mpd_error")
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) error {
	if r.Body == nil {
		return errors.New("empty request body")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// ---------------------------------------------------------------------------
// Shared state helpers
// ---------------------------------------------------------------------------

// kernelSnapshot returns the supervised kernel snapshot, or a zero value when
// the kernel is not wired (tests, degraded startup).
func (s *Server) kernelSnapshot() mpd.Snapshot {
	if s.engine == nil || s.engine.Manager() == nil {
		return mpd.Snapshot{}
	}
	return s.engine.Manager().Snapshot()
}

// activeSource reports which writer currently owns the sound card.
func (s *Server) activeSource() string {
	if s.engine == nil {
		return string(output.SourceIdle)
	}
	return string(s.engine.ActiveSource())
}

// ready reports whether playback can be attempted right now.
func (s *Server) ready() bool {
	if s.engine == nil || s.client == nil {
		return false
	}
	mixerState := alsa.State{}
	if s.mixer != nil {
		mixerState = s.mixer.State()
	}
	return s.report.Ready() && mixerState.Initialized && s.kernelSnapshot().Running
}

// healthPayload builds the /healthz body.
func (s *Server) healthPayload() (map[string]any, bool) {
	mixerState := alsa.State{}
	if s.mixer != nil {
		mixerState = s.mixer.State()
	}
	kernel := s.kernelSnapshot()
	ready := s.ready()

	status := "ok"
	if !ready {
		status = "degraded"
		if !s.report.Present {
			status = "unhealthy"
		}
	}

	payload := map[string]any{
		"status":         status,
		"ready":          ready,
		"version":        Version,
		"uptime_seconds": int(time.Since(s.startedAt).Seconds()),
		"container":      map[string]any{"alive": true},
		"sound_card":     s.report,
		"mixer":          mixerState,
		"mpd":            kernel,
		// auto_mute_fixed is duplicated at the top level because it is the one
		// flag that explains "everything green but no sound".
		"auto_mute_fixed": mixerState.AutoMuteFixed,
	}
	if !ready {
		reasons := []string{}
		if !s.report.Present {
			reasons = append(reasons, "sound card is not available")
		}
		if !mixerState.Initialized {
			reasons = append(reasons, "mixer is not initialised")
		}
		if !kernel.Running {
			reasons = append(reasons, "playback kernel is not running")
		}
		payload["reasons"] = reasons
	}
	return payload, ready
}

// healthz is the container and operator health probe.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	payload, ready := s.healthPayload()
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, payload)
}

// playbackSnapshot reads live playback state from the kernel.
func (s *Server) playbackSnapshot(ctx context.Context) (map[string]any, error) {
	if s.client == nil {
		return nil, mpd.ErrUnavailable
	}
	status, err := s.client.Status(ctx)
	if err != nil {
		return nil, err
	}
	song, err := s.client.CurrentSong(ctx)
	if err != nil {
		return nil, err
	}
	playback := map[string]any{
		"state":       status["state"],
		"volume":      atoiOr(status["volume"], -1),
		"elapsed":     atofOr(status["elapsed"], 0),
		"duration":    atofOr(status["duration"], 0),
		"bitrate":     status["bitrate"],
		"audio":       status["audio"],
		"error":       status["error"],
		"repeat":      status["repeat"] == "1",
		"random":      status["random"] == "1",
		"single":      status["single"] == "1",
		"consume":     status["consume"] == "1",
		"queue_index": atoiOr(status["song"], -1),
	}
	if song != nil {
		playback["title"] = song["Title"]
		playback["artist"] = song["Artist"]
		playback["album"] = song["Album"]
		playback["uri"] = song["file"]
		playback["duration_song"] = atofOr(song["duration"], 0)
	}
	return playback, nil
}

// status returns the full bridge status.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	mixerState := alsa.State{}
	if s.mixer != nil {
		mixerState = s.mixer.State()
	}
	kernel := s.kernelSnapshot()
	playback, err := s.playbackSnapshot(r.Context())
	if err != nil {
		playback = map[string]any{"state": "unknown", "error": err.Error()}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":         true,
		"service_online":  true,
		"version":         Version,
		"uptime_seconds":  int(time.Since(s.startedAt).Seconds()),
		"sound_card":      s.report,
		"mixer":           mixerState,
		"mpd":             kernel,
		"playback":        playback,
		"auto_mute_fixed": mixerState.AutoMuteFixed,
		"active_source":   s.activeSource(),
		"miplay":          s.miplayStatus(),
		"dlna":            s.dlnaStatus(),
	})
}

// miplayStatus is the MiPlay slice of /api/status.
func (s *Server) miplayStatus() any {
	if s.miplay == nil {
		return map[string]any{"enabled": false}
	}
	return s.miplay.Status()
}

// dlnaStatus is the DLNA slice of /api/status.
func (s *Server) dlnaStatus() any {
	if s.dlna == nil {
		return map[string]any{"enabled": false}
	}
	return s.dlna.Status()
}

// devices lists the ALSA cards the container can see.
func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"cards":   s.report.Cards,
		"selected": map[string]any{
			"card":            s.report.Card,
			"pcm_device":      s.report.PCMDevice,
			"hardware_device": s.report.HardwareDevice,
		},
		"exclusive": true,
	})
}
