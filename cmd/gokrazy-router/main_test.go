package main

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/consolving/gokrazy-router/pkg/config"
	"github.com/consolving/gokrazy-router/pkg/dhcp"
	"github.com/consolving/gokrazy-router/pkg/pxe"
)

func TestApplyGlobalDNS6(t *testing.T) {
	global := []string{"2606:4700:4700::1111", "2001:4860:4860::8888"}

	// Scope without a dns6 inherits the global default.
	cfg := config.Default()
	cfg.DNS6 = global
	cfg.VLANs = []config.VLANConfig{{ID: 31}}
	applyGlobalDNS6(cfg)
	if len(cfg.LAN.DNS6) != len(global) || cfg.LAN.DNS6[0] != global[0] {
		t.Errorf("LAN.DNS6 not inherited: %v", cfg.LAN.DNS6)
	}
	if len(cfg.VLANs[0].DNS6) != len(global) || cfg.VLANs[0].DNS6[0] != global[0] {
		t.Errorf("VLAN.DNS6 not inherited: %v", cfg.VLANs[0].DNS6)
	}
	if len(cfg.WiFi.DNS6) != len(global) {
		t.Errorf("WiFi.DNS6 not inherited: %v", cfg.WiFi.DNS6)
	}

	// A scope with an explicit empty (non-nil) dns6 advertises no IPv6 DNS.
	cfg = config.Default()
	cfg.DNS6 = global
	cfg.VLANs = []config.VLANConfig{{ID: 31, DNS6: []string{}}}
	cfg.WiFi.DNS6 = []string{}
	applyGlobalDNS6(cfg)
	if cfg.VLANs[0].DNS6 == nil || len(cfg.VLANs[0].DNS6) != 0 {
		t.Errorf("VLAN empty dns6 overwritten: %v", cfg.VLANs[0].DNS6)
	}
	if cfg.WiFi.DNS6 == nil || len(cfg.WiFi.DNS6) != 0 {
		t.Errorf("WiFi empty dns6 overwritten: %v", cfg.WiFi.DNS6)
	}
	if len(cfg.LAN.DNS6) == 0 {
		t.Errorf("LAN should have inherited global dns6, got %v", cfg.LAN.DNS6)
	}
}

func TestMergedReservations(t *testing.T) {
	got := mergedReservations(
		map[string]string{"aa:bb:cc:dd:ee:01": "10.0.0.50", "aa:bb:cc:dd:ee:02": "10.0.0.51"},
		map[string]string{"aa:bb:cc:dd:ee:02": "10.0.0.99", "aa:bb:cc:dd:ee:03": "10.0.0.52"},
	)
	if got["aa:bb:cc:dd:ee:01"] != "10.0.0.50" {
		t.Errorf("base reservation dropped: %v", got)
	}
	if got["aa:bb:cc:dd:ee:02"] != "10.0.0.99" {
		t.Errorf("extras reservation should win over base: %v", got)
	}
	if got["aa:bb:cc:dd:ee:03"] != "10.0.0.52" {
		t.Errorf("extras-only reservation missing: %v", got)
	}
}

func TestReloaderRejectsVLANAddressChange(t *testing.T) {
	cfg := config.Default()
	cfg.VLANs = []config.VLANConfig{{ID: 31, Address: "10.0.31.1/24"}}
	base := cloneConfig(cfg)

	// Unchanged override is accepted.
	r := &reloader{cfg: cfg, base: base}
	if err := r.checkRestartRequired(&config.ExtrasConfig{VLANAddresses: map[int]string{31: "10.0.31.1/24"}}); err != nil {
		t.Errorf("unchanged vlanAddresses rejected: %v", err)
	}

	// A changed override requires a restart.
	err := r.checkRestartRequired(&config.ExtrasConfig{VLANAddresses: map[int]string{31: "10.0.31.2/24"}})
	var restartErr *restartRequiredError
	if !errors.As(err, &restartErr) {
		t.Fatalf("expected restartRequiredError, got %v", err)
	}
}

func TestReloaderRejectsSMBUserChange(t *testing.T) {
	cfg := config.Default()
	base := cloneConfig(cfg)

	r := &reloader{cfg: cfg, base: base}
	if err := r.checkRestartRequired(&config.ExtrasConfig{}); err != nil {
		t.Errorf("unchanged (empty) smbUsers rejected: %v", err)
	}

	err := r.checkRestartRequired(&config.ExtrasConfig{
		SMBUsers: []config.SMBUser{{Name: "alice", Password: "pw"}},
	})
	var restartErr *restartRequiredError
	if !errors.As(err, &restartErr) {
		t.Fatalf("expected restartRequiredError for new smbUsers, got %v", err)
	}
}

// TestConcurrentReload exercises the reload path from multiple goroutines.
// The reloader serializes reloads; without the mutex, go test -race would
// report data races on the shared cfg while ApplyExtras mutates it.
func TestConcurrentReload(t *testing.T) {
	dir := t.TempDir()
	extrasPath := filepath.Join(dir, "extras.toml")
	extras := &config.ExtrasConfig{
		Reservations: map[string]string{"aa:bb:cc:dd:ee:99": "10.0.0.77"},
		MacImages:    map[string]string{"aa:bb:cc:dd:ee:99": "custom.efi"},
		DefaultImage: "netboot.xyz.efi",
	}
	if err := extras.Save(extrasPath); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Services.PXE.Enabled = true
	cfg.Services.PXE.MacImages = map[string]string{"aa:bb:cc:dd:ee:01": "base.efi"}
	cfg.LAN.DHCP.Reservations = map[string]string{"aa:bb:cc:dd:ee:01": "10.0.0.60"}
	base := cloneConfig(cfg)

	srv, err := dhcp.New("eth0", "10.0.0.1/24", "10.0.0.100", "10.0.0.200", []string{"8.8.8.8"}, 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	entry := dhcpEntry{srv: srv, tftpAddr: "10.0.0.1", baseScope: &base.LAN.DHCP}

	r := &reloader{
		cfg:         cfg,
		base:        base,
		extrasPath:  extrasPath,
		dhcpEntries: []dhcpEntry{entry},
		pxeSrv:      pxe.New(cfg.Services.PXE),
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if err := r.Reload(); err != nil {
					t.Errorf("reload: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// The base-JSON reservation must survive reloads alongside the extras one.
	if cfg.LAN.DHCP.Reservations["aa:bb:cc:dd:ee:01"] != "10.0.0.60" {
		t.Errorf("base reservation lost after reload: %v", cfg.LAN.DHCP.Reservations)
	}
	if cfg.LAN.DHCP.Reservations["aa:bb:cc:dd:ee:99"] != "10.0.0.77" {
		t.Errorf("extras reservation not applied after reload: %v", cfg.LAN.DHCP.Reservations)
	}
}
