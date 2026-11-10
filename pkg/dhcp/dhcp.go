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
	macRootPaths    map[string]string // normalized MAC -> NFS root-path (DHCP option 17)
	ipxeBootFile    string            // bootfile for clients that already run iPXE
	uefiBootFile    string            // bootfile for UEFI PXE clients
	legacyBootFile  string            // bootfile for legacy BIOS PXE clients
	appleNetBoot    appleNetBootConfig
	onLease         LeaseCallback
	onLeaseExpired  LeaseExpiredCallback
}

// appleNetBootConfig holds the per-architecture settings for Apple NetBoot
// (BSDP) clients. The booter is fetched via TFTP, the root image (a .dmg) is
// mounted from an external NFS server via the root-path option.
type appleNetBootConfig struct {
	Enabled       bool
	IntelBootFile string
	PPCBootFile   string
	IntelRootPath string
	PPCRootPath   string
	IntelImageID  uint32
	PPCImageID    uint32
}

// BSDP constants from the reference recipe for ISC dhcpd ("Mac OS X Maschine
// über Netzwerk booten", kb.pocnet.net) plus the Apple BSDP specification
// (bootp-170 Documentation/BSDP.doc, NetBoot 2.0 / version 0x0101): Apple
// clients are matched by the AAPLBSDPC vendor class and answered with
// vendor-encapsulated options carrying the boot image list (ACK[LIST]) and
// the selected boot image (ACK[SELECT]).
const (
	appleVendorClass = "AAPLBSDPC"

	// BSDP option 43 sub-option codes (Apple BSDP spec 3.4).
	bsdpSubOptionMessage    = 1 // 1=LIST, 2=SELECT, 3=FAILED
	bsdpSubOptionVersion    = 2 // uint16 0x0101 for NetBoot 2.0
	bsdpSubOptionServerID   = 3 // ipaddr
	bsdpSubOptionPriority   = 4 // uint16
	bsdpSubOptionDefaultID  = 7 // boot_image_id
	bsdpSubOptionSelectedID = 8 // boot_image_id
	bsdpSubOptionImageList  = 9 // entries: 4-byte ID + 1-byte count + name

	bsdpMsgTypeList   = 1
	bsdpMsgTypeSelect = 2

	// BSDP boot_image_id attributes (byte 0): bit 7 = Install, bits 0-6 =
	// Kind (0x01 = Mac OS X). Index (bytes 1-2) 0x1000+ = globally unique.
	bsdpAttrInstall = 0x80
	bsdpKindMacOSX  = 0x01
	bsdpIndexBase   = 0x1000

	appleIntelVendorClass = appleVendorClass + "/i386"
	applePPCVendorClass   = appleVendorClass + "/ppc"
)

// bsdpVersion0101 is the BSDP protocol version of NetBoot 2.0 clients.
var bsdpVersion0101 = [2]byte{0x01, 0x01}

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

// SetMacRootPaths replaces the per-client DHCP option 17 root-path map.
// Entries override the Apple NetBoot per-architecture root paths; a non-Apple
// client with an entry also gets the root path (explicit admin intent, e.g.
// an NFS-rooted Linux PXE client). Always replaces: an empty map clears
// entries removed from the extras file at runtime.
func (s *Server) SetMacRootPaths(m map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.macRootPaths = make(map[string]string, len(m))
	for mac, path := range m {
		s.macRootPaths[normalizeMAC(mac)] = path
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

// SetAppleNetBoot configures answers for Apple NetBoot (BSDP) clients. Boot
// files and root paths are selected by the client architecture reported in the
// vendor class identifier (AAPLBSDPC/i386 for Intel EFI, AAPLBSDPC/ppc for
// PowerPC Open Firmware). Empty boot files fall back to the conventional
// macnbi-<arch>/booter TFTP paths; a zero image ID selects the default from
// the reference recipe (0x81000067 for i386, 0x81000009 for ppc).
func (s *Server) SetAppleNetBoot(enabled bool, intelBootFile, ppcBootFile, intelRootPath, ppcRootPath string, imageID uint32) {
	if intelBootFile == "" {
		intelBootFile = "macnbi-i386/booter"
	}
	if ppcBootFile == "" {
		ppcBootFile = "macnbi-ppc/booter"
	}
	intelID, ppcID := imageID, imageID
	if intelID == 0 {
		intelID = 0 // 0 = derive a spec-shaped ID from the root path
	}
	if ppcID == 0 {
		ppcID = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appleNetBoot = appleNetBootConfig{
		Enabled:       enabled,
		IntelBootFile: intelBootFile,
		PPCBootFile:   ppcBootFile,
		IntelRootPath: intelRootPath,
		PPCRootPath:   ppcRootPath,
		IntelImageID:  intelID,
		PPCImageID:    ppcID,
	}
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
	log.Printf("dhcp: === %s mac=%s peer=%s ===", req.MessageType(), req.ClientHWAddr, peer)
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
		if ip == nil {
			log.Printf("dhcp: pool exhausted, no free address for %s", mac)
			return
		}
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
		if ip == nil {
			log.Printf("dhcp: pool exhausted, no free address for %s", mac)
			return
		}
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
	case dhcpv4.MessageTypeInform:
		// RFC 2131 DHCPINFORM: the client already has an address and asks
		// for remaining configuration. Answer with an ACK carrying the
		// scope options and the per-client boot config (bootfile, root-path,
		// BSDP payload) — no lease, no yiaddr. Apple's PPC booter sends
		// INFORM after its lease and waits for the reply before TFTP.
		resp, err = dhcpv4.NewReplyFromRequest(req,
			dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
			dhcpv4.WithServerIP(s.serverIP),
			dhcpv4.WithOption(dhcpv4.OptSubnetMask(s.mask)),
			dhcpv4.WithOption(dhcpv4.OptDNS(s.dns...)),
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(s.serverIP)),
			opts,
		)
		log.Printf("dhcp: INFORM %s -> ACK (config only)", mac)
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

	// Find next free IP. Scan until we complete a full pool cycle; if every
	// address is leased or reserved, allocation must fail rather than hand out
	// a duplicate.
	start := dupIP(s.nextIP)
	ip := start
	for {
		if !s.isLeasedOrReserved(ip) {
			break
		}
		next := incIP(ip)
		if ipGreater(next, s.rangeEnd) {
			next = dupIP(s.rangeStart)
		}
		if next.Equal(start) {
			// Full pool cycle completed without finding a free address.
			return nil
		}
		ip = next
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
		macRoot, hasMacRoot := s.macRootPaths[normalizeMAC(mac)]
		ipxe := s.ipxeBootFile
		uefi := s.uefiBootFile
		legacy := s.legacyBootFile
		apple := s.appleNetBoot
		serverIP := s.serverIP
		s.mu.Unlock()

		classified := ""
		var rootPath string
		var appleImageID uint32
		switch {
		case ipxe != "" && req != nil && isIPXEClient(req):
			bootFile = ipxe
			classified = "ipxe"
		case hasMacFile:
			bootFile = macFile
			classified = "mac"
		case apple.Enabled && req != nil && isAppleBSDPClient(req):
			// Apple NetBoot (BSDP): booter file and root path depend on the
			// client architecture from the vendor class identifier.
			switch appleVendorArch(req) {
			case "i386":
				bootFile = apple.IntelBootFile
				rootPath = apple.IntelRootPath
				appleImageID = apple.IntelImageID
				classified = "apple-i386"
			case "ppc":
				bootFile = apple.PPCBootFile
				rootPath = apple.PPCRootPath
				appleImageID = apple.PPCImageID
				classified = "apple-ppc"
			}
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
		// Per-MAC root paths override the Apple per-architecture default and
		// also apply to non-Apple clients (explicit admin intent).
		if hasMacRoot {
			if rootPath != macRoot {
				log.Printf("dhcp: %s per-MAC root-path override=%s", mac, macRoot)
			}
			rootPath = macRoot
		}
		if rootPath != "" {
			log.Printf("dhcp: %s sending option 17 root-path=%s", mac, rootPath)
			d.UpdateOption(dhcpv4.OptRootPath(rootPath))
		}
		if classified == "apple-i386" || classified == "apple-ppc" {
			// BSDP option 43 payloads. Apple BSDP spec (bootp-170
			// Documentation/BSDP.doc): NetBoot 2.0 clients (version 0x0101)
			// ask via INFORM[LIST] / INFORM[SELECT] and expect the boot
			// image list resp. the selected boot image; the DISCOVER path
			// stays recipe-style for NetBoot 1.0 clients.
			if req == nil {
				return
			}
			imageID := appleImageID
			if imageID == 0 {
				imageID = bsdpImageIDForRootPath(rootPath)
			}
			class := []byte(applePPCVendorClass)
			if classified == "apple-i386" {
				class = []byte(appleIntelVendorClass)
			}
			switch {
			case req.MessageType() == dhcpv4.MessageTypeDiscover:
				// Recipe-style OFFER*: selected boot image ID.
				d.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionClassIdentifier, class))
				d.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionVendorSpecificInformation, bsdpSelectImage(imageID)))
			case bsdpClientMessageType(req) == bsdpMsgTypeList:
				// INFORM[LIST] -> ACK[LIST]: version, server identifier,
				// priority, default boot image and the boot image list.
				var payload []byte
				payload = append(payload, bsdpSubOptionMessage, 1, bsdpMsgTypeList)
				payload = append(payload, bsdpSubOptionVersion, 2)
				payload = append(payload, bsdpVersion0101[:]...)
				payload = append(payload, bsdpSubOptionServerID, 4)
				payload = append(payload, serverIP.To4()...)
				payload = append(payload, bsdpSubOptionPriority, 2, 0x03, 0xe8)
				payload = append(payload, bsdpSubOptionDefaultID, 4)
				payload = append(payload, byte(imageID>>24), byte(imageID>>16), byte(imageID>>8), byte(imageID))
				entry := bsdpImageListEntry(imageID, bsdpImageNameForRootPath(rootPath))
				payload = append(payload, bsdpSubOptionImageList, byte(len(entry)))
				payload = append(payload, entry...)
				d.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionClassIdentifier, class))
				d.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionVendorSpecificInformation, payload))
			case bsdpClientMessageType(req) == bsdpMsgTypeSelect:
				// INFORM[SELECT] -> ACK[SELECT]: confirm the client's
				// selected boot image.
				selected := bsdpClientSelectedImage(req)
				if selected == 0 {
					selected = imageID
				}
				var payload []byte
				payload = append(payload, bsdpSubOptionMessage, 1, bsdpMsgTypeSelect)
				payload = append(payload, bsdpSubOptionVersion, 2)
				payload = append(payload, bsdpVersion0101[:]...)
				payload = append(payload, bsdpSubOptionSelectedID, 4)
				payload = append(payload, byte(selected>>24), byte(selected>>16), byte(selected>>8), byte(selected))
				d.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionClassIdentifier, class))
				d.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionVendorSpecificInformation, payload))
			case classified == "apple-ppc" && req.MessageType() == dhcpv4.MessageTypeInform:
				// Reference recipe ppc INFORM payload for NetBoot 1.0
				// clients: BSDP message type 2 + selected boot image ID.
				inform := append([]byte{0x01, 0x01, 0x02}, bsdpSelectImage(imageID)...)
				d.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionClassIdentifier, class))
				d.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionVendorSpecificInformation, inform))
			case classified == "apple-ppc":
				// The ppc class in the reference recipe answers any message
				// type other than DISCOVER/INFORM with the static fallback
				// payload.
				d.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionClassIdentifier, class))
				d.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionVendorSpecificInformation,
					[]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}))
			}
		}
	}
}

// bsdpParse splits a vendor-encapsulated options blob into BSDP sub-options.
// Returns nil for empty or malformed input.
func bsdpParse(b []byte) map[uint8][]byte {
	if len(b) == 0 {
		return nil
	}
	out := make(map[uint8][]byte)
	for len(b) >= 2 {
		code, l := b[0], int(b[1])
		if code == 0 || 2+l > len(b) {
			break
		}
		out[code] = append([]byte(nil), b[2:2+l]...)
		b = b[2+l:]
	}
	return out
}

// bsdpClientMessageType extracts the BSDP message type (sub-option 1: 1=LIST,
// 2=SELECT) from the client's request; 0 when absent or unparseable.
func bsdpClientMessageType(req *dhcpv4.DHCPv4) uint8 {
	m := bsdpParse(req.Options.Get(dhcpv4.OptionVendorSpecificInformation))
	if mt, ok := m[bsdpSubOptionMessage]; ok && len(mt) == 1 {
		return mt[0]
	}
	return 0
}

// bsdpClientSelectedImage extracts the client's BSDP Selected Boot Image ID
// (sub-option 8); 0 when absent.
func bsdpClientSelectedImage(req *dhcpv4.DHCPv4) uint32 {
	m := bsdpParse(req.Options.Get(dhcpv4.OptionVendorSpecificInformation))
	if id, ok := m[bsdpSubOptionSelectedID]; ok && len(id) == 4 {
		return binary.BigEndian.Uint32(id)
	}
	return 0
}

// bsdpImageIDForRootPath derives a spec-shaped boot_image_id from the root
// path (attributes byte 0x81 = Install + Kind Mac OS X for NetInstall-style
// paths, 0x01 = Mac OS X non-install for NetBoot-style paths; index 0x1000 =
// globally unique range). An explicitly configured ID wins (see replyOptions).
func bsdpImageIDForRootPath(rootPath string) uint32 {
	attr := uint32(bsdpAttrInstall | bsdpKindMacOSX)
	if strings.Contains(strings.ToLower(bsdpImageFile(rootPath)), "netboot") {
		attr = uint32(bsdpKindMacOSX)
	}
	return attr<<24 | bsdpIndexBase
}

// bsdpImageFile returns the file name component of an NFS root path
// ("nfs:host:/export:file.dmg" -> "file.dmg").
func bsdpImageFile(rootPath string) string {
	if i := strings.LastIndex(rootPath, ":"); i >= 0 {
		return rootPath[i+1:]
	}
	return rootPath
}

// bsdpImageNameForRootPath derives the boot image name shown in the client's
// boot picker from the root path's .dmg basename, e.g.
// "nfs:…:MacOSX-10.5.8-NetBoot-ppc.dmg" -> "MacOSX-10.5.8-NetBoot".
func bsdpImageNameForRootPath(rootPath string) string {
	file := rootPath
	if i := strings.LastIndex(rootPath, ":"); i >= 0 {
		file = rootPath[i+1:]
	}
	name := strings.TrimSuffix(file, ".dmg")
	for _, suffix := range []string{"-ppc", "-i386", "-x86_64"} {
		name = strings.TrimSuffix(name, suffix)
	}
	if name == "" {
		name = "NetBoot image"
	}
	return name
}

// bsdpImageListEntry encodes one Boot Image List (sub-option 9) entry:
// 4-byte boot_image_id, 1-byte name count, name bytes.
func bsdpImageListEntry(id uint32, name string) []byte {
	entry := make([]byte, 0, 5+len(name))
	entry = append(entry, byte(id>>24), byte(id>>16), byte(id>>8), byte(id), byte(len(name)))
	entry = append(entry, name...)
	return entry
}

// isAppleBSDPClient reports whether the request carries the Apple NetBoot
// (BSDP) vendor class identifier (AAPLBSDPC, AAPLBSDPC/i386 or AAPLBSDPC/ppc).
func isAppleBSDPClient(req *dhcpv4.DHCPv4) bool {
	vc := req.Options.Get(dhcpv4.OptionClassIdentifier)
	return len(vc) >= len(appleVendorClass) &&
		bytes.Equal(bytes.ToLower(vc[:len(appleVendorClass)]), []byte(strings.ToLower(appleVendorClass)))
}

// appleVendorArch returns the architecture of an AAPLBSDPC client: "i386" for
// Intel EFI clients, "ppc" for PowerPC Open Firmware clients. A bare
// AAPLBSDPC vendor class is treated as ppc (old-world Open Firmware).
func appleVendorArch(req *dhcpv4.DHCPv4) string {
	if strings.HasPrefix(string(req.Options.Get(dhcpv4.OptionClassIdentifier)), appleVendorClass+"/i386") {
		return "i386"
	}
	return "ppc"
}

// bsdpSelectImage builds the BSDP option 43 "selected boot image" payload:
// sub-option 8 with a 4-byte big-endian boot image ID.
func bsdpSelectImage(id uint32) []byte {
	b := make([]byte, 6)
	b[0] = bsdpSubOptionSelectedID
	b[1] = 4
	binary.BigEndian.PutUint32(b[2:6], id)
	return b
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
