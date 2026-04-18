package config

import (
	"encoding/json"
	"os"
	"time"
)

type Config struct {
	WAN   WANConfig    `json:"wan"`
	LAN   LANConfig    `json:"lan"`
	VLANs []VLANConfig `json:"vlans,omitempty"`
	NAT   NATConfig    `json:"nat"`
	WiFi  WiFiConfig   `json:"wifi,omitempty"`
}

type WANConfig struct {
	Interface string `json:"interface"`
	Mode      string `json:"mode"` // "dhcp" or "static"
}

type LANConfig struct {
	Bridge     string     `json:"bridge"`
	Interfaces []string   `json:"interfaces"`
	Address    string     `json:"address"`
	DHCP       DHCPConfig `json:"dhcp"`
}

type DHCPConfig struct {
	Enabled       bool     `json:"enabled"`
	RangeStart    string   `json:"rangeStart"`
	RangeEnd      string   `json:"rangeEnd"`
	LeaseDuration string   `json:"leaseDuration"`
	DNS           []string `json:"dns"`
}

type VLANConfig struct {
	ID      int        `json:"id"`
	Name    string     `json:"name"`
	Ports   []string   `json:"ports"`
	Address string     `json:"address"`
	DHCP    DHCPConfig `json:"dhcp"`
	NAT     bool       `json:"nat"`
}

type NATConfig struct {
	Enabled      bool   `json:"enabled"`
	OutInterface string `json:"outInterface"`
}

type WiFiConfig struct {
	Enabled     bool   `json:"enabled"`
	Interface   string `json:"interface"`
	Bridge      string `json:"bridge"`
	HostapdBin  string `json:"hostapdBin"`
	SSID        string `json:"ssid"`
	Passphrase  string `json:"passphrase"`
	Channel     int    `json:"channel"`
	HWMode      string `json:"hwMode"`      // "g" for 2.4GHz, "a" for 5GHz
	HTCapab     string `json:"htCapab"`      // e.g. "[HT40+][SHORT-GI-20]"
	CountryCode string `json:"countryCode"`
	WPA         int    `json:"wpa"`          // 2 for WPA2
}

func (d DHCPConfig) ParseLeaseDuration() time.Duration {
	dur, err := time.ParseDuration(d.LeaseDuration)
	if err != nil {
		return 12 * time.Hour
	}
	return dur
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg Config
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func Default() *Config {
	return &Config{
		WAN: WANConfig{Interface: "wan", Mode: "dhcp"},
		LAN: LANConfig{
			Bridge:     "br-lan",
			Interfaces: []string{"lan1", "lan2", "lan3", "lan4"},
			Address:    "10.0.0.1/24",
			DHCP: DHCPConfig{
				Enabled:       true,
				RangeStart:    "10.0.0.100",
				RangeEnd:      "10.0.0.250",
				LeaseDuration: "12h",
				DNS:           []string{"1.1.1.1", "8.8.8.8"},
			},
		},
		NAT: NATConfig{Enabled: true, OutInterface: "wan"},
	}
}
