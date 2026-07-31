// Package dhcp implements a minimal DHCPv4 server for LAN clients.
package dhcp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"golang.org/x/sys/unix"
)

// LeaseCallback is called when a new client gets a lease.
type LeaseCallback func(ip net.IP, mac string)

// LeaseExpiredCallback is called when a client's lease expires without renewal.
type LeaseExpiredCallback func(ip net.IP, mac string)

// Server is a minimal DHCPv4 server that hands out leases from a
// configured range on a specific interface.
type Server struct {
	iface      string
	serverIP   net.IP
	mask       net.IPMask
	rangeStart net.IP
	rangeEnd   net.IP
	dns        []net.IP
	lease      time.Duration
	router     net.IP

	mu              sync.Mutex
	leases          map[string]lease // MAC -> lease
	nextIP          net.IP
	reservations    map[string]net.IP // normalized MAC -> IP
	pxeBootServer   net.IP
	pxeBootFile     string
	macPXEBootFiles map[string]string // normalized MAC -> bootfile (DHCP option 67)
	ipxeBootFile    string            // bootfile for clients that already run iPXE
	uefiBootFile    string            // bootfile for UEFI PXE clients
	legacyBootFile  string            // bootfile for legacy BIOS PXE clients
	onLease         LeaseCallback
	onLeaseExpired  LeaseExpiredCallback
}

type lease struct {
	IP      net.IP
	Expires time.Time
}

// Reservation describes optional static lease and PXE overrides for a MAC address.
type Reservation struct {
	IP          string
	PXEBootFile string // optional per-client PXE boot file
}

// New creates a DHCP server. serverAddr is in CIDR notation (e.g. "10.0.0.1/24").
func New(iface, serverAddr, rangeStart, rangeEnd string, dns []string, leaseDur time.Duration) (*Server, error) {
	sIP, sNet, err := net.ParseCIDR(serverAddr)
	if err != nil {
		return nil, fmt.Errorf("parse server addr: %w", err)
	}

	parsedDNS := make([]net.IP, 0, len(dns))
	for _, d := range dns {
		ip := net.ParseIP(d)
		if ip == nil {
			return nil, fmt.Errorf("invalid DNS server: %s", d)
		}
		parsedDNS = append(parsedDNS, ip.To4())
	}

	rs := net.ParseIP(rangeStart).To4()
	re := net.ParseIP(rangeEnd).To4()
	if rs == nil || re == nil {
		return nil, fmt.Errorf("invalid DHCP range")
	}

	s := &Server{
		iface:      iface,
		serverIP:   sIP.To4(),
		mask:       sNet.Mask,
		rangeStart: rs,
		rangeEnd:   re,
		dns:        parsedDNS,
		lease:      leaseDur,
		router:     sIP.To4(),
		leases:     make(map[string]lease),
		nextIP:     dupIP(rs),
	}
	return s, nil
}

// OnLease registers a callback for new lease assignments.
func (s *Server) OnLease(cb LeaseCallback) {
	s.onLease = cb
}

// OnLeaseExpired registers a callback for lease expirations.
func (s *Server) OnLeaseExpired(cb LeaseExpiredCallback) {
	s.onLeaseExpired = cb
}

// SetReservations replaces the static MAC -> IP reservation map.
// MAC addresses are normalized to lower-case xx:xx:xx:xx:xx:xx.
// Reservations outside this server's subnet are ignored so a reservation
// for one VLAN is not accidentally served by another VLAN's DHCP server.
func (s *Server) SetReservations(reservations map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reservations = make(map[string]net.IP, len(reservations))
	subnet := &net.IPNet{IP: s.serverIP.Mask(s.mask), Mask: s.mask}
	for mac, ip := range reservations {
		parsed := net.ParseIP(ip).To4()
		if parsed == nil {
			continue
		}
		if !subnet.Contains(parsed) {
			continue
		}
		s.reservations[normalizeMAC(mac)] = parsed
	}
}

// SetPXEOptions configures DHCP option 66 (TFTP server) and 67 (boot file).
func (s *Server) SetPXEOptions(bootServer string, bootFile string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pxeBootServer = nil
	if bootServer != "" {
		if ip := net.ParseIP(bootServer).To4(); ip != nil {
			s.pxeBootServer = ip
		}
	}
	s.pxeBootFile = bootFile
}

// SetMacPXEBootFiles replaces the per-client DHCP option 67 bootfile map.
func (s *Server) SetMacPXEBootFiles(m map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.macPXEBootFiles = make(map[string]string, len(m))
	for mac, file := range m {
		s.macPXEBootFiles[normalizeMAC(mac)] = file
	}
}

// SetIPXEBootFile sets the bootfile offered to clients that identify as iPXE.
func (s *Server) SetIPXEBootFile(file string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ipxeBootFile = file
}

// SetUEFIBootFile sets the bootfile offered to UEFI PXE clients.
func (s *Server) SetUEFIBootFile(file string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uefiBootFile = file
}

// SetLegacyBootFile sets the bootfile offered to legacy BIOS PXE clients.
func (s *Server) SetLegacyBootFile(file string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.legacyBootFile = file
}

func normalizeMAC(mac string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(mac, "-", ":")))
}

// Run starts the DHCP server. It blocks until an error occurs.
func (s *Server) Run() error {
	// Create a UDP socket bound exclusively to our interface.
	// We intentionally avoid SO_REUSEPORT (which the library's default
	// NewIPv4UDPConn enables) because with multiple DHCP servers on
	// different interfaces, SO_REUSEPORT causes the kernel to
	// load-balance broadcast packets across sockets, delivering
	// requests to the wrong server.
	conn, err := newDHCPConn(s.iface)
	if err != nil {
		return fmt.Errorf("dhcp server: %w", err)
	}
	srv, err := server4.NewServer(s.iface, nil, s.handler,
		server4.WithSummaryLogger(),
		server4.WithConn(conn))
	if err != nil {
		return fmt.Errorf("dhcp server: %w", err)
	}
	log.Printf("dhcp: serving on %s (%s, range %s-%s)", s.iface, s.serverIP, s.rangeStart, s.rangeEnd)

	// Start lease expiry checker.
	go s.reapExpiredLeases()

	return srv.Serve()
}

// newDHCPConn creates a UDP socket on port 67 bound to the given interface,
// with SO_BROADCAST and SO_REUSEADDR but without SO_REUSEPORT.
func newDHCPConn(iface string) (*net.UDPConn, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	f := os.NewFile(uintptr(fd), "dhcp-"+iface)
	defer f.Close()

	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BROADCAST, 1); err != nil {
		return nil, fmt.Errorf("SO_BROADCAST: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		return nil, fmt.Errorf("SO_REUSEADDR: %w", err)
	}
	// Bind to interface so we only receive packets from this interface.
	if err := unix.BindToDevice(fd, iface); err != nil {
		return nil, fmt.Errorf("SO_BINDTODEVICE(%s): %w", iface, err)
	}
	sa := unix.SockaddrInet4{Port: 67}
	if err := unix.Bind(fd, &sa); err != nil {
		return nil, fmt.Errorf("bind :67: %w", err)
	}
	conn, err := net.FilePacketConn(f)
	if err != nil {
		return nil, err
	}
	return conn.(*net.UDPConn), nil
}

// reapExpiredLeases periodically checks for expired leases and fires callbacks.
func (s *Server) reapExpiredLeases() {
	// Check every 1/4 of the lease duration, minimum 30 seconds.
	interval := s.lease / 4
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		var expired []lease
		var expiredMACs []string
		for mac, l := range s.leases {
			if now.After(l.Expires) {
				expired = append(expired, l)
				expiredMACs = append(expiredMACs, mac)
			}
		}
		for _, mac := range expiredMACs {
			delete(s.leases, mac)
		}
		s.mu.Unlock()

		if s.onLeaseExpired != nil {
			for i, l := range expired {
				log.Printf("dhcp: lease expired for %s (%s)", l.IP, expiredMACs[i])
				s.onLeaseExpired(dupIP(l.IP), expiredMACs[i])
			}
		}
	}
}

func (s *Server) handler(conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4) {
	log.Printf("dhcp: received %s from %s (hw=%s)", req.MessageType(), peer, req.ClientHWAddr)
	if req.OpCode != dhcpv4.OpcodeBootRequest {
		return
	}

	mac := req.ClientHWAddr.String()
	msgType := req.MessageType()

	var resp *dhcpv4.DHCPv4
	var err error
	opts := s.replyOptions(mac, req)

	switch msgType {
	case dhcpv4.MessageTypeDiscover:
		ip := s.allocate(mac)
		resp, err = dhcpv4.NewReplyFromRequest(req,
			dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer),
			dhcpv4.WithServerIP(s.serverIP),
			dhcpv4.WithYourIP(ip),
			dhcpv4.WithOption(dhcpv4.OptSubnetMask(s.mask)),
			dhcpv4.WithOption(dhcpv4.OptRouter(s.router)),
			dhcpv4.WithOption(dhcpv4.OptDNS(s.dns...)),
			dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(s.lease)),
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(s.serverIP)),
			opts,
		)
	case dhcpv4.MessageTypeRequest:
		ip := s.allocate(mac)
		resp, err = dhcpv4.NewReplyFromRequest(req,
			dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
			dhcpv4.WithServerIP(s.serverIP),
			dhcpv4.WithYourIP(ip),
			dhcpv4.WithOption(dhcpv4.OptSubnetMask(s.mask)),
			dhcpv4.WithOption(dhcpv4.OptRouter(s.router)),
			dhcpv4.WithOption(dhcpv4.OptDNS(s.dns...)),
			dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(s.lease)),
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(s.serverIP)),
			opts,
		)
		log.Printf("dhcp: ACK %s -> %s", mac, ip)
	default:
		return
	}

	if err != nil {
		log.Printf("dhcp: error building reply: %v", err)
		return
	}

	// DHCP clients without an IP send from 0.0.0.0 — we must reply to broadcast.
	dst := peer
	if upeer, ok := peer.(*net.UDPAddr); ok {
		if upeer.IP == nil || upeer.IP.To4().Equal(net.IPv4zero) {
			dst = &net.UDPAddr{IP: net.IPv4bcast, Port: 68}
		}
	}

	if _, err := conn.WriteTo(resp.ToBytes(), dst); err != nil {
		log.Printf("dhcp: error sending reply to %s: %v", dst, err)
	} else {
		log.Printf("dhcp: sent %s to %s", resp.MessageType(), dst)
	}
}

func (s *Server) allocate(mac string) net.IP {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalized := normalizeMAC(mac)

	// Honor static reservation first.
	if reserved, ok := s.reservations[normalized]; ok {
		// Ensure an expired dynamic lease for that IP is not handed out to someone else.
		s.clearLeasesForIP(reserved)
		s.leases[mac] = lease{IP: dupIP(reserved), Expires: time.Now().Add(s.lease)}
		if s.onLease != nil {
			go s.onLease(dupIP(reserved), mac)
		}
		return reserved
	}

	// Return existing lease if valid.
	if l, ok := s.leases[mac]; ok && time.Now().Before(l.Expires) {
		return l.IP
	}

	// Find next free IP.
	ip := dupIP(s.nextIP)
	for {
		if !s.isLeasedOrReserved(ip) {
			break
		}
		ip = incIP(ip)
		if ip.Equal(s.rangeEnd) || ipGreater(ip, s.rangeEnd) {
			ip = dupIP(s.rangeStart) // wrap around
			break
		}
	}

	s.leases[mac] = lease{IP: dupIP(ip), Expires: time.Now().Add(s.lease)}
	s.nextIP = incIP(ip)
	if ipGreater(s.nextIP, s.rangeEnd) {
		s.nextIP = dupIP(s.rangeStart)
	}

	// Notify callback (outside the hot path — fire and forget).
	if s.onLease != nil {
		go s.onLease(dupIP(ip), mac)
	}

	return ip
}

func (s *Server) isLeasedOrReserved(ip net.IP) bool {
	for _, reserved := range s.reservations {
		if reserved.Equal(ip) {
			return true
		}
	}
	for _, l := range s.leases {
		if l.IP.Equal(ip) && time.Now().Before(l.Expires) {
			return true
		}
	}
	return false
}

func (s *Server) clearLeasesForIP(ip net.IP) {
	for mac, l := range s.leases {
		if l.IP.Equal(ip) {
			delete(s.leases, mac)
		}
	}
}

func (s *Server) replyOptions(mac string, req *dhcpv4.DHCPv4) dhcpv4.Modifier {
	return func(d *dhcpv4.DHCPv4) {
		s.mu.Lock()
		bootServer := s.pxeBootServer
		bootFile := s.pxeBootFile
		macFile, hasMacFile := s.macPXEBootFiles[normalizeMAC(mac)]
		ipxe := s.ipxeBootFile
		uefi := s.uefiBootFile
		legacy := s.legacyBootFile
		s.mu.Unlock()

		classified := ""
		switch {
		case ipxe != "" && req != nil && isIPXEClient(req):
			bootFile = ipxe
			classified = "ipxe"
		case hasMacFile:
			bootFile = macFile
			classified = "mac"
		case uefi != "" && req != nil && isUEFIClient(req):
			bootFile = uefi
			classified = "uefi"
		case legacy != "":
			bootFile = legacy
			classified = "legacy"
		}

		if bootServer != nil {
			log.Printf("dhcp: %s sending option 66 tftp=%s option 67 bootfile=%s", mac, bootServer, bootFile)
			d.UpdateOption(dhcpv4.OptTFTPServerName(bootServer.String()))
		}
		if bootFile != "" {
			log.Printf("dhcp: %s selected %s bootfile=%s", mac, classified, bootFile)
			d.UpdateOption(dhcpv4.OptBootFileName(bootFile))
		}
	}
}

func isIPXEClient(req *dhcpv4.DHCPv4) bool {
	uc := req.Options.Get(dhcpv4.OptionUserClassInformation)
	return len(uc) > 0 && bytes.Contains(bytes.ToLower(uc), []byte("ipxe"))
}

func isUEFIClient(req *dhcpv4.DHCPv4) bool {
	archBytes := req.Options.Get(dhcpv4.OptionClientSystemArchitectureType)
	if len(archBytes) < 2 {
		return false
	}
	switch binary.BigEndian.Uint16(archBytes) {
	case 7, 9:
		return true
	}
	return false
}

func dupIP(ip net.IP) net.IP {
	dup := make(net.IP, len(ip))
	copy(dup, ip)
	return dup
}

func incIP(ip net.IP) net.IP {
	next := dupIP(ip)
	for i := len(next) - 1; i >= 0; i-- {
		next[i]++
		if next[i] != 0 {
			break
		}
	}
	return next
}

func ipGreater(a, b net.IP) bool {
	a = a.To4()
	b = b.To4()
	for i := 0; i < 4; i++ {
		if a[i] > b[i] {
			return true
		}
		if a[i] < b[i] {
			return false
		}
	}
	return false
}
