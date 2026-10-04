// SSDP discovery: multicast search responses plus alive/byebye announcements
// so DLNA controllers find the renderer without manual configuration.
package dlna

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	ssdpGroup = "239.255.255.250"
	ssdpPort  = 1900
)

// ssdpTarget is one advertisement endpoint (NT/USN pair).
type ssdpTarget struct {
	NotificationType  string
	UniqueServiceName string
}

// ssdpServer advertises the MediaRenderer over SSDP and answers M-SEARCH.
type ssdpServer struct {
	logger  *slog.Logger
	address string
	port    int
	udn     string
	server  string
	maxAge  int
	bootID  int64

	mu   sync.Mutex
	conn *net.UDPConn
}

func newSSDPServer(logger *slog.Logger, address string, port int, udn, version string, maxAge int) *ssdpServer {
	return &ssdpServer{
		logger:  logger,
		address: address,
		port:    port,
		udn:     udn,
		server:  serverHeader(version),
		maxAge:  maxAge,
		bootID:  time.Now().Unix(),
	}
}

// serverHeader is the SERVER header value shared by SSDP and GENA responses.
func serverHeader(version string) string {
	return "Linux/6.1 UPnP/1.0 MiPlay-Bridge/" + version
}

// location is the device description URL carried in SSDP headers.
func (s *ssdpServer) location() string {
	return fmt.Sprintf("http://%s:%d/dlna/description.xml", s.address, s.port)
}

// targets lists the advertised NT/USN pairs: root device, the UDN itself,
// the device type and every service type.
func (s *ssdpServer) targets() []ssdpTarget {
	targets := []ssdpTarget{
		{NotificationType: "upnp:rootdevice", UniqueServiceName: s.udn + "::upnp:rootdevice"},
		{NotificationType: s.udn, UniqueServiceName: s.udn},
		{NotificationType: "urn:schemas-upnp-org:device:MediaRenderer:1",
			UniqueServiceName: s.udn + "::urn:schemas-upnp-org:device:MediaRenderer:1"},
	}
	for _, service := range mediaRendererServices {
		targets = append(targets, ssdpTarget{
			NotificationType:  service.Type,
			UniqueServiceName: s.udn + "::" + service.Type,
		})
	}
	return targets
}

// start binds the SSDP socket and launches the responder and announcer.
func (s *ssdpServer) start(ctx context.Context) error {
	connection, err := listenSSDP()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.conn = connection
	s.mu.Unlock()
	go s.serve(ctx, connection)
	go s.announce(ctx, connection)
	s.logger.Info("dlna ssdp advertising", "location", s.location(), "udn", s.udn)
	return nil
}

// stop sends the byebye cascade and closes the socket.
func (s *ssdpServer) stop() {
	s.mu.Lock()
	connection := s.conn
	s.conn = nil
	s.mu.Unlock()
	if connection == nil {
		return
	}
	destination := &net.UDPAddr{IP: net.ParseIP(ssdpGroup), Port: ssdpPort}
	for _, target := range s.targets() {
		_, _ = connection.WriteToUDP([]byte(s.notifyMessage(target, "ssdp:byebye")), destination)
	}
	_ = connection.Close()
}

// serve answers incoming M-SEARCH datagrams.
func (s *ssdpServer) serve(ctx context.Context, connection *net.UDPConn) {
	buffer := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return
		}
		_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
		length, source, err := connection.ReadFromUDP(buffer)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			if isTimeoutError(err) {
				continue
			}
			s.logger.Debug("ssdp read failed", "error", err)
			continue
		}
		datagram := string(buffer[:length])
		go s.handleSearch(ctx, connection, datagram, source)
	}
}

// handleSearch answers one M-SEARCH datagram, honouring the MX jitter so
// several devices on the same LAN do not answer in lockstep.
func (s *ssdpServer) handleSearch(ctx context.Context, connection *net.UDPConn, datagram string, source *net.UDPAddr) {
	lines := strings.Split(datagram, "\r\n")
	if len(lines) == 0 || !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(lines[0])), "M-SEARCH ") {
		return
	}
	headers := map[string]string{}
	for _, line := range lines[1:] {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}
	if man := headers["man"]; man != "" && !strings.Contains(man, "ssdp:discover") {
		return
	}
	searchTarget := headers["st"]
	if searchTarget == "" {
		return
	}
	jitter := 0
	if mx, err := strconv.Atoi(headers["mx"]); err == nil && mx > 0 {
		if mx > 3 {
			mx = 3
		}
		jitter = rand.Intn(mx * 1000)
	}
	if jitter > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(jitter) * time.Millisecond):
		}
	}
	for _, target := range s.matchingTargets(searchTarget) {
		_, _ = connection.WriteToUDP([]byte(s.searchResponse(target)), source)
	}
}

// matchingTargets resolves an ST value against the advertised set.
func (s *ssdpServer) matchingTargets(searchTarget string) []ssdpTarget {
	if searchTarget == "ssdp:all" {
		return s.targets()
	}
	matched := []ssdpTarget{}
	for _, target := range s.targets() {
		if target.NotificationType == searchTarget {
			matched = append(matched, target)
		}
	}
	return matched
}

func (s *ssdpServer) searchResponse(target ssdpTarget) string {
	return strings.Join([]string{
		"HTTP/1.1 200 OK",
		"HOST: " + ssdpGroup + ":" + strconv.Itoa(ssdpPort),
		"CACHE-CONTROL: max-age=" + strconv.Itoa(s.maxAge),
		"DATE: " + time.Now().UTC().Format(http.TimeFormat),
		"EXT:",
		"LOCATION: " + s.location(),
		"SERVER: " + s.server,
		"ST: " + target.NotificationType,
		"USN: " + target.UniqueServiceName,
		"BOOTID.UPNP.ORG: " + strconv.FormatInt(s.bootID, 10),
		"CONFIGID.UPNP.ORG: 1",
		"", "",
	}, "\r\n")
}

func (s *ssdpServer) notifyMessage(target ssdpTarget, nts string) string {
	lines := []string{
		"NOTIFY * HTTP/1.1",
		"HOST: " + ssdpGroup + ":" + strconv.Itoa(ssdpPort),
	}
	if nts == "ssdp:alive" {
		lines = append(lines,
			"CACHE-CONTROL: max-age="+strconv.Itoa(s.maxAge),
			"LOCATION: "+s.location(),
			"SERVER: "+s.server,
		)
	}
	lines = append(lines,
		"NT: "+target.NotificationType,
		"NTS: "+nts,
		"USN: "+target.UniqueServiceName,
		"BOOTID.UPNP.ORG: "+strconv.FormatInt(s.bootID, 10),
		"CONFIGID.UPNP.ORG: 1",
		"", "",
	)
	return strings.Join(lines, "\r\n")
}

// announce sends the alive cascade twice at startup, then refreshes it at
// half the advertised max-age.
func (s *ssdpServer) announce(ctx context.Context, connection *net.UDPConn) {
	destination := &net.UDPAddr{IP: net.ParseIP(ssdpGroup), Port: ssdpPort}
	sendAlive := func() {
		for _, target := range s.targets() {
			if _, err := connection.WriteToUDP([]byte(s.notifyMessage(target, "ssdp:alive")), destination); err != nil {
				s.logger.Debug("ssdp announce failed", "error", err)
			}
		}
	}
	sendAlive()
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Second):
	}
	sendAlive()
	refresh := time.Duration(s.maxAge) * time.Second / 2
	ticker := time.NewTicker(refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendAlive()
		}
	}
}

// ---------------------------------------------------------------------------
// socket helpers
// ---------------------------------------------------------------------------

// listenSSDP binds 1900/udp with address and port reuse and joins the SSDP
// group on every multicast-capable IPv4 interface.
func listenSSDP() (*net.UDPConn, error) {
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
				controlErr = joinSSDPGroup(int(fd))
			}); err != nil {
				return err
			}
			return controlErr
		},
	}
	packetConnection, err := listener.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", ssdpPort))
	if err != nil {
		return nil, fmt.Errorf("bind ssdp %d/udp: %w", ssdpPort, err)
	}
	connection, ok := packetConnection.(*net.UDPConn)
	if !ok {
		packetConnection.Close()
		return nil, errors.New("ssdp listener is not a UDP connection")
	}
	return connection, nil
}

func joinSSDPGroup(fd int) error {
	group := net.ParseIP(ssdpGroup).To4()
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
			return fmt.Errorf("join ssdp group: %w", err)
		}
	}
	return nil
}

func isTimeoutError(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

// detectAdvertiseAddress picks the LAN IPv4 used in LOCATION headers: the
// interface address that carries the default route, with a hostname fallback.
func detectAdvertiseAddress() (string, error) {
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
		return "", fmt.Errorf("detect dlna advertise address: %v / %v", err, hostnameErr)
	}
	addresses, lookupErr := net.LookupHost(hostname)
	if lookupErr != nil {
		return "", fmt.Errorf("detect dlna advertise address: %v / %v", err, lookupErr)
	}
	for _, candidate := range addresses {
		if ip := net.ParseIP(candidate); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
			return ip.String(), nil
		}
	}
	return "", errors.New("no LAN IPv4 address found")
}
