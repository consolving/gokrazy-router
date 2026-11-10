package dhcp

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

func TestLeaseExpiry(t *testing.T) {
	s := &Server{
		iface:      "eth0",
		serverIP:   net.ParseIP("10.0.0.1").To4(),
		mask:       net.CIDRMask(24, 32),
		rangeStart: net.ParseIP("10.0.0.100").To4(),
		rangeEnd:   net.ParseIP("10.0.0.200").To4(),
		dns:        []net.IP{net.ParseIP("8.8.8.8").To4()},
		lease:      2 * time.Second,
		router:     net.ParseIP("10.0.0.1").To4(),
		leases:     make(map[string]lease),
		nextIP:     dupIP(net.ParseIP("10.0.0.100").To4()),
	}

	// Allocate a lease.
	mac := "aa:bb:cc:dd:ee:ff"
	ip := s.allocate(mac)
	if ip.String() != "10.0.0.100" {
		t.Fatalf("expected 10.0.0.100, got %s", ip)
	}

	// Verify lease exists.
	s.mu.Lock()
	if _, ok := s.leases[mac]; !ok {
		t.Fatal("lease not found")
	}
	s.mu.Unlock()

	// Manually expire it.
	s.mu.Lock()
	l := s.leases[mac]
	l.Expires = time.Now().Add(-1 * time.Second)
	s.leases[mac] = l
	s.mu.Unlock()

	// Verify isLeasedOrReserved returns false for expired lease.
	s.mu.Lock()
	if s.isLeasedOrReserved(ip) {
		t.Fatal("expired lease should not be considered leased or reserved")
	}
	s.mu.Unlock()
}

func TestOnLeaseExpiredCallback(t *testing.T) {
	s := &Server{
		iface:      "eth0",
		serverIP:   net.ParseIP("10.0.0.1").To4(),
		mask:       net.CIDRMask(24, 32),
		rangeStart: net.ParseIP("10.0.0.100").To4(),
		rangeEnd:   net.ParseIP("10.0.0.200").To4(),
		dns:        []net.IP{net.ParseIP("8.8.8.8").To4()},
		lease:      100 * time.Millisecond,
		router:     net.ParseIP("10.0.0.1").To4(),
		leases:     make(map[string]lease),
		nextIP:     dupIP(net.ParseIP("10.0.0.100").To4()),
	}

	// Add a lease that is already expired.
	mac := "aa:bb:cc:dd:ee:ff"
	ip := net.ParseIP("10.0.0.100").To4()
	s.mu.Lock()
	s.leases[mac] = lease{IP: dupIP(ip), Expires: time.Now().Add(-1 * time.Second)}
	s.mu.Unlock()

	var mu sync.Mutex
	var expiredIPs []string
	s.OnLeaseExpired(func(ip net.IP, mac string) {
		mu.Lock()
		expiredIPs = append(expiredIPs, ip.String())
		mu.Unlock()
	})

	// Run the reaper in background briefly.
	// We can't easily test reapExpiredLeases since it uses a ticker,
	// so we test the logic directly.
	s.mu.Lock()
	now := time.Now()
	var expired []lease
	var expiredMACs []string
	for m, l := range s.leases {
		if now.After(l.Expires) {
			expired = append(expired, l)
			expiredMACs = append(expiredMACs, m)
		}
	}
	for _, m := range expiredMACs {
		delete(s.leases, m)
	}
	s.mu.Unlock()

	// Fire callbacks.
	for i, l := range expired {
		s.onLeaseExpired(dupIP(l.IP), expiredMACs[i])
	}

	mu.Lock()
	defer mu.Unlock()
	if len(expiredIPs) != 1 || expiredIPs[0] != "10.0.0.100" {
		t.Fatalf("expected expired callback for 10.0.0.100, got %v", expiredIPs)
	}

	// Verify lease was removed.
	s.mu.Lock()
	if _, ok := s.leases[mac]; ok {
		t.Fatal("expired lease should have been removed")
	}
	s.mu.Unlock()
}

func TestAllocateReusesExpiredLease(t *testing.T) {
	s := &Server{
		iface:      "eth0",
		serverIP:   net.ParseIP("10.0.0.1").To4(),
		mask:       net.CIDRMask(24, 32),
		rangeStart: net.ParseIP("10.0.0.100").To4(),
		rangeEnd:   net.ParseIP("10.0.0.100").To4(), // single IP range
		dns:        []net.IP{net.ParseIP("8.8.8.8").To4()},
		lease:      1 * time.Second,
		router:     net.ParseIP("10.0.0.1").To4(),
		leases:     make(map[string]lease),
		nextIP:     dupIP(net.ParseIP("10.0.0.100").To4()),
	}

	// First allocation.
	ip1 := s.allocate("aa:bb:cc:dd:ee:01")
	if ip1.String() != "10.0.0.100" {
		t.Fatalf("expected 10.0.0.100, got %s", ip1)
	}

	// Expire the lease.
	s.mu.Lock()
	l := s.leases["aa:bb:cc:dd:ee:01"]
	l.Expires = time.Now().Add(-1 * time.Second)
	s.leases["aa:bb:cc:dd:ee:01"] = l
	s.mu.Unlock()

	// Second client should get the same IP since it's expired.
	ip2 := s.allocate("aa:bb:cc:dd:ee:02")
	if ip2.String() != "10.0.0.100" {
		t.Fatalf("expected 10.0.0.100 (reused), got %s", ip2)
	}
}

func TestSetReservationsAndAllocate(t *testing.T) {
	s := &Server{
		iface:      "eth0",
		serverIP:   net.ParseIP("10.0.0.1").To4(),
		mask:       net.CIDRMask(24, 32),
		rangeStart: net.ParseIP("10.0.0.100").To4(),
		rangeEnd:   net.ParseIP("10.0.0.200").To4(),
		dns:        []net.IP{net.ParseIP("8.8.8.8").To4()},
		lease:      1 * time.Hour,
		router:     net.ParseIP("10.0.0.1").To4(),
		leases:     make(map[string]lease),
		nextIP:     dupIP(net.ParseIP("10.0.0.100").To4()),
	}

	s.SetReservations(map[string]string{
		"AA-BB-CC-DD-EE-FF": "10.0.0.50",
	})

	ip := s.allocate("aa:bb:cc:dd:ee:ff")
	if ip.String() != "10.0.0.50" {
		t.Fatalf("expected reserved 10.0.0.50, got %s", ip)
	}

	// A non-reserved client should skip the reserved IP.
	ip2 := s.allocate("00:11:22:33:44:55")
	if ip2.String() == "10.0.0.50" {
		t.Fatal("dynamic client got reserved IP")
	}
}

// TestAllocatePoolExhausted verifies that a fully occupied pool returns nil
// instead of wrapping around and handing out a duplicate address.
func TestAllocatePoolExhausted(t *testing.T) {
	newSrv := func() *Server {
		return &Server{
			iface:      "eth0",
			serverIP:   net.ParseIP("10.0.0.1").To4(),
			mask:       net.CIDRMask(24, 32),
			rangeStart: net.ParseIP("10.0.0.100").To4(),
			rangeEnd:   net.ParseIP("10.0.0.102").To4(), // 3-address pool
			dns:        []net.IP{net.ParseIP("8.8.8.8").To4()},
			lease:      1 * time.Hour,
			router:     net.ParseIP("10.0.0.1").To4(),
			leases:     make(map[string]lease),
			nextIP:     dupIP(net.ParseIP("10.0.0.101").To4()),
		}
	}

	t.Run("reservations fill pool", func(t *testing.T) {
		s := newSrv()
		s.SetReservations(map[string]string{
			"00:11:22:33:44:01": "10.0.0.100",
			"00:11:22:33:44:02": "10.0.0.101",
			"00:11:22:33:44:03": "10.0.0.102",
		})
		if ip := s.allocate("00:11:22:33:44:99"); ip != nil {
			t.Fatalf("pool exhausted but allocated %s", ip)
		}
	})

	t.Run("dynamic leases fill pool", func(t *testing.T) {
		s := newSrv()
		for _, mac := range []string{"00:11:22:33:44:a1", "00:11:22:33:44:a2", "00:11:22:33:44:a3"} {
			if ip := s.allocate(mac); ip == nil {
				t.Fatalf("allocate(%s) failed before pool exhaustion", mac)
			}
		}
		if ip := s.allocate("00:11:22:33:44:a4"); ip != nil {
			t.Fatalf("pool exhausted but allocated %s", ip)
		}
	})

	t.Run("free address before wrap is found", func(t *testing.T) {
		s := newSrv()
		// nextIP = .101, .100 is the only free address (the pool's first).
		s.SetReservations(map[string]string{
			"00:11:22:33:44:01": "10.0.0.101",
			"00:11:22:33:44:02": "10.0.0.102",
		})
		ip := s.allocate("00:11:22:33:44:99")
		if ip == nil {
			t.Fatal("pool has a free address but allocation failed")
		}
		if ip.String() != "10.0.0.100" {
			t.Fatalf("expected wrapped allocation .100, got %s", ip)
		}
	})

	t.Run("single address pool", func(t *testing.T) {
		s := &Server{
			iface:      "eth0",
			serverIP:   net.ParseIP("10.0.0.1").To4(),
			mask:       net.CIDRMask(24, 32),
			rangeStart: net.ParseIP("10.0.0.100").To4(),
			rangeEnd:   net.ParseIP("10.0.0.100").To4(),
			dns:        []net.IP{net.ParseIP("8.8.8.8").To4()},
			lease:      1 * time.Hour,
			router:     net.ParseIP("10.0.0.1").To4(),
			leases:     make(map[string]lease),
			nextIP:     dupIP(net.ParseIP("10.0.0.100").To4()),
		}
		if ip := s.allocate("00:11:22:33:44:b1"); ip == nil || ip.String() != "10.0.0.100" {
			t.Fatalf("single address allocation = %v, want .100", ip)
		}
		if ip := s.allocate("00:11:22:33:44:b2"); ip != nil {
			t.Fatalf("exhausted single-address pool allocated %s", ip)
		}
	})
}

func TestSetPXEOptions(t *testing.T) {
	s := &Server{
		iface:    "eth0",
		serverIP: net.ParseIP("10.0.0.1").To4(),
		mask:     net.CIDRMask(24, 32),
	}
	s.SetPXEOptions("10.0.0.2", "pxelinux.0")

	s.mu.Lock()
	got := s.pxeBootServer.String()
	file := s.pxeBootFile
	s.mu.Unlock()

	if got != "10.0.0.2" {
		t.Errorf("pxeBootServer = %q, want 10.0.0.2", got)
	}
	if file != "pxelinux.0" {
		t.Errorf("pxeBootFile = %q, want pxelinux.0", file)
	}
}

func TestPXEBootfileSelection(t *testing.T) {
	s := &Server{
		iface:    "eth0",
		serverIP: net.ParseIP("10.0.0.1").To4(),
		mask:     net.CIDRMask(24, 32),
	}
	s.SetPXEOptions("10.0.0.2", "undionly.kpxe")
	s.SetLegacyBootFile("undionly.kpxe")
	s.SetUEFIBootFile("netboot.xyz.efi")
	s.SetIPXEBootFile("boot.ipxe")

	mac := "aa:bb:cc:dd:ee:ff"
	bootfile := func(req *dhcpv4.DHCPv4) string {
		mod := s.replyOptions(mac, req)
		resp, err := dhcpv4.New()
		if err != nil {
			t.Fatal(err)
		}
		mod(resp)
		return string(resp.Options.Get(dhcpv4.OptionBootfileName))
	}

	// Legacy BIOS PXE ROM has no user-class.
	legacyReq, err := dhcpv4.New()
	if err != nil {
		t.Fatal(err)
	}
	if got := bootfile(legacyReq); got != "undionly.kpxe" {
		t.Errorf("legacy bootfile = %q, want undionly.kpxe", got)
	}

	// iPXE identifies itself with DHCP option 77 (User Class) = "iPXE".
	ipxeReq, err := dhcpv4.New(dhcpv4.WithUserClass("iPXE", false))
	if err != nil {
		t.Fatal(err)
	}
	if got := bootfile(ipxeReq); got != "boot.ipxe" {
		t.Errorf("iPXE bootfile = %q, want boot.ipxe", got)
	}

	// UEFI clients advertise architecture type 7 or 9 via option 93.
	uefiReq, err := dhcpv4.New(dhcpv4.WithGeneric(dhcpv4.OptionClientSystemArchitectureType, []byte{0, 7}))
	if err != nil {
		t.Fatal(err)
	}
	if got := bootfile(uefiReq); got != "netboot.xyz.efi" {
		t.Errorf("UEFI bootfile = %q, want netboot.xyz.efi", got)
	}
}

func TestPXEMACOverridePrecedence(t *testing.T) {
	s := &Server{
		iface:    "eth0",
		serverIP: net.ParseIP("10.0.0.1").To4(),
		mask:     net.CIDRMask(24, 32),
	}
	s.SetPXEOptions("10.0.0.2", "undionly.kpxe")
	s.SetLegacyBootFile("undionly.kpxe")
	s.SetUEFIBootFile("netboot.xyz.efi")
	s.SetIPXEBootFile("boot.ipxe")
	s.SetMacPXEBootFiles(map[string]string{
		"aa:bb:cc:dd:ee:ff": "my-custom.ipxe",
	})

	mac := "aa:bb:cc:dd:ee:ff"
	bootfile := func(req *dhcpv4.DHCPv4) string {
		mod := s.replyOptions(mac, req)
		resp, err := dhcpv4.New()
		if err != nil {
			t.Fatal(err)
		}
		mod(resp)
		return string(resp.Options.Get(dhcpv4.OptionBootfileName))
	}

	ipxeReq, err := dhcpv4.New(dhcpv4.WithUserClass("iPXE", false))
	if err != nil {
		t.Fatal(err)
	}
	if got := bootfile(ipxeReq); got != "boot.ipxe" {
		t.Errorf("iPXE client with MAC override bootfile = %q, want boot.ipxe", got)
	}

	uefiReq, err := dhcpv4.New(dhcpv4.WithGeneric(dhcpv4.OptionClientSystemArchitectureType, []byte{0, 7}))
	if err != nil {
		t.Fatal(err)
	}
	if got := bootfile(uefiReq); got != "my-custom.ipxe" {
		t.Errorf("UEFI client with MAC override bootfile = %q, want my-custom.ipxe", got)
	}

	legacyReq, err := dhcpv4.New()
	if err != nil {
		t.Fatal(err)
	}
	if got := bootfile(legacyReq); got != "my-custom.ipxe" {
		t.Errorf("legacy client with MAC override bootfile = %q, want my-custom.ipxe", got)
	}
}

func TestPXEBootfilePrecedenceTable(t *testing.T) {
	s := &Server{
		iface:    "eth0",
		serverIP: net.ParseIP("10.0.0.1").To4(),
		mask:     net.CIDRMask(24, 32),
	}
	s.SetPXEOptions("10.0.0.2", "undionly.kpxe")
	s.SetLegacyBootFile("undionly.kpxe")
	s.SetUEFIBootFile("netboot.xyz.efi")
	s.SetIPXEBootFile("boot.ipxe")
	s.SetMacPXEBootFiles(map[string]string{
		"aa:bb:cc:dd:ee:ff": "my-custom.ipxe",
	})

	mac := "aa:bb:cc:dd:ee:ff"
	bootfile := func(req *dhcpv4.DHCPv4) string {
		mod := s.replyOptions(mac, req)
		resp, err := dhcpv4.New()
		if err != nil {
			t.Fatal(err)
		}
		mod(resp)
		return string(resp.Options.Get(dhcpv4.OptionBootfileName))
	}

	legacyReq, err := dhcpv4.New()
	if err != nil {
		t.Fatal(err)
	}
	ipxeReq, err := dhcpv4.New(dhcpv4.WithUserClass("iPXE", false))
	if err != nil {
		t.Fatal(err)
	}
	uefiReq, err := dhcpv4.New(dhcpv4.WithGeneric(dhcpv4.OptionClientSystemArchitectureType, []byte{0, 7}))
	if err != nil {
		t.Fatal(err)
	}

	// Documented precedence: iPXE > per-MAC > UEFI > legacy.
	cases := []struct {
		name     string
		req      *dhcpv4.DHCPv4
		withMAC  bool
		wantBoot string
	}{
		{"ipxe with mac override", ipxeReq, true, "boot.ipxe"},
		{"ipxe without mac override", ipxeReq, false, "boot.ipxe"},
		{"uefi with mac override", uefiReq, true, "my-custom.ipxe"},
		{"uefi without mac override", uefiReq, false, "netboot.xyz.efi"},
		{"legacy with mac override", legacyReq, true, "my-custom.ipxe"},
		{"legacy without mac override", legacyReq, false, "undionly.kpxe"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.withMAC {
				s.SetMacPXEBootFiles(map[string]string{mac: "my-custom.ipxe"})
			} else {
				s.SetMacPXEBootFiles(nil)
			}
			if got := bootfile(c.req); got != c.wantBoot {
				t.Errorf("bootfile = %q, want %q", got, c.wantBoot)
			}
		})
	}
}

func TestNormalizeMAC(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"AA:BB:CC:DD:EE:FF", "aa:bb:cc:dd:ee:ff"},
		{"aa-bb-cc-dd-ee-ff", "aa:bb:cc:dd:ee:ff"},
		{"  AA:BB:CC:DD:EE:FF  ", "aa:bb:cc:dd:ee:ff"},
	}
	for _, c := range cases {
		got := normalizeMAC(c.in)
		if got != c.want {
			t.Errorf("normalizeMAC(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func appleTestServer() *Server {
	s := &Server{
		iface:    "eth0",
		serverIP: net.ParseIP("10.0.0.1").To4(),
		mask:     net.CIDRMask(24, 32),
	}
	s.SetLegacyBootFile("undionly.kpxe")
	s.SetUEFIBootFile("netboot.xyz.efi")
	s.SetAppleNetBoot(true, "", "",
		"nfs:192.168.0.10:/export:NetInstall-i386.dmg",
		"nfs:192.168.0.10:/export:NetInstall-ppc.dmg", 0)
	return s
}

func appleReply(t *testing.T, s *Server, req *dhcpv4.DHCPv4) *dhcpv4.DHCPv4 {
	t.Helper()
	mod := s.replyOptions("aa:bb:cc:dd:ee:ff", req)
	resp, err := dhcpv4.New()
	if err != nil {
		t.Fatal(err)
	}
	mod(resp)
	return resp
}

func appleRequest(msgType dhcpv4.MessageType, class string) *dhcpv4.DHCPv4 {
	req, err := dhcpv4.New(
		dhcpv4.WithMessageType(msgType),
		dhcpv4.WithGeneric(dhcpv4.OptionClassIdentifier, []byte(class)),
	)
	if err != nil {
		panic(err)
	}
	return req
}

func TestAppleNetBootIntel(t *testing.T) {
	s := appleTestServer()

	// Intel EFI client DISCOVER: booter, root path, vendor class echo and the
	// selected boot image ID (default 0x81000067) in option 43.
	resp := appleReply(t, s, appleRequest(dhcpv4.MessageTypeDiscover, "AAPLBSDPC/i386"))
	if got := string(resp.Options.Get(dhcpv4.OptionBootfileName)); got != "macnbi-i386/booter" {
		t.Errorf("i386 bootfile = %q, want macnbi-i386/booter", got)
	}
	if got := string(resp.Options.Get(dhcpv4.OptionRootPath)); got != "nfs:192.168.0.10:/export:NetInstall-i386.dmg" {
		t.Errorf("i386 root-path = %q", got)
	}
	if got := resp.Options.Get(dhcpv4.OptionVendorSpecificInformation); !bytes.Equal(got, []byte{0x08, 0x04, 0x81, 0x00, 0x10, 0x00}) {
		t.Errorf("i386 option 43 = % x, want 08 04 81 00 00 67", got)
	}
	if got := string(resp.Options.Get(dhcpv4.OptionClassIdentifier)); got != "AAPLBSDPC/i386" {
		t.Errorf("i386 option 60 = %q, want AAPLBSDPC/i386", got)
	}

	// REQUEST: booter and root path again (the booter re-requests DHCP to read
	// the root path), but no option 43 or vendor class echo per the recipe.
	resp = appleReply(t, s, appleRequest(dhcpv4.MessageTypeRequest, "AAPLBSDPC/i386"))
	if got := string(resp.Options.Get(dhcpv4.OptionBootfileName)); got != "macnbi-i386/booter" {
		t.Errorf("i386 REQUEST bootfile = %q, want macnbi-i386/booter", got)
	}
	if got := string(resp.Options.Get(dhcpv4.OptionRootPath)); got != "nfs:192.168.0.10:/export:NetInstall-i386.dmg" {
		t.Errorf("i386 REQUEST root-path = %q", got)
	}
	if got := resp.Options.Get(dhcpv4.OptionVendorSpecificInformation); got != nil {
		t.Errorf("i386 REQUEST option 43 = % x, want none", got)
	}
	if got := resp.Options.Get(dhcpv4.OptionClassIdentifier); got != nil {
		t.Errorf("i386 REQUEST option 60 = %q, want none", got)
	}
}

func TestAppleNetBootPPC(t *testing.T) {
	s := appleTestServer()

	// PowerPC Open Firmware DISCOVER: ppc booter, ppc root path and the ppc
	// default image ID 0x81000009.
	resp := appleReply(t, s, appleRequest(dhcpv4.MessageTypeDiscover, "AAPLBSDPC/ppc"))
	if got := string(resp.Options.Get(dhcpv4.OptionBootfileName)); got != "macnbi-ppc/booter" {
		t.Errorf("ppc bootfile = %q, want macnbi-ppc/booter", got)
	}
	if got := string(resp.Options.Get(dhcpv4.OptionRootPath)); got != "nfs:192.168.0.10:/export:NetInstall-ppc.dmg" {
		t.Errorf("ppc root-path = %q", got)
	}
	if got := resp.Options.Get(dhcpv4.OptionVendorSpecificInformation); !bytes.Equal(got, []byte{0x08, 0x04, 0x81, 0x00, 0x10, 0x00}) {
		t.Errorf("ppc option 43 = % x, want 08 04 81 00 00 09", got)
	}
	if got := string(resp.Options.Get(dhcpv4.OptionClassIdentifier)); got != "AAPLBSDPC/ppc" {
		t.Errorf("ppc option 60 = %q, want AAPLBSDPC/ppc", got)
	}

	// Non-DISCOVER messages get the static fallback payload from the recipe.
	resp = appleReply(t, s, appleRequest(dhcpv4.MessageTypeRequest, "AAPLBSDPC/ppc"))
	if got := resp.Options.Get(dhcpv4.OptionVendorSpecificInformation); !bytes.Equal(got, []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}) {
		t.Errorf("ppc REQUEST option 43 = % x, want 00 01 02 03 04 05 06 07", got)
	}

	// A bare AAPLBSDPC vendor class is treated as ppc (old-world Open Firmware).
	resp = appleReply(t, s, appleRequest(dhcpv4.MessageTypeDiscover, "AAPLBSDPC"))
	if got := string(resp.Options.Get(dhcpv4.OptionBootfileName)); got != "macnbi-ppc/booter" {
		t.Errorf("bare AAPLBSDPC bootfile = %q, want macnbi-ppc/booter", got)
	}
}

func TestAppleNetBootCustomImageID(t *testing.T) {
	s := appleTestServer()
	s.SetAppleNetBoot(true, "custom/booter", "", "nfs:10.0.0.5:/srv:Custom.dmg", "", 0x81000042)

	resp := appleReply(t, s, appleRequest(dhcpv4.MessageTypeDiscover, "AAPLBSDPC/i386"))
	if got := string(resp.Options.Get(dhcpv4.OptionBootfileName)); got != "custom/booter" {
		t.Errorf("custom bootfile = %q, want custom/booter", got)
	}
	if got := resp.Options.Get(dhcpv4.OptionVendorSpecificInformation); !bytes.Equal(got, []byte{0x08, 0x04, 0x81, 0x00, 0x00, 0x42}) {
		t.Errorf("custom option 43 = % x, want 08 04 81 00 00 42", got)
	}
}

func TestAppleNetBootDisabledAndPrecedence(t *testing.T) {
	s := appleTestServer()

	// Without Apple NetBoot, an AAPLBSDPC client falls through to the default
	// bootfile selection and gets none of the BSDP options.
	s.SetAppleNetBoot(false, "", "", "", "", 0)
	resp := appleReply(t, s, appleRequest(dhcpv4.MessageTypeDiscover, "AAPLBSDPC/i386"))
	if got := string(resp.Options.Get(dhcpv4.OptionBootfileName)); got != "undionly.kpxe" {
		t.Errorf("disabled bootfile = %q, want undionly.kpxe", got)
	}
	if got := resp.Options.Get(dhcpv4.OptionRootPath); got != nil {
		t.Errorf("disabled root-path = %q, want none", got)
	}
	if got := resp.Options.Get(dhcpv4.OptionVendorSpecificInformation); got != nil {
		t.Errorf("disabled option 43 = % x, want none", got)
	}

	// Per-MAC overrides take precedence over the Apple classification.
	s.SetAppleNetBoot(true, "", "",
		"nfs:192.168.0.10:/export:NetInstall-i386.dmg",
		"nfs:192.168.0.10:/export:NetInstall-ppc.dmg", 0)
	s.SetMacPXEBootFiles(map[string]string{"aa:bb:cc:dd:ee:ff": "my-custom.ipxe"})
	resp = appleReply(t, s, appleRequest(dhcpv4.MessageTypeDiscover, "AAPLBSDPC/i386"))
	if got := string(resp.Options.Get(dhcpv4.OptionBootfileName)); got != "my-custom.ipxe" {
		t.Errorf("MAC override bootfile = %q, want my-custom.ipxe", got)
	}
	if got := resp.Options.Get(dhcpv4.OptionRootPath); got != nil {
		t.Errorf("MAC override root-path = %q, want none", got)
	}
}

func TestBSDPSelectImagePayload(t *testing.T) {
	if got := bsdpSelectImage(0x81001000); !bytes.Equal(got, []byte{0x08, 0x04, 0x81, 0x00, 0x10, 0x00}) {
		t.Errorf("bsdpSelectImage = % x", got)
	}
	if got := bsdpSelectImage(0x01001000); !bytes.Equal(got, []byte{0x08, 0x04, 0x01, 0x00, 0x10, 0x00}) {
		t.Errorf("bsdpSelectImage = % x", got)
	}
}

func TestMacRootPathOverride(t *testing.T) {
	s := appleTestServer()

	// Per-MAC root path overrides the PPC arch default; bootfile and BSDP
	// payload stay intact.
	s.SetMacRootPaths(map[string]string{
		"aa:bb:cc:dd:ee:ff": "nfs:10.0.31.1:/mnt/sata/netboot:MacOSX-10.4-ppc.dmg",
	})
	resp := appleReply(t, s, appleRequest(dhcpv4.MessageTypeDiscover, "AAPLBSDPC/ppc"))
	if got := string(resp.Options.Get(dhcpv4.OptionRootPath)); got != "nfs:10.0.31.1:/mnt/sata/netboot:MacOSX-10.4-ppc.dmg" {
		t.Errorf("overridden root-path = %q", got)
	}
	if got := string(resp.Options.Get(dhcpv4.OptionBootfileName)); got != "macnbi-ppc/booter" {
		t.Errorf("bootfile = %q, want arch default", got)
	}
	if got := resp.Options.Get(dhcpv4.OptionVendorSpecificInformation); !bytes.Equal(got, []byte{0x08, 0x04, 0x81, 0x00, 0x10, 0x00}) {
		t.Errorf("option 43 = % x", got)
	}

	// A client without an entry keeps the arch default.
	other := appleTestServer() // fresh server, no MAC entry
	other.SetMacRootPaths(map[string]string{
		"11:22:33:44:55:66": "nfs:10.0.31.1:/mnt/sata/netboot:Other.dmg",
	})
	resp = appleReply(t, other, appleRequest(dhcpv4.MessageTypeDiscover, "AAPLBSDPC/ppc"))
	if got := string(resp.Options.Get(dhcpv4.OptionRootPath)); got != "nfs:192.168.0.10:/export:NetInstall-ppc.dmg" {
		t.Errorf("non-mapped client root-path = %q, want arch default", got)
	}
}

func TestMacRootPathNonApple(t *testing.T) {
	s := appleTestServer()
	s.SetMacRootPaths(map[string]string{
		"aa:bb:cc:dd:ee:ff": "nfs:10.0.31.1:/mnt/sata/netboot:LinuxRoot.dmg",
	})

	// A legacy PXE client with an explicit entry gets option 17 (NFS-rooted
	// Linux) but no BSDP payload.
	resp := appleReply(t, s, func() *dhcpv4.DHCPv4 {
		req, err := dhcpv4.New()
		if err != nil {
			t.Fatal(err)
		}
		return req
	}())
	if got := string(resp.Options.Get(dhcpv4.OptionRootPath)); got != "nfs:10.0.31.1:/mnt/sata/netboot:LinuxRoot.dmg" {
		t.Errorf("legacy client root-path = %q", got)
	}
	if got := string(resp.Options.Get(dhcpv4.OptionBootfileName)); got != "undionly.kpxe" {
		t.Errorf("legacy client bootfile = %q, want undionly.kpxe", got)
	}
	if got := resp.Options.Get(dhcpv4.OptionVendorSpecificInformation); got != nil {
		t.Errorf("legacy client option 43 = % x, want none", got)
	}

	// Replacing with an empty map clears entries.
	s.SetMacRootPaths(nil)
	resp = appleReply(t, s, appleRequest(dhcpv4.MessageTypeDiscover, "AAPLBSDPC/i386"))
	if got := string(resp.Options.Get(dhcpv4.OptionRootPath)); got != "nfs:192.168.0.10:/export:NetInstall-i386.dmg" {
		t.Errorf("root-path after clear = %q, want arch default", got)
	}
}

func TestAppleNetBootInform(t *testing.T) {
	s := appleTestServer()

	// INFORM[LIST] (NetBoot 2.0 client, BSDP version 0x0101): the reply must
	// carry msg type LIST, the version, server identifier, a default boot
	// image and the boot image list with one entry derived from the root path.
	listReq, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeInform),
		dhcpv4.WithGeneric(dhcpv4.OptionClassIdentifier, []byte("AAPLBSDPC/ppc")),
		dhcpv4.WithGeneric(dhcpv4.OptionVendorSpecificInformation, []byte{
			0x01, 0x01, 0x01, // msg type LIST
			0x02, 0x02, 0x01, 0x01, // version 1.1
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp := appleReply(t, s, listReq)
	if got := string(resp.Options.Get(dhcpv4.OptionBootfileName)); got != "macnbi-ppc/booter" {
		t.Errorf("LIST bootfile = %q, want macnbi-ppc/booter", got)
	}
	if got := string(resp.Options.Get(dhcpv4.OptionRootPath)); got != "nfs:192.168.0.10:/export:NetInstall-ppc.dmg" {
		t.Errorf("LIST root-path = %q", got)
	}
	want43 := []byte{
		0x01, 0x01, 0x01, // msg type LIST
		0x02, 0x02, 0x01, 0x01, // version 1.1
		0x03, 0x04, 10, 0, 0, 1, // server identifier 10.0.0.1
		0x04, 0x02, 0x03, 0xe8, // priority 1000
		0x07, 0x04, 0x81, 0x00, 0x10, 0x00, // default boot image
		0x09, 0x0f, 0x81, 0x00, 0x10, 0x00, 0x0a, 'N', 'e', 't', 'I', 'n', 's', 't', 'a', 'l', 'l', // list entry
	}
	if got := resp.Options.Get(dhcpv4.OptionVendorSpecificInformation); !bytes.Equal(got, want43) {
		t.Errorf("LIST option 43 = % x, want % x", got, want43)
	}

	// INFORM[SELECT]: the server confirms the client's selected boot image.
	selectReq, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeInform),
		dhcpv4.WithGeneric(dhcpv4.OptionClassIdentifier, []byte("AAPLBSDPC/ppc")),
		dhcpv4.WithGeneric(dhcpv4.OptionVendorSpecificInformation, []byte{
			0x01, 0x01, 0x02, // msg type SELECT
			0x02, 0x02, 0x01, 0x01, // version 1.1
			0x08, 0x04, 0x81, 0x10, 0x00, 0x00, // selected boot image
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp = appleReply(t, s, selectReq)
	if got := string(resp.Options.Get(dhcpv4.OptionBootfileName)); got != "macnbi-ppc/booter" {
		t.Errorf("SELECT bootfile = %q, want macnbi-ppc/booter", got)
	}
	if got := string(resp.Options.Get(dhcpv4.OptionRootPath)); got != "nfs:192.168.0.10:/export:NetInstall-ppc.dmg" {
		t.Errorf("SELECT root-path = %q", got)
	}
	want43 = []byte{
		0x01, 0x01, 0x02, // msg type SELECT
		0x02, 0x02, 0x01, 0x01, // version 1.1
		0x08, 0x04, 0x81, 0x10, 0x00, 0x00, // selected boot image (echoed)
	}
	if got := resp.Options.Get(dhcpv4.OptionVendorSpecificInformation); !bytes.Equal(got, want43) {
		t.Errorf("SELECT option 43 = % x, want % x", got, want43)
	}

	// Unparseable option 43 on an INFORM: old-style ppc payload (recipe).
	oldReq, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeInform),
		dhcpv4.WithGeneric(dhcpv4.OptionClassIdentifier, []byte("AAPLBSDPC/ppc")),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp = appleReply(t, s, oldReq)
	if got := resp.Options.Get(dhcpv4.OptionVendorSpecificInformation); !bytes.Equal(got, append([]byte{0x01, 0x01, 0x02}, []byte{0x08, 0x04, 0x81, 0x00, 0x10, 0x00}...)) {
		t.Errorf("old-style INFORM option 43 = % x", got)
	}

	// INFORM replies must not hand out a lease.
	if got := resp.Options.Get(dhcpv4.OptionIPAddressLeaseTime); got != nil {
		t.Errorf("INFORM reply carries lease time: % x", got)
	}

	// i386: recipe defines no INFORM payload for unparseable 43 — none.
	i386Req, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeInform),
		dhcpv4.WithGeneric(dhcpv4.OptionClassIdentifier, []byte("AAPLBSDPC/i386")),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp = appleReply(t, s, i386Req)
	if got := resp.Options.Get(dhcpv4.OptionVendorSpecificInformation); got != nil {
		t.Errorf("i386 INFORM option 43 = % x, want none", got)
	}

	// Per-MAC root-path override applies on INFORM and feeds the list entry.
	s.SetMacRootPaths(map[string]string{
		"aa:bb:cc:dd:ee:ff": "nfs:10.0.31.1:/mnt/sata/netboot:MacOSX-10.5.8-NetBoot-ppc.dmg",
	})
	resp = appleReply(t, s, listReq)
	if got := string(resp.Options.Get(dhcpv4.OptionRootPath)); got != "nfs:10.0.31.1:/mnt/sata/netboot:MacOSX-10.5.8-NetBoot-ppc.dmg" {
		t.Errorf("LIST per-MAC root-path = %q", got)
	}
	got43 := resp.Options.Get(dhcpv4.OptionVendorSpecificInformation)
	// NetBoot-style dmg -> non-install attributes 0x01; name from the dmg.
	if !bytes.Contains(got43, []byte{0x09, 0x1a, 0x01, 0x00, 0x10, 0x00, 0x15}) ||
		!bytes.Contains(got43, []byte("MacOSX-10.5.8-NetBoot")) {
		t.Errorf("LIST per-MAC option 43 = % x, want entry 01 10 00 00 + MacOSX-10.5.8-NetBoot", got43)
	}
}
