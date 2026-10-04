package dlna

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// genaNotifyRecord captures one delivered NOTIFY request.
type genaNotifyRecord struct {
	method string
	nt     string
	nts    string
	sid    string
	seq    string
	body   string
}

func awaitNotify(t *testing.T, records <-chan genaNotifyRecord) genaNotifyRecord {
	t.Helper()
	select {
	case record := <-records:
		return record
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a NOTIFY delivery")
		return genaNotifyRecord{}
	}
}

func TestParseCallback(t *testing.T) {
	cases := []struct {
		header string
		host   string
		path   string
		ok     bool
	}{
		{"<http://192.168.1.5:8092/dlna/event/avtransport>", "192.168.1.5:8092", "/dlna/event/avtransport", true},
		{"<https://secure.example/cb> <http://10.0.0.2:1234/cb>", "10.0.0.2:1234", "/cb", true},
		{"<garbage> <http://10.0.0.2:1234/cb>", "10.0.0.2:1234", "/cb", true},
		{"<https://secure.example/cb>", "", "", false},
		{"<not a url at all>", "", "", false},
		{"", "", "", false},
		{"http://bare.example/cb", "", "", false},
	}
	for _, testCase := range cases {
		parsed, err := parseCallback(testCase.header)
		if !testCase.ok {
			if err == nil {
				t.Fatalf("parseCallback(%q) must fail", testCase.header)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseCallback(%q): %v", testCase.header, err)
		}
		if parsed.Host != testCase.host || parsed.Path != testCase.path {
			t.Fatalf("parseCallback(%q) = %q", testCase.header, parsed.String())
		}
	}
}

func TestParseTimeoutHeader(t *testing.T) {
	fallback := 5 * time.Minute
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"Second-300", 300 * time.Second},
		{"second-300", 300 * time.Second},
		{" SECOND-120 ", 120 * time.Second},
		{"Second-1800", genaMaxTimeout},
		{"", fallback},
		{"infinite", fallback},
		{"garbage", fallback},
		{"Second-0", fallback},
		{"Second--5", fallback},
	}
	for _, testCase := range cases {
		if got := parseTimeoutHeader(testCase.header, fallback); got != testCase.want {
			t.Fatalf("parseTimeoutHeader(%q) = %v, want %v", testCase.header, got, testCase.want)
		}
	}
}

func TestEventManagerSubscriptionLifecycle(t *testing.T) {
	manager := newEventManager(testLogger())
	defer manager.shutdown()

	if manager.count() != 0 {
		t.Fatal("a fresh manager must have no subscribers")
	}
	if _, _, err := manager.subscribe("no brackets here", "Second-300"); err == nil {
		t.Fatal("a CALLBACK header without a usable URL must be rejected")
	}

	sid, granted, err := manager.subscribe("<http://192.0.2.10:49152/event>", "Second-300")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sid, "uuid:") || len(sid) != len("uuid:")+32 {
		t.Fatalf("sid = %q", sid)
	}
	if granted != 300*time.Second {
		t.Fatalf("granted = %v", granted)
	}
	if manager.count() != 1 {
		t.Fatalf("count = %d", manager.count())
	}

	if renewed, ok := manager.renew(sid, "Second-60"); !ok || renewed != 60*time.Second {
		t.Fatalf("renew = %v, %v", renewed, ok)
	}
	if _, ok := manager.renew("uuid:absent", "Second-60"); ok {
		t.Fatal("renewing an unknown subscription must fail")
	}

	if !manager.unsubscribe(sid) || manager.unsubscribe(sid) {
		t.Fatal("unsubscribe must report true exactly once")
	}
	if manager.count() != 0 {
		t.Fatalf("count = %d after unsubscribe", manager.count())
	}

	for _, callback := range []string{"<http://192.0.2.10:49152/event>", "<http://192.0.2.11:49152/event>"} {
		if _, _, err := manager.subscribe(callback, "Second-60"); err != nil {
			t.Fatal(err)
		}
	}
	manager.closeAll()
	if manager.count() != 0 {
		t.Fatalf("closeAll must drop every subscription, count = %d", manager.count())
	}
}

func TestEventManagerExpire(t *testing.T) {
	manager := newEventManager(testLogger())
	defer manager.shutdown()

	sid, _, err := manager.subscribe("<http://192.0.2.10:49152/event>", "Second-300")
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.subs[sid].expires = time.Now().Add(-time.Second)
	manager.mu.Unlock()

	manager.expire()
	if manager.count() != 0 {
		t.Fatal("an expired subscription must be dropped")
	}
}

func TestEventManagerDeliversNotificationsInOrder(t *testing.T) {
	records := make(chan genaNotifyRecord, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		records <- genaNotifyRecord{
			method: request.Method,
			nt:     request.Header.Get("NT"),
			nts:    request.Header.Get("NTS"),
			sid:    request.Header.Get("SID"),
			seq:    request.Header.Get("SEQ"),
			body:   string(body),
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	manager := newEventManager(testLogger())
	defer manager.shutdown()
	sid, _, err := manager.subscribe("<"+server.URL+"/notify>", "Second-300")
	if err != nil {
		t.Fatal(err)
	}

	manager.broadcast([]byte("first"))
	manager.broadcast([]byte("second"))

	first := awaitNotify(t, records)
	second := awaitNotify(t, records)
	if first.method != methodNotify || first.nt != "upnp:event" || first.nts != "upnp:propchange" {
		t.Fatalf("first record = %+v", first)
	}
	if first.sid != sid {
		t.Fatalf("sid = %q, want %q", first.sid, sid)
	}
	if first.seq != "0" || second.seq != "1" {
		t.Fatalf("sequence = %q then %q, want 0 then 1", first.seq, second.seq)
	}
	if first.body != "first" || second.body != "second" {
		t.Fatalf("bodies = %q / %q", first.body, second.body)
	}
}

func TestEventManagerDropsUnreachableSubscriber(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	manager := newEventManager(testLogger())
	defer manager.shutdown()
	if _, _, err := manager.subscribe("<"+server.URL+"/notify>", "Second-300"); err != nil {
		t.Fatal(err)
	}

	manager.broadcast([]byte("one"))
	manager.broadcast([]byte("two"))

	deadline := time.Now().Add(5 * time.Second)
	for manager.count() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a subscriber whose callback keeps failing must be dropped")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestEventManagerSendToUnknownSubscriberIsNoop(t *testing.T) {
	manager := newEventManager(testLogger())
	defer manager.shutdown()
	manager.sendTo("uuid:absent", []byte("payload"))
	if manager.count() != 0 {
		t.Fatal("sendTo must not create a subscription")
	}
}

func TestPropertysetRendering(t *testing.T) {
	payload := string(propertyset([]soapArgument{{Name: "A", Value: "x<y&z"}}))
	if !strings.HasPrefix(payload, `<?xml version="1.0" encoding="utf-8"?>`+"\n") {
		t.Fatalf("payload = %q", payload)
	}
	if !strings.Contains(payload, `<e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0">`) {
		t.Fatalf("payload = %q", payload)
	}
	if !strings.Contains(payload, "<e:property><A>x&lt;y&amp;z</A></e:property>") {
		t.Fatalf("payload = %q", payload)
	}
	if !strings.HasSuffix(payload, "</e:propertyset>") {
		t.Fatalf("payload = %q", payload)
	}
}

func TestAVTEventFragment(t *testing.T) {
	fragment := avtEventFragment([]soapArgument{
		{Name: "TransportState", Value: "PLAYING"},
		{Name: "CurrentTrackURI", Value: "http://host/song?a=1&b=2"},
	})
	if !strings.Contains(fragment, `<Event xmlns="urn:schemas-upnp-org:metadata-1-0/AVT/"><InstanceID val="0">`) {
		t.Fatalf("fragment = %q", fragment)
	}
	if !strings.Contains(fragment, `<TransportState val="PLAYING"/>`) {
		t.Fatalf("fragment = %q", fragment)
	}
	if !strings.Contains(fragment, `a=1&amp;b=2`) {
		t.Fatalf("fragment must escape values: %q", fragment)
	}
	if !strings.HasSuffix(fragment, "</InstanceID></Event>") {
		t.Fatalf("fragment = %q", fragment)
	}
}

func TestRCSEventFragment(t *testing.T) {
	fragment := rcsEventFragment(42, false)
	if !strings.Contains(fragment, `<Volume channel="Master" val="42"/>`) {
		t.Fatalf("fragment = %q", fragment)
	}
	if !strings.Contains(fragment, `<Mute channel="Master" val="0"/>`) {
		t.Fatalf("fragment = %q", fragment)
	}
	muted := rcsEventFragment(0, true)
	if !strings.Contains(muted, `<Mute channel="Master" val="1"/>`) {
		t.Fatalf("muted fragment = %q", muted)
	}
}

func TestLastChangePayloadEscapesFragment(t *testing.T) {
	payload := string(lastChangePayload(`<Event xmlns="x"><InstanceID val="0"><TransportState val="STOPPED"/></InstanceID></Event>`))
	if !strings.Contains(payload, "<e:property><LastChange>") {
		t.Fatalf("payload = %q", payload)
	}
	if !strings.Contains(payload, "&lt;Event xmlns=&#34;x&#34;&gt;") {
		t.Fatalf("payload must escape the fragment: %q", payload)
	}
	if !strings.Contains(payload, "&lt;/Event&gt;</LastChange></e:property>") {
		t.Fatalf("payload = %q", payload)
	}
}

func TestConnectionManagerPayload(t *testing.T) {
	payload := string(connectionManagerPayload())
	if !strings.Contains(payload, "<e:property><SourceProtocolInfo></SourceProtocolInfo></e:property>") {
		t.Fatalf("payload = %q", payload)
	}
	if !strings.Contains(payload, "<SinkProtocolInfo>"+sinkProtocolInfo+"</SinkProtocolInfo>") {
		t.Fatalf("payload = %q", payload)
	}
	if !strings.Contains(payload, "<CurrentConnectionIDs>0</CurrentConnectionIDs>") {
		t.Fatalf("payload = %q", payload)
	}
}
