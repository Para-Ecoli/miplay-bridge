package miplay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// mDNS/DNS constants.
const (
	mdnsGroup   = "224.0.0.251"
	mdnsPort    = 5353
	mdnsTTL     = 4500
	mdnsHostTTL = 120

	dnsTypeA   = 1
	dnsTypePTR = 12
	dnsTypeTXT = 16
	dnsTypeSRV = 33
	dnsTypeANY = 255

	dnsClassIN         = 1
	dnsClassCacheFlush = 0x8000
	dnsFlagResponse    = 0x8400

	// ServiceType is the MiPlay/mi-connect service advertisement.
	ServiceType = "_mi-connect._udp.local."

	// CoapPort is the advertised SRV port (the phones probe this port over
	// CoAP; the actual control channel is the TCP port in appsData).
	CoapPort = 56666
)

// Identity is the advertised MiPlay receiver identity.
type Identity struct {
	FriendlyName string
	Instance     string
	Host         string
	DeviceID     []byte // 16 raw bytes of the UUID
	ControlPort  int
	Address      net.IP // IPv4
}

// IDHash is the base64 of the first three sha256 bytes of the device UUID.
func (i Identity) IDHash() string {
	digest := sha256.Sum256(i.DeviceID)
	return base64.StdEncoding.EncodeToString(digest[:3])
}

// AppsData is the base64 container carrying the 25-byte header plus the
// identity JSON, as observed in the TXT record.
func (i Identity) AppsData() (string, error) {
	jsonBytes := []byte(fmt.Sprintf(`{"mico":{"device_id":"%s"}}`, i.canonicalUUID()))
	payload := make([]byte, 25+len(jsonBytes))
	binary.BigEndian.PutUint16(payload[0:2], 1155)
	binary.BigEndian.PutUint16(payload[2:4], uint16(i.ControlPort))
	digest := sha256.Sum256(i.DeviceID)
	payload[4] = (digest[0] & 0xFC) | 0x02
	copy(payload[5:10], digest[1:6])
	// payload[10:24] stays zero; payload[24] is explicitly zero.
	copy(payload[25:], jsonBytes)
	if len(payload) > 255 {
		return "", protocolErrorf("MiPlay appsData exceeds the one-byte container length")
	}
	container := append([]byte{0x81, 0x00, byte(len(payload))}, payload...)
	return base64.StdEncoding.EncodeToString(container), nil
}

func (i Identity) canonicalUUID() string {
	return canonicalUUID(i.DeviceID)
}

func canonicalUUID(raw []byte) string {
	if len(raw) != 16 {
		return ""
	}
	encoded := hex.EncodeToString(raw)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32])
}

// txtRecords renders the TXT properties, one string per record.
func (i Identity) txtRecords() ([]string, error) {
	appsData, err := i.AppsData()
	if err != nil {
		return nil, err
	}
	return []string{
		"name=" + i.FriendlyName,
		"version=65545",
		"apps=[5]",
		"appsData=" + appsData,
		"dev=4",
		"sec=2",
		"flags=Ag==",
		"idHash=" + i.IDHash(),
		"commonData=0",
	}, nil
}

// LoadOrCreateDeviceID reads the persistent UUID (creating it on first run).
// A stable identity is what keeps phones from accumulating duplicate entries
// in their casting pickers.
func LoadOrCreateDeviceID(path string) ([]byte, error) {
	if raw, err := os.ReadFile(path); err == nil {
		text := strings.TrimSpace(string(raw))
		normalized := strings.ReplaceAll(text, "-", "")
		if decoded, decodeErr := hex.DecodeString(normalized); decodeErr == nil && len(decoded) == 16 {
			return decoded, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read device id %s: %w", path, err)
	}
	generated := make([]byte, 16)
	if _, err := rand.Read(generated); err != nil {
		return nil, fmt.Errorf("generate device id: %w", err)
	}
	generated[6] = (generated[6] & 0x0F) | 0x40 // version 4
	generated[8] = (generated[8] & 0x3F) | 0x80 // variant 10
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(canonicalUUID(generated)), 0o644); err != nil {
		return nil, fmt.Errorf("persist device id %s: %w", path, err)
	}
	return generated, nil
}

// sanitizeLabel converts a friendly name into a DNS label that is safe on the
// wire while the UTF-8 original stays in the TXT "name" record.
func sanitizeLabel(value, fallback string) string {
	var builder strings.Builder
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '-', character == '_':
			builder.WriteRune(character)
		default:
			builder.WriteByte('-')
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		result = fallback
	}
	if len(result) > 63 {
		result = result[:63]
	}
	return result
}

// ---------------------------------------------------------------------------
// DNS wire helpers
// ---------------------------------------------------------------------------

func encodeName(name string) []byte {
	trimmed := strings.TrimSuffix(name, ".")
	encoded := make([]byte, 0, len(name)+1)
	if trimmed != "" {
		for _, label := range strings.Split(trimmed, ".") {
			encoded = append(encoded, byte(len(label)))
			encoded = append(encoded, label...)
		}
	}
	return append(encoded, 0)
}

// readName decodes a DNS name at offset, following compression pointers.
func readName(message []byte, offset int) (string, int, error) {
	labels := []string{}
	next := offset
	jumped := false
	for steps := 0; steps < 128; steps++ {
		if offset >= len(message) {
			return "", 0, protocolErrorf("DNS name runs past the message")
		}
		length := int(message[offset])
		switch {
		case length == 0:
			if !jumped {
				next = offset + 1
			}
			return strings.Join(labels, ".") + ".", next, nil
		case length&0xC0 == 0xC0:
			if offset+1 >= len(message) {
				return "", 0, protocolErrorf("DNS compression pointer is truncated")
			}
			pointer := int(message[offset]&0x3F)<<8 | int(message[offset+1])
			if !jumped {
				next = offset + 2
			}
			jumped = true
			offset = pointer
		case length > 63:
			return "", 0, protocolErrorf("DNS label length is invalid")
		default:
			if offset+1+length > len(message) {
				return "", 0, protocolErrorf("DNS label runs past the message")
			}
			labels = append(labels, string(message[offset+1:offset+1+length]))
			offset += 1 + length
		}
	}
	return "", 0, protocolErrorf("DNS name exceeds the compression step limit")
}

type dnsQuestion struct {
	Name  string
	Type  uint16
	Class uint16
}

type dnsRecord struct {
	Name       string
	Type       uint16
	TTL        uint32
	CacheFlush bool
	Data       []byte
}

func appendRecord(buffer []byte, record dnsRecord) []byte {
	buffer = append(buffer, encodeName(record.Name)...)
	buffer = binary.BigEndian.AppendUint16(buffer, record.Type)
	class := uint16(dnsClassIN)
	if record.CacheFlush {
		class |= dnsClassCacheFlush
	}
	buffer = binary.BigEndian.AppendUint16(buffer, class)
	buffer = binary.BigEndian.AppendUint32(buffer, record.TTL)
	buffer = binary.BigEndian.AppendUint16(buffer, uint16(len(record.Data)))
	return append(buffer, record.Data...)
}

func buildMessage(id uint16, flags uint16, questions []dnsQuestion, answers, additionals []dnsRecord) []byte {
	buffer := make([]byte, 0, 512)
	buffer = binary.BigEndian.AppendUint16(buffer, id)
	buffer = binary.BigEndian.AppendUint16(buffer, flags)
	buffer = binary.BigEndian.AppendUint16(buffer, uint16(len(questions)))
	buffer = binary.BigEndian.AppendUint16(buffer, uint16(len(answers)))
	buffer = binary.BigEndian.AppendUint16(buffer, 0)
	buffer = binary.BigEndian.AppendUint16(buffer, uint16(len(additionals)))
	for _, question := range questions {
		buffer = append(buffer, encodeName(question.Name)...)
		buffer = binary.BigEndian.AppendUint16(buffer, question.Type)
		buffer = binary.BigEndian.AppendUint16(buffer, question.Class)
	}
	for _, record := range answers {
		buffer = appendRecord(buffer, record)
	}
	for _, record := range additionals {
		buffer = appendRecord(buffer, record)
	}
	return buffer
}

// ---------------------------------------------------------------------------
// Responder
// ---------------------------------------------------------------------------

// ResponderConfig configures the mDNS advertisement.
type ResponderConfig struct {
	FriendlyName string
	ControlPort  int
	DeviceIDFile string
}

// Responder advertises the receiver over mDNS and answers PTR/SRV/TXT/A
// queries. It is a deliberately small hand-rolled responder: the plugin
// keeps its zero third-party dependency guarantee.
type Responder struct {
	cfg    ResponderConfig
	logger *slog.Logger
	client Identity

	mu    sync.Mutex
	ready bool
}

// NewResponder creates the responder (identity is resolved in Start).
func NewResponder(cfg ResponderConfig, logger *slog.Logger) *Responder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Responder{cfg: cfg, logger: logger}
}

// Identity returns the resolved identity after Start.
func (r *Responder) Identity() Identity { return r.client }

// Start resolves the identity, binds the multicast socket and serves queries
// until the context is cancelled. The advertise address is a parameter rather
// than configuration: the receiver resolves it (explicit override or LAN
// detection) immediately before startup, so the responder can never hold a
// stale copy of the configuration field.
func (r *Responder) Start(ctx context.Context, advertiseAddress string) error {
	address := net.ParseIP(advertiseAddress)
	if address == nil || address.To4() == nil {
		return fmt.Errorf("mDNS advertise address %q is not an IPv4 address", advertiseAddress)
	}
	deviceID, err := LoadOrCreateDeviceID(r.cfg.DeviceIDFile)
	if err != nil {
		return err
	}
	r.client = Identity{
		FriendlyName: r.cfg.FriendlyName,
		Instance:     sanitizeLabel(r.cfg.FriendlyName, "MiPlay-Bridge"),
		Host:         "miplay-bridge",
		DeviceID:     deviceID,
		ControlPort:  r.cfg.ControlPort,
		Address:      address.To4(),
	}
	if _, err := r.client.txtRecords(); err != nil {
		return err
	}

	connection, err := listenMDNS()
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.ready = true
	r.mu.Unlock()
	r.logger.Info("miplay mDNS responder online",
		"service", ServiceType,
		"instance", r.client.Instance,
		"control_port", r.client.ControlPort,
		"device_id", r.client.canonicalUUID(),
	)

	go func() {
		<-ctx.Done()
		_ = connection.Close()
	}()
	go r.announceLoop(ctx, connection)
	r.serve(ctx, connection)
	return ctx.Err()
}

// serve reads multicast queries and answers them.
func (r *Responder) serve(ctx context.Context, connection *net.UDPConn) {
	buffer := make([]byte, 9000)
	for {
		length, source, err := connection.ReadFromUDP(buffer)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			r.logger.Debug("miplay mDNS read error", "error", err)
			continue
		}
		response := r.handleQuery(buffer[:length], source)
		if response == nil {
			continue
		}
		destination := source
		if source.Port == mdnsPort {
			// Multicast query: answer to the group so every listener caches.
			destination = &net.UDPAddr{IP: net.ParseIP(mdnsGroup), Port: mdnsPort}
		}
		if _, err := connection.WriteToUDP(response, destination); err != nil {
			r.logger.Debug("miplay mDNS write error", "error", err)
		}
	}
}

// handleQuery parses a query and builds the matching response, or nil when
// the query does not concern us.
func (r *Responder) handleQuery(message []byte, source *net.UDPAddr) []byte {
	if len(message) < 12 {
		return nil
	}
	flags := binary.BigEndian.Uint16(message[2:4])
	if flags&0x8000 != 0 {
		return nil // a response, not a query
	}
	questionCount := int(binary.BigEndian.Uint16(message[4:6]))
	offset := 12
	questions := make([]dnsQuestion, 0, questionCount)
	for index := 0; index < questionCount; index++ {
		name, next, err := readName(message, offset)
		if err != nil || next+4 > len(message) {
			return nil
		}
		questions = append(questions, dnsQuestion{
			Name:  name,
			Type:  binary.BigEndian.Uint16(message[next : next+2]),
			Class: binary.BigEndian.Uint16(message[next+2 : next+4]),
		})
		offset = next + 4
	}

	instanceName := r.client.Instance + "." + ServiceType
	hostName := r.client.Host + ".local."
	answers := []dnsRecord{}
	additionals := []dnsRecord{}

	addFullRecordSet := func() {
		ptrData := encodeName(instanceName)
		answers = append(answers, dnsRecord{Name: ServiceType, Type: dnsTypePTR, TTL: mdnsTTL, Data: ptrData})
		answers = append(answers, r.srvRecord(instanceName, hostName))
		if txt := r.txtRecord(instanceName); txt != nil {
			answers = append(answers, *txt)
		}
		additionals = append(additionals, r.addressRecord(hostName))
	}

	for _, question := range questions {
		switch {
		case question.Type == dnsTypePTR && strings.EqualFold(question.Name, ServiceType):
			addFullRecordSet()
		case (question.Type == dnsTypeSRV || question.Type == dnsTypeANY) && strings.EqualFold(question.Name, instanceName):
			answers = append(answers, r.srvRecord(instanceName, hostName))
			additionals = append(additionals, r.addressRecord(hostName))
		case (question.Type == dnsTypeTXT || question.Type == dnsTypeANY) && strings.EqualFold(question.Name, instanceName):
			if txt := r.txtRecord(instanceName); txt != nil {
				answers = append(answers, *txt)
			}
		case (question.Type == dnsTypeA || question.Type == dnsTypeANY) && strings.EqualFold(question.Name, hostName):
			answers = append(answers, r.addressRecord(hostName))
		}
	}
	if len(answers) == 0 {
		return nil
	}
	id := binary.BigEndian.Uint16(message[0:2])
	if source != nil && source.Port == mdnsPort {
		id = 0
	}
	return buildMessage(id, dnsFlagResponse, nil, answers, additionals)
}

func (r *Responder) srvRecord(instanceName, hostName string) dnsRecord {
	data := make([]byte, 0, 32)
	data = binary.BigEndian.AppendUint16(data, 0) // priority
	data = binary.BigEndian.AppendUint16(data, 0) // weight
	data = binary.BigEndian.AppendUint16(data, CoapPort)
	data = append(data, encodeName(hostName)...)
	return dnsRecord{Name: instanceName, Type: dnsTypeSRV, TTL: mdnsTTL, CacheFlush: true, Data: data}
}

func (r *Responder) txtRecord(instanceName string) *dnsRecord {
	records, err := r.client.txtRecords()
	if err != nil {
		return nil
	}
	data := make([]byte, 0, 128)
	for _, text := range records {
		if len(text) > 255 {
			continue
		}
		data = append(data, byte(len(text)))
		data = append(data, text...)
	}
	return &dnsRecord{Name: instanceName, Type: dnsTypeTXT, TTL: mdnsTTL, CacheFlush: true, Data: data}
}

func (r *Responder) addressRecord(hostName string) dnsRecord {
	return dnsRecord{Name: hostName, Type: dnsTypeA, TTL: mdnsHostTTL, CacheFlush: true, Data: []byte(r.client.Address.To4())}
}

// announceLoop sends unsolicited announcements so phones see the device
// without waiting for a query cycle.
func (r *Responder) announceLoop(ctx context.Context, connection *net.UDPConn) {
	instanceName := r.client.Instance + "." + ServiceType
	hostName := r.client.Host + ".local."
	answers := []dnsRecord{
		{Name: ServiceType, Type: dnsTypePTR, TTL: mdnsTTL, Data: encodeName(instanceName)},
		r.srvRecord(instanceName, hostName),
	}
	if txt := r.txtRecord(instanceName); txt != nil {
		answers = append(answers, *txt)
	}
	answers = append(answers, r.addressRecord(hostName))
	announcement := buildMessage(0, dnsFlagResponse, nil, answers, nil)
	destination := &net.UDPAddr{IP: net.ParseIP(mdnsGroup), Port: mdnsPort}

	send := func() {
		if _, err := connection.WriteToUDP(announcement, destination); err != nil {
			r.logger.Debug("miplay mDNS announce failed", "error", err)
		}
	}
	send()
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Second):
	}
	send()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

// listenMDNS binds 5353/udp with address and port reuse so the responder can
// coexist with a host avahi/systemd-resolved instance, and joins the mDNS
// group on every IPv4 interface.
func listenMDNS() (*net.UDPConn, error) {
	listener := net.ListenConfig{
		Control: func(network, address string, raw syscall.RawConn) error {
			var controlErr error
			if err := raw.Control(func(fd uintptr) {
				if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
					controlErr = err
					return
				}
				if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1); err != nil {
					controlErr = err
					return
				}
				controlErr = joinMDNSGroup(int(fd))
			}); err != nil {
				return err
			}
			return controlErr
		},
	}
	packetConnection, err := listener.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", mdnsPort))
	if err != nil {
		return nil, fmt.Errorf("bind mDNS %d/udp: %w", mdnsPort, err)
	}
	connection, ok := packetConnection.(*net.UDPConn)
	if !ok {
		packetConnection.Close()
		return nil, fmt.Errorf("mDNS listener is not a UDP connection")
	}
	return connection, nil
}

func joinMDNSGroup(fd int) error {
	group := net.ParseIP(mdnsGroup).To4()
	joined := 0
	interfaces, err := net.Interfaces()
	if err == nil {
		for _, networkInterface := range interfaces {
			if networkInterface.Flags&net.FlagUp == 0 || networkInterface.Flags&net.FlagMulticast == 0 {
				continue
			}
			addresses, addressErr := networkInterface.Addrs()
			if addressErr != nil {
				continue
			}
			for _, address := range addresses {
				ipNet, ok := address.(*net.IPNet)
				if !ok || ipNet.IP.To4() == nil {
					continue
				}
				var request syscall.IPMreq
				copy(request.Multiaddr[:], group)
				copy(request.Interface[:], ipNet.IP.To4())
				if err := syscall.SetsockoptIPMreq(fd, syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, &request); err == nil {
					joined++
				}
			}
		}
	}
	if joined == 0 {
		var request syscall.IPMreq
		copy(request.Multiaddr[:], group)
		if err := syscall.SetsockoptIPMreq(fd, syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, &request); err != nil {
			return fmt.Errorf("join mDNS group: %w", err)
		}
	}
	return nil
}

// DetectAdvertiseAddress picks the LAN IPv4 used in advertisements: the
// interface address that carries the default route to the outside, with a
// hostname lookup fallback.
func DetectAdvertiseAddress() (string, error) {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	connection, err := dialer.Dial("udp4", "203.0.113.1:9") // TEST-NET, no packets sent
	if err == nil {
		defer connection.Close()
		if udpAddress, ok := connection.LocalAddr().(*net.UDPAddr); ok && udpAddress.IP.To4() != nil {
			return udpAddress.IP.String(), nil
		}
	}
	hostname, hostnameErr := os.Hostname()
	if hostnameErr != nil {
		return "", fmt.Errorf("detect advertise address: %v / %v", err, hostnameErr)
	}
	addresses, lookupErr := net.LookupHost(hostname)
	if lookupErr != nil {
		return "", fmt.Errorf("detect advertise address: %v / %v", err, lookupErr)
	}
	for _, candidate := range addresses {
		if ip := net.ParseIP(candidate); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("no LAN IPv4 address found")
}

// Advertising reports whether the responder is bound.
func (r *Responder) Advertising() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready
}
