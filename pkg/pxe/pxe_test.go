package pxe

import (
	"testing"

	"github.com/consolving/gokrazy-router/pkg/config"
)

func TestResolveImage(t *testing.T) {
	s := New(cfgWithDefaults())
	s.cfg.DefaultImage = "undionly.kpxe"
	s.SetMacImages(map[string]string{
		"82:78:97:41:e1:db": "netboot.xyz.img",
	})

	cases := []struct {
		req  string
		want string
	}{
		{"boot.ipxe", "boot.ipxe"},
		{"undionly.kpxe", "undionly.kpxe"},
		{"netboot.xyz.efi", "netboot.xyz.efi"},
		{"01-82-78-97-41-e1-db", "netboot.xyz.img"},
		{"01-00-11-22-33-44-55", "undionly.kpxe"},
	}
	for _, c := range cases {
		if got := s.resolveImage(c.req); got != c.want {
			t.Errorf("resolveImage(%q) = %q, want %q", c.req, got, c.want)
		}
	}
}

func TestResolveImageNoDefault(t *testing.T) {
	s := New(cfgWithDefaults())
	s.SetMacImages(map[string]string{
		"82:78:97:41:e1:db": "netboot.xyz.img",
	})

	cases := []struct {
		req  string
		want string
	}{
		{"boot.ipxe", "boot.ipxe"},
		{"01-00-11-22-33-44-55", "01-00-11-22-33-44-55"},
	}
	for _, c := range cases {
		if got := s.resolveImage(c.req); got != c.want {
			t.Errorf("resolveImage(%q) = %q, want %q", c.req, got, c.want)
		}
	}
}

func cfgWithDefaults() config.PXEConfig {
	return config.PXEConfig{Enabled: true, TFTPRoot: "/tmp/tftpboot"}
}
