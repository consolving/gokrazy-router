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
		{"", 12 * time.Hour},      // invalid → default
		{"bogus", 12 * time.Hour}, // invalid → default
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

func TestExpandEnv(t *testing.T) {
	t.Setenv("DISK_DEVICE", "/dev/sda1")
	t.Setenv("DISK_TARGET", "/mnt/data")
	t.Setenv("SMB_USER", "alice")
	t.Setenv("TFTP_ROOT", "/tftpboot")

	cfg := &Config{
		Services: ServicesConfig{
			Mount: MountConfig{
				Device: "${DISK_DEVICE}",
				Target: "${DISK_TARGET}",
			},
			SMB: SMBConfig{
				User: "${SMB_USER}",
			},
			PXE: PXEConfig{
				TFTPRoot: "${TFTP_ROOT}",
				MacImages: map[string]string{
					"aa:bb:cc:dd:ee:ff": "${TFTP_ROOT}/img.bin",
				},
			},
		},
	}
	cfg.ExpandEnv()

	if cfg.Services.Mount.Device != "/dev/sda1" {
		t.Errorf("Mount.Device = %q, want /dev/sda1", cfg.Services.Mount.Device)
	}
	if cfg.Services.Mount.Target != "/mnt/data" {
		t.Errorf("Mount.Target = %q, want /mnt/data", cfg.Services.Mount.Target)
	}
	if cfg.Services.SMB.User != "alice" {
		t.Errorf("SMB.User = %q, want alice", cfg.Services.SMB.User)
	}
	if cfg.Services.PXE.TFTPRoot != "/tftpboot" {
		t.Errorf("PXE.TFTPRoot = %q, want /tftpboot", cfg.Services.PXE.TFTPRoot)
	}
	if cfg.Services.PXE.MacImages["aa:bb:cc:dd:ee:ff"] != "/tftpboot/img.bin" {
		t.Errorf("PXE.MacImages expanded = %q, want /tftpboot/img.bin", cfg.Services.PXE.MacImages["aa:bb:cc:dd:ee:ff"])
	}
}

func TestLoadExtras(t *testing.T) {
	tomlData := `
reservations = { "AA-BB-CC-DD-EE-FF" = "10.0.0.50", "00:11:22:33:44:55" = "10.0.0.51" }
macImages = { "AA-BB-CC-DD-EE-FF" = "netboot-arm.bin" }

[[smbUsers]]
name = "bob"
password = "secret"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "extras.toml")
	if err := os.WriteFile(path, []byte(tomlData), 0644); err != nil {
		t.Fatal(err)
	}

	extras, err := LoadExtras(path)
	if err != nil {
		t.Fatalf("LoadExtras() error: %v", err)
	}
	if extras.Reservations["AA-BB-CC-DD-EE-FF"] != "10.0.0.50" {
		t.Errorf("reservation = %q, want 10.0.0.50", extras.Reservations["AA-BB-CC-DD-EE-FF"])
	}
	if extras.MacImages["AA-BB-CC-DD-EE-FF"] != "netboot-arm.bin" {
		t.Errorf("mac image = %q, want netboot-arm.bin", extras.MacImages["AA-BB-CC-DD-EE-FF"])
	}
	if len(extras.SMBUsers) != 1 || extras.SMBUsers[0].Name != "bob" {
		t.Errorf("SMB users = %+v, want [bob]", extras.SMBUsers)
	}
}

func TestApplyExtras(t *testing.T) {
	cfg := Default()
	cfg.VLANs = []VLANConfig{{ID: 1, Name: "test"}}
	cfg.Services.PXE.MacImages = map[string]string{
		"00:00:00:00:00:00": "fallback.bin",
	}

	extras := &ExtrasConfig{
		Reservations: map[string]string{
			"AA-BB-CC-DD-EE-FF": "10.0.0.50",
		},
		MacImages: map[string]string{
			"AA-BB-CC-DD-EE-FF": "netboot-arm.bin",
		},
		DefaultImage: "netboot.xyz.efi",
		PXEBootFile:  "netboot.xyz.efi",
	}
	cfg.ApplyExtras(extras)

	if cfg.LAN.DHCP.Reservations["aa:bb:cc:dd:ee:ff"] != "10.0.0.50" {
		t.Errorf("LAN reservation = %v", cfg.LAN.DHCP.Reservations)
	}
	if cfg.Services.PXE.MacImages["aa:bb:cc:dd:ee:ff"] != "netboot-arm.bin" {
		t.Errorf("PXE mac image = %v", cfg.Services.PXE.MacImages)
	}
	if _, ok := cfg.Services.PXE.MacImages["00:00:00:00:00:00"]; ok {
		t.Error("base mac image should be replaced, not merged, when extras are present")
	}
	if cfg.Services.PXE.DefaultImage != "netboot.xyz.efi" {
		t.Errorf("DefaultImage = %q, want netboot.xyz.efi", cfg.Services.PXE.DefaultImage)
	}
	if cfg.VLANs[0].DHCP.PXEBootFile != "netboot.xyz.efi" {
		t.Errorf("VLAN PXEBootFile = %q, want netboot.xyz.efi", cfg.VLANs[0].DHCP.PXEBootFile)
	}
	if cfg.WiFi.DHCP.PXEBootFile != "netboot.xyz.efi" {
		t.Errorf("WiFi PXEBootFile = %q, want netboot.xyz.efi", cfg.WiFi.DHCP.PXEBootFile)
	}
}

func TestApplyExtrasReplacesRemovedValues(t *testing.T) {
	cfg := Default()
	cfg.VLANs = []VLANConfig{{ID: 1, Name: "test"}}
	cfg.Services.PXE.MacImages = map[string]string{
		"aa:bb:cc:dd:ee:01": "one.bin",
		"aa:bb:cc:dd:ee:02": "two.bin",
	}
	cfg.Services.PXE.DefaultImage = "base.efi"
	cfg.Services.PXE.LegacyBootFile = "base-legacy"
	cfg.VLANs[0].DHCP.PXEBootFile = "base-boot"

	// Reload 1: extras replaces and adds.
	cfg.ApplyExtras(&ExtrasConfig{
		MacImages: map[string]string{
			"aa:bb:cc:dd:ee:01": "one-v2.bin",
			"aa:bb:cc:dd:ee:03": "three.bin",
		},
		DefaultImage: "new.efi",
		PXEBootFile:  "new-boot",
	})
	if got := cfg.Services.PXE.MacImages["aa:bb:cc:dd:ee:01"]; got != "one-v2.bin" {
		t.Errorf("changed mac image = %q, want one-v2.bin", got)
	}
	if _, ok := cfg.Services.PXE.MacImages["aa:bb:cc:dd:ee:02"]; ok {
		t.Error("removed mac image still present")
	}
	if got := cfg.Services.PXE.MacImages["aa:bb:cc:dd:ee:03"]; got != "three.bin" {
		t.Errorf("added mac image = %q, want three.bin", got)
	}
	if got := cfg.Services.PXE.DefaultImage; got != "new.efi" {
		t.Errorf("DefaultImage = %q, want new.efi", got)
	}
	if got := cfg.VLANs[0].DHCP.PXEBootFile; got != "new-boot" {
		t.Errorf("VLAN PXEBootFile = %q, want new-boot", got)
	}

	// Reload 2: clearing values in the extras file must clear the runtime state.
	cfg.ApplyExtras(&ExtrasConfig{
		MacImages: map[string]string{},
	})
	if len(cfg.Services.PXE.MacImages) != 0 {
		t.Errorf("mac images after clear = %v, want empty", cfg.Services.PXE.MacImages)
	}
	if got := cfg.Services.PXE.DefaultImage; got != "" {
		t.Errorf("DefaultImage after clear = %q, want empty", got)
	}
	if got := cfg.Services.PXE.LegacyBootFile; got != "" {
		t.Errorf("LegacyBootFile after clear = %q, want empty", got)
	}
	if got := cfg.VLANs[0].DHCP.PXEBootFile; got != "" {
		t.Errorf("VLAN PXEBootFile after clear = %q, want empty", got)
	}
}

func TestExtrasSetReservation(t *testing.T) {
	e := &ExtrasConfig{}
	if err := e.SetReservation("aa:bb:cc:dd:ee:ff", "10.0.1.10"); err != nil {
		t.Fatalf("SetReservation: %v", err)
	}
	if e.Reservations["aa:bb:cc:dd:ee:ff"] != "10.0.1.10" {
		t.Errorf("expected 10.0.1.10, got %s", e.Reservations["aa:bb:cc:dd:ee:ff"])
	}
	if err := e.SetReservation("aa:bb:cc:dd:ee:ff", "10.0.1.10"); err != ErrNotModified {
		t.Errorf("expected ErrNotModified, got %v", err)
	}
	if err := e.SetReservation("aa:bb:cc:dd:ee:ff", "10.0.1.11"); err != nil {
		t.Fatalf("SetReservation update: %v", err)
	}
	if e.Reservations["aa:bb:cc:dd:ee:ff"] != "10.0.1.11" {
		t.Errorf("expected 10.0.1.11, got %s", e.Reservations["aa:bb:cc:dd:ee:ff"])
	}
}

func TestExtrasRemoveReservation(t *testing.T) {
	e := &ExtrasConfig{}
	if err := e.RemoveReservation("aa:bb:cc:dd:ee:ff"); err != ErrNotModified {
		t.Errorf("expected ErrNotModified for empty, got %v", err)
	}
	e.Reservations = map[string]string{"aa:bb:cc:dd:ee:ff": "10.0.1.10"}
	if err := e.RemoveReservation("aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("RemoveReservation: %v", err)
	}
	if _, ok := e.Reservations["aa:bb:cc:dd:ee:ff"]; ok {
		t.Error("reservation should be deleted")
	}
}

func TestExtrasSetMacImage(t *testing.T) {
	e := &ExtrasConfig{}
	if err := e.SetMacImage("aa:bb:cc:dd:ee:ff", "ipxe.efi"); err != nil {
		t.Fatalf("SetMacImage: %v", err)
	}
	if e.MacImages["aa:bb:cc:dd:ee:ff"] != "ipxe.efi" {
		t.Errorf("expected ipxe.efi, got %s", e.MacImages["aa:bb:cc:dd:ee:ff"])
	}
	if err := e.SetMacImage("aa:bb:cc:dd:ee:ff", "ipxe.efi"); err != ErrNotModified {
		t.Errorf("expected ErrNotModified, got %v", err)
	}
}

func TestExtrasRemoveMacImage(t *testing.T) {
	e := &ExtrasConfig{}
	if err := e.RemoveMacImage("aa:bb:cc:dd:ee:ff"); err != ErrNotModified {
		t.Errorf("expected ErrNotModified for empty, got %v", err)
	}
	e.MacImages = map[string]string{"aa:bb:cc:dd:ee:ff": "ipxe.efi"}
	if err := e.RemoveMacImage("aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("RemoveMacImage: %v", err)
	}
}

func TestExtrasSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "extras.toml")

	orig := &ExtrasConfig{
		Reservations: map[string]string{"aa:bb:cc:dd:ee:ff": "10.0.1.10"},
		MacImages:    map[string]string{"11:22:33:44:55:66": "ubuntu.efi"},
		DefaultImage: "netboot.xyz.efi",
		PXEBootFile:  "netboot.xyz.efi",
		SMBUsers:     []SMBUser{{Name: "backup", Password: "secret"}},
	}
	if err := orig.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadExtras(path)
	if err != nil {
		t.Fatalf("LoadExtras: %v", err)
	}
	if loaded.Reservations["aa:bb:cc:dd:ee:ff"] != "10.0.1.10" {
		t.Errorf("reservation mismatch: %s", loaded.Reservations["aa:bb:cc:dd:ee:ff"])
	}
	if loaded.MacImages["11:22:33:44:55:66"] != "ubuntu.efi" {
		t.Errorf("mac image mismatch: %s", loaded.MacImages["11:22:33:44:55:66"])
	}
	if loaded.DefaultImage != "netboot.xyz.efi" {
		t.Errorf("DefaultImage = %q, want netboot.xyz.efi", loaded.DefaultImage)
	}
	if loaded.PXEBootFile != "netboot.xyz.efi" {
		t.Errorf("PXEBootFile = %q, want netboot.xyz.efi", loaded.PXEBootFile)
	}
	if len(loaded.SMBUsers) != 1 || loaded.SMBUsers[0].Name != "backup" {
		t.Errorf("smb users mismatch: %+v", loaded.SMBUsers)
	}
}

func TestExtrasSaveLoadEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.toml")

	e := &ExtrasConfig{}
	if err := e.Save(path); err != nil {
		t.Fatalf("Save empty: %v", err)
	}

	loaded, err := LoadExtras(path)
	if err != nil {
		t.Fatalf("LoadExtras: %v", err)
	}
	if len(loaded.Reservations) != 0 || len(loaded.MacImages) != 0 {
		t.Error("expected empty extras")
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

func TestLoadIPv6Config(t *testing.T) {
	json := `{
		"wan": {"mode6": "static", "address6": "2001:db8::2/64", "gateway6": "2001:db8::1"},
		"lan": {
			"address6": "fd00::1/64",
			"ra": true,
			"dhcp6": true,
			"dns6": ["2001:db8::53"]
		},
		"nat": {"enabled6": true},
		"dns6": ["2606:4700:4700::1111"]
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

	if cfg.WAN.Mode6 != "static" {
		t.Errorf("WAN.Mode6 = %q, want static", cfg.WAN.Mode6)
	}
	if cfg.WAN.Address6 != "2001:db8::2/64" {
		t.Errorf("WAN.Address6 = %q", cfg.WAN.Address6)
	}
	if cfg.WAN.Gateway6 != "2001:db8::1" {
		t.Errorf("WAN.Gateway6 = %q", cfg.WAN.Gateway6)
	}
	if cfg.LAN.Address6 != "fd00::1/64" {
		t.Errorf("LAN.Address6 = %q", cfg.LAN.Address6)
	}
	if !cfg.LAN.RA {
		t.Error("LAN.RA = false, want true")
	}
	if !cfg.LAN.DHCP6 {
		t.Error("LAN.DHCP6 = false, want true")
	}
	if len(cfg.LAN.DNS6) != 1 || cfg.LAN.DNS6[0] != "2001:db8::53" {
		t.Errorf("LAN.DNS6 = %v", cfg.LAN.DNS6)
	}
	if !cfg.NAT.Enabled6 {
		t.Error("NAT.Enabled6 = false, want true")
	}
	if len(cfg.DNS6) != 1 || cfg.DNS6[0] != "2606:4700:4700::1111" {
		t.Errorf("DNS6 = %v", cfg.DNS6)
	}
}

func TestApplyExtrasVLANAddresses6(t *testing.T) {
	cfg := Default()
	cfg.VLANs = []VLANConfig{
		{ID: 1, Name: "main", Address: "10.0.0.1/24"},
		{ID: 2, Name: "guest", Address: "10.0.1.1/24"},
	}

	extras := &ExtrasConfig{
		VLANAddresses:  map[int]string{2: "10.0.9.1/24"},
		VLANAddresses6: map[int]string{1: "fd00::1/64"},
		DNS6:           []string{"2001:db8::53"},
	}
	cfg.ApplyExtras(extras)

	if cfg.VLANs[1].Address != "10.0.9.1/24" {
		t.Errorf("VLAN2 Address = %q, want 10.0.9.1/24", cfg.VLANs[1].Address)
	}
	if cfg.VLANs[0].Address6 != "fd00::1/64" {
		t.Errorf("VLAN1 Address6 = %q, want fd00::1/64", cfg.VLANs[0].Address6)
	}
	for _, v := range cfg.VLANs {
		if len(v.DNS6) != 1 || v.DNS6[0] != "2001:db8::53" {
			t.Errorf("VLAN %d DNS6 = %v", v.ID, v.DNS6)
		}
	}
	if len(cfg.WiFi.DNS6) != 1 || cfg.WiFi.DNS6[0] != "2001:db8::53" {
		t.Errorf("WiFi DNS6 = %v", cfg.WiFi.DNS6)
	}
}

func TestExtrasFromConfigVLANAddresses6(t *testing.T) {
	cfg := Default()
	cfg.VLANs = []VLANConfig{{ID: 1, Name: "main", Address6: "fd00::1/64"}}

	e := ExtrasFromConfig(cfg)
	if e.VLANAddresses6[1] != "fd00::1/64" {
		t.Errorf("ExtrasFromConfig VLANAddresses6 = %v", e.VLANAddresses6)
	}
	if e.VLANAddresses[1] != cfg.VLANs[0].Address {
		t.Errorf("ExtrasFromConfig VLANAddresses = %v", e.VLANAddresses)
	}
}

func TestExtrasSaveLoadIPv6(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "extras.toml")

	orig := &ExtrasConfig{
		VLANAddresses6: map[int]string{1: "fd00::1/64"},
		DNS6:           []string{"2001:db8::53"},
	}
	if err := orig.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadExtras(path)
	if err != nil {
		t.Fatalf("LoadExtras: %v", err)
	}
	if loaded.VLANAddresses6[1] != "fd00::1/64" {
		t.Errorf("VLANAddresses6 mismatch: %v", loaded.VLANAddresses6)
	}
	if len(loaded.DNS6) != 1 || loaded.DNS6[0] != "2001:db8::53" {
		t.Errorf("DNS6 mismatch: %v", loaded.DNS6)
	}
}
