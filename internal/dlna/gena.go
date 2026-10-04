// GENA eventing: subscriber registry, NOTIFY delivery and the LastChange
// payload builders shared by the AVTransport and RenderingControl services.
package dlna

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// genaMaxTimeout caps the granted subscription lifetime, per UPnP the
// controller may ask for more but is expected to renew.
const genaMaxTimeout = 10 * time.Minute

// methodNotify is the GENA event HTTP method; net/http does not define it.
const methodNotify = "NOTIFY"

// genaDelivery is one queued NOTIFY delivery.
type genaDelivery struct {
	subscription *genaSubscription
	sequence     int
	payload      []byte
}

// genaSubscription is one active subscriber.
type genaSubscription struct {
	sid      string
	callback *url.URL
	expires  time.Time
	sequence int
	failures int
}

// eventManager tracks subscribers and delivers NOTIFY messages through a
// single serialising worker, so sequence numbers always arrive in order.
type eventManager struct {
	logger *slog.Logger
	client *http.Client

	mu   sync.Mutex
	subs map[string]*genaSubscription

	queue chan genaDelivery
	stop  chan struct{}
	once  sync.Once
}

func newEventManager(logger *slog.Logger) *eventManager {
	manager := &eventManager{
		logger: logger,
		client: &http.Client{Timeout: 5 * time.Second},
		subs:   map[string]*genaSubscription{},
		queue:  make(chan genaDelivery, 64),
		stop:   make(chan struct{}),
	}
	go manager.worker()
	return manager
}

// worker delivers queued NOTIFY messages strictly in order.
func (m *eventManager) worker() {
	for {
		select {
		case <-m.stop:
			return
		case delivery := <-m.queue:
			m.deliver(delivery)
		}
	}
}

// shutdown stops the worker.
func (m *eventManager) shutdown() {
	m.once.Do(func() { close(m.stop) })
}

// parseCallback extracts the first usable <http://...> URL of a CALLBACK
// header. Several candidates may be listed; the first one wins.
func parseCallback(header string) (*url.URL, error) {
	remaining := header
	for {
		start := strings.Index(remaining, "<")
		if start < 0 {
			break
		}
		end := strings.Index(remaining[start:], ">")
		if end < 0 {
			break
		}
		candidate := strings.TrimSpace(remaining[start+1 : start+end])
		remaining = remaining[start+end+1:]
		parsed, err := url.Parse(candidate)
		if err == nil && parsed.Scheme == "http" && parsed.Host != "" {
			return parsed, nil
		}
	}
	return nil, errors.New("CALLBACK header carries no usable http url")
}

// parseTimeoutHeader reads "Second-300" style timeouts, capped at the granted
// maximum with the given fallback for absent or invalid values.
func parseTimeoutHeader(header string, fallback time.Duration) time.Duration {
	trimmed := strings.ToLower(strings.TrimSpace(header))
	trimmed = strings.TrimPrefix(trimmed, "second-")
	if trimmed == "" || trimmed == "infinite" {
		return fallback
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(trimmed))
	if err != nil || seconds <= 0 {
		return fallback
	}
	granted := time.Duration(seconds) * time.Second
	if granted > genaMaxTimeout {
		return genaMaxTimeout
	}
	return granted
}

// subscribe registers a new subscriber.
func (m *eventManager) subscribe(callbackHeader, timeoutHeader string) (string, time.Duration, error) {
	callback, err := parseCallback(callbackHeader)
	if err != nil {
		return "", 0, err
	}
	granted := parseTimeoutHeader(timeoutHeader, 5*time.Minute)
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", 0, fmt.Errorf("generate subscription id: %w", err)
	}
	sid := "uuid:" + hex.EncodeToString(tokenBytes)
	m.mu.Lock()
	m.subs[sid] = &genaSubscription{
		sid:      sid,
		callback: callback,
		expires:  time.Now().Add(granted),
	}
	m.mu.Unlock()
	m.logger.Debug("gena subscription added", "sid", sid, "callback", callback.String(), "timeout", granted)
	return sid, granted, nil
}

// renew extends an existing subscription.
func (m *eventManager) renew(sid, timeoutHeader string) (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	subscription, ok := m.subs[sid]
	if !ok {
		return 0, false
	}
	granted := parseTimeoutHeader(timeoutHeader, 5*time.Minute)
	subscription.expires = time.Now().Add(granted)
	return granted, true
}

// unsubscribe removes one subscription.
func (m *eventManager) unsubscribe(sid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.subs[sid]; !ok {
		return false
	}
	delete(m.subs, sid)
	m.logger.Debug("gena subscription removed", "sid", sid)
	return true
}

// closeAll drops every subscription, e.g. on a console disconnect.
func (m *eventManager) closeAll() {
	m.mu.Lock()
	previous := len(m.subs)
	m.subs = map[string]*genaSubscription{}
	m.mu.Unlock()
	if previous > 0 {
		m.logger.Info("gena subscriptions released", "count", previous)
	}
}

// expire drops timed-out subscriptions; called from the renderer poll loop.
func (m *eventManager) expire() {
	now := time.Now()
	m.mu.Lock()
	for sid, subscription := range m.subs {
		if now.After(subscription.expires) {
			delete(m.subs, sid)
			m.logger.Debug("gena subscription expired", "sid", sid)
		}
	}
	m.mu.Unlock()
}

// count reports the active subscriber count for /api/status.
func (m *eventManager) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subs)
}

// broadcast queues one payload for every subscriber.
func (m *eventManager) broadcast(payload []byte) {
	m.mu.Lock()
	deliveries := make([]genaDelivery, 0, len(m.subs))
	for _, subscription := range m.subs {
		deliveries = append(deliveries, genaDelivery{
			subscription: subscription,
			sequence:     subscription.sequence,
			payload:      payload,
		})
		subscription.sequence++
	}
	m.mu.Unlock()
	for _, delivery := range deliveries {
		m.enqueue(delivery)
	}
}

// sendTo queues one payload for a single subscriber (initial event).
func (m *eventManager) sendTo(sid string, payload []byte) {
	m.mu.Lock()
	subscription, ok := m.subs[sid]
	if !ok {
		m.mu.Unlock()
		return
	}
	delivery := genaDelivery{subscription: subscription, sequence: subscription.sequence, payload: payload}
	subscription.sequence++
	m.mu.Unlock()
	m.enqueue(delivery)
}

func (m *eventManager) enqueue(delivery genaDelivery) {
	select {
	case m.queue <- delivery:
	default:
		m.logger.Warn("gena delivery queue is full; dropping one notification", "sid", delivery.subscription.sid)
	}
}

// deliver performs one NOTIFY round trip. Two consecutive failures drop the
// subscriber, matching the UPnP recovery model (the controller re-subscribes).
func (m *eventManager) deliver(delivery genaDelivery) {
	subscription := delivery.subscription
	request, err := http.NewRequest(methodNotify, subscription.callback.String(), bytes.NewReader(delivery.payload))
	if err != nil {
		m.drop(subscription.sid, "invalid callback")
		return
	}
	request.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	request.Header.Set("NT", "upnp:event")
	request.Header.Set("NTS", "upnp:propchange")
	request.Header.Set("SID", subscription.sid)
	request.Header.Set("SEQ", strconv.Itoa(delivery.sequence))
	response, err := m.client.Do(request)
	if err != nil {
		m.recordFailure(subscription.sid)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		m.recordFailure(subscription.sid)
		return
	}
	m.mu.Lock()
	if current, ok := m.subs[subscription.sid]; ok {
		current.failures = 0
	}
	m.mu.Unlock()
}

func (m *eventManager) recordFailure(sid string) {
	m.mu.Lock()
	subscription, ok := m.subs[sid]
	if !ok {
		m.mu.Unlock()
		return
	}
	subscription.failures++
	drop := subscription.failures >= 2
	m.mu.Unlock()
	if drop {
		m.drop(sid, "callback unreachable")
	}
}

func (m *eventManager) drop(sid, reason string) {
	if m.unsubscribe(sid) {
		m.logger.Info("gena subscriber dropped", "sid", sid, "reason", reason)
	}
}

// ---------------------------------------------------------------------------
// Property payload builders
// ---------------------------------------------------------------------------

// propertyset wraps properties into the GENA event envelope.
func propertyset(properties []soapArgument) []byte {
	var builder strings.Builder
	builder.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	builder.WriteString(`<e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0">`)
	for _, property := range properties {
		fmt.Fprintf(&builder, "<e:property><%s>%s</%s></e:property>", property.Name, xmlEscape(property.Value), property.Name)
	}
	builder.WriteString("</e:propertyset>")
	return []byte(builder.String())
}

// avtEventFragment renders the AVTransport LastChange fragment.
func avtEventFragment(properties []soapArgument) string {
	var builder strings.Builder
	builder.WriteString(`<Event xmlns="urn:schemas-upnp-org:metadata-1-0/AVT/"><InstanceID val="0">`)
	for _, property := range properties {
		fmt.Fprintf(&builder, `<%s val="%s"/>`, property.Name, xmlEscape(property.Value))
	}
	builder.WriteString("</InstanceID></Event>")
	return builder.String()
}

// rcsEventFragment renders the RenderingControl LastChange fragment.
func rcsEventFragment(volume int, muted bool) string {
	muteValue := "0"
	if muted {
		muteValue = "1"
	}
	return fmt.Sprintf(`<Event xmlns="urn:schemas-upnp-org:metadata-1-0/RCS/"><InstanceID val="0"><Volume channel="Master" val="%d"/><Mute channel="Master" val="%s"/></InstanceID></Event>`,
		volume, muteValue)
}

// lastChangePayload wraps one LastChange fragment for the wire.
func lastChangePayload(fragment string) []byte {
	return propertyset([]soapArgument{{Name: "LastChange", Value: fragment}})
}

// connectionManagerPayload is the static event body of ConnectionManager.
func connectionManagerPayload() []byte {
	return propertyset([]soapArgument{
		{Name: "SourceProtocolInfo", Value: ""},
		{Name: "SinkProtocolInfo", Value: sinkProtocolInfo},
		{Name: "CurrentConnectionIDs", Value: "0"},
	})
}
