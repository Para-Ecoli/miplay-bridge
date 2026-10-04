// DLNA device identity, description document and SCPD rendering.
//
// The bridge presents itself as a UPnP MediaRenderer:1 with the AVTransport,
// RenderingControl and ConnectionManager services, so any standard DLNA
// controller (BubbleUPnP, Hi-Fi Cast, Windows Media Player, ...) can push a
// stream URL onto the MPD kernel and drive transport and volume.
package dlna

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// serviceDescriptor describes one UPnP service of the MediaRenderer device.
type serviceDescriptor struct {
	Name        string // "AVTransport"
	Type        string // "urn:schemas-upnp-org:service:AVTransport:1"
	ID          string // "urn:upnp-org:serviceId:AVTransport"
	SCPDPath    string // "/dlna/scpd/avtransport.xml"
	ControlPath string // "/dlna/control/avtransport"
	EventPath   string // "/dlna/event/avtransport"
}

// mediaRendererServices is the fixed service list of the device.
var mediaRendererServices = []serviceDescriptor{
	{
		Name:        "AVTransport",
		Type:        "urn:schemas-upnp-org:service:AVTransport:1",
		ID:          "urn:upnp-org:serviceId:AVTransport",
		SCPDPath:    "/dlna/scpd/avtransport.xml",
		ControlPath: "/dlna/control/avtransport",
		EventPath:   "/dlna/event/avtransport",
	},
	{
		Name:        "RenderingControl",
		Type:        "urn:schemas-upnp-org:service:RenderingControl:1",
		ID:          "urn:upnp-org:serviceId:RenderingControl",
		SCPDPath:    "/dlna/scpd/renderingcontrol.xml",
		ControlPath: "/dlna/control/renderingcontrol",
		EventPath:   "/dlna/event/renderingcontrol",
	},
	{
		Name:        "ConnectionManager",
		Type:        "urn:schemas-upnp-org:service:ConnectionManager:1",
		ID:          "urn:upnp-org:serviceId:ConnectionManager",
		SCPDPath:    "/dlna/scpd/connectionmanager.xml",
		ControlPath: "/dlna/control/connectionmanager",
		EventPath:   "/dlna/event/connectionmanager",
	},
}

// serviceByKey resolves the lower-case key used in the /dlna/ URLs.
func serviceByKey(key string) (serviceDescriptor, bool) {
	for _, service := range mediaRendererServices {
		if strings.EqualFold(service.Name, key) {
			return service, true
		}
	}
	return serviceDescriptor{}, false
}

// LoadOrCreateUDN returns the stable "uuid:<canonical>" device UDN of the
// renderer, creating and persisting one on first start. Without it, every
// restart would look like a brand-new device to controllers.
func LoadOrCreateUDN(path string) (string, error) {
	if raw, err := os.ReadFile(path); err == nil {
		text := strings.TrimSpace(string(raw))
		text = strings.TrimPrefix(strings.ToLower(text), "uuid:")
		normalized := strings.ReplaceAll(text, "-", "")
		if decoded, decodeErr := hex.DecodeString(normalized); decodeErr == nil && len(decoded) == 16 {
			return "uuid:" + canonicalUUID(decoded), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read dlna udn %s: %w", path, err)
	}
	generated := make([]byte, 16)
	if _, err := rand.Read(generated); err != nil {
		return "", fmt.Errorf("generate dlna udn: %w", err)
	}
	generated[6] = (generated[6] & 0x0F) | 0x40 // version 4
	generated[8] = (generated[8] & 0x3F) | 0x80 // variant 10
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create the data directory: %w", err)
	}
	canonical := "uuid:" + canonicalUUID(generated)
	if err := os.WriteFile(path, []byte(canonical), 0o644); err != nil {
		return "", fmt.Errorf("persist dlna udn %s: %w", path, err)
	}
	return canonical, nil
}

// canonicalUUID renders 16 bytes as the canonical lower-case form.
func canonicalUUID(raw []byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// deviceDescription renders the root device description document.
func deviceDescription(friendlyName, udn, version string) []byte {
	var builder strings.Builder
	builder.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	builder.WriteString(`<root xmlns="urn:schemas-upnp-org:device-1-0" xmlns:dlna="urn:schemas-dlna-org:device-1-0">` + "\n")
	builder.WriteString(`<specVersion><major>1</major><minor>0</minor></specVersion>` + "\n")
	builder.WriteString(`<device>` + "\n")
	builder.WriteString(`<deviceType>urn:schemas-upnp-org:device:MediaRenderer:1</deviceType>` + "\n")
	fmt.Fprintf(&builder, "<friendlyName>%s</friendlyName>\n", xmlEscape(friendlyName))
	builder.WriteString("<manufacturer>MiPlay Bridge</manufacturer>\n")
	builder.WriteString("<manufacturerURL>https://github.com/miplay-bridge</manufacturerURL>\n")
	builder.WriteString("<modelDescription>MiPlay and DLNA receiver bridging Wi-Fi streams onto the host sound card through MPD</modelDescription>\n")
	builder.WriteString("<modelName>MiPlay Bridge</modelName>\n")
	fmt.Fprintf(&builder, "<modelNumber>%s</modelNumber>\n", xmlEscape(version))
	builder.WriteString("<modelURL>https://github.com/miplay-bridge/miplay-bridge</modelURL>\n")
	fmt.Fprintf(&builder, "<serialNumber>%s</serialNumber>\n", xmlEscape(strings.TrimPrefix(udn, "uuid:")))
	fmt.Fprintf(&builder, "<UDN>%s</UDN>\n", xmlEscape(udn))
	builder.WriteString("<dlna:X_DLNADOC>DMR-1.50</dlna:X_DLNADOC>\n")
	builder.WriteString("<serviceList>\n")
	for _, service := range mediaRendererServices {
		builder.WriteString("<service>\n")
		fmt.Fprintf(&builder, "<serviceType>%s</serviceType>\n", service.Type)
		fmt.Fprintf(&builder, "<serviceId>%s</serviceId>\n", service.ID)
		fmt.Fprintf(&builder, "<SCPDURL>%s</SCPDURL>\n", service.SCPDPath)
		fmt.Fprintf(&builder, "<controlURL>%s</controlURL>\n", service.ControlPath)
		fmt.Fprintf(&builder, "<eventSubURL>%s</eventSubURL>\n", service.EventPath)
		builder.WriteString("</service>\n")
	}
	builder.WriteString("</serviceList>\n")
	builder.WriteString("</device>\n")
	builder.WriteString("</root>\n")
	return []byte(builder.String())
}

// scpdArgument is one action argument in the generated service templates.
type scpdArgument struct {
	Name      string
	Direction string // "in" or "out"
	Related   string // related state variable
}

// scpdAction is one action declaration.
type scpdAction struct {
	Name string
	Args []scpdArgument
}

// scpdSpec is the compact description of one service's implemented surface.
type scpdSpec struct {
	Actions        []scpdAction
	StateVariables string // pre-rendered <stateVariableList> block
}

// scpdDocument renders the service description of one /dlna/scpd key.
func scpdDocument(key string) ([]byte, bool) {
	spec, ok := scpdSpecs[strings.ToLower(key)]
	if !ok {
		return nil, false
	}
	var builder strings.Builder
	builder.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	builder.WriteString(`<scpd xmlns="urn:schemas-upnp-org:service-1-0">` + "\n")
	builder.WriteString("<specVersion><major>1</major><minor>0</minor></specVersion>\n")
	builder.WriteString("<actionList>\n")
	for _, action := range spec.Actions {
		fmt.Fprintf(&builder, "<action><name>%s</name>", action.Name)
		if len(action.Args) > 0 {
			builder.WriteString("<argumentList>")
			for _, argument := range action.Args {
				fmt.Fprintf(&builder, "<argument><name>%s</name><direction>%s</direction><relatedStateVariable>%s</relatedStateVariable></argument>",
					argument.Name, argument.Direction, argument.Related)
			}
			builder.WriteString("</argumentList>")
		}
		builder.WriteString("</action>\n")
	}
	builder.WriteString("</actionList>\n")
	builder.WriteString(spec.StateVariables)
	builder.WriteString("</scpd>\n")
	return []byte(builder.String()), true
}

// scpdSpecs holds the three service descriptions. The action lists match the
// handlers in renderer.go one-to-one; anything not listed answers with a
// standard UPnP "invalid action" fault.
var scpdSpecs = map[string]scpdSpec{
	"avtransport": {
		Actions: []scpdAction{
			{Name: "SetAVTransportURI", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "CurrentURI", Direction: "in", Related: "AVTransportURI"},
				{Name: "CurrentURIMetaData", Direction: "in", Related: "AVTransportURIMetaData"},
			}},
			{Name: "SetNextAVTransportURI", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "NextURI", Direction: "in", Related: "NextAVTransportURI"},
				{Name: "NextURIMetaData", Direction: "in", Related: "NextAVTransportURIMetaData"},
			}},
			{Name: "GetMediaInfo", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "NrTracks", Direction: "out", Related: "NumberOfTracks"},
				{Name: "MediaDuration", Direction: "out", Related: "CurrentMediaDuration"},
				{Name: "CurrentURI", Direction: "out", Related: "AVTransportURI"},
				{Name: "CurrentURIMetaData", Direction: "out", Related: "AVTransportURIMetaData"},
				{Name: "NextURI", Direction: "out", Related: "NextAVTransportURI"},
				{Name: "NextURIMetaData", Direction: "out", Related: "NextAVTransportURIMetaData"},
				{Name: "PlayMedium", Direction: "out", Related: "PlaybackStorageMedium"},
				{Name: "RecordMedium", Direction: "out", Related: "RecordStorageMedium"},
				{Name: "WriteStatus", Direction: "out", Related: "RecordMediumWriteStatus"},
			}},
			{Name: "GetTransportInfo", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "CurrentTransportState", Direction: "out", Related: "TransportState"},
				{Name: "CurrentTransportStatus", Direction: "out", Related: "TransportStatus"},
				{Name: "CurrentSpeed", Direction: "out", Related: "TransportPlaySpeed"},
			}},
			{Name: "GetPositionInfo", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "Track", Direction: "out", Related: "CurrentTrack"},
				{Name: "TrackDuration", Direction: "out", Related: "CurrentTrackDuration"},
				{Name: "TrackMetaData", Direction: "out", Related: "CurrentTrackMetaData"},
				{Name: "TrackURI", Direction: "out", Related: "CurrentTrackURI"},
				{Name: "RelTime", Direction: "out", Related: "RelativeTimePosition"},
				{Name: "AbsTime", Direction: "out", Related: "AbsoluteTimePosition"},
				{Name: "RelCount", Direction: "out", Related: "RelativeCounterPosition"},
				{Name: "AbsCount", Direction: "out", Related: "AbsoluteCounterPosition"},
			}},
			{Name: "GetDeviceCapabilities", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "PlayMedia", Direction: "out", Related: "PossiblePlaybackStorageMedia"},
				{Name: "RecMedia", Direction: "out", Related: "PossibleRecordStorageMedia"},
				{Name: "RecQualityModes", Direction: "out", Related: "PossibleRecordQualityModes"},
			}},
			{Name: "GetTransportSettings", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "PlayMode", Direction: "out", Related: "CurrentPlayMode"},
				{Name: "RecQualityMode", Direction: "out", Related: "CurrentRecordQualityMode"},
			}},
			{Name: "Stop", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
			}},
			{Name: "Play", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "Speed", Direction: "in", Related: "TransportPlaySpeed"},
			}},
			{Name: "Pause", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
			}},
			{Name: "Seek", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "Unit", Direction: "in", Related: "A_ARG_TYPE_SeekMode"},
				{Name: "Target", Direction: "in", Related: "A_ARG_TYPE_SeekTarget"},
			}},
			{Name: "Next", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
			}},
			{Name: "Previous", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
			}},
			{Name: "GetCurrentTransportActions", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "Actions", Direction: "out", Related: "CurrentTransportActions"},
			}},
		},
		StateVariables: avTransportStateVariables,
	},
	"renderingcontrol": {
		Actions: []scpdAction{
			{Name: "ListPresets", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "CurrentPresetNameList", Direction: "out", Related: "PresetNameList"},
			}},
			{Name: "SelectPreset", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "PresetName", Direction: "in", Related: "A_ARG_TYPE_PresetName"},
			}},
			{Name: "GetVolume", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "Channel", Direction: "in", Related: "A_ARG_TYPE_Channel"},
				{Name: "CurrentVolume", Direction: "out", Related: "Volume"},
			}},
			{Name: "SetVolume", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "Channel", Direction: "in", Related: "A_ARG_TYPE_Channel"},
				{Name: "DesiredVolume", Direction: "in", Related: "Volume"},
			}},
			{Name: "GetMute", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "Channel", Direction: "in", Related: "A_ARG_TYPE_Channel"},
				{Name: "CurrentMute", Direction: "out", Related: "Mute"},
			}},
			{Name: "SetMute", Args: []scpdArgument{
				{Name: "InstanceID", Direction: "in", Related: "A_ARG_TYPE_InstanceID"},
				{Name: "Channel", Direction: "in", Related: "A_ARG_TYPE_Channel"},
				{Name: "DesiredMute", Direction: "in", Related: "Mute"},
			}},
		},
		StateVariables: renderingControlStateVariables,
	},
	"connectionmanager": {
		Actions: []scpdAction{
			{Name: "GetProtocolInfo", Args: []scpdArgument{
				{Name: "Source", Direction: "out", Related: "SourceProtocolInfo"},
				{Name: "Sink", Direction: "out", Related: "SinkProtocolInfo"},
			}},
			{Name: "GetCurrentConnectionIDs", Args: []scpdArgument{
				{Name: "ConnectionIDs", Direction: "out", Related: "CurrentConnectionIDs"},
			}},
			{Name: "GetCurrentConnectionInfo", Args: []scpdArgument{
				{Name: "ConnectionID", Direction: "in", Related: "A_ARG_TYPE_ConnectionID"},
				{Name: "RcsID", Direction: "out", Related: "A_ARG_TYPE_RcsID"},
				{Name: "AVTransportID", Direction: "out", Related: "A_ARG_TYPE_AVTransportID"},
				{Name: "ProtocolInfo", Direction: "out", Related: "A_ARG_TYPE_ProtocolInfo"},
				{Name: "PeerConnectionManager", Direction: "out", Related: "A_ARG_TYPE_ConnectionManager"},
				{Name: "PeerConnectionID", Direction: "out", Related: "A_ARG_TYPE_ConnectionID"},
				{Name: "Direction", Direction: "out", Related: "A_ARG_TYPE_Direction"},
				{Name: "Status", Direction: "out", Related: "A_ARG_TYPE_ConnectionStatus"},
			}},
		},
		StateVariables: connectionManagerStateVariables,
	},
}

// The state-variable blocks below keep the SCPD documents valid for strict
// controllers. Only LastChange (AVTransport / RenderingControl) and the three
// ConnectionManager variables are evented.
const avTransportStateVariables = `<serviceStateTable>
<stateVariable sendEvents="yes"><name>LastChange</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>TransportState</name><dataType>string</dataType><allowedValueList><allowedValue>STOPPED</allowedValue><allowedValue>PLAYING</allowedValue><allowedValue>PAUSED_PLAYBACK</allowedValue><allowedValue>TRANSITIONING</allowedValue><allowedValue>NO_MEDIA_PRESENT</allowedValue></allowedValueList></stateVariable>
<stateVariable sendEvents="no"><name>TransportStatus</name><dataType>string</dataType><allowedValueList><allowedValue>OK</allowedValue><allowedValue>ERROR_OCCURRED</allowedValue></allowedValueList></stateVariable>
<stateVariable sendEvents="no"><name>TransportPlaySpeed</name><dataType>string</dataType><allowedValueList><allowedValue>1</allowedValue></allowedValueList></stateVariable>
<stateVariable sendEvents="no"><name>NumberOfTracks</name><dataType>ui4</dataType></stateVariable>
<stateVariable sendEvents="no"><name>CurrentTrack</name><dataType>ui4</dataType></stateVariable>
<stateVariable sendEvents="no"><name>CurrentMediaDuration</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>CurrentTrackDuration</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>CurrentTrackMetaData</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>CurrentTrackURI</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>AVTransportURI</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>AVTransportURIMetaData</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>NextAVTransportURI</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>NextAVTransportURIMetaData</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>RelativeTimePosition</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>AbsoluteTimePosition</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>RelativeCounterPosition</name><dataType>i4</dataType></stateVariable>
<stateVariable sendEvents="no"><name>AbsoluteCounterPosition</name><dataType>i4</dataType></stateVariable>
<stateVariable sendEvents="no"><name>CurrentPlayMode</name><dataType>string</dataType><allowedValueList><allowedValue>NORMAL</allowedValue></allowedValueList><defaultValue>NORMAL</defaultValue></stateVariable>
<stateVariable sendEvents="no"><name>CurrentTransportActions</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>PlaybackStorageMedium</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>RecordStorageMedium</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>RecordMediumWriteStatus</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>PossiblePlaybackStorageMedia</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>PossibleRecordStorageMedia</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>PossibleRecordQualityModes</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>CurrentRecordQualityMode</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_SeekMode</name><dataType>string</dataType><allowedValueList><allowedValue>REL_TIME</allowedValue><allowedValue>ABS_TIME</allowedValue></allowedValueList></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_SeekTarget</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_InstanceID</name><dataType>ui4</dataType></stateVariable>
</serviceStateTable>
`

const renderingControlStateVariables = `<serviceStateTable>
<stateVariable sendEvents="yes"><name>LastChange</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>PresetNameList</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>Volume</name><dataType>ui2</dataType><allowedValueRange><minimum>0</minimum><maximum>100</maximum><step>1</step></allowedValueRange></stateVariable>
<stateVariable sendEvents="no"><name>Mute</name><dataType>boolean</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_Channel</name><dataType>string</dataType><allowedValueList><allowedValue>Master</allowedValue></allowedValueList></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_PresetName</name><dataType>string</dataType><allowedValueList><allowedValue>FactoryDefaults</allowedValue></allowedValueList></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_InstanceID</name><dataType>ui4</dataType></stateVariable>
</serviceStateTable>
`

const connectionManagerStateVariables = `<serviceStateTable>
<stateVariable sendEvents="yes"><name>SourceProtocolInfo</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="yes"><name>SinkProtocolInfo</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="yes"><name>CurrentConnectionIDs</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionStatus</name><dataType>string</dataType><allowedValueList><allowedValue>OK</allowedValue><allowedValue>ContentFormatMismatch</allowedValue><allowedValue>InsufficientBandwidth</allowedValue><allowedValue>UnreliableChannel</allowedValue><allowedValue>Unknown</allowedValue></allowedValueList></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionManager</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_Direction</name><dataType>string</dataType><allowedValueList><allowedValue>Input</allowedValue><allowedValue>Output</allowedValue></allowedValueList></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_ProtocolInfo</name><dataType>string</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionID</name><dataType>i4</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_AVTransportID</name><dataType>i4</dataType></stateVariable>
<stateVariable sendEvents="no"><name>A_ARG_TYPE_RcsID</name><dataType>i4</dataType></stateVariable>
</serviceStateTable>
`
