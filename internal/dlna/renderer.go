// Renderer is the UPnP MediaRenderer device: identity, description, SOAP
// control, GENA eventing and the transport state machine that drives the
// playback engine.
package dlna

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
	"github.com/miplay-bridge/miplay-bridge/internal/mpd"
	"github.com/miplay-bridge/miplay-bridge/internal/output"
)

// The three service type URNs, used in SOAP responses.
const (
	avTransportService      = "urn:schemas-upnp-org:service:AVTransport:1"
	renderingControlService = "urn:schemas-upnp-org:service:RenderingControl:1"
)

// sinkProtocolInfo is the Sink protocolInfo list advertised through
// ConnectionManager; the kernel can fetch any HTTP stream it can decode.
const sinkProtocolInfo = "http-get:*:audio/mpeg:*,http-get:*:audio/mp4:*,http-get:*:audio/aac:*," +
	"http-get:*:audio/flac:*,http-get:*:audio/x-flac:*,http-get:*:audio/wav:*,http-get:*:audio/x-wav:*," +
	"http-get:*:audio/ogg:*,http-get:*:audio/L16;rate=44100;channels=2:*,http-get:*:audio/L16;rate=48000;channels=2:*"

// Status is the DLNA slice of /api/status.
type Status struct {
	Enabled        bool   `json:"enabled"`
	FriendlyName   string `json:"friendly_name"`
	UDN            string `json:"udn,omitempty"`
	Address        string `json:"address,omitempty"`
	Subscribers    int    `json:"subscribers"`
	TransportState string `json:"transport_state"`
	CurrentURI     string `json:"current_uri,omitempty"`
	Title          string `json:"title,omitempty"`
	Artist         string `json:"artist,omitempty"`
	Album          string `json:"album,omitempty"`
}

// Renderer is one MediaRenderer device backed by the playback engine.
type Renderer struct {
	cfg     config.Config
	logger  *slog.Logger
	player  output.Player
	version string
	server  string

	udn     string
	address string
	events  *eventManager
	ssdp    *ssdpServer

	mu                 sync.Mutex
	currentURI         string
	currentURIMetaData string
	title              string
	artist             string
	album              string
	lastTransportState string
	started            bool
	closed             bool
}

// NewRenderer assembles the device; call Start to bring it online.
func NewRenderer(cfg config.Config, logger *slog.Logger, player output.Player, version string) *Renderer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Renderer{
		cfg:     cfg,
		logger:  logger,
		player:  player,
		version: version,
		server:  serverHeader(version),
		events:  newEventManager(logger),
	}
}

// Start resolves the advertise address, persists the UDN and starts SSDP.
func (r *Renderer) Start(ctx context.Context) error {
	address := strings.TrimSpace(r.cfg.DLNAAdvertiseAddress)
	if address == "" {
		detected, err := detectAdvertiseAddress()
		if err != nil {
			return fmt.Errorf("resolve the DLNA advertise address (set DLNA_ADVERTISE_ADDRESS to override): %w", err)
		}
		address = detected
	}
	udn, err := LoadOrCreateUDN(r.cfg.DLNAUDNFile())
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.address = address
	r.udn = udn
	r.mu.Unlock()

	r.ssdp = newSSDPServer(r.logger, address, r.cfg.Port, udn, r.version, r.cfg.DLNASSDPMaxAge)
	if err := r.ssdp.start(ctx); err != nil {
		return fmt.Errorf("start the DLNA SSDP responder: %w", err)
	}
	go r.supervise(ctx)
	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
	r.logger.Info("dlna renderer online",
		"friendly_name", r.cfg.DLNAFriendlyName,
		"udn", udn,
		"address", address,
		"port", r.cfg.Port,
	)
	return nil
}

// Shutdown stops SSDP advertising and the event worker.
func (r *Renderer) Shutdown() {
	r.mu.Lock()
	alreadyClosed := r.closed
	r.closed = true
	r.mu.Unlock()
	if alreadyClosed {
		return
	}
	if r.ssdp != nil {
		r.ssdp.stop()
	}
	r.events.closeAll()
	r.events.shutdown()
}

// ---------------------------------------------------------------------------
// HTTP surface
// ---------------------------------------------------------------------------

// Handler serves every /dlna/ path: description, SCPD, control and eventing.
func (r *Renderer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dlna/description.xml", r.handleDescription)
	mux.HandleFunc("GET /dlna/scpd/{name}", r.handleSCPD)
	mux.HandleFunc("POST /dlna/control/{service}", r.handleControl)
	mux.HandleFunc("SUBSCRIBE /dlna/event/{service}", r.handleSubscribe)
	mux.HandleFunc("UNSUBSCRIBE /dlna/event/{service}", r.handleUnsubscribe)
	return mux
}

func (r *Renderer) handleDescription(w http.ResponseWriter, request *http.Request) {
	r.mu.Lock()
	udn := r.udn
	r.mu.Unlock()
	if udn == "" {
		http.Error(w, "dlna renderer is not started", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(deviceDescription(r.cfg.DLNAFriendlyName, udn, r.version))
}

func (r *Renderer) handleSCPD(w http.ResponseWriter, request *http.Request) {
	name := strings.TrimSuffix(strings.ToLower(request.PathValue("name")), ".xml")
	document, ok := scpdDocument(name)
	if !ok {
		http.NotFound(w, request)
		return
	}
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(document)
}

func (r *Renderer) handleControl(w http.ResponseWriter, request *http.Request) {
	service, ok := serviceByKey(request.PathValue("service"))
	if !ok {
		http.NotFound(w, request)
		return
	}
	action, ok := parseSOAPAction(request)
	if !ok {
		writeSOAPFault(w, errInvalidAction)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, request.Body, 1<<20))
	if err != nil {
		writeSOAPFault(w, errInvalidArgs)
		return
	}
	bodyAction, args, err := parseActionArgs(body)
	if err != nil {
		writeSOAPFault(w, errInvalidArgs)
		return
	}
	if bodyAction != action {
		writeSOAPFault(w, errInvalidAction)
		return
	}
	switch service.Name {
	case "AVTransport":
		r.handleAVTransport(w, request, action, args)
	case "RenderingControl":
		r.handleRenderingControl(w, request, action, args)
	case "ConnectionManager":
		r.handleConnectionManager(w, request, action, args)
	default:
		writeSOAPFault(w, errInvalidAction)
	}
}

func (r *Renderer) handleSubscribe(w http.ResponseWriter, request *http.Request) {
	if _, ok := serviceByKey(request.PathValue("service")); !ok {
		http.NotFound(w, request)
		return
	}
	w.Header().Set("SERVER", r.server)
	if sid := strings.TrimSpace(request.Header.Get("SID")); sid != "" {
		if strings.TrimSpace(request.Header.Get("NT")) != "" || strings.TrimSpace(request.Header.Get("CALLBACK")) != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		granted, ok := r.events.renew(sid, request.Header.Get("TIMEOUT"))
		if !ok {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.Header().Set("SID", sid)
		w.Header().Set("TIMEOUT", "Second-"+strconv.Itoa(int(granted.Seconds())))
		w.WriteHeader(http.StatusOK)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(request.Header.Get("NT")), "upnp:event") {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	sid, granted, err := r.events.subscribe(request.Header.Get("CALLBACK"), request.Header.Get("TIMEOUT"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("SID", sid)
	w.Header().Set("TIMEOUT", "Second-"+strconv.Itoa(int(granted.Seconds())))
	w.WriteHeader(http.StatusOK)
	// The initial event carries the complete current state.
	for _, payload := range r.initialEventBodies(request, request.PathValue("service")) {
		r.events.sendTo(sid, payload)
	}
}

func (r *Renderer) handleUnsubscribe(w http.ResponseWriter, request *http.Request) {
	if _, ok := serviceByKey(request.PathValue("service")); !ok {
		http.NotFound(w, request)
		return
	}
	sid := strings.TrimSpace(request.Header.Get("SID"))
	if sid == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !r.events.unsubscribe(sid) {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	w.Header().Set("SERVER", r.server)
	w.WriteHeader(http.StatusOK)
}

// initialEventBodies renders the first NOTIFY payload of a new subscription.
func (r *Renderer) initialEventBodies(request *http.Request, serviceKey string) [][]byte {
	service, ok := serviceByKey(serviceKey)
	if !ok {
		return nil
	}
	switch service.Name {
	case "AVTransport":
		ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
		defer cancel()
		r.mu.Lock()
		defer r.mu.Unlock()
		return [][]byte{lastChangePayload(avtEventFragment(r.transportProperties(ctx)))}
	case "RenderingControl":
		volume, muted := r.player.Volume()
		return [][]byte{lastChangePayload(rcsEventFragment(volume, muted))}
	case "ConnectionManager":
		return [][]byte{connectionManagerPayload()}
	}
	return nil
}

// ---------------------------------------------------------------------------
// AVTransport
// ---------------------------------------------------------------------------

func (r *Renderer) handleAVTransport(w http.ResponseWriter, request *http.Request, action string, args map[string]string) {
	if !validInstanceID(args) {
		writeSOAPFault(w, errInvalidInstanceID)
		return
	}
	switch action {
	case "SetAVTransportURI":
		r.actionSetURI(w, request, args)
	case "SetNextAVTransportURI":
		// Accepted for controller compatibility; single-stream device.
		writeSOAPResponse(w, avTransportService, action, nil)
	case "GetMediaInfo":
		r.actionGetMediaInfo(w, request)
	case "GetTransportInfo":
		ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
		defer cancel()
		r.mu.Lock()
		state := deriveTransportState(r.player.Info(ctx), r.currentURI != "")
		r.mu.Unlock()
		writeSOAPResponse(w, avTransportService, action, []soapArgument{
			{Name: "CurrentTransportState", Value: state},
			{Name: "CurrentTransportStatus", Value: "OK"},
			{Name: "CurrentSpeed", Value: "1"},
		})
	case "GetPositionInfo":
		r.actionGetPositionInfo(w, request)
	case "GetDeviceCapabilities":
		writeSOAPResponse(w, avTransportService, action, []soapArgument{
			{Name: "PlayMedia", Value: "NETWORK,HDD"},
			{Name: "RecMedia", Value: "NOT_IMPLEMENTED"},
			{Name: "RecQualityModes", Value: "NOT_IMPLEMENTED"},
		})
	case "GetTransportSettings":
		writeSOAPResponse(w, avTransportService, action, []soapArgument{
			{Name: "PlayMode", Value: "NORMAL"},
			{Name: "RecQualityMode", Value: "NOT_IMPLEMENTED"},
		})
	case "Stop":
		r.actionStop(w, request)
	case "Play":
		r.actionPlay(w, request, args)
	case "Pause":
		r.actionPause(w, request)
	case "Seek":
		r.actionSeek(w, request, args)
	case "Next", "Previous":
		// Single-entry queue: nothing to skip to, but the action succeeds.
		writeSOAPResponse(w, avTransportService, action, nil)
	case "GetCurrentTransportActions":
		r.mu.Lock()
		actions := r.currentTransportActionsLocked()
		r.mu.Unlock()
		writeSOAPResponse(w, avTransportService, action, []soapArgument{{Name: "Actions", Value: actions}})
	default:
		writeSOAPFault(w, errInvalidAction)
	}
}

func (r *Renderer) actionSetURI(w http.ResponseWriter, request *http.Request, args map[string]string) {
	uri := strings.TrimSpace(args["CurrentURI"])
	if uri == "" {
		writeSOAPFault(w, errInvalidArgs)
		return
	}
	metadata := args["CurrentURIMetaData"]
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.player.Load(ctx, uri); err != nil {
		writeSOAPFault(w, kernelFault(err))
		return
	}
	title, artist, album := parseDIDL(metadata)
	r.currentURI = uri
	r.currentURIMetaData = metadata
	r.title, r.artist, r.album = title, artist, album
	r.emitTransportLocked(ctx)
	writeSOAPResponse(w, avTransportService, "SetAVTransportURI", nil)
}

func (r *Renderer) actionGetMediaInfo(w http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	info := r.player.Info(ctx)
	tracks := "0"
	if r.currentURI != "" {
		tracks = "1"
	}
	writeSOAPResponse(w, avTransportService, "GetMediaInfo", []soapArgument{
		{Name: "NrTracks", Value: tracks},
		{Name: "MediaDuration", Value: formatUPnPTime(info.Duration)},
		{Name: "CurrentURI", Value: r.currentURI},
		{Name: "CurrentURIMetaData", Value: r.currentURIMetaData},
		{Name: "NextURI", Value: ""},
		{Name: "NextURIMetaData", Value: ""},
		{Name: "PlayMedium", Value: "NETWORK"},
		{Name: "RecordMedium", Value: "NOT_IMPLEMENTED"},
		{Name: "WriteStatus", Value: "NOT_IMPLEMENTED"},
	})
}

func (r *Renderer) actionGetPositionInfo(w http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	info := r.player.Info(ctx)
	track := "0"
	relTime := "0:00:00"
	if r.currentURI != "" {
		track = "1"
		relTime = formatUPnPTime(info.Elapsed)
	}
	writeSOAPResponse(w, avTransportService, "GetPositionInfo", []soapArgument{
		{Name: "Track", Value: track},
		{Name: "TrackDuration", Value: formatUPnPTime(info.Duration)},
		{Name: "TrackMetaData", Value: r.currentURIMetaData},
		{Name: "TrackURI", Value: r.currentURI},
		{Name: "RelTime", Value: relTime},
		{Name: "AbsTime", Value: relTime},
		{Name: "RelCount", Value: "0"},
		{Name: "AbsCount", Value: "2147483647"},
	})
}

func (r *Renderer) actionStop(w http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.player.Stop(ctx); err != nil {
		writeSOAPFault(w, kernelFault(err))
		return
	}
	r.emitTransportLocked(ctx)
	writeSOAPResponse(w, avTransportService, "Stop", nil)
}

func (r *Renderer) actionPlay(w http.ResponseWriter, request *http.Request, args map[string]string) {
	if speed := strings.TrimSpace(args["Speed"]); speed != "" && speed != "1" && speed != "1/1" {
		writeSOAPFault(w, errPlaySpeed)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentURI == "" {
		writeSOAPFault(w, errTransition)
		return
	}
	if err := r.player.ClaimDLNA(); err != nil {
		r.logger.Info("dlna play rejected: the sound card is busy", "error", err)
		writeSOAPFault(w, errDeviceBusy)
		return
	}
	if err := r.player.Play(ctx); err != nil {
		r.player.ReleaseDLNA()
		writeSOAPFault(w, kernelFault(err))
		return
	}
	r.emitTransportLocked(ctx)
	writeSOAPResponse(w, avTransportService, "Play", nil)
}

func (r *Renderer) actionPause(w http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.player.Pause(ctx); err != nil {
		writeSOAPFault(w, kernelFault(err))
		return
	}
	r.emitTransportLocked(ctx)
	writeSOAPResponse(w, avTransportService, "Pause", nil)
}

func (r *Renderer) actionSeek(w http.ResponseWriter, request *http.Request, args map[string]string) {
	unit := strings.ToUpper(strings.TrimSpace(args["Unit"]))
	if unit != "REL_TIME" && unit != "ABS_TIME" {
		writeSOAPFault(w, errSeekMode)
		return
	}
	target, err := parseUPnPTime(args["Target"])
	if err != nil {
		writeSOAPFault(w, errSeekTarget)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentURI == "" {
		writeSOAPFault(w, errTransition)
		return
	}
	info := r.player.Info(ctx)
	if info.Duration > 0 && target > info.Duration+1 {
		writeSOAPFault(w, errSeekTarget)
		return
	}
	if err := r.player.Seek(ctx, target); err != nil {
		writeSOAPFault(w, kernelFault(err))
		return
	}
	r.emitTransportLocked(ctx)
	writeSOAPResponse(w, avTransportService, "Seek", nil)
}

// ---------------------------------------------------------------------------
// RenderingControl
// ---------------------------------------------------------------------------

func (r *Renderer) handleRenderingControl(w http.ResponseWriter, request *http.Request, action string, args map[string]string) {
	if !validInstanceID(args) {
		writeSOAPFault(w, errInvalidInstanceID)
		return
	}
	if channel := strings.TrimSpace(args["Channel"]); channel != "" && !strings.EqualFold(channel, "Master") {
		writeSOAPFault(w, errInvalidArgs)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	switch action {
	case "GetVolume":
		volume, _ := r.player.Volume()
		if volume < 0 {
			volume = 0
		}
		writeSOAPResponse(w, renderingControlService, action, []soapArgument{
			{Name: "CurrentVolume", Value: strconv.Itoa(volume)},
		})
	case "SetVolume":
		desired, err := strconv.Atoi(strings.TrimSpace(args["DesiredVolume"]))
		if err != nil || desired < 0 || desired > 100 {
			writeSOAPFault(w, errInvalidArgs)
			return
		}
		applied, err := r.player.SetVolume(ctx, desired)
		if err != nil {
			writeSOAPFault(w, kernelFault(err))
			return
		}
		r.emitRenderingLocked()
		_ = applied
		writeSOAPResponse(w, renderingControlService, action, nil)
	case "GetMute":
		_, muted := r.player.Volume()
		value := "0"
		if muted {
			value = "1"
		}
		writeSOAPResponse(w, renderingControlService, action, []soapArgument{
			{Name: "CurrentMute", Value: value},
		})
	case "SetMute":
		desired := strings.TrimSpace(args["DesiredMute"])
		if desired != "0" && desired != "1" {
			writeSOAPFault(w, errInvalidArgs)
			return
		}
		if _, err := r.player.SetMute(ctx, desired == "1"); err != nil {
			writeSOAPFault(w, kernelFault(err))
			return
		}
		r.emitRenderingLocked()
		writeSOAPResponse(w, renderingControlService, action, nil)
	case "ListPresets":
		writeSOAPResponse(w, renderingControlService, action, []soapArgument{
			{Name: "CurrentPresetNameList", Value: "FactoryDefaults"},
		})
	case "SelectPreset":
		if !strings.EqualFold(strings.TrimSpace(args["PresetName"]), "FactoryDefaults") {
			writeSOAPFault(w, errInvalidArgs)
			return
		}
		writeSOAPResponse(w, renderingControlService, action, nil)
	default:
		writeSOAPFault(w, errInvalidAction)
	}
}

// ---------------------------------------------------------------------------
// ConnectionManager
// ---------------------------------------------------------------------------

func (r *Renderer) handleConnectionManager(w http.ResponseWriter, request *http.Request, action string, args map[string]string) {
	switch action {
	case "GetProtocolInfo":
		writeSOAPResponse(w, "urn:schemas-upnp-org:service:ConnectionManager:1", action, []soapArgument{
			{Name: "Source", Value: ""},
			{Name: "Sink", Value: sinkProtocolInfo},
		})
	case "GetCurrentConnectionIDs":
		writeSOAPResponse(w, "urn:schemas-upnp-org:service:ConnectionManager:1", action, []soapArgument{
			{Name: "ConnectionIDs", Value: "0"},
		})
	case "GetCurrentConnectionInfo":
		if id := strings.TrimSpace(args["ConnectionID"]); id != "" && id != "0" {
			writeSOAPFault(w, errConnectionRef)
			return
		}
		writeSOAPResponse(w, "urn:schemas-upnp-org:service:ConnectionManager:1", action, []soapArgument{
			{Name: "RcsID", Value: "0"},
			{Name: "AVTransportID", Value: "0"},
			{Name: "ProtocolInfo", Value: "http-get:*:*:*"},
			{Name: "PeerConnectionManager", Value: ""},
			{Name: "PeerConnectionID", Value: "-1"},
			{Name: "Direction", Value: "Input"},
			{Name: "Status", Value: "OK"},
		})
	default:
		writeSOAPFault(w, errInvalidAction)
	}
}

// ---------------------------------------------------------------------------
// State machine, eventing and lifecycle
// ---------------------------------------------------------------------------

// transportProperties renders the full AVTransport LastChange property set.
// The renderer is a single-stream device, so the full set is small enough to
// send on every change.
func (r *Renderer) transportProperties(ctx context.Context) []soapArgument {
	info := r.player.Info(ctx)
	tracks := "0"
	if r.currentURI != "" {
		tracks = "1"
	}
	duration := formatUPnPTime(info.Duration)
	relTime := "0:00:00"
	if r.currentURI != "" {
		relTime = formatUPnPTime(info.Elapsed)
	}
	actions := r.currentTransportActionsLocked()
	return []soapArgument{
		{Name: "TransportState", Value: deriveTransportState(info, r.currentURI != "")},
		{Name: "TransportStatus", Value: "OK"},
		{Name: "TransportPlaySpeed", Value: "1"},
		{Name: "PlaybackStorageMedium", Value: "NETWORK"},
		{Name: "NumberOfTracks", Value: tracks},
		{Name: "CurrentTrack", Value: tracks},
		{Name: "CurrentTrackDuration", Value: duration},
		{Name: "CurrentMediaDuration", Value: duration},
		{Name: "CurrentTrackURI", Value: r.currentURI},
		{Name: "CurrentTrackMetaData", Value: r.currentURIMetaData},
		{Name: "AVTransportURI", Value: r.currentURI},
		{Name: "AVTransportURIMetaData", Value: r.currentURIMetaData},
		{Name: "RelTime", Value: relTime},
		{Name: "CurrentPlayMode", Value: "NORMAL"},
		{Name: "CurrentTransportActions", Value: actions},
	}
}

// emitTransportLocked broadcasts the AVTransport LastChange; callers must
// hold r.mu.
func (r *Renderer) emitTransportLocked(ctx context.Context) {
	properties := r.transportProperties(ctx)
	r.lastTransportState = propertyValue(properties, "TransportState")
	r.events.broadcast(lastChangePayload(avtEventFragment(properties)))
}

// emitRenderingLocked broadcasts the RenderingControl LastChange.
func (r *Renderer) emitRenderingLocked() {
	volume, muted := r.player.Volume()
	r.events.broadcast(lastChangePayload(rcsEventFragment(volume, muted)))
}

// currentTransportActionsLocked renders GetCurrentTransportActions.
func (r *Renderer) currentTransportActionsLocked() string {
	if r.currentURI == "" {
		return "Stop"
	}
	return "Play,Stop,Pause,Seek"
}

// supervise polls the kernel so natural end-of-stream still produces the
// STOPPED notification controllers rely on.
func (r *Renderer) supervise(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		info := r.player.Info(ctx)
		r.mu.Lock()
		state := deriveTransportState(info, r.currentURI != "")
		changed := state != r.lastTransportState
		var payload []byte
		if changed {
			r.lastTransportState = state
			payload = lastChangePayload(avtEventFragment(r.transportProperties(ctx)))
		}
		r.mu.Unlock()
		if changed {
			r.events.broadcast(payload)
		}
		r.events.expire()
	}
}

// Disconnect stops playback, clears the queue and metadata, pushes a final
// STOPPED event and drops every subscription. Idempotent.
func (r *Renderer) Disconnect() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	info := r.player.Info(ctx)
	wasActive := r.player.ActiveSource() == output.SourceDLNA || info.State == "play" || info.State == "pause"
	var firstErr error
	if err := r.player.Stop(ctx); err != nil {
		firstErr = err
	}
	r.player.ReleaseDLNA()
	if err := r.player.Clear(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	r.currentURI = ""
	r.currentURIMetaData = ""
	r.title, r.artist, r.album = "", "", ""
	r.lastTransportState = ""
	r.events.broadcast(lastChangePayload(avtEventFragment([]soapArgument{
		{Name: "TransportState", Value: "STOPPED"},
		{Name: "TransportStatus", Value: "OK"},
		{Name: "NumberOfTracks", Value: "0"},
		{Name: "CurrentTrack", Value: "0"},
		{Name: "AVTransportURI", Value: ""},
		{Name: "CurrentTrackURI", Value: ""},
		{Name: "CurrentTransportActions", Value: "Stop"},
	})))
	r.events.closeAll()
	r.logger.Info("dlna disconnect completed", "was_active", wasActive, "error", firstErr)
	if firstErr != nil {
		return wasActive, fmt.Errorf("dlna disconnect: %w", firstErr)
	}
	return wasActive, nil
}

// Status returns the DLNA slice of /api/status.
func (r *Renderer) Status() Status {
	r.mu.Lock()
	status := Status{
		Enabled:      true,
		FriendlyName: r.cfg.DLNAFriendlyName,
		UDN:          r.udn,
		Address:      r.address,
		CurrentURI:   r.currentURI,
		Title:        r.title,
		Artist:       r.artist,
		Album:        r.album,
	}
	hasMedia := r.currentURI != ""
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status.TransportState = deriveTransportState(r.player.Info(ctx), hasMedia)
	status.Subscribers = r.events.count()
	return status
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// deriveTransportState maps the kernel snapshot onto the UPnP transport state.
func deriveTransportState(info output.PlaybackInfo, hasMedia bool) string {
	switch info.State {
	case "play":
		return "PLAYING"
	case "pause":
		return "PAUSED_PLAYBACK"
	}
	if !hasMedia {
		return "NO_MEDIA_PRESENT"
	}
	return "STOPPED"
}

func propertyValue(properties []soapArgument, name string) string {
	for _, property := range properties {
		if property.Name == name {
			return property.Value
		}
	}
	return ""
}

func validInstanceID(args map[string]string) bool {
	value := strings.TrimSpace(args["InstanceID"])
	if value == "" {
		return true // absent values are tolerated for controller compatibility
	}
	id, err := strconv.Atoi(value)
	return err == nil && id == 0
}

// kernelFault maps a playback error onto the closest UPnP fault.
func kernelFault(err error) *upnpError {
	switch {
	case err == nil:
		return errActionFailed
	case errors.Is(err, output.ErrSourceBusy), mpd.IsDeviceBusy(err):
		return errDeviceBusy
	default:
		return &upnpError{Code: 501, Description: "Action Failed: " + err.Error()}
	}
}
