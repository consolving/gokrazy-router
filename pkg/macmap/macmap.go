// Package macmap handles MAC-to-VLAN mapping via TOML configuration files.
//
// The mapping file assigns network clients (identified by MAC address) to
// VLANs. It is used by the router daemon to configure hostapd dynamic VLAN
// assignment and by the status CLI to export/merge client lists.
//
// File format (TOML):
//
//	default_vlan = 30
//
//	[[clients]]
//	mac = "aa:bb:cc:dd:ee:ff"
//	vlan = 10
//	name = "Philipp's laptop"
//
//	[[clients]]
//	mac = "11:22:33:44:55:66"
//	vlan = 20
//	name = "thermostat"
//	hostname = "tado-bridge"
package macmap

import (
	"fmt"
	"net"
	"os"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// MapFile represents the top-level structure of a MAC-to-VLAN mapping file.
type MapFile struct {
	DefaultVLAN int      `toml:"default_vlan"`
	Clients     []Client `toml:"clients"`
}

// Client represents a single client entry in the mapping file.
type Client struct {
	MAC      string `toml:"mac"`
	VLAN     int    `toml:"vlan"`
	Name     string `toml:"name,omitempty"`
	Hostname string `toml:"hostname,omitempty"`
}

// Load reads and parses a MAC-to-VLAN mapping file.
func Load(path string) (*MapFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("macmap: %w", err)
	}
	return Parse(data)
}

// Parse parses TOML data into a MapFile.
func Parse(data []byte) (*MapFile, error) {
	var mf MapFile
	if err := toml.Unmarshal(data, &mf); err != nil {
		return nil, fmt.Errorf("macmap: %w", err)
	}

	// Normalize MAC addresses to lowercase.
	for i := range mf.Clients {
		mac, err := normalizeMAC(mf.Clients[i].MAC)
		if err != nil {
			return nil, fmt.Errorf("macmap: client %d: %w", i, err)
		}
		mf.Clients[i].MAC = mac
	}

	if err := mf.validate(); err != nil {
		return nil, err
	}
	return &mf, nil
}

// Save writes the mapping file to disk in TOML format.
func (mf *MapFile) Save(path string) error {
	data, err := toml.Marshal(mf)
	if err != nil {
		return fmt.Errorf("macmap: marshal: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// Encode returns the TOML representation as bytes.
func (mf *MapFile) Encode() ([]byte, error) {
	return toml.Marshal(mf)
}

// Lookup returns the VLAN ID for a given MAC address.
// If the MAC is not found, it returns DefaultVLAN.
func (mf *MapFile) Lookup(mac string) int {
	mac = strings.ToLower(mac)
	for _, c := range mf.Clients {
		if c.MAC == mac {
			return c.VLAN
		}
	}
	return mf.DefaultVLAN
}

// VLANs returns a sorted, deduplicated list of all VLAN IDs referenced
// in the mapping (including DefaultVLAN).
func (mf *MapFile) VLANs() []int {
	seen := map[int]bool{mf.DefaultVLAN: true}
	for _, c := range mf.Clients {
		seen[c.VLAN] = true
	}
	// Sort for deterministic output.
	ids := make([]int, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sortInts(ids)
	return ids
}

// ClientsByVLAN returns a map of VLAN ID to the clients assigned to it.
func (mf *MapFile) ClientsByVLAN() map[int][]Client {
	result := make(map[int][]Client)
	for _, c := range mf.Clients {
		result[c.VLAN] = append(result[c.VLAN], c)
	}
	return result
}

// Merge adds clients from other that are not already present (by MAC).
// Existing entries are preserved. New entries get vlan=0 (unassigned)
// unless they already have a VLAN in other.
func (mf *MapFile) Merge(other *MapFile) {
	existing := make(map[string]bool, len(mf.Clients))
	for _, c := range mf.Clients {
		existing[c.MAC] = true
	}
	for _, c := range other.Clients {
		mac := strings.ToLower(c.MAC)
		if !existing[mac] {
			mf.Clients = append(mf.Clients, c)
			existing[mac] = true
		}
	}
}

// HostapdAcceptMACFile generates the content of a hostapd accept_mac_file.
// Format: one line per client: "MAC VLAN_ID"
func (mf *MapFile) HostapdAcceptMACFile() string {
	var b strings.Builder
	for _, c := range mf.Clients {
		if c.VLAN > 0 {
			fmt.Fprintf(&b, "%s %d\n", c.MAC, c.VLAN)
		}
	}
	return b.String()
}

func (mf *MapFile) validate() error {
	macs := make(map[string]bool, len(mf.Clients))
	for i, c := range mf.Clients {
		if macs[c.MAC] {
			return fmt.Errorf("macmap: duplicate MAC %s at client %d", c.MAC, i)
		}
		macs[c.MAC] = true

		if c.VLAN < 0 || c.VLAN > 4094 {
			return fmt.Errorf("macmap: client %d (%s): VLAN ID %d out of range (0-4094)", i, c.MAC, c.VLAN)
		}
	}

	if mf.DefaultVLAN < 0 || mf.DefaultVLAN > 4094 {
		return fmt.Errorf("macmap: default_vlan %d out of range (0-4094)", mf.DefaultVLAN)
	}
	return nil
}

func normalizeMAC(s string) (string, error) {
	hw, err := net.ParseMAC(s)
	if err != nil {
		return "", fmt.Errorf("invalid MAC %q: %w", s, err)
	}
	return strings.ToLower(hw.String()), nil
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}
