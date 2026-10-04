package miplay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
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
	// Claim reserves the realtime output slot. Returning ErrDeviceBusy is
	// surfaced to the phone as a failed cast attempt.
	Claim func() error
	// Release frees the realtime output slot. Must be idempotent.
	Release func()
	// SetVolume mirrors the phone's SET_VOLUME onto the hardware mixer.
	SetVolume func(percent int)
}

// SessionSummary is the post-mortem of the most recent control session, kept
// for /api/status and FAQ debugging.
type SessionSummary struct {
	Peer          string             `json:"peer"`
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
	Enabled        bool               `json:"enabled"`
	Advertising    bool               `json:"advertising"`
	ListenPort     int                `json:"listen_port"`
	DeviceID       string             `json:"device_id"`
	Connected      bool               `json:"connected"`
	ConnectedSince time.Time          `json:"connected_since,omitempty"`
	Peer           string             `json:"peer,omitempty"`
	SourceName     string             `json:"source_name,omitempty"`
	MediaInfo      map[string]string  `json:"media_info,omitempty"`
	Volume         int                `json:"volume"`
	State          string             `json:"state"`
	Paused         bool               `json:"paused,omitempty"`
	MediaFrames    int64              `json:"media_frames"`
	MediaBytes     int64              `json:"media_bytes"`
	Safety         *SafetyDiagnostics `json:"safety,omitempty"`
	Pipeline       PipelineStats      `json:"pipeline"`
	LastSession    *SessionSummary    `json:"last_session,omitempty"`
}

// Receiver is the assembled MiPlay receiver.
type Receiver struct {
	cfg    config.Config
	logger *slog.Logger
	hooks  Hooks

	pipeline  *Pipeline
	responder *Responder

	mu          sync.Mutex
	listener    net.Listener
	active      *sessionRuntime
	lastSession *SessionSummary
	startedAt   time.Time
	closed      bool
}

// sessionRuntime tracks the single active control session. All mutable
// fields are guarded by mu because the control loop, the WFD goroutine and
// HTTP status readers touch it concurrently.
type sessionRuntime struct {
	mu sync.Mutex

	peer      string
	startedAt time.Time
	session   *LegacyReceiverSession
	conn      net.Conn
	claimed   bool

	cancelWFD context.CancelFunc
	paused    bool

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
	report := &sessionRuntime{peer: peerHost, startedAt: time.Now().UTC()}
	r.logger.Info("miplay control connection accepted", "peer", peerHost, "local_port", localPort)

	challenge, err := GenerateLegacyChallenge()
	if err != nil {
		r.logger.Error("miplay cannot generate the challenge", "error", err)
		connection.Close()
		return
	}
	session, err := NewLegacyReceiverSession(challenge, 0, r.cfg.MiPlayName, r.cfg.DefaultVolume,
		localHost, localPort, peerHost, peerPort)
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
				if !r.beginSession(report) {
					report.setError("concurrent sender rejected")
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
func (r *Receiver) beginSession(report *sessionRuntime) bool {
	r.mu.Lock()
	if r.active != nil {
		r.mu.Unlock()
		return false
	}
	r.active = report
	r.mu.Unlock()

	if r.hooks.Claim != nil {
		if err := r.hooks.Claim(); err != nil {
			r.mu.Lock()
			r.active = nil
			r.mu.Unlock()
			r.logger.Warn("miplay cast rejected: the sound card is owned by another source", "error", err)
			return false
		}
	}
	r.mu.Lock()
	report.claimed = true
	r.mu.Unlock()
	return true
}

// endSession tears down all session resources and frees the output slot.
func (r *Receiver) endSession(report *sessionRuntime) {
	r.pipeline.Stop()
	if report.cancelWFD != nil {
		report.cancelWFD()
	}
	r.mu.Lock()
	if r.active == report {
		r.active = nil
	}
	claimed := report.claimed
	report.claimed = false
	r.mu.Unlock()
	if claimed && r.hooks.Release != nil {
		r.hooks.Release()
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
		rtspDone <- r.runRTSPControl(rtspCtx, rtspConnection, request.Host, rtspReadyCh)
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
			packet, decodeErr := DecodeRTPMPEGTS(frame)
			if decodeErr != nil {
				return fmt.Errorf("media packet: %w", decodeErr)
			}
			if !r.pipeline.Running() {
				if startErr := r.pipeline.Start(); startErr != nil {
					return startErr
				}
			}
			if writeErr := r.pipeline.Write(packet.TransportStream); writeErr != nil {
				return writeErr
			}
			report.addMedia(int64(len(packet.TransportStream)))
			// Media flowing again proves the source is playing: drop any stale
			// pause hint left by an earlier source-side pause command.
			report.clearPause()
			if firstPayload {
				firstPayload = false
				r.logger.Info("miplay first media payload decoded", "ts_bytes", len(packet.TransportStream))
				if notifyErr := session.MediaStarted(writer.write); notifyErr != nil {
					r.logger.Warn("miplay media-started notification failed", "error", notifyErr)
				}
			}
		}
	}
}

// runRTSPControl drives the RTSP ladder on the first source connection and
// signals rtspReadyCh once the session reaches READY.
func (r *Receiver) runRTSPControl(ctx context.Context, connection net.Conn, sourceAddress string, rtspReadyCh chan<- struct{}) error {
	session := NewReceiverRtspSession(sourceAddress)
	decoder := NewRTSPDecoder()
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
			transition, err := session.Process(message)
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
	if report.conn != nil {
		_ = report.conn.SetReadDeadline(time.Now())
		_ = report.conn.Close()
	}
	if report.cancelWFD != nil {
		report.cancelWFD()
	}
	r.pipeline.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		stillActive := r.active == report
		r.mu.Unlock()
		if !stillActive {
			return true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true, nil
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
		Volume:      r.cfg.DefaultVolume,
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
	if report.session != nil {
		status.SourceName = report.session.SourceName()
		status.Volume = report.session.Volume()
		status.Safety = report.session.SafetyDiagnostics()
		status.MediaInfo = report.session.MediaInfo()
		status.LastSession = nil
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
	s.paused = value
	s.mu.Unlock()
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
	s.paused = false
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
