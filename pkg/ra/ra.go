// Package ra implements an IPv6 Router Advertisement (SLAAC) server and a
// stateful/stateless DHCPv6 server for LAN scopes.
package ra

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"time"

	"github.com/mdlayher/ndp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	raInterval = 30 * time.Second

	prefixValid     = 2 * time.Hour
	prefixPreferred = 1 * time.Hour
	routerLifetime  = 1800 * time.Second
)

// Config describes one IPv6 scope (interface) to advertise.
type Config struct {
	Interface string   // interface name, e.g. "br-vlan1" or "wlan0"
	Address6  string   // router address on the interface, CIDR, e.g. "fd00::1/64"
	DNS6      []string // recursive DNS servers announced via RDNSS and DHCPv6
	Managed   bool     // set the M flag: clients should use DHCPv6 for addresses
	Other     bool     // set the O flag: clients should use DHCPv6 for other config
}

// Server periodically sends Router Advertisements and answers Router
// Solicitations on one interface.
type Server struct {
	cfg       Config
	prefix    netip.Addr
	prefixLen uint8

	conn   *ndp.Conn
	stopCh chan struct{}
	done   chan struct{}
}

// New validates the config and prepares a Server.
func New(cfg Config) (*Server, error) {
	ip, ipNet, err := net.ParseCIDR(cfg.Address6)
	if err != nil {
		return nil, fmt.Errorf("ra: parse address6 %q: %w", cfg.Address6, err)
	}
	if ip.To4() != nil {
		return nil, fmt.Errorf("ra: %q is not an IPv6 address", cfg.Address6)
	}
	ones, _ := ipNet.Mask.Size()
	pfx, ok := netip.AddrFromSlice(ipNet.IP)
	if !ok {
		return nil, fmt.Errorf("ra: invalid prefix %q", ipNet.IP)
	}
	return &Server{
		cfg:       cfg,
		prefix:    pfx,
		prefixLen: uint8(ones),
		stopCh:    make(chan struct{}),
	}, nil
}

// Start opens the NDP connection and begins advertising. It is non-blocking.
func (s *Server) Start() error {
	ifi, err := net.InterfaceByName(s.cfg.Interface)
	if err != nil {
		return fmt.Errorf("ra: interface %s: %w", s.cfg.Interface, err)
	}
	if err := ensureLinkLocal(ifi); err != nil {
		return fmt.Errorf("ra: %s: %w", s.cfg.Interface, err)
	}
	conn, src, err := ndp.Listen(ifi, ndp.LinkLocal)
	if err != nil {
		return fmt.Errorf("ra: listen on %s: %w", s.cfg.Interface, err)
	}
	s.conn = conn
	s.done = make(chan struct{})

	// Join the all-routers multicast group so we receive Router Solicitations.
	if err := conn.JoinGroup(netip.MustParseAddr("ff02::2")); err != nil {
		log.Printf("ra: %s: join all-routers group: %v", s.cfg.Interface, err)
	}

	go s.loop()
	log.Printf("ra: advertising %s on %s (link-local %s, managed=%t)", s.cfg.Address6, s.cfg.Interface, src, s.cfg.Managed)
	return nil
}

// ensureLinkLocal guarantees the interface has a usable link-local IPv6
// address so the NDP listener can bind. Right after a global address is
// assigned, the kernel generates fe80:: asynchronously via addrconf, and the
// address stays tentative (unbindable) while DAD runs, so we poll until a
// non-tentative link-local address exists. If addrconf never produces one,
// we install fe80::1 explicitly.
func ensureLinkLocal(ifi *net.Interface) error {
	link, err := netlink.LinkByName(ifi.Name)
	if err != nil {
		return fmt.Errorf("lookup %s: %w", ifi.Name, err)
	}

	for i := 0; i < 100; i++ {
		if usableLinkLocal(link) != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	ll, err := netlink.ParseAddr("fe80::1/64")
	if err != nil {
		return fmt.Errorf("parse fe80::1/64: %w", err)
	}
	if err := netlink.AddrAdd(link, ll); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("assign fe80::1/64 to %s: %w", ifi.Name, err)
	}
	for i := 0; i < 100; i++ {
		if usableLinkLocal(link) != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// usableLinkLocal returns a non-tentative link-local unicast address of the
// interface, or nil while none exists or DAD is still in progress.
func usableLinkLocal(link netlink.Link) net.IP {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		ip := a.IP.To16()
		if ip == nil || !ip.IsLinkLocalUnicast() {
			continue
		}
		if a.Flags&unix.IFA_F_TENTATIVE != 0 {
			continue
		}
		return ip
	}
	return nil
}

func (s *Server) loop() {
	defer close(s.done)

	// Send an initial RA so clients do not have to wait for the next interval.
	if err := s.sendRA(); err != nil {
		log.Printf("ra: %s: initial advertisement: %v", s.cfg.Interface, err)
	}

	ticker := time.NewTicker(raInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		// Read a packet; the deadline keeps us responsive to stop/tick.
		s.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		msg, _, src, err := s.conn.ReadFrom()
		switch {
		case err == nil:
			if _, ok := msg.(*ndp.RouterSolicitation); ok {
				if err := s.sendRATo(src); err != nil {
					log.Printf("ra: %s: respond to solicitation from %s: %v", s.cfg.Interface, src, err)
				}
			}
		default:
			// Read timeout or transient error — fall through to the ticker.
		}

		select {
		case <-ticker.C:
			if err := s.sendRA(); err != nil {
				log.Printf("ra: %s: periodic advertisement: %v", s.cfg.Interface, err)
			}
		case <-s.stopCh:
			return
		default:
		}
	}
}

// ra builds the Router Advertisement message for this scope.
func (s *Server) ra() *ndp.RouterAdvertisement {
	ra := &ndp.RouterAdvertisement{
		CurrentHopLimit:      64,
		ManagedConfiguration: s.cfg.Managed,
		OtherConfiguration:   s.cfg.Other,
		RouterLifetime:       routerLifetime,
		Options: []ndp.Option{
			&ndp.PrefixInformation{
				PrefixLength:                   s.prefixLen,
				OnLink:                         true,
				AutonomousAddressConfiguration: true,
				ValidLifetime:                  prefixValid,
				PreferredLifetime:              prefixPreferred,
				Prefix:                         s.prefix,
			},
		},
	}

	var dns []netip.Addr
	for _, d := range s.cfg.DNS6 {
		if a, err := netip.ParseAddr(d); err == nil {
			dns = append(dns, a)
		}
	}
	if len(dns) > 0 {
		ra.Options = append(ra.Options, &ndp.RecursiveDNSServer{
			Lifetime: routerLifetime,
			Servers:  dns,
		})
	}
	return ra
}

// sendRA multicasts an RA to the link-local all-nodes group.
func (s *Server) sendRA() error {
	return s.sendRATo(netip.MustParseAddr("ff02::1"))
}

func (s *Server) sendRATo(dst netip.Addr) error {
	return s.conn.WriteTo(s.ra(), nil, dst)
}

// Stop closes the connection and stops advertising.
func (s *Server) Stop() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	if s.conn == nil {
		return
	}
	s.conn.Close()
	<-s.done
}
