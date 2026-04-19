package wifi

import (
	"testing"
	"time"
)

func TestParseStationResponse(t *testing.T) {
	resp := `aa:bb:cc:dd:ee:ff
flags=[AUTH][ASSOC][AUTHORIZED]
aid=1
signal=-42
connected_time=3600
tx_bitrate=72.2 MBit/s MCS 7 40MHz
rx_bitrate=65.0 MBit/s MCS 6 40MHz`

	si := parseStationResponse(resp)

	if si.MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("MAC = %q, want aa:bb:cc:dd:ee:ff", si.MAC)
	}
	if si.Signal != -42 {
		t.Errorf("Signal = %d, want -42", si.Signal)
	}
	if si.Connected != 3600*time.Second {
		t.Errorf("Connected = %v, want 3600s", si.Connected)
	}
	// 72.2 MBit/s -> 72200 Kbps
	if si.TxBitrate != 72200 {
		t.Errorf("TxBitrate = %d, want 72200", si.TxBitrate)
	}
	if si.RxBitrate != 65000 {
		t.Errorf("RxBitrate = %d, want 65000", si.RxBitrate)
	}
}

func TestParseBitrate(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"72.2 MBit/s MCS 7 40MHz", 72200},
		{"6.0 MBit/s", 6000},
		{"54", 5400},   // plain integer, 100 Kbps units
		{"", 0},
	}
	for _, tt := range tests {
		got := parseBitrate(tt.in)
		if got != tt.want {
			t.Errorf("parseBitrate(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestParseStationResponseEmpty(t *testing.T) {
	si := parseStationResponse("")
	if si.MAC != "" {
		t.Errorf("expected empty MAC for empty response, got %q", si.MAC)
	}
}
