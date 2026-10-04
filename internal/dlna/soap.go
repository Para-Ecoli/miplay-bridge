// SOAP plumbing of the UPnP control plane: action parsing, response and
// fault rendering, seek-time helpers and DIDL-Lite metadata extraction.
package dlna

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// soapEnvelopeOpen is the shared envelope prefix of every SOAP response.
const soapEnvelopeOpen = `<?xml version="1.0" encoding="utf-8"?>` + "\n" +
	`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`

const soapEnvelopeClose = `</s:Body></s:Envelope>` + "\n"

// soapArgument is one ordered response argument.
type soapArgument struct {
	Name  string
	Value string
}

// parseSOAPAction extracts the action name from the SOAPACTION header, e.g.
// "urn:schemas-upnp-org:service:AVTransport:1#Play" -> "Play".
func parseSOAPAction(request *http.Request) (string, bool) {
	raw := strings.TrimSpace(request.Header.Get("SOAPAction"))
	raw = strings.Trim(raw, `"`)
	if raw == "" {
		return "", false
	}
	if index := strings.LastIndex(raw, "#"); index >= 0 {
		raw = raw[index+1:]
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	return raw, true
}

// parseActionArgs walks a SOAP body and returns the action name plus its
// argument values. Entity-escaped payloads (CurrentURIMetaData carrying
// DIDL-Lite XML) arrive decoded; unescaped nested XML is concatenated.
func parseActionArgs(body []byte) (string, map[string]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	action := ""
	args := map[string]string{}
	depth := 0
	currentArg := ""
	var text strings.Builder
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("soap body is not valid XML: %w", err)
		}
		switch typed := token.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 3 && action == "":
				action = typed.Name.Local
			case depth == 4:
				currentArg = typed.Name.Local
				text.Reset()
			}
		case xml.CharData:
			if depth >= 4 && currentArg != "" {
				text.Write(typed)
			}
		case xml.EndElement:
			if depth == 4 && currentArg != "" {
				args[currentArg] = strings.TrimSpace(text.String())
				currentArg = ""
			}
			if depth > 0 {
				depth--
			}
		}
	}
	if action == "" {
		return "", nil, errors.New("soap body carries no action element")
	}
	return action, args, nil
}

// writeSOAPResponse renders a successful action response.
func writeSOAPResponse(w http.ResponseWriter, serviceType, action string, args []soapArgument) {
	var builder strings.Builder
	builder.WriteString(soapEnvelopeOpen)
	fmt.Fprintf(&builder, `<u:%sResponse xmlns:u="%s">`, action, xmlEscape(serviceType))
	for _, argument := range args {
		fmt.Fprintf(&builder, "<%s>%s</%s>", argument.Name, xmlEscape(argument.Value), argument.Name)
	}
	fmt.Fprintf(&builder, "</u:%sResponse>", action)
	builder.WriteString(soapEnvelopeClose)
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, builder.String())
}

// upnpError is a SOAP fault with a UPnP error code.
type upnpError struct {
	Code        int
	Description string
}

func (e *upnpError) Error() string {
	return fmt.Sprintf("upnp error %d: %s", e.Code, e.Description)
}

// Standard UPnP error codes used by this renderer.
var (
	errInvalidAction     = &upnpError{Code: 401, Description: "Invalid Action"}
	errInvalidArgs       = &upnpError{Code: 402, Description: "Invalid Args"}
	errActionFailed      = &upnpError{Code: 501, Description: "Action Failed"}
	errConnectionRef     = &upnpError{Code: 706, Description: "Invalid connection reference"}
	errTransition        = &upnpError{Code: 701, Description: "Transition not available"}
	errSeekMode          = &upnpError{Code: 710, Description: "Seek mode not supported"}
	errSeekTarget        = &upnpError{Code: 711, Description: "Illegal seek target"}
	errPlaySpeed         = &upnpError{Code: 712, Description: "Play speed not supported"}
	errInvalidInstanceID = &upnpError{Code: 718, Description: "Invalid InstanceID"}
	errDeviceBusy        = &upnpError{Code: 701, Description: "The sound card is held by another source; clear the MiPlay cast or the API playback first"}
)

// writeSOAPFault renders a SOAP fault envelope.
func writeSOAPFault(w http.ResponseWriter, fault *upnpError) {
	var builder strings.Builder
	builder.WriteString(soapEnvelopeOpen)
	builder.WriteString("<s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail>")
	fmt.Fprintf(&builder, `<UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError>`,
		fault.Code, xmlEscape(fault.Description))
	builder.WriteString("</detail></s:Fault>")
	builder.WriteString(soapEnvelopeClose)
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = io.WriteString(w, builder.String())
}

// xmlEscape escapes a value for embedding into an XML document.
func xmlEscape(value string) string {
	var buffer bytes.Buffer
	_ = xml.EscapeText(&buffer, []byte(value))
	return buffer.String()
}

// formatUPnPTime renders seconds as the UPnP "H:MM:SS" form.
func formatUPnPTime(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	total := int(seconds + 0.5)
	return fmt.Sprintf("%d:%02d:%02d", total/3600, (total%3600)/60, total%60)
}

// parseUPnPTime parses "H:MM:SS", "H:MM:SS.mmm" or a raw seconds float.
func parseUPnPTime(value string) (float64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, errors.New("empty time value")
	}
	if !strings.Contains(trimmed, ":") {
		seconds, err := strconv.ParseFloat(trimmed, 64)
		if err != nil || seconds < 0 {
			return 0, fmt.Errorf("invalid time value %q", value)
		}
		return seconds, nil
	}
	parts := strings.Split(trimmed, ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("invalid time value %q", value)
	}
	hours, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || hours < 0 {
		return 0, fmt.Errorf("invalid time value %q", value)
	}
	minutes, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || minutes < 0 || minutes > 59 {
		return 0, fmt.Errorf("invalid time value %q", value)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
	if err != nil || seconds < 0 || seconds >= 60 {
		return 0, fmt.Errorf("invalid time value %q", value)
	}
	return float64(hours*3600+minutes*60) + seconds, nil
}

// parseDIDL extracts title/artist/album from DIDL-Lite metadata. The walk is
// namespace-agnostic, and a metadata blob that is not valid XML never fails
// the cast: the fields simply stay empty.
func parseDIDL(metadata string) (title, artist, album string) {
	trimmed := strings.TrimSpace(metadata)
	if trimmed == "" {
		return "", "", ""
	}
	decoder := xml.NewDecoder(strings.NewReader(trimmed))
	stack := []string{}
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch typed := token.(type) {
		case xml.StartElement:
			stack = append(stack, typed.Name.Local)
		case xml.CharData:
			if len(stack) == 0 {
				continue
			}
			value := strings.TrimSpace(string(typed))
			if value == "" {
				continue
			}
			switch stack[len(stack)-1] {
			case "title":
				if title == "" {
					title = truncateMetadata(value, 200)
				}
			case "artist":
				if artist == "" {
					artist = truncateMetadata(value, 200)
				}
			case "album":
				if album == "" {
					album = truncateMetadata(value, 200)
				}
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return title, artist, album
}

func truncateMetadata(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
