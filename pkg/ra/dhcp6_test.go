package ra

import (
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/iana"
)

func newTestDHCP6Server() *DHCP6Server {
	_, prefix, _ := net.ParseCIDR("fd00::/64")
	return &DHCP6Server{
		iface:    "eth0",
		addr6:    "fd00::1/64",
		prefix:   *prefix,
		dns:      []net.IP{net.ParseIP("fd00::53")},
		serverID: &dhcpv6.DUIDLL{HWType: iana.HWTypeEthernet, LinkLayerAddr: net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}},
		leases:   make(map[string]lease6),
	}
}

func solicitWithArch(t *testing.T, archs ...iana.Arch) *dhcpv6.Message {
	t.Helper()
	m, err := dhcpv6.NewSolicit(net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01},
		dhcpv6.WithOption(dhcpv6.OptClientArchType(archs...)))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func solicitWithoutArch(t *testing.T) *dhcpv6.Message {
	t.Helper()
	m, err := dhcpv6.NewSolicit(net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x02})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDHCP6PXEBootfileSelection(t *testing.T) {
	s := newTestDHCP6Server()
	s.SetPXEBootFile6(net.ParseIP("fd00::1"), "undionly.kpxe", "netboot.xyz.efi")

	cases := []struct {
		name     string
		msg      *dhcpv6.Message
		wantURL  string
	}{
		{"legacy BIOS", solicitWithArch(t, iana.INTEL_X86PC), "tftp://[fd00::1]/undionly.kpxe"},
		{"UEFI x86-64", solicitWithArch(t, iana.EFI_X86_64), "tftp://[fd00::1]/netboot.xyz.efi"},
		{"UEFI ARM64", solicitWithArch(t, iana.EFI_ARM64), "tftp://[fd00::1]/netboot.xyz.efi"},
		{"dual-stack prefers UEFI", solicitWithArch(t, iana.INTEL_X86PC, iana.EFI_X86_64), "tftp://[fd00::1]/netboot.xyz.efi"},
		{"no arch, no URL", solicitWithoutArch(t), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.pxeBootfileURL(c.msg); got != c.wantURL {
				t.Errorf("pxeBootfileURL = %q, want %q", got, c.wantURL)
			}
		})
	}
}

func TestDHCP6PXEBootfileSelectionUnconfigured(t *testing.T) {
	t.Run("no PXE configured", func(t *testing.T) {
		s := newTestDHCP6Server()
		if got := s.pxeBootfileURL(solicitWithArch(t, iana.EFI_X86_64)); got != "" {
			t.Errorf("pxeBootfileURL = %q, want empty when PXE unconfigured", got)
		}
	})
	t.Run("no boot server", func(t *testing.T) {
		s := newTestDHCP6Server()
		s.SetPXEBootFile6(nil, "undionly.kpxe", "netboot.xyz.efi")
		if got := s.pxeBootfileURL(solicitWithArch(t, iana.EFI_X86_64)); got != "" {
			t.Errorf("pxeBootfileURL = %q, want empty without boot server", got)
		}
	})
	t.Run("legacy file missing for legacy client", func(t *testing.T) {
		s := newTestDHCP6Server()
		s.SetPXEBootFile6(net.ParseIP("fd00::1"), "", "netboot.xyz.efi")
		if got := s.pxeBootfileURL(solicitWithArch(t, iana.INTEL_X86PC)); got != "" {
			t.Errorf("pxeBootfileURL = %q, want empty without legacy bootfile", got)
		}
	})
	t.Run("UEFI file missing for UEFI client", func(t *testing.T) {
		s := newTestDHCP6Server()
		s.SetPXEBootFile6(net.ParseIP("fd00::1"), "undionly.kpxe", "")
		if got := s.pxeBootfileURL(solicitWithArch(t, iana.EFI_X86_64)); got != "" {
			t.Errorf("pxeBootfileURL = %q, want empty without UEFI bootfile", got)
		}
	})
}

// TestDHCP6PXEBootfileInReply verifies the option 59 bootfile URL is actually
// carried in the advertised reply, not just computed.
func TestDHCP6PXEBootfileInReply(t *testing.T) {
	s := newTestDHCP6Server()
	s.SetPXEBootFile6(net.ParseIP("fd00::1"), "undionly.kpxe", "netboot.xyz.efi")

	msg := solicitWithArch(t, iana.EFI_X86_64)
	reply, err := dhcpv6.NewAdvertiseFromSolicit(msg, s.modifiers(msg, nil)...)
	if err != nil {
		t.Fatal(err)
	}

	opt := reply.GetOneOption(dhcpv6.OptionBootfileURL)
	if opt == nil {
		t.Fatal("reply has no option 59 (bootfile URL)")
	}
	if got := string(opt.ToBytes()); got != "tftp://[fd00::1]/netboot.xyz.efi" {
		t.Errorf("option 59 = %q, want tftp://[fd00::1]/netboot.xyz.efi", got)
	}
}
