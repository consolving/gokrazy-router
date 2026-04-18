// Package dhcp implements a minimal DHCPv4 server for LAN clients.
package dhcp

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
)

// LeaseCallback is called when a new client gets a lease.
type LeaseCallback func(ip net.IP, mac string)

// Server is a minimal DHCPv4 server that hands out leases from a
// configured range on a specific interface.
type Server struct {
	iface     string
	serverIP  net.IP
	mask      net.IPMask
	rangeStart net.IP
	rangeEnd   net.IP
	dns       []net.IP
	lease     time.Duration
	router    net.IP

	mu        sync.Mutex
	leases    map[string]lease // MAC -> lease
	nextIP    net.IP
	onLease   LeaseCallback
}

type lease struct {
	IP      net.IP
	Expires time.Time
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

// Run starts the DHCP server. It blocks until an error occurs.
func (s *Server) Run() error {
	laddr := &net.UDPAddr{IP: net.IPv4zero, Port: 67}
	srv, err := server4.NewServer(s.iface, laddr, s.handler, server4.WithSummaryLogger())
	if err != nil {
		return fmt.Errorf("dhcp server: %w", err)
	}
	log.Printf("dhcp: serving on %s (%s, range %s-%s)", s.iface, s.serverIP, s.rangeStart, s.rangeEnd)
	return srv.Serve()
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

	// Return existing lease if valid.
	if l, ok := s.leases[mac]; ok && time.Now().Before(l.Expires) {
		return l.IP
	}

	// Find next free IP.
	ip := dupIP(s.nextIP)
	for {
		if !s.isLeased(ip) {
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

func (s *Server) isLeased(ip net.IP) bool {
	for _, l := range s.leases {
		if l.IP.Equal(ip) && time.Now().Before(l.Expires) {
			return true
		}
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
