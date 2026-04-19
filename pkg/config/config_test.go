package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	cfg := Default()
	if cfg.WAN.Interface != "wan" {
		t.Errorf("WAN.Interface = %q, want %q", cfg.WAN.Interface, "wan")
	}
	if cfg.LAN.Bridge != "br-lan" {
		t.Errorf("LAN.Bridge = %q, want %q", cfg.LAN.Bridge, "br-lan")
	}
	if len(cfg.LAN.Interfaces) != 4 {
		t.Errorf("LAN.Interfaces = %d, want 4", len(cfg.LAN.Interfaces))
	}
	if cfg.LAN.Address != "10.0.0.1/24" {
		t.Errorf("LAN.Address = %q, want %q", cfg.LAN.Address, "10.0.0.1/24")
	}
	if !cfg.LAN.DHCP.Enabled {
		t.Error("LAN.DHCP.Enabled = false, want true")
	}
	if !cfg.NAT.Enabled {
		t.Error("NAT.Enabled = false, want true")
	}
	if cfg.NAT.OutInterface != "wan" {
		t.Errorf("NAT.OutInterface = %q, want %q", cfg.NAT.OutInterface, "wan")
	}
	if cfg.WiFi.Enabled {
		t.Error("WiFi should be disabled by default")
	}
}

func TestParseLeaseDuration(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"12h", 12 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"30m", 30 * time.Minute},
		{"", 12 * time.Hour},       // invalid → default
		{"bogus", 12 * time.Hour},  // invalid → default
	}
	for _, tt := range tests {
		d := DHCPConfig{LeaseDuration: tt.input}
		got := d.ParseLeaseDuration()
		if got != tt.want {
			t.Errorf("ParseLeaseDuration(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestLoad(t *testing.T) {
	json := `{
		"wan": {"interface": "eth0", "mode": "static"},
		"lan": {
			"bridge": "br0",
			"interfaces": ["lan1"],
			"address": "192.168.1.1/24",
			"dhcp": {
				"enabled": false,
				"rangeStart": "192.168.1.100",
				"rangeEnd": "192.168.1.200",
				"leaseDuration": "6h",
				"dns": ["9.9.9.9"]
			}
		},
		"nat": {"enabled": false, "outInterface": "eth0"},
		"wifi": {
			"enabled": true,
			"interface": "wlan1",
			"ssid": "TestNet",
			"passphrase": "testpass123",
			"channel": 11,
			"hwMode": "a",
			"wpa": 2
		}
	}`

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(json), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.WAN.Interface != "eth0" {
		t.Errorf("WAN.Interface = %q, want %q", cfg.WAN.Interface, "eth0")
	}
	if cfg.WAN.Mode != "static" {
		t.Errorf("WAN.Mode = %q, want %q", cfg.WAN.Mode, "static")
	}
	if cfg.WiFi.SSID != "TestNet" {
		t.Errorf("WiFi.SSID = %q, want %q", cfg.WiFi.SSID, "TestNet")
	}
	if cfg.WiFi.Channel != 11 {
		t.Errorf("WiFi.Channel = %d, want 11", cfg.WiFi.Channel)
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path.json")
	if err == nil {
		t.Error("Load() should fail for nonexistent file")
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{invalid"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Error("Load() should fail for invalid JSON")
	}
}
