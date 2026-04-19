package macmap

import (
	"testing"
)

const testTOML = `
default_vlan = 30

[[clients]]
mac = "AA:BB:CC:DD:EE:FF"
vlan = 10
name = "Philipp's laptop"

[[clients]]
mac = "11:22:33:44:55:66"
vlan = 20
name = "thermostat"
hostname = "tado-bridge"

[[clients]]
mac = "de:ad:be:ef:00:01"
vlan = 10
name = "workstation"
`

func TestParse(t *testing.T) {
	mf, err := Parse([]byte(testTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if mf.DefaultVLAN != 30 {
		t.Errorf("DefaultVLAN = %d, want 30", mf.DefaultVLAN)
	}
	if len(mf.Clients) != 3 {
		t.Fatalf("len(Clients) = %d, want 3", len(mf.Clients))
	}

	// MACs should be normalized to lowercase.
	if mf.Clients[0].MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("Clients[0].MAC = %q, want lowercase", mf.Clients[0].MAC)
	}
}

func TestLookup(t *testing.T) {
	mf, err := Parse([]byte(testTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	tests := []struct {
		mac  string
		want int
	}{
		{"aa:bb:cc:dd:ee:ff", 10},
		{"AA:BB:CC:DD:EE:FF", 10}, // case-insensitive
		{"11:22:33:44:55:66", 20},
		{"ff:ff:ff:ff:ff:ff", 30}, // unknown -> default
	}
	for _, tt := range tests {
		got := mf.Lookup(tt.mac)
		if got != tt.want {
			t.Errorf("Lookup(%q) = %d, want %d", tt.mac, got, tt.want)
		}
	}
}

func TestVLANs(t *testing.T) {
	mf, err := Parse([]byte(testTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	vlans := mf.VLANs()
	want := []int{10, 20, 30}
	if len(vlans) != len(want) {
		t.Fatalf("VLANs() = %v, want %v", vlans, want)
	}
	for i, v := range vlans {
		if v != want[i] {
			t.Errorf("VLANs()[%d] = %d, want %d", i, v, want[i])
		}
	}
}

func TestClientsByVLAN(t *testing.T) {
	mf, err := Parse([]byte(testTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	byVLAN := mf.ClientsByVLAN()
	if len(byVLAN[10]) != 2 {
		t.Errorf("VLAN 10 has %d clients, want 2", len(byVLAN[10]))
	}
	if len(byVLAN[20]) != 1 {
		t.Errorf("VLAN 20 has %d clients, want 1", len(byVLAN[20]))
	}
}

func TestMerge(t *testing.T) {
	mf, err := Parse([]byte(testTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	other := &MapFile{
		Clients: []Client{
			{MAC: "aa:bb:cc:dd:ee:ff", VLAN: 99, Name: "duplicate"}, // should be skipped
			{MAC: "00:11:22:33:44:55", VLAN: 0, Name: "new client"},
		},
	}

	mf.Merge(other)

	if len(mf.Clients) != 4 {
		t.Fatalf("after merge: len(Clients) = %d, want 4", len(mf.Clients))
	}
	// Original entry should be preserved.
	if mf.Lookup("aa:bb:cc:dd:ee:ff") != 10 {
		t.Errorf("duplicate MAC should keep original VLAN 10, got %d", mf.Lookup("aa:bb:cc:dd:ee:ff"))
	}
	// New entry should be added.
	if mf.Lookup("00:11:22:33:44:55") != 0 {
		t.Errorf("new client should have VLAN 0, got %d", mf.Lookup("00:11:22:33:44:55"))
	}
}

func TestHostapdAcceptMACFile(t *testing.T) {
	mf, err := Parse([]byte(testTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got := mf.HostapdAcceptMACFile()
	// Should have 3 lines (all clients have vlan > 0).
	lines := 0
	for _, line := range splitLines(got) {
		if line != "" {
			lines++
		}
	}
	if lines != 3 {
		t.Errorf("HostapdAcceptMACFile has %d lines, want 3:\n%s", lines, got)
	}
}

func TestDuplicateMAC(t *testing.T) {
	input := `
[[clients]]
mac = "aa:bb:cc:dd:ee:ff"
vlan = 10

[[clients]]
mac = "AA:BB:CC:DD:EE:FF"
vlan = 20
`
	_, err := Parse([]byte(input))
	if err == nil {
		t.Fatal("expected error for duplicate MAC, got nil")
	}
}

func TestInvalidMAC(t *testing.T) {
	input := `
[[clients]]
mac = "not-a-mac"
vlan = 10
`
	_, err := Parse([]byte(input))
	if err == nil {
		t.Fatal("expected error for invalid MAC, got nil")
	}
}

func TestInvalidVLAN(t *testing.T) {
	input := `
[[clients]]
mac = "aa:bb:cc:dd:ee:ff"
vlan = 5000
`
	_, err := Parse([]byte(input))
	if err == nil {
		t.Fatal("expected error for VLAN > 4094, got nil")
	}
}

func TestRoundTrip(t *testing.T) {
	mf, err := Parse([]byte(testTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	data, err := mf.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	mf2, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse roundtrip: %v", err)
	}

	if mf2.DefaultVLAN != mf.DefaultVLAN {
		t.Errorf("roundtrip DefaultVLAN = %d, want %d", mf2.DefaultVLAN, mf.DefaultVLAN)
	}
	if len(mf2.Clients) != len(mf.Clients) {
		t.Errorf("roundtrip len(Clients) = %d, want %d", len(mf2.Clients), len(mf.Clients))
	}
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
