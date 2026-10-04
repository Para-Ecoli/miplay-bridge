package miplay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testIdentity() Identity {
	return Identity{
		FriendlyName: "妙播桥",
		Instance:     "MiPlay-Bridge",
		Host:         "miplay-bridge",
		DeviceID:     []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		ControlPort:  8899,
		Address:      net.IPv4(192, 168, 1, 10),
	}
}

func TestIdentityIDHashFixedVector(t *testing.T) {
	// sha256(000102...0f)[:3] base64 == "vkXL", computed independently.
	if got := testIdentity().IDHash(); got != "vkXL" {
		t.Fatalf("IDHash() = %q, want vkXL", got)
	}
}

func TestIdentityAppsDataFixedVector(t *testing.T) {
	// Computed independently with Python hashlib/base64 (see README).
	const want = "gQBWBIMiw75FyyYFvwAAAAAAAAAAAAAAAAAAAHsibWljbyI6eyJkZXZpY2VfaWQiOiIwMDAxMDIwMy0wNDA1LTA2MDctMDgwOS0wYTBiMGMwZDBlMGYifX0="
	got, err := testIdentity().AppsData()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("AppsData() = %q, want %q", got, want)
	}
}

func TestIdentityAppsDataStructure(t *testing.T) {
	encoded, err := testIdentity().AppsData()
	if err != nil {
		t.Fatal(err)
	}
	container, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if container[0] != 0x81 || container[1] != 0x00 {
		t.Fatalf("container prefix = % x", container[:2])
	}
	if int(container[2]) != len(container)-3 {
		t.Fatalf("container length = %d, want %d", container[2], len(container)-3)
	}
	payload := container[3:]
	if len(payload) != 25+61 {
		t.Fatalf("payload length = %d", len(payload))
	}
	if binary.BigEndian.Uint16(payload[0:2]) != 1155 {
		t.Fatalf("magic = %d, want 1155", binary.BigEndian.Uint16(payload[0:2]))
	}
	if binary.BigEndian.Uint16(payload[2:4]) != 8899 {
		t.Fatalf("control port = %d", binary.BigEndian.Uint16(payload[2:4]))
	}
	if payload[4] != 0xBE { // sha256[0] & 0xFC | 0x02
		t.Fatalf("shared byte = 0x%02x", payload[4])
	}
	if !bytes.Equal(payload[5:10], []byte{0x45, 0xCB, 0x26, 0x05, 0xBF}) {
		t.Fatalf("digest slice = % x", payload[5:10])
	}
	if payload[24] != 0 {
		t.Fatalf("payload[24] = 0x%02x, want 0", payload[24])
	}
	if !strings.Contains(string(payload[25:]), `"device_id":"00010203-0405-0607-0809-0a0b0c0d0e0f"`) {
		t.Fatalf("payload JSON = %s", payload[25:])
	}
}

func TestIdentityTxtRecords(t *testing.T) {
	records, err := testIdentity().txtRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 9 {
		t.Fatalf("records = %d, want 9", len(records))
	}
	expected := map[int]string{
		0: "name=妙播桥",
		1: "version=65545",
		2: "apps=[5]",
		4: "dev=4",
		5: "sec=2",
		6: "flags=Ag==",
		8: "commonData=0",
	}
	for index, want := range expected {
		if records[index] != want {
			t.Fatalf("records[%d] = %q, want %q", index, records[index], want)
		}
	}
	if !strings.HasPrefix(records[3], "appsData=") {
		t.Fatalf("records[3] = %q", records[3])
	}
	if records[7] != "idHash=vkXL" {
		t.Fatalf("records[7] = %q", records[7])
	}
}

func TestCanonicalUUID(t *testing.T) {
	if got := canonicalUUID([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}); got != "00010203-0405-0607-0809-0a0b0c0d0e0f" {
		t.Fatalf("canonicalUUID = %q", got)
	}
	if got := canonicalUUID([]byte{1, 2, 3}); got != "" {
		t.Fatalf("canonicalUUID of a short slice = %q, want empty", got)
	}
}

func TestLoadOrCreateDeviceID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "miplay-device-id")
	first, err := LoadOrCreateDeviceID(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 16 {
		t.Fatalf("device id length = %d", len(first))
	}
	if first[6]&0xF0 != 0x40 || first[8]&0xC0 != 0x80 {
		t.Fatalf("device id is not a random UUID: % x", first)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != canonicalUUID(first) {
		t.Fatalf("persisted %q, want the canonical UUID", raw)
	}

	second, err := LoadOrCreateDeviceID(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("the device id must be stable across loads")
	}

	// A corrupted file is regenerated and repaired.
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := LoadOrCreateDeviceID(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 16 {
		t.Fatalf("repaired device id length = %d", len(third))
	}
	fourth, err := LoadOrCreateDeviceID(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(third, fourth) {
		t.Fatal("the repaired device id must persist")
	}
}

func TestSanitizeLabel(t *testing.T) {
	if got := sanitizeLabel("MiPlay 妙播桥", "fallback"); got != "MiPlay" {
		t.Fatalf("sanitizeLabel = %q, want MiPlay", got)
	}
	if got := sanitizeLabel("妙播桥", "fallback"); got != "fallback" {
		t.Fatalf("sanitizeLabel = %q, want the fallback", got)
	}
	if got := sanitizeLabel("NAS-01_ok", "fallback"); got != "NAS-01_ok" {
		t.Fatalf("sanitizeLabel = %q", got)
	}
	long := sanitizeLabel(strings.Repeat("a", 100), "fallback")
	if len(long) != 63 {
		t.Fatalf("sanitizeLabel length = %d, want 63", len(long))
	}
}

func newTestResponder() *Responder {
	responder := NewResponder(ResponderConfig{}, discardLogger())
	responder.client = testIdentity()
	return responder
}

func parseDNSRecords(t *testing.T, message []byte, wantAnswers, wantAdditionals int) []dnsRecord {
	t.Helper()
	questionCount := int(binary.BigEndian.Uint16(message[4:6]))
	answerCount := int(binary.BigEndian.Uint16(message[6:8]))
	additionalCount := int(binary.BigEndian.Uint16(message[10:12]))
	if answerCount != wantAnswers || additionalCount != wantAdditionals {
		t.Fatalf("answer/additional counts = %d/%d, want %d/%d", answerCount, additionalCount, wantAnswers, wantAdditionals)
	}
	offset := 12
	for index := 0; index < questionCount; index++ {
		_, next, err := readName(message, offset)
		if err != nil {
			t.Fatal(err)
		}
		offset = next + 4
	}
	records := []dnsRecord{}
	for index := 0; index < answerCount+additionalCount; index++ {
		name, next, err := readName(message, offset)
		if err != nil {
			t.Fatal(err)
		}
		recordType := binary.BigEndian.Uint16(message[next : next+2])
		dataLength := int(binary.BigEndian.Uint16(message[next+8 : next+10]))
		data := message[next+10 : next+10+dataLength]
		records = append(records, dnsRecord{Name: name, Type: recordType, Data: data})
		offset = next + 10 + dataLength
	}
	return records
}

func parseTXTStrings(t *testing.T, data []byte) []string {
	t.Helper()
	values := []string{}
	for offset := 0; offset < len(data); {
		length := int(data[offset])
		offset++
		if offset+length > len(data) {
			t.Fatal("TXT data overruns")
		}
		values = append(values, string(data[offset:offset+length]))
		offset += length
	}
	return values
}

func TestHandleQueryAnswersPTRWithFullRecordSet(t *testing.T) {
	responder := newTestResponder()
	query := buildMessage(0x4242, 0, []dnsQuestion{{Name: ServiceType, Type: dnsTypePTR, Class: dnsClassIN}}, nil, nil)
	response := responder.handleQuery(query, &net.UDPAddr{IP: net.IPv4(192, 168, 1, 50), Port: 55555})
	if response == nil {
		t.Fatal("a PTR query for the service type must be answered")
	}
	if binary.BigEndian.Uint16(response[0:2]) != 0x4242 {
		t.Fatalf("transaction id = 0x%04x", binary.BigEndian.Uint16(response[0:2]))
	}
	if binary.BigEndian.Uint16(response[2:4]) != dnsFlagResponse {
		t.Fatalf("flags = 0x%04x", binary.BigEndian.Uint16(response[2:4]))
	}

	records := parseDNSRecords(t, response, 3, 1)
	instanceName := testIdentity().Instance + "." + ServiceType
	if records[0].Type != dnsTypePTR || records[0].Name != ServiceType {
		t.Fatalf("PTR record = %+v", records[0])
	}
	pointer, _, err := readName(records[0].Data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pointer != instanceName {
		t.Fatalf("PTR target = %q, want %q", pointer, instanceName)
	}
	if records[1].Type != dnsTypeSRV || binary.BigEndian.Uint16(records[1].Data[4:6]) != CoapPort {
		t.Fatalf("SRV record = %+v", records[1])
	}
	if records[2].Type != dnsTypeTXT {
		t.Fatalf("TXT record type = %d", records[2].Type)
	}
	txtValues := parseTXTStrings(t, records[2].Data)
	joined := strings.Join(txtValues, "|")
	if !strings.Contains(joined, "version=65545") || !strings.Contains(joined, "commonData=0") {
		t.Fatalf("TXT values = %v", txtValues)
	}
	if records[3].Type != dnsTypeA || !bytes.Equal(records[3].Data, net.IPv4(192, 168, 1, 10).To4()) {
		t.Fatalf("A record = %+v", records[3])
	}
}

func TestHandleQueryRewritesMulticastTransactionID(t *testing.T) {
	responder := newTestResponder()
	query := buildMessage(0x7777, 0, []dnsQuestion{{Name: ServiceType, Type: dnsTypePTR, Class: dnsClassIN}}, nil, nil)
	response := responder.handleQuery(query, &net.UDPAddr{IP: net.IPv4(192, 168, 1, 50), Port: mdnsPort})
	if response == nil {
		t.Fatal("a multicast PTR query must be answered")
	}
	if binary.BigEndian.Uint16(response[0:2]) != 0 {
		t.Fatalf("multicast transaction id = 0x%04x, want 0", binary.BigEndian.Uint16(response[0:2]))
	}
}

func TestHandleQueryAnswersSRVAndA(t *testing.T) {
	responder := newTestResponder()
	instanceName := testIdentity().Instance + "." + ServiceType

	srvQuery := buildMessage(1, 0, []dnsQuestion{{Name: instanceName, Type: dnsTypeSRV, Class: dnsClassIN}}, nil, nil)
	srvResponse := responder.handleQuery(srvQuery, &net.UDPAddr{IP: net.IPv4(192, 168, 1, 50), Port: 55555})
	if srvResponse == nil {
		t.Fatal("an SRV query for the instance must be answered")
	}
	records := parseDNSRecords(t, srvResponse, 1, 1)
	if records[0].Type != dnsTypeSRV {
		t.Fatalf("records = %+v", records)
	}

	hostName := testIdentity().Host + ".local."
	aQuery := buildMessage(2, 0, []dnsQuestion{{Name: hostName, Type: dnsTypeA, Class: dnsClassIN}}, nil, nil)
	aResponse := responder.handleQuery(aQuery, &net.UDPAddr{IP: net.IPv4(192, 168, 1, 50), Port: 55555})
	if aResponse == nil {
		t.Fatal("an A query for the host must be answered")
	}
	records = parseDNSRecords(t, aResponse, 1, 0)
	if records[0].Type != dnsTypeA {
		t.Fatalf("records = %+v", records)
	}
}

func TestHandleQueryIgnoresIrrelevantMessages(t *testing.T) {
	responder := newTestResponder()
	source := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 50), Port: 55555}

	if response := responder.handleQuery([]byte{0, 1, 2}, source); response != nil {
		t.Fatal("a truncated message must be ignored")
	}
	otherQuery := buildMessage(3, 0, []dnsQuestion{{Name: "example.com.", Type: dnsTypeA, Class: dnsClassIN}}, nil, nil)
	if response := responder.handleQuery(otherQuery, source); response != nil {
		t.Fatal("a query for an unrelated name must be ignored")
	}
	responseMessage := buildMessage(4, dnsFlagResponse, nil, []dnsRecord{{Name: ServiceType, Type: dnsTypePTR, TTL: 120, Data: encodeName("x.")}}, nil)
	if response := responder.handleQuery(responseMessage, source); response != nil {
		t.Fatal("an mDNS response must not be answered")
	}
}

func TestResponderStartRejectsInvalidAdvertiseAddress(t *testing.T) {
	// Regression guard for the startup bug where the auto-detected LAN
	// address never reached the responder: Start must validate the address
	// handed to it explicitly instead of a stale configuration copy.
	cases := []struct {
		name    string
		address string
	}{
		{name: "empty", address: ""},
		{name: "not-an-ip", address: "not-an-ip"},
		{name: "ipv6", address: "fe80::1"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			responder := NewResponder(ResponderConfig{
				FriendlyName: "妙播桥",
				ControlPort:  8899,
				DeviceIDFile: filepath.Join(t.TempDir(), "miplay-device-id"),
			}, discardLogger())
			err := responder.Start(context.Background(), testCase.address)
			if err == nil || !strings.Contains(err.Error(), "not an IPv4 address") {
				t.Fatalf("Start(%q) = %v, want an IPv4 rejection", testCase.address, err)
			}
		})
	}
}
