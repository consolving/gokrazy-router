package wifi

import (
	"bytes"
	"strings"
	"testing"

	"github.com/consolving/gokrazy-router/pkg/config"
)

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.WiFiConfig
		wantErr string
	}{
		{"empty SSID", config.WiFiConfig{Passphrase: "12345678"}, "SSID must not be empty"},
		{"empty passphrase", config.WiFiConfig{SSID: "test"}, "passphrase must not be empty"},
		{"short passphrase", config.WiFiConfig{SSID: "test", Passphrase: "short"}, "8-63 characters"},
		{"long passphrase", config.WiFiConfig{SSID: "test", Passphrase: strings.Repeat("a", 64)}, "8-63 characters"},
		{"valid", config.WiFiConfig{SSID: "test", Passphrase: "12345678"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg, "")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewDefaults(t *testing.T) {
	ap, err := New(config.WiFiConfig{SSID: "test", Passphrase: "12345678"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ap.cfg.Interface != "wlan0" {
		t.Errorf("Interface = %q, want wlan0", ap.cfg.Interface)
	}
	if ap.cfg.HWMode != "g" {
		t.Errorf("HWMode = %q, want g", ap.cfg.HWMode)
	}
	if ap.cfg.Channel != 6 {
		t.Errorf("Channel = %d, want 6", ap.cfg.Channel)
	}
	if ap.cfg.WPA != 2 {
		t.Errorf("WPA = %d, want 2", ap.cfg.WPA)
	}
}

func TestConfTemplate(t *testing.T) {
	data := struct {
		config.WiFiConfig
		Bridge        string
		DynamicVLAN   bool
		VLANFile      string
		AcceptMACFile string
		VLANBridge    string
	}{
		WiFiConfig: config.WiFiConfig{
			Interface:   "wlan0",
			SSID:        "MyNet",
			Passphrase:  "secret123",
			HWMode:      "g",
			Channel:     6,
			CountryCode: "DE",
			WPA:         2,
		},
		Bridge: "br-lan",
	}

	var buf bytes.Buffer
	if err := confTemplate.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		"interface=wlan0",
		"bridge=br-lan",
		"ssid=MyNet",
		"hw_mode=g",
		"channel=6",
		"country_code=DE",
		"wpa=2",
		"wpa_passphrase=secret123",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config missing %q", want)
		}
	}
	// Should NOT contain dynamic VLAN directives.
	if strings.Contains(out, "dynamic_vlan") {
		t.Error("config should not contain dynamic_vlan when DynamicVLAN=false")
	}
}

func TestConfTemplateNoBridge(t *testing.T) {
	data := struct {
		config.WiFiConfig
		Bridge        string
		DynamicVLAN   bool
		VLANFile      string
		AcceptMACFile string
		VLANBridge    string
	}{
		WiFiConfig: config.WiFiConfig{
			Interface:  "wlan0",
			SSID:       "Test",
			Passphrase: "12345678",
			HWMode:     "g",
			Channel:    1,
			WPA:        2,
		},
	}

	var buf bytes.Buffer
	if err := confTemplate.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "bridge=") {
		t.Error("config should not contain bridge= in routed mode")
	}
}

func TestConfTemplateDynamicVLAN(t *testing.T) {
	data := struct {
		config.WiFiConfig
		Bridge        string
		DynamicVLAN   bool
		VLANFile      string
		AcceptMACFile string
		VLANBridge    string
	}{
		WiFiConfig: config.WiFiConfig{
			Interface:  "wlan0",
			SSID:       "Test",
			Passphrase: "12345678",
			HWMode:     "g",
			Channel:    6,
			WPA:        2,
		},
		DynamicVLAN:   true,
		VLANFile:      "/tmp/hostapd.vlan",
		AcceptMACFile: "/tmp/hostapd.accept",
		VLANBridge:    "br-vlan",
	}

	var buf bytes.Buffer
	if err := confTemplate.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		"dynamic_vlan=1",
		"vlan_file=/tmp/hostapd.vlan",
		"accept_mac_file=/tmp/hostapd.accept",
		"vlan_bridge=br-vlan",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config missing %q:\n%s", want, out)
		}
	}
}

func TestParseClientEvents(t *testing.T) {
	ap := &AP{
		clients: make(map[string]bool),
	}

	var events []ClientEvent
	ap.onClient = func(ev ClientEvent) {
		events = append(events, ev)
	}

	input := strings.Join([]string{
		"wlan0: STA aa:bb:cc:dd:ee:ff IEEE 802.11: associated",
		"wlan0: AP-STA-CONNECTED aa:bb:cc:dd:ee:ff",
		"wlan0: AP-STA-DISCONNECTED aa:bb:cc:dd:ee:ff",
		"wlan0: STA 11:22:33:44:55:66 IEEE 802.11: disassociated",
	}, "\n")

	ap.parseOutput(strings.NewReader(input))

	// Check that we got events
	if len(events) == 0 {
		t.Fatal("expected client events, got none")
	}

	// Check client map state: both should be disconnected at the end
	ap.mu.Lock()
	if ap.clients["aa:bb:cc:dd:ee:ff"] {
		t.Error("aa:bb:cc:dd:ee:ff should be disconnected")
	}
	if ap.clients["11:22:33:44:55:66"] {
		t.Error("11:22:33:44:55:66 should be disconnected")
	}
	ap.mu.Unlock()
}

func TestClients(t *testing.T) {
	ap := &AP{clients: make(map[string]bool)}
	ap.clients["aa:bb:cc:dd:ee:ff"] = true
	ap.clients["11:22:33:44:55:66"] = false

	got := ap.Clients()
	if len(got) != 1 {
		t.Errorf("Clients() returned %d, want 1", len(got))
	}
	if len(got) > 0 && got[0] != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("Clients()[0] = %q, want aa:bb:cc:dd:ee:ff", got[0])
	}
}
