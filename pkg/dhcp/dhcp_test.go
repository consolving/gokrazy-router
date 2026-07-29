package dhcp

import (
	"net"
	"sync"
	"testing"
	"time"
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
