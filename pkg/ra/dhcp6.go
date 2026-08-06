package ra

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/server6"
	"github.com/insomniacslk/dhcp/iana"
)

const (
	dhcp6Preferred = 1 * time.Hour
	dhcp6Valid     = 2 * time.Hour
)

// DHCP6Server is a DHCPv6 server for a single interface. It hands out IA_NA
// addresses from the interface's prefix (stateful) and always announces the
// configured recursive DNS servers (stateless). Both are controlled by the
// caller via the Managed flag in Config: when Managed is set, clients are
// expected to use DHCPv6 for their addresses, otherwise they only use it for
// DNS and other configuration.
type DHCP6Server struct {
	iface    string
	addr6    string // router address, CIDR
	prefix   net.IPNet
	dns      []net.IP
	serverID dhcpv6.DUID

	// RFC 5970 netboot (option 61 -> option 59). pxe6Server is the TFTP
	// server address embedded in the bootfile URL.
	pxe6Server  net.IP
	pxe6Legacy  string
	pxe6UEFI    string

	mu     sync.Mutex
	leases map[string]lease6 // DUID string -> lease
	nextIP net.IP

	onLease        func(ip net.IP, mac string)
	onLeaseExpired func(ip net.IP, mac string)

	srv *server6.Server
}

type lease6 struct {
	IP      net.IP
	MAC     string
	Expires time.Time
}

// NewDHCP6Server validates the scope and prepares a DHCPv6 server. The prefix
// used for IA_NA assignment is taken from addr6's CIDR.
func NewDHCP6Server(iface, addr6 string, dns []string) (*DHCP6Server, error) {
	ip, ipNet, err := net.ParseCIDR(addr6)
	if err != nil {
		return nil, fmt.Errorf("dhcp6: parse address6 %q: %w", addr6, err)
	}
	if ip.To4() != nil {
		return nil, fmt.Errorf("dhcp6: %q is not an IPv6 address", addr6)
	}

	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("dhcp6: interface %s: %w", iface, err)
	}

	var dnsIPs []net.IP
	for _, d := range dns {
		if p := net.ParseIP(d); p != nil && p.To4() == nil {
			dnsIPs = append(dnsIPs, p)
		}
	}

	s := &DHCP6Server{
		iface:    iface,
		addr6:    addr6,
		prefix:   *ipNet,
		dns:      dnsIPs,
		serverID: &dhcpv6.DUIDLL{HWType: iana.HWTypeEthernet, LinkLayerAddr: ifi.HardwareAddr},
		leases:   make(map[string]lease6),
	}
	// Hand out addresses starting just after the router's own host address.
	s.nextIP = firstHostIP(ipNet, ip)
	return s, nil
}

// SetPXEBootFile6 configures RFC 5970 netboot: the TFTP server IPv6 address
// used to build the option 59 bootfile URL, plus the boot files served to
// legacy BIOS clients (architecture 0) and UEFI clients (any other
// architecture). The bootfile URL is only offered to clients that advertise
// their architecture via option 61.
func (s *DHCP6Server) SetPXEBootFile6(server net.IP, legacyFile, uefiFile string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pxe6Server = server
	s.pxe6Legacy = legacyFile
	s.pxe6UEFI = uefiFile
}

// OnLease registers a callback for new IA_NA leases (mac may be empty when
// the client does not send option 79).
func (s *DHCP6Server) OnLease(cb func(ip net.IP, mac string)) {
	s.onLease = cb
}

// OnLeaseExpired registers a callback for expired leases.
func (s *DHCP6Server) OnLeaseExpired(cb func(ip net.IP, mac string)) {
	s.onLeaseExpired = cb
}

// Start launches the server in the background.
func (s *DHCP6Server) Start() error {
	laddr := &net.UDPAddr{IP: net.IPv6unspecified, Port: dhcpv6.DefaultServerPort}
	srv, err := server6.NewServer(s.iface, laddr, s.handler)
	if err != nil {
		return fmt.Errorf("dhcp6: %w", err)
	}
	s.srv = srv

	go func() {
		if err := srv.Serve(); err != nil {
			log.Printf("dhcp6: %s: %v", s.iface, err)
		}
	}()
	go s.reapExpiredLeases()

	log.Printf("dhcp6: serving on %s (%s, %d DNS servers)", s.iface, s.addr6, len(s.dns))
	return nil
}

// Stop shuts the server down.
func (s *DHCP6Server) Stop() {
	if s.srv != nil {
		s.srv.Close()
	}
}

func (s *DHCP6Server) handler(conn net.PacketConn, peer net.Addr, m dhcpv6.DHCPv6) {
	msg, ok := m.(*dhcpv6.Message)
	if !ok {
		return
	}
	log.Printf("dhcp6: %s: %s", s.iface, msg.Summary())

	switch msg.Type() {
	case dhcpv6.MessageTypeSolicit:
		s.replySolicit(conn, peer, msg)
	case dhcpv6.MessageTypeRequest, dhcpv6.MessageTypeRenew, dhcpv6.MessageTypeRebind, dhcpv6.MessageTypeConfirm:
		s.replyRequest(conn, peer, msg)
	case dhcpv6.MessageTypeRelease, dhcpv6.MessageTypeDecline:
		s.release(msg)
	case dhcpv6.MessageTypeInformationRequest:
		s.replyInfoRequest(conn, peer, msg)
	default:
		log.Printf("dhcp6: %s: ignoring %s", s.iface, msg.Type())
	}
}

func (s *DHCP6Server) replySolicit(conn net.PacketConn, peer net.Addr, msg *dhcpv6.Message) {
	addr := s.allocate(msg)
	var reply dhcpv6.DHCPv6
	var err error
	if msg.GetOneOption(dhcpv6.OptionRapidCommit) != nil {
		reply, err = dhcpv6.NewReplyFromMessage(msg, s.modifiers(msg, addr)...)
	} else {
		reply, err = dhcpv6.NewAdvertiseFromSolicit(msg, s.modifiers(msg, addr)...)
	}
	if err != nil {
		log.Printf("dhcp6: %s: build reply to %s: %v", s.iface, msg.String(), err)
		return
	}
	s.send(conn, peer, reply, "solicit")
}

func (s *DHCP6Server) replyRequest(conn net.PacketConn, peer net.Addr, msg *dhcpv6.Message) {
	addr := s.allocate(msg)
	reply, err := dhcpv6.NewReplyFromMessage(msg, s.modifiers(msg, addr)...)
	if err != nil {
		log.Printf("dhcp6: %s: build reply to %s: %v", s.iface, msg.String(), err)
		return
	}
	s.send(conn, peer, reply, "request")
}

func (s *DHCP6Server) replyInfoRequest(conn net.PacketConn, peer net.Addr, msg *dhcpv6.Message) {
	reply, err := dhcpv6.NewReplyFromMessage(msg, s.modifiers(msg, nil)...)
	if err != nil {
		log.Printf("dhcp6: %s: build reply to information-request: %v", s.iface, err)
		return
	}
	s.send(conn, peer, reply, "information-request")
}

func (s *DHCP6Server) send(conn net.PacketConn, peer net.Addr, reply dhcpv6.DHCPv6, kind string) {
	if _, err := conn.WriteTo(reply.ToBytes(), peer); err != nil {
		log.Printf("dhcp6: %s: send %s reply: %v", s.iface, kind, err)
	} else {
		log.Printf("dhcp6: %s: sent %s reply to %s", s.iface, kind, peer)
	}
}

func (s *DHCP6Server) modifiers(msg *dhcpv6.Message, addr net.IP) []dhcpv6.Modifier {
	mods := []dhcpv6.Modifier{dhcpv6.WithServerID(s.serverID)}
	if iana := msg.Options.OneIANA(); iana != nil {
		mods = append(mods, dhcpv6.WithIAID(iana.IaId))
	}
	if addr != nil {
		mods = append(mods, dhcpv6.WithIANA(dhcpv6.OptIAAddress{
			IPv6Addr:          addr,
			PreferredLifetime: dhcp6Preferred,
			ValidLifetime:     dhcp6Valid,
		}))
	}
	if len(s.dns) > 0 {
		mods = append(mods, dhcpv6.WithDNS(s.dns...))
	}
	if url := s.pxeBootfileURL(msg); url != "" {
		mods = append(mods, dhcpv6.WithOption(dhcpv6.OptBootFileURL(url)))
	}
	return mods
}

// pxeBootfileURL returns the option 59 bootfile URL for a netboot client, or
// an empty string when the client did not advertise its architecture (option
// 61) or no matching boot file is configured.
func (s *DHCP6Server) pxeBootfileURL(msg *dhcpv6.Message) string {
	archs := clientArchs(msg)
	if len(archs) == 0 || s.pxe6Server == nil {
		return ""
	}
	file := s.pxe6Legacy
	if !clientIsLegacy(archs) {
		file = s.pxe6UEFI
	}
	if file == "" {
		return ""
	}
	return "tftp://[" + s.pxe6Server.String() + "]/" + file
}

// clientArchs extracts the client architecture types from option 61 (RFC
// 5970). Returns nil when the option is absent.
func clientArchs(msg *dhcpv6.Message) []uint16 {
	opt := msg.GetOneOption(dhcpv6.OptionClientArchType)
	if opt == nil {
		return nil
	}
	b := opt.ToBytes()
	var archs []uint16
	for len(b) >= 2 {
		archs = append(archs, binary.BigEndian.Uint16(b[:2]))
		b = b[2:]
	}
	return archs
}

// clientIsLegacy reports whether the client boots via legacy BIOS PXE: it
// advertises architecture 0 (Intel x86PC) and nothing else. A client that
// also advertises a UEFI/EFI architecture (option 61 may list several, e.g.
// {0, 7} from a UEFI machine with CSM enabled) is served the UEFI boot file.
func clientIsLegacy(archs []uint16) bool {
	return len(archs) == 1 && archs[0] == uint16(iana.INTEL_X86PC)
}

// allocate returns the IA_NA address for a client, allocating a new one from
// the prefix if needed. Returns nil for clients that do not request an
// address (no IA_NA option).
func (s *DHCP6Server) allocate(msg *dhcpv6.Message) net.IP {
	if msg.Options.OneIANA() == nil {
		return nil
	}

	key := duidKey(msg)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Honor an existing lease first.
	if l, ok := s.leases[key]; ok && time.Now().Before(l.Expires) {
		s.leases[key] = lease6{IP: l.IP, MAC: l.MAC, Expires: time.Now().Add(dhcp6Valid)}
		return l.IP
	}

	// Allocate the next free address.
	for i := 0; i < 1<<16; i++ {
		if !s.addressLeased(s.nextIP) && !s.addressReserved(s.nextIP) {
			ip := dupIP(s.nextIP)
			s.leases[key] = lease6{IP: ip, Expires: time.Now().Add(dhcp6Valid)}
			s.nextIP = incIP(s.nextIP)
			if s.onLease != nil {
				go s.onLease(dupIP(ip), "")
			}
			return ip
		}
		s.nextIP = incIP(s.nextIP)
	}
	return nil
}

func (s *DHCP6Server) addressLeased(ip net.IP) bool {
	for _, l := range s.leases {
		if l.IP.Equal(ip) && time.Now().Before(l.Expires) {
			return true
		}
	}
	return false
}

func (s *DHCP6Server) addressReserved(ip net.IP) bool {
	// Never hand out the network address or the router's own address.
	if ip.Equal(s.prefix.IP) {
		return true
	}
	router, _, err := net.ParseCIDR(s.addr6)
	if err != nil {
		return false
	}
	return ip.Equal(router)
}

func (s *DHCP6Server) release(msg *dhcpv6.Message) {
	s.mu.Lock()
	delete(s.leases, duidKey(msg))
	s.mu.Unlock()
	log.Printf("dhcp6: %s: released lease for %s", s.iface, duidKey(msg))
}

// reapExpiredLeases periodically drops expired leases so their addresses can
// be reused and fires expiry callbacks.
func (s *DHCP6Server) reapExpiredLeases() {
	ticker := time.NewTicker(dhcp6Valid / 4)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		var expired []lease6
		for key, l := range s.leases {
			if now.After(l.Expires) {
				expired = append(expired, l)
				delete(s.leases, key)
			}
		}
		s.mu.Unlock()
		for _, l := range expired {
			log.Printf("dhcp6: %s: lease expired for %s", s.iface, l.IP)
			if s.onLeaseExpired != nil {
				s.onLeaseExpired(dupIP(l.IP), l.MAC)
			}
		}
	}
}

// duidKey returns a stable string identifying a client by its DUID.
func duidKey(msg *dhcpv6.Message) string {
	return fmt.Sprintf("%x", msg.Options.ClientID().ToBytes())
}

func firstHostIP(prefix *net.IPNet, router net.IP) net.IP {
	base := dupIP(prefix.IP.To16())
	// Skip the network address itself, then the router's host address.
	cand := incIP(base)
	for i := 0; i < 64 && cand != nil; i++ {
		if !cand.Equal(router) {
			return cand
		}
		cand = incIP(cand)
	}
	return cand
}

func dupIP(ip net.IP) net.IP {
	d := make(net.IP, len(ip))
	copy(d, ip)
	return d
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
