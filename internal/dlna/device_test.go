package dlna

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateUDN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "dlna-udn")
	first, err := LoadOrCreateUDN(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "uuid:") || len(first) != len("uuid:")+36 {
		t.Fatalf("udn = %q", first)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != first {
		t.Fatalf("persisted %q, want %q", raw, first)
	}

	second, err := LoadOrCreateUDN(path)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("the UDN must be stable across loads")
	}

	// A corrupted file is regenerated.
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := LoadOrCreateUDN(path)
	if err != nil {
		t.Fatal(err)
	}
	if third == "" || !strings.HasPrefix(third, "uuid:") {
		t.Fatalf("repaired udn = %q", third)
	}
}

func TestLoadOrCreateUDNNormalizesExistingValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dlna-udn")
	// Ultra-wide can write the UDN with an upper-case prefix; loading must
	// normalize it but keep the same identity bytes.
	if err := os.WriteFile(path, []byte("UUID:00010203-0405-0607-0809-0A0B0C0D0E0F\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	udn, err := LoadOrCreateUDN(path)
	if err != nil {
		t.Fatal(err)
	}
	if udn != "uuid:00010203-0405-0607-0809-0a0b0c0d0e0f" {
		t.Fatalf("udn = %q", udn)
	}
}

func TestDeviceDescription(t *testing.T) {
	udn := "uuid:00010203-0405-0607-0809-0a0b0c0d0e0f"
	document := deviceDescription("我的客厅音箱", udn, "0.1.0")
	body := string(document)

	for _, expected := range []string{
		"<deviceType>urn:schemas-upnp-org:device:MediaRenderer:1</deviceType>",
		"<friendlyName>我的客厅音箱</friendlyName>",
		"<UDN>" + udn + "</UDN>",
		"<modelNumber>0.1.0</modelNumber>",
		"<dlna:X_DLNADOC>DMR-1.50</dlna:X_DLNADOC>",
		"<serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>",
		"<controlURL>/dlna/control/avtransport</controlURL>",
		"<eventSubURL>/dlna/event/renderingcontrol</eventSubURL>",
		"<SCPDURL>/dlna/scpd/connectionmanager.xml</SCPDURL>",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("device description missing %q:\n%s", expected, body)
		}
	}

	// The document must be well-formed XML.
	var root struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal(document, &root); err != nil {
		t.Fatalf("device description is not valid XML: %v", err)
	}
}

func TestDeviceDescriptionEscapesFriendlyName(t *testing.T) {
	document := deviceDescription("A&B <box>", "uuid:00010203-0405-0607-0809-0a0b0c0d0e0f", "1")
	if strings.Contains(string(document), "A&B <box>") {
		t.Fatal("the friendly name must be XML-escaped")
	}
	if !strings.Contains(string(document), "A&amp;B &lt;box&gt;") {
		t.Fatalf("document = %s", document)
	}
}

func TestSCPDDocuments(t *testing.T) {
	cases := []struct {
		key      string
		expected []string
	}{
		{"avtransport", []string{"SetAVTransportURI", "GetPositionInfo", "Seek", "LastChange"}},
		{"renderingcontrol", []string{"GetVolume", "SetVolume", "SetMute", "LastChange"}},
		{"connectionmanager", []string{"GetProtocolInfo", "GetCurrentConnectionIDs"}},
	}
	for _, testCase := range cases {
		document, ok := scpdDocument(testCase.key)
		if !ok {
			t.Fatalf("scpdDocument(%q) not found", testCase.key)
		}
		for _, expected := range testCase.expected {
			if !strings.Contains(string(document), "<name>"+expected+"</name>") {
				t.Fatalf("scpd %s missing %q", testCase.key, expected)
			}
		}
		if !strings.HasPrefix(string(document), `<?xml version="1.0" encoding="utf-8"?>`) {
			t.Fatalf("scpd %s has no XML declaration", testCase.key)
		}
	}
	if _, ok := scpdDocument("nope"); ok {
		t.Fatal("an unknown SCPD key must not resolve")
	}
}

func TestServiceByKey(t *testing.T) {
	service, ok := serviceByKey("avtransport")
	if !ok || service.Name != "AVTransport" || service.ControlPath != "/dlna/control/avtransport" {
		t.Fatalf("service = %+v, ok = %v", service, ok)
	}
	if _, ok := serviceByKey("RENDERINGCONTROL"); !ok {
		t.Fatal("service lookup must be case-insensitive")
	}
	if _, ok := serviceByKey("bogus"); ok {
		t.Fatal("an unknown service key must not resolve")
	}
}
