package miplay

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
)

// ErrDeviceBusy is returned when another source currently owns the sound
// card, mirroring the 409 device_busy semantics of the REST API.
var ErrDeviceBusy = errors.New("sound card is busy")

// controlIdleTimeout bounds how long an authenticated control session may go
// completely silent before it is reaped. Phones heartbeat every few seconds
// for the whole life of a session, including while the user browses the 妙播
// panel without casting, so a minute of silence means the peer is gone even
// when the TCP connection was never closed.
const controlIdleTimeout = 60 * time.Second

// Hooks connect the receiver to the sound-card arbitration engine.
type Hooks struct {
	// Claim asks the arbitration engine for the exclusive realtime output slot.
	// Returning ErrDeviceBusy surfaces to the phone as a failed cast attempt.
	//
	// req.Evict is how the engine preempts this sender later, when a newer
	// sender wants the card: it must tear this cast down and return only once
	// the ALSA device is genuinely free. req.Peer identifies the sender so a
	// re-claim by the same phone never evicts itself.
	Claim func(ctx context.Context, req ClaimRequest) error
	// Release frees the realtime output slot for the given session. Must be
	// idempotent, and must not free a slot now held by a different session.
	Release func(peer, token string)
	// SetVolume mirrors the phone's SET_VOLUME onto the hardware mixer.
	SetVolume func(percent int)
}

// ClaimRequest carries the identity of one cast attempt plus the teardown the
// engine needs to preempt it.
type ClaimRequest struct {
	// Peer is the casting phone. A reconnect from the same peer is not a
	// newcomer and must not evict the session it is replacing.
	Peer string
	// Token identifies this session instance, so a superseded session unwinding
	// late cannot free the slot its successor holds.
	Token string
	// Evict stops this cast and returns only after the output device is free.
	Evict func(ctx context.Context) error
}

// SessionSummary is the post-mortem of the most recent control session, kept
// for /api/status and FAQ debugging.
type SessionSummary struct {
	Peer string `json:"peer"`
	// Volume 是本会话最后生效的协议音量。会话结束后 Status 仍回落到
	// cfg.DefaultVolume，手机重开时会看到「音量被重置」——记住它才能延续。
	Volume        int                `json:"volume"`
	StartedAt     time.Time          `json:"started_at"`
	EndedAt       time.Time          `json:"ended_at"`
	Authenticated bool               `json:"authenticated"`
	ControlFrames int                `json:"control_frames"`
	Safety        *SafetyDiagnostics `json:"safety,omitempty"`
	RTSPReady     bool               `json:"rtsp_ready"`
	WFDUnstable   int                `json:"wfd_stability_events"`
	MediaFrames   int64              `json:"media_frames"`
	MediaBytes    int64              `json:"media_bytes"`
	Error         string             `json:"error,omitempty"`
	Trace         []TraceEntry       `json:"trace,omitempty"`
}

// Status is the MiPlay slice of /api/status.
type Status struct {
	Enabled        bool              `json:"enabled"`
	Advertising    bool              `json:"advertising"`
	ListenPort     int               `json:"listen_port"`
	DeviceID       string            `json:"device_id"`
	Connected      bool              `json:"connected"`
	ConnectedSince time.Time         `json:"connected_since,omitempty"`
	Peer           string            `json:"peer,omitempty"`
	SourceName     string            `json:"source_name,omitempty"`
	MediaInfo      map[string]string `json:"media_info,omitempty"`
	// DurationSeconds / CoverURL / Position feed the console's "now playing"
	// strip. They come from the phone's metadata, except PositionSeconds, which
	// is a LOCAL estimate — see PositionSource. The protocol lets the phone ask
	// the receiver for a position (0x0010 inbound), never the other way round,
	// so the bridge has no way to read a real playhead out of a MiPlay cast.
	DurationSeconds float64            `json:"duration_seconds,omitempty"`
	CoverURL        string             `json:"cover_url,omitempty"`
	PositionSeconds float64            `json:"position_seconds"`
	PositionSource  string             `json:"position_source"`
	Volume          int                `json:"volume"`
	State           string             `json:"state"`
	Paused          bool               `json:"paused,omitempty"`
	MediaFrames     int64              `json:"media_frames"`
	MediaBytes      int64              `json:"media_bytes"`
	Safety          *SafetyDiagnostics `json:"safety,omitempty"`
	Pipeline        PipelineStats      `json:"pipeline"`
	LastSession     *SessionSummary    `json:"last_session,omitempty"`
}

// PositionSource values reported by Status.PositionSource.
const (
	// PositionSourceNone means the bridge has no playhead to show.
	PositionSourceNone = "none"
	// PositionSourceLocalEstimate means the playhead is a local clock started
	// from the first media frame — it is NOT a value the phone reported. The
	// protocol only lets the phone query the receiver for a position, so a
	// MiPlay cast has no readable playhead; the console labels this estimate
	// rather than passing it off as measured.
	PositionSourceLocalEstimate = "local_estimate"
)

// Receiver is the assembled MiPlay receiver.
type Receiver struct {
	cfg    config.Config
	logger *slog.Logger
	hooks  Hooks

	pipeline  *Pipeline
	responder *Responder

	mu sync.Mutex
	// claimMu serialises cast claims so two phones racing for the exclusive
	// output device cannot both believe they won.
	claimMu     sync.Mutex
	listener    net.Listener
	active      *sessionRuntime
	lastSession *SessionSummary
	lastVolume  int // 最近一次生效的协议音量（-1 = 尚未记录），供会话结束后回显
	startedAt   time.Time
	closed      bool
}

// sessionSeq hands out the per-session tokens. A token must be unique per
// session instance, not just per peer: the same phone reconnecting produces a
// new session that has to be distinguishable from the one it replaces.
var sessionSeq atomic.Uint64

func nextSessionToken(peer string) string {
	return fmt.Sprintf("%s#%d", peer, sessionSeq.Add(1))
}

// sessionRuntime tracks the single active control session. All mutable
// fields are guarded by mu because the control loop, the WFD goroutine and
// HTTP status readers touch it concurrently.
type sessionRuntime struct {
	mu sync.Mutex

	peer      string
	startedAt time.Time
	// token identifies this session instance (not just the peer). It is the
	// pipeline ownership key, so a superseded session can neither stop nor
	// adopt the pipeline that replaced it.
	token   string
	session *LegacyReceiverSession
	conn    net.Conn
	claimed bool
	// retired is set the moment this session enters teardown. Its read loop can
	// still be parked on a frame, and that frame must not restart the pipeline
	// (see ensurePipeline): a pipeline nobody feeds holds the exclusive device
	// open and silences every later cast.
	retired atomic.Bool

	cancelWFD context.CancelFunc
	paused    bool
	// Playhead bookkeeping for the console. mediaStartedAt is set from the
	// first media frame; the pause ledger freezes it while the phone says it is
	// paused; trackVersion restarts it when the phone moves to the next song.
	mediaStartedAt time.Time
	pausedAt       time.Time
	pausedTotal    time.Duration
	trackVersion   int

	controlFrames int
	rtspReady     bool
	wfdEvents     int
	mediaFrames   int64
	mediaBytes    int64
	lastError     string
}

// NewReceiver assembles the receiver; call Start to bring it online.
func NewReceiver(cfg config.Config, logger *slog.Logger, hooks Hooks) *Receiver {
	if logger == nil {
		logger = slog.Default()
	}
	return &Receiver{
		cfg:      cfg,
		logger:   logger,
		hooks:    hooks,
		pipeline: NewPipeline(cfg, logger),
		responder: NewResponder(ResponderConfig{
			FriendlyName: cfg.MiPlayName,
			ControlPort:  cfg.MiPlayControlPort,
			DeviceIDFile: cfg.MiPlayDeviceIDFile(),
		}, logger),
		lastVolume: -1,
	}
}

// Start resolves the advertise address, opens the control port and starts
// mDNS advertising. It returns once the control server accepts connections.
func (r *Receiver) Start(ctx context.Context) error {
	advertiseAddress := r.cfg.MiPlayAdvertiseAddress
	if advertiseAddress == "" {
		detected, err := DetectAdvertiseAddress()
		if err != nil {
			return fmt.Errorf("resolve the LAN advertise address (set MIPLAY_ADVERTISE_ADDRESS to override): %w", err)
		}
		advertiseAddress = detected
	}
	r.cfg.MiPlayAdvertiseAddress = advertiseAddress

	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", r.cfg.MiPlayControlPort))
	if err != nil {
		return fmt.Errorf("listen on the MiPlay control port %d: %w", r.cfg.MiPlayControlPort, err)
	}
	r.mu.Lock()
	r.listener = listener
	r.startedAt = time.Now().UTC()
	r.mu.Unlock()

	responderErr := make(chan error, 1)
	go func() { responderErr <- r.responder.Start(ctx, advertiseAddress) }()
	readyDeadline := time.Now().Add(5 * time.Second)
	for !r.responder.Advertising() {
		select {
		case err := <-responderErr:
			listener.Close()
			return fmt.Errorf("start the MiPlay mDNS responder: %w", err)
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(readyDeadline) {
			listener.Close()
			return fmt.Errorf("miplay mDNS responder did not become ready within 5s")
		}
	}

	r.logger.Info("miplay receiver online",
		"control_port", r.cfg.MiPlayControlPort,
		"advertise_address", advertiseAddress,
		"name", r.cfg.MiPlayName,
	)

	go r.acceptLoop(ctx)
	go func() {
		if err := <-responderErr; err != nil && ctx.Err() == nil {
			r.logger.Error("miplay mDNS responder stopped", "error", err)
		}
	}()
	return nil
}

// acceptLoop serves control connections until the context ends.
func (r *Receiver) acceptLoop(ctx context.Context) {
	for {
		connection, err := r.listener.Accept()
		if err != nil {
			r.mu.Lock()
			closed := r.closed
			r.mu.Unlock()
			if closed || ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			r.logger.Warn("miplay accept error", "error", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		go r.handleControl(ctx, connection)
	}
}

// handleControl runs one control connection end to end.
func (r *Receiver) handleControl(ctx context.Context, connection net.Conn) {
	peerHost, peerPort := endpointOf(connection.RemoteAddr())
	localHost, localPort := endpointOf(connection.LocalAddr())
	report := &sessionRuntime{
		peer:         peerHost,
		startedAt:    time.Now().UTC(),
		trackVersion: -1,
		token:        nextSessionToken(peerHost),
	}
	r.logger.Info("miplay control connection accepted", "peer", peerHost, "local_port", localPort)

	challenge, err := GenerateLegacyChallenge()
	if err != nil {
		r.logger.Error("miplay cannot generate the challenge", "error", err)
		connection.Close()
		return
	}
	// 新会话的初始音量取「上次生效值」而不是启动默认值：手机在会话建立后会先
	// GetVolume 查询，用启动默认值（100）应答会让它的滑块每次都跳回满格。
	sessionVolume := r.lastVolumeOrDefault(r.cfg.DefaultVolume)
	session, err := NewLegacyReceiverSession(challenge, 0, r.cfg.MiPlayName, sessionVolume,
		localHost, localPort, peerHost, peerPort)
	if session != nil {
		session.onWarn = func(msg string) { r.logger.Warn(msg) }
	}
	if err != nil {
		r.logger.Error("miplay cannot create the session", "error", err)
		connection.Close()
		return
	}
	report.session = session
	report.conn = connection

	var sessionCtx context.Context
	sessionCtx, report.cancelWFD = context.WithCancel(ctx)
	defer report.cancelWFD()
	defer connection.Close()

	writer := &controlWriter{conn: connection}
	if err := session.Start(writer.write); err != nil {
		r.logger.Warn("miplay cannot send the challenge", "error", err, "peer", peerHost)
		return
	}

	// The fixed deadline guards only the unauthenticated window, matching the
	// MIPLAY_HANDSHAKE_TIMEOUT_SECONDS contract. Once the legacy challenge is
	// answered the phone owns the session: it heartbeats for minutes while the
	// user pre-loads playback, so the deadline becomes an idle guard that every
	// inbound frame refreshes instead of a hard cut-off.
	handshakeDeadline := time.Now().Add(r.cfg.MiPlayHandshakeTimeout)
	decoder := NewFrameDecoder()
	buffer := make([]byte, 16*1024)

	for {
		if session.Authenticated() {
			_ = connection.SetReadDeadline(time.Now().Add(controlIdleTimeout))
		} else {
			_ = connection.SetReadDeadline(handshakeDeadline)
		}
		length, readErr := connection.Read(buffer)
		if readErr != nil {
			if isTimeout(readErr) {
				if session.Authenticated() {
					report.setError("control idle timeout")
				} else {
					report.setError("handshake timed out")
				}
			} else if sessionCtx.Err() == nil {
				report.setError(fmt.Sprintf("control read: %v", readErr))
			}
			break
		}
		frames, feedErr := decoder.Feed(buffer[:length])
		if feedErr != nil {
			report.setError(feedErr.Error())
			break
		}
		stop := false
		for _, frame := range frames {
			report.bumpControlFrames()
			// 取证：确认音量/播放命令究竟走哪条连接（10-04 真机排查）。
			r.logger.Debug("miplay command frame", "command", frame.Command,
				"seq", frame.Sequence, "payload", len(frame.Payload),
				"payload_hex", hex.EncodeToString(truncateBytes(frame.Payload, 32)))
			result, stepErr := session.Step(frame, writer.write)
			if stepErr != nil {
				report.setError(stepErr.Error())
				stop = true
				break
			}
			if !result.Accepted {
				report.setError(result.Reason)
				stop = true
				break
			}
			if result.VolumeSet {
				// 立刻记住，不等会话结束：进程随时可能被重启，等 summary 才写就丢了。
				r.mu.Lock()
				r.lastVolume = result.Volume
				r.mu.Unlock()
				if r.hooks.SetVolume != nil {
					// The protocol volume is authoritative for the current
					// source; mirror it onto the hardware mixer.
					r.hooks.SetVolume(result.Volume)
				}
			}
			if result.PauseRequested {
				// A source-side pause is only a state hint: the reference
				// receiver keeps accepting and playing media, and the phone may
				// resume its stream at any time without sending Resume. The
				// realtime pipeline is deliberately left running.
				report.setPaused(true)
			}
			if result.ResumeRequested {
				report.setPaused(false)
			}
			if result.CloseRequested {
				report.setError("source closed the session")
				stop = true
				break
			}
			if result.OpenRequest != nil {
				if !r.beginSession(sessionCtx, report) {
					report.setError("cast could not take the output device")
					stop = true
					break
				}
				go func(request OpenDeviceRequest) {
					if wfdErr := r.runWFD(sessionCtx, request, session, writer, report); wfdErr != nil && sessionCtx.Err() == nil {
						r.logger.Warn("miplay WFD session ended", "error", wfdErr, "peer", peerHost)
						report.setError(wfdErr.Error())
					}
					// A dead WFD leg ends the whole control session: the
					// phone is gone, whatever the control socket thinks.
					// Closing the connection wakes the blocked control read
					// deterministically; a read-deadline poke could be
					// overwritten by the loop's own idle-deadline refresh.
					_ = connection.Close()
				}(*result.OpenRequest)
			}
		}
		if stop {
			break
		}
	}

	r.endSession(report)
	summary := report.summary(session)
	r.mu.Lock()
	r.lastSession = summary
	if summary != nil {
		r.lastVolume = summary.Volume
	}
	r.mu.Unlock()
	r.logger.Info("miplay control session finished",
		"peer", peerHost,
		"authenticated", summary.Authenticated,
		"control_frames", summary.ControlFrames,
		"media_frames", summary.MediaFrames,
		"media_bytes", summary.MediaBytes,
		"rtsp_ready", summary.RTSPReady,
		"error", summary.Error,
	)
}

// beginSession reserves the exclusive output slot for one sender.
//
// A newer sender preempts the current one: the arbitration engine calls back
// into evictActiveCast, which returns only after the previous aplay has been
// reaped. Rejecting instead of preempting would leave two half-dead streams
// racing for one exclusive device, which is what the live box was doing.
func (r *Receiver) beginSession(ctx context.Context, report *sessionRuntime) bool {
	if r.hooks.Claim == nil {
		// No arbitration engine (unit tests build the receiver bare): keep the
		// receiver-local "one session at a time" rule.
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.active != nil {
			return false
		}
		r.active = report
		return true
	}

	// Claims are serialised: two phones racing for the card must not both
	// believe they won, and a loser must be torn down before the winner starts.
	r.claimMu.Lock()
	defer r.claimMu.Unlock()

	req := ClaimRequest{Peer: report.peer, Token: report.token, Evict: r.evictActiveCast}
	if err := r.hooks.Claim(ctx, req); err != nil {
		r.logger.Warn("miplay cast rejected", "peer", report.peer, "error", err)
		return false
	}
	r.mu.Lock()
	previous := r.active
	r.active = report
	report.claimed = true
	r.mu.Unlock()

	if previous != nil && previous != report {
		// Same phone reconnecting: the engine keeps the slot (same peer, so it
		// is not a newcomer), but the previous session's carcass still owns the
		// pipeline. Collect it, or the new session cannot open the device.
		r.logger.Info("replacing the previous session from the same peer",
			"peer", previous.peer, "token", previous.token)
		r.stopCast(previous)
		r.waitForSessionExit(context.Background(), previous, 3*time.Second)
	}
	return true
}

// evictActiveCast yields the output device to a newer sender. It is handed to
// the arbitration engine as the current cast's teardown and only returns once
// the pipeline is gone, so the winner can open hw:0,0 without a
// "Resource busy" race.
func (r *Receiver) evictActiveCast(ctx context.Context) error {
	r.mu.Lock()
	report := r.active
	r.mu.Unlock()

	if report == nil {
		// A pipeline outlived its session (crash path): clear it so the winner
		// gets a free device.
		r.pipeline.Stop()
		return nil
	}

	r.logger.Info("miplay cast preempted by a newer sender", "peer", report.peer, "token", report.token)
	r.stopCast(report)
	return r.waitForSessionExit(ctx, report, 5*time.Second)
}

// markRetired records that this session is being torn down. It is called before
// the pipeline is stopped, so that a frame arriving afterwards is dropped
// instead of restarting ffmpeg/aplay for a session that no longer exists.
func (s *sessionRuntime) markRetired() {
	s.retired.Store(true)
}

// isRetired reports whether this session has entered teardown.
func (s *sessionRuntime) isRetired() bool {
	return s.retired.Load()
}

// stopCast ends one session's media path and blocks until its ffmpeg/aplay are
// reaped (StopOwned has a bounded grace period inside).
func (r *Receiver) stopCast(report *sessionRuntime) {
	if report == nil {
		return
	}
	report.markRetired()
	if report.conn != nil {
		// Closing wakes the blocked control read deterministically.
		_ = report.conn.SetReadDeadline(time.Now())
		_ = report.conn.Close()
	}
	if report.cancelWFD != nil {
		report.cancelWFD()
	}
	r.pipeline.StopOwned(report.token)
}

// waitForSessionExit blocks until report is no longer the active session: the
// session goroutine still has to unwind (clear r.active, release its slot).
func (r *Receiver) waitForSessionExit(ctx context.Context, report *sessionRuntime, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		stillActive := r.active == report
		r.mu.Unlock()
		if !stillActive {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	r.logger.Warn("cast did not unwind in time", "peer", report.peer, "token", report.token)
	return nil
}

// endSession tears down all session resources and frees the output slot.
func (r *Receiver) endSession(report *sessionRuntime) {
	// Retire before stopping: the read loop may be parked mid-frame and must not
	// bring the pipeline back up once this session is gone.
	report.markRetired()
	// Ownership-checked: a session that never started the pipeline (for example
	// one turned away while another cast was live) must not stop it.
	r.pipeline.StopOwned(report.token)
	if report.cancelWFD != nil {
		report.cancelWFD()
	}
	r.mu.Lock()
	if r.active == report {
		r.active = nil
	}
	claimed := report.claimed
	report.claimed = false
	peer := report.peer
	token := report.token
	r.mu.Unlock()
	if claimed && r.hooks.Release != nil {
		r.hooks.Release(peer, token)
	}
}

// runWFD dials the three source-side connections, drives the RTSP control
// ladder and pumps media frames into the realtime pipeline.
func (r *Receiver) runWFD(ctx context.Context, request OpenDeviceRequest, session *LegacyReceiverSession,
	writer *controlWriter, report *sessionRuntime) error {
	target := net.JoinHostPort(request.Host, fmt.Sprintf("%d", request.Port))
	dialer := net.Dialer{Timeout: 5 * time.Second}

	connections := make([]net.Conn, 0, 3)
	defer func() {
		for _, connection := range connections {
			_ = connection.Close()
		}
	}()
	for index := 0; index < 3; index++ {
		connection, err := dialer.DialContext(ctx, "tcp", target)
		if err != nil {
			return fmt.Errorf("dial WFD endpoint %s (%d/3): %w", target, index+1, err)
		}
		connections = append(connections, connection)
	}
	rtspConnection := connections[0]
	auxConnection := connections[1]
	mediaConnection := connections[2]
	if tcpConnection, ok := auxConnection.(*net.TCPConn); ok {
		// The aux leg stays open but idle for the lifetime of the cast;
		// keep its socket alive so half-open detection still notices a
		// vanished phone.
		_ = tcpConnection.SetKeepAlive(true)
	}

	rtspReadyCh := make(chan struct{}, 1)
	rtspDone := make(chan error, 1)
	rtspCtx, cancelRTSP := context.WithCancel(ctx)
	defer cancelRTSP()
	go func() {
		rtspDone <- r.runRTSPControl(rtspCtx, rtspConnection, request.Host, rtspReadyCh, report, session, writer)
	}()
	select {
	case <-rtspReadyCh:
		// Ladder completed; keepalives continue in the goroutine above.
		go func() {
			if err := <-rtspDone; err != nil && rtspCtx.Err() == nil {
				r.logger.Warn("miplay RTSP control leg ended", "error", err)
			}
		}()
	case err := <-rtspDone:
		return fmt.Errorf("WFD RTSP handshake: %w", err)
	case <-time.After(8 * time.Second):
		return fmt.Errorf("WFD RTSP handshake did not complete within 8s")
	case <-ctx.Done():
		return ctx.Err()
	}
	report.setRTSPReady(true)
	r.logger.Info("miplay WFD RTSP session ready", "source", target)

	mediaDecoder := NewMediaFrameDecoder()
	mediaBuffer := make([]byte, 32*1024)
	firstPayload := true
	for {
		if ctx.Err() != nil {
			return nil
		}
		_ = mediaConnection.SetReadDeadline(time.Now().Add(30 * time.Second))
		length, err := mediaConnection.Read(mediaBuffer)
		if err != nil {
			if isTimeout(err) {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("media read: %w", err)
		}
		frames, err := mediaDecoder.Feed(mediaBuffer[:length])
		if err != nil {
			return fmt.Errorf("media framing: %w", err)
		}
		for _, frame := range frames {
			if err := r.pumpMediaFrame(frame, report, session, writer, &firstPayload); err != nil {
				if errors.Is(err, errSessionOver) {
					return nil // 会话已被取代：干净收尾，不是故障
				}
				return err
			}
		}
	}
}

// ensurePipeline makes the realtime pipeline live and owned by this session.
//
// Two rules, and the live 2026-10-05 outage was a violation of both:
//
//   - Only the session that currently holds the card may own the pipeline, and a
//     session that has entered teardown may never touch it again. Otherwise a
//     superseded session's late frame keeps a writer alive on the exclusive
//     device.
//   - The pipeline is *reclaimed*, not merely started. A pipeline left behind by
//     a session that is already gone has to be collected, because reporting it
//     as busy is what made every frame of the next cast fail and left the owner
//     with no sound at all.
func (r *Receiver) ensurePipeline(report *sessionRuntime) error {
	if report.isRetired() {
		return errSessionOver
	}
	r.mu.Lock()
	active := r.active == report
	r.mu.Unlock()
	if !active {
		return errSessionOver
	}

	replaced, err := r.pipeline.Reclaim(report.token)
	if replaced != "" {
		r.logger.Warn("reclaimed the realtime pipeline from a session that already ended",
			"token", report.token, "stale_owner", replaced)
	}
	return err
}

// pumpMediaFrame decodes one wire media frame (RTP/MPEG-TS payload) into the
// realtime pipeline. Both the dedicated media leg and the RTSP control leg (where
// MiUI interleaves media frames) funnel through here so the first-payload
// bookkeeping and the media-started notification happen exactly once per session.
func (r *Receiver) pumpMediaFrame(frame []byte, report *sessionRuntime, session *LegacyReceiverSession,
	writer *controlWriter, firstPayload *bool) error {
	packet, err := DecodeRTPMPEGTS(frame)
	if err != nil {
		return fmt.Errorf("media packet: %w", err)
	}
	// Owner-aware and self-healing: a session that finds a pipeline belonging to
	// somebody else must not feed a second writer, but the session holding the
	// card collects the leftover instead of failing every frame.
	if startErr := r.ensurePipeline(report); startErr != nil {
		return startErr
	}
	if err := r.pipeline.Write(packet.TransportStream); err != nil {
		return err
	}
	report.addMedia(int64(len(packet.TransportStream)))
	// Media flowing again proves the source is playing: drop any stale
	// pause hint left by an earlier source-side pause command.
	report.clearPause()
	// Playhead bookkeeping for the console: stamp the first frame of the
	// session and restart whenever the phone reports a different track.
	report.markMediaStart()
	report.noteTrack(session.TrackVersion())
	if *firstPayload {
		*firstPayload = false
		r.logger.Info("miplay first media payload decoded", "ts_bytes", len(packet.TransportStream))
		if notifyErr := session.MediaStarted(writer.write); notifyErr != nil {
			r.logger.Warn("miplay media-started notification failed", "error", notifyErr)
		}
	}
	return nil
}

// decodeScalarVolume reads a bare four-byte big-endian volume, rejecting values
// outside 0..100 so a malformed frame is ignored instead of clamping the mixer.
func decodeScalarVolume(payload []byte) (int, bool) {
	if len(payload) != 4 {
		return 0, false
	}
	volume := int(binary.BigEndian.Uint32(payload))
	if volume < 0 || volume > 100 {
		return 0, false
	}
	return volume, true
}

// decryptVolume unwraps a SafetyData-wrapped SetVolume and returns the percentage.
// It never returns an error to the caller: an IV that has not settled yet is a
// transient condition, and tearing the session down over it is far worse than
// skipping one command.
func (s *LegacyReceiverSession) decryptVolume(payload []byte) (int, bool) {
	plain, err := s.safety.decryptEnvelope(payload, false)
	if err != nil {
		return 0, false
	}
	if volume, ok := decodeScalarVolume(plain); ok {
		return volume, true
	}
	// 信封内可能还包了一层 Safety envelope，解开后重试。
	if inner, innerErr := decodeSafetyEnvelope(plain, boolPointer(false)); innerErr == nil {
		if volume, ok := decodeScalarVolume(inner); ok {
			return volume, true
		}
	}
	return 0, false
}

// loggerWarn routes a warning through the session logger when the receiver wired one.
func (s *LegacyReceiverSession) loggerWarn(msg string) {
	if s.onWarn != nil {
		s.onWarn(msg)
	}
}

// runRTSPControl drives the RTSP ladder on the first source connection and
// signals rtspReadyCh once the session reaches READY.
func (r *Receiver) runRTSPControl(ctx context.Context, connection net.Conn, sourceAddress string,
	rtspReadyCh chan<- struct{}, report *sessionRuntime, session *LegacyReceiverSession,
	writer *controlWriter) error {
	// MiUI 会把媒体帧（0x24 帧头）混写在这条 RTSP 控制连接上，实测（10-04）：
	// 接收端按连接序号假定媒体走 connections[2]，但真机并没有那样分。所以这里
	// 把控制腿上剥下来的媒体帧直接喂给播放管线，否则连接虽然活着却全程无声。
	firstPayload := true // 控制腿也有媒体帧，首帧通知必须触发（与媒体腿一致）
	var mediaMu sync.Mutex
	mediaOver := false
	rtspSession := NewReceiverRtspSession(sourceAddress)
	decoder := NewRTSPDecoder()
	decoder.SetMediaSink(func(frame []byte) {
		mediaMu.Lock()
		defer mediaMu.Unlock()
		if mediaOver {
			return
		}
		if err := r.pumpMediaFrame(frame, report, session, writer, &firstPayload); err != nil {
			if errors.Is(err, errSessionOver) {
				// 本会话已被取代/已断开：此后静默丢帧。真机 14:01 那条每秒几十条
				// 的 "control-leg media frame failed" 刷屏就出在这里。
				mediaOver = true
				return
			}
			r.logger.Warn("miplay control-leg media frame failed", "error", err)
		}
	})
	buffer := make([]byte, 16*1024)
	for {
		if ctx.Err() != nil {
			return nil
		}
		_ = connection.SetReadDeadline(time.Now().Add(30 * time.Second))
		length, err := connection.Read(buffer)
		if err != nil {
			if isTimeout(err) {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("RTSP read: %w", err)
		}
		messages, err := decoder.Feed(buffer[:length])
		if err != nil {
			return err
		}
		for _, message := range messages {
			// 取证：RTSP 控制腿上真正跑通的是这条连接，手机的音量/播放命令很可能
			// 也走这里。记录首行与头名，便于核对协议形状（10-04 真机排查用）。
			r.logger.Debug("miplay rtsp message", "start", message.StartLine,
				"headers", len(message.Headers), "body", len(message.Body))
			// 取证：元数据可能藏在 RTSP 头（自定义 header）或 body 里，打出来核对。
			if len(message.Body) > 0 {
				r.logger.Debug("miplay rtsp body", "hex",
					hex.EncodeToString(truncateBytes(message.Body, 48)),
					"text", truncateForLog(string(message.Body), 200))
			}
			for _, h := range message.Headers {
				if !isStandardRTSPHeader(h.Name) {
					r.logger.Debug("miplay rtsp custom header", "name", h.Name,
						"value", truncateForLog(h.Value, 200))
				}
			}
			transition, err := rtspSession.Process(message)
			if err != nil {
				return err
			}
			if len(transition.Writes) > 0 {
				_ = connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
				for _, write := range transition.Writes {
					if _, err := connection.Write(write); err != nil {
						return fmt.Errorf("RTSP write: %w", err)
					}
				}
			}
			if transition.Ready {
				select {
				case rtspReadyCh <- struct{}{}:
				default:
				}
			}
		}
	}
}

// Disconnect tears down the active cast. It is idempotent and safe to call
// concurrently.
func (r *Receiver) Disconnect() (bool, error) {
	r.mu.Lock()
	report := r.active
	r.mu.Unlock()
	if report == nil {
		return false, nil
	}
	r.logger.Info("miplay disconnect requested", "peer", report.peer)
	// Operator-initiated: this one may force the device free even if the
	// pipeline is not the session's own.
	r.stopCast(report)
	r.pipeline.Stop()
	return true, r.waitForSessionExit(context.Background(), report, 5*time.Second)
}

// lastVolumeOrDefault reports the volume to echo when no session is active.
// Without this the phone sees DefaultVolume on every reconnect and its slider
// snaps back to 100% (or whatever the startup default is) after each cast.
func (r *Receiver) lastVolumeOrDefault(fallback int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastVolume >= 0 {
		return r.lastVolume
	}
	return fallback
}

// Status returns the MiPlay slice of /api/status.
func (r *Receiver) Status() Status {
	r.mu.Lock()
	report := r.active
	lastSession := r.lastSession
	r.mu.Unlock()

	status := Status{
		Enabled:     true,
		Advertising: r.responder.Advertising(),
		ListenPort:  r.cfg.MiPlayControlPort,
		DeviceID:    r.responder.Identity().canonicalUUID(),
		Volume:      r.lastVolumeOrDefault(r.cfg.DefaultVolume),
		State:       "idle",
		Pipeline:    r.pipeline.Stats(),
		LastSession: lastSession,
	}
	if report == nil {
		return status
	}
	status.Connected = true
	status.ConnectedSince = report.startedAt
	status.Peer = report.peer
	status.State = "connected"
	status.Paused = report.isPaused()
	if status.Paused {
		status.State = "paused"
	} else if status.Pipeline.Running {
		status.State = "streaming"
	}
	status.PositionSource = PositionSourceNone
	if report.session != nil {
		status.SourceName = report.session.SourceName()
		status.Volume = report.session.Volume()
		status.Safety = report.session.SafetyDiagnostics()
		status.MediaInfo = report.session.MediaInfo()
		status.DurationSeconds = report.session.DurationSeconds()
		status.CoverURL = report.session.CoverURL()
		status.LastSession = nil
	}
	if elapsed, measured := report.elapsed(); measured {
		status.PositionSeconds = elapsed.Seconds()
		status.PositionSource = PositionSourceLocalEstimate
	}
	status.MediaFrames, status.MediaBytes = report.mediaTotals()
	return status
}

// Shutdown stops the listener, the responder and the pipeline.
func (r *Receiver) Shutdown() {
	r.mu.Lock()
	r.closed = true
	listener := r.listener
	r.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
	_, _ = r.Disconnect()
	r.pipeline.Stop()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// controlWriter serialises control-connection writes with a deadline so a
// stalled phone cannot wedge the session lock.
type controlWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (w *controlWriter) write(frames [][]byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == nil {
		return fmt.Errorf("control connection is closed")
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	for _, frame := range frames {
		if _, err := w.conn.Write(frame); err != nil {
			return err
		}
	}
	return nil
}

func endpointOf(address net.Addr) (string, int) {
	if address == nil {
		return "", 0
	}
	if udpAddress, ok := address.(*net.TCPAddr); ok {
		return udpAddress.IP.String(), udpAddress.Port
	}
	host, portText, err := net.SplitHostPort(address.String())
	if err != nil {
		return address.String(), 0
	}
	port := 0
	fmt.Sscanf(portText, "%d", &port)
	return host, port
}

func isTimeout(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

// session runtime mutators (see sessionRuntime). Every accessor locks the
// session mutex: the control loop, the WFD goroutine and HTTP status readers
// touch these fields concurrently.

func (s *sessionRuntime) setError(message string) {
	s.mu.Lock()
	// Keep the first failure: follow-up errors (for example the control read
	// that fails once a dead WFD leg closed the socket) describe symptoms,
	// not the cause.
	if s.lastError == "" {
		s.lastError = message
	}
	s.mu.Unlock()
}

func (s *sessionRuntime) setRTSPReady(value bool) {
	s.mu.Lock()
	s.rtspReady = value
	s.mu.Unlock()
}

func (s *sessionRuntime) setPaused(value bool) {
	s.mu.Lock()
	s.setPausedLocked(value)
	s.mu.Unlock()
}

// setPausedLocked also maintains the pause ledger that the console's playhead
// reads: while the phone says it is paused, elapsed time must stand still.
func (s *sessionRuntime) setPausedLocked(value bool) {
	if value == s.paused {
		return
	}
	s.paused = value
	if value {
		s.pausedAt = time.Now()
		return
	}
	if !s.pausedAt.IsZero() {
		s.pausedTotal += time.Since(s.pausedAt)
		s.pausedAt = time.Time{}
	}
}

// markMediaStart stamps the first media frame of the session.
func (s *sessionRuntime) markMediaStart() {
	s.mu.Lock()
	if s.mediaStartedAt.IsZero() {
		s.mediaStartedAt = time.Now()
	}
	s.mu.Unlock()
}

// noteTrack restarts the playhead when the phone moves to another song: the
// console must not show the previous track's elapsed time under the new title.
func (s *sessionRuntime) noteTrack(version int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if version == s.trackVersion {
		return
	}
	s.trackVersion = version
	s.mediaStartedAt = time.Now()
	s.pausedTotal = 0
	s.pausedAt = time.Time{}
}

// elapsed reports the locally measured playhead of the active cast. The second
// return value is false until the first media frame has arrived, so the caller
// can say "unknown" instead of showing 0:00 as if it were measured.
func (s *sessionRuntime) elapsed() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mediaStartedAt.IsZero() {
		return 0, false
	}
	frozen := s.pausedTotal
	if !s.pausedAt.IsZero() {
		frozen += time.Since(s.pausedAt)
	}
	elapsed := time.Since(s.mediaStartedAt) - frozen
	if elapsed < 0 {
		elapsed = 0
	}
	return elapsed, true
}

func (s *sessionRuntime) bumpControlFrames() {
	s.mu.Lock()
	s.controlFrames++
	s.mu.Unlock()
}

func (s *sessionRuntime) addMedia(bytes int64) {
	s.mu.Lock()
	s.mediaFrames++
	s.mediaBytes += bytes
	s.mu.Unlock()
}

func (s *sessionRuntime) isPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

func (s *sessionRuntime) clearPause() {
	s.mu.Lock()
	s.setPausedLocked(false)
	s.mu.Unlock()
}

func (s *sessionRuntime) mediaTotals() (int64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mediaFrames, s.mediaBytes
}

func (s *sessionRuntime) summary(session *LegacyReceiverSession) *SessionSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &SessionSummary{
		Peer:          s.peer,
		Volume:        session.Volume(),
		StartedAt:     s.startedAt,
		EndedAt:       time.Now().UTC(),
		Authenticated: session.Authenticated(),
		ControlFrames: s.controlFrames,
		Safety:        session.SafetyDiagnostics(),
		RTSPReady:     s.rtspReady,
		WFDUnstable:   s.wfdEvents,
		MediaFrames:   s.mediaFrames,
		MediaBytes:    s.mediaBytes,
		Error:         s.lastError,
		Trace:         session.Trace(),
	}
}

// truncateBytes caps a payload for logging so a debug line never dumps megabytes.
func truncateBytes(data []byte, limit int) []byte {
	if len(data) <= limit {
		return data
	}
	return data[:limit]
}

// isStandardRTSPHeader reports whether a header is part of plain RTSP, so the
// debug trace only surfaces the custom ones where MiUI carries metadata.
func isStandardRTSPHeader(name string) bool {
	switch strings.ToLower(name) {
	case "cseq", "content-length", "content-type", "session", "transport",
		"range", "user-agent", "authorization", "www-authenticate", "public":
		return true
	}
	return false
}
