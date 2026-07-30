package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// ErrNotModified is returned when a change operation would result in no modification.
var ErrNotModified = fmt.Errorf("not modified")

type Config struct {
	WAN      WANConfig      `json:"wan"`
	LAN      LANConfig      `json:"lan"`
	VLANs    []VLANConfig   `json:"vlans,omitempty"`
	NAT      NATConfig      `json:"nat"`
	WiFi     WiFiConfig     `json:"wifi,omitempty"`
	Services ServicesConfig `json:"services,omitempty"`
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
	Enabled       bool              `json:"enabled"`
	RangeStart    string            `json:"rangeStart"`
	RangeEnd      string            `json:"rangeEnd"`
	LeaseDuration string            `json:"leaseDuration"`
	DNS           []string          `json:"dns"`
	Reservations  map[string]string `json:"reservations,omitempty"`  // MAC (lower case) -> IP
	PXEBootServer string            `json:"pxeBootServer,omitempty"` // DHCP option 66
	PXEBootFile   string            `json:"pxeBootFile,omitempty"`   // DHCP option 67
}

type VLANConfig struct {
	ID       int        `json:"id"`
	Name     string     `json:"name"`
	Ports    []string   `json:"ports"`
	Address  string     `json:"address"`
	DHCP     DHCPConfig `json:"dhcp"`
	NAT      bool       `json:"nat"`
	Isolated bool       `json:"isolated,omitempty"` // if true, block inter-VLAN traffic (internet-only)
}

type NATConfig struct {
	Enabled      bool   `json:"enabled"`
	OutInterface string `json:"outInterface"`
}

type WiFiConfig struct {
	Enabled     bool       `json:"enabled"`
	Interface   string     `json:"interface"`
	Bridge      string     `json:"bridge"`      // if empty, wlan0 gets its own subnet
	Address     string     `json:"address"`     // CIDR for wlan0 when not bridged, e.g. "10.0.1.1/24"
	DHCP        DHCPConfig `json:"dhcp"`        // DHCP pool for WiFi clients (when not bridged)
	MacMapFile  string     `json:"macMapFile"`  // path to MAC-to-VLAN TOML mapping file
	DefaultVLAN int        `json:"defaultVlan"` // VLAN for WiFi clients not in the map (0 = use macmap default)
	SSID        string     `json:"ssid"`
	Passphrase  string     `json:"passphrase"`
	Channel     int        `json:"channel"`
	HWMode      string     `json:"hwMode"`  // "g" for 2.4GHz, "a" for 5GHz
	HTCapab     string     `json:"htCapab"` // e.g. "[HT40+][SHORT-GI-20]"
	CountryCode string     `json:"countryCode"`
	WPA         int        `json:"wpa"` // 2 for WPA2
}

// ServicesConfig groups optional add-on services that are not part of the core router.
type ServicesConfig struct {
	Mount      MountConfig `json:"mount,omitempty"`
	SMB        SMBConfig   `json:"smb,omitempty"`
	PXE        PXEConfig   `json:"pxe,omitempty"`
	ExtrasFile string      `json:"extrasFile,omitempty"` // path to TOML extras file (dynamic reservations, images, ...)
}

// MountConfig describes a block device to mount before optional services start.
type MountConfig struct {
	Enabled bool   `json:"enabled"`
	Device  string `json:"device"`  // e.g. /dev/sda1 or ${DISK_DEVICE}
	FsType  string `json:"fsType"`  // e.g. ext4
	Target  string `json:"target"`  // e.g. /mnt/data or ${DISK_TARGET}
	Options string `json:"options"` // comma separated mount options, e.g. ${DISK_OPTS}
}

// SMBConfig starts an external smbd process to share the mounted volume.
// Credentials and paths support ${ENV} expansion.
type SMBConfig struct {
	Enabled           bool   `json:"enabled"`
	BinPath           string `json:"binPath,omitempty"`           // default: /usr/local/bin/smbd
	Listen            string `json:"listen,omitempty"`            // default: 0.0.0.0:445
	ShareName         string `json:"shareName,omitempty"`         // default: data
	SharePath         string `json:"sharePath,omitempty"`         // default: mount target
	User              string `json:"user,omitempty"`              // e.g. ${SMB_USER}
	Password          string `json:"password,omitempty"`          // e.g. ${SMB_PASSWORD}
	UsePortableServer bool   `json:"usePortableServer,omitempty"` // use fiddyschmitt/portable-smb-server instead of Samba smbd
}

// PXEConfig starts a TFTP server and answers PXE boot requests.
type PXEConfig struct {
	Enabled       bool              `json:"enabled"`
	Listen        string            `json:"listen,omitempty"`        // default: 0.0.0.0:69
	BindInterface string            `json:"bindInterface,omitempty"` // SO_BINDTODEVICE target interface (e.g., br-vlan31)
	TFTPRoot      string            `json:"tftpRoot,omitempty"`      // directory containing boot images
	DefaultImage  string            `json:"defaultImage,omitempty"`  // filename used when MAC is unknown
	MacImages     map[string]string `json:"macImages,omitempty"`     // MAC (lower case) -> filename
	BootFile      string            `json:"bootFile,omitempty"`      // legacy option 67 value
}

// ExtrasConfig can be placed on the mounted volume and edited at runtime.
// It is loaded after the JSON config and merged into the active configuration.
type ExtrasConfig struct {
	Reservations  map[string]string `toml:"reservations"`            // MAC -> IP
	MacImages     map[string]string `toml:"macImages"`               // MAC -> PXE image filename
	DefaultImage  string            `toml:"defaultImage"`            // PXE default image (overrides router.json)
	PXEBootFile   string            `toml:"pxeBootFile"`             // DHCP option 67 bootfile (applied to all subnets)
	SMBUsers      []SMBUser         `toml:"smbUsers"`                // additional SMB users
	VLANAddresses map[int]string    `toml:"vlanAddresses,omitempty"` // VLAN ID -> CIDR address override
}

// SMBUser describes a user for the SMB share (only valid inside ExtrasConfig).
type SMBUser struct {
	Name     string `toml:"name"`
	Password string `toml:"password"`
}

// Encode marshals the extras config to TOML bytes.
func (e *ExtrasConfig) Encode() ([]byte, error) {
	return toml.Marshal(e)
}

// Save writes the extras config to a TOML file.
func (e *ExtrasConfig) Save(path string) error {
	data, err := e.Encode()
	if err != nil {
		return fmt.Errorf("extras: marshal: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// SetReservation adds or updates a MAC-to-IP reservation.
// Returns ErrNotModified if the reservation already exists with the same IP.
func (e *ExtrasConfig) SetReservation(mac, ip string) error {
	mac = normalizeMAC(mac)
	if e.Reservations == nil {
		e.Reservations = make(map[string]string)
	}
	if e.Reservations[mac] == ip {
		return ErrNotModified
	}
	e.Reservations[mac] = ip
	return nil
}

// RemoveReservation deletes a MAC reservation. Returns ErrNotModified if not found.
func (e *ExtrasConfig) RemoveReservation(mac string) error {
	mac = normalizeMAC(mac)
	if e.Reservations == nil {
		return ErrNotModified
	}
	if _, ok := e.Reservations[mac]; !ok {
		return ErrNotModified
	}
	delete(e.Reservations, mac)
	return nil
}

// SetMacImage adds or updates a MAC-to-PXE-image mapping.
// Returns ErrNotModified if the mapping already exists with the same image.
func (e *ExtrasConfig) SetMacImage(mac, image string) error {
	mac = normalizeMAC(mac)
	if e.MacImages == nil {
		e.MacImages = make(map[string]string)
	}
	if e.MacImages[mac] == image {
		return ErrNotModified
	}
	e.MacImages[mac] = image
	return nil
}

// RemoveMacImage deletes a MAC image mapping. Returns ErrNotModified if not found.
func (e *ExtrasConfig) RemoveMacImage(mac string) error {
	mac = normalizeMAC(mac)
	if e.MacImages == nil {
		return ErrNotModified
	}
	if _, ok := e.MacImages[mac]; !ok {
		return ErrNotModified
	}
	delete(e.MacImages, mac)
	return nil
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

// LoadExtras reads optional TOML overrides from a file on the mounted volume.
func LoadExtras(path string) (*ExtrasConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var extras ExtrasConfig
	if err := toml.NewDecoder(f).Decode(&extras); err != nil {
		return nil, fmt.Errorf("decode extras TOML: %w", err)
	}
	return &extras, nil
}

// ExpandEnv replaces ${VAR} and $VAR in strings inside the config.
// Call after loading and after any disk mount so that paths are known.
func (c *Config) ExpandEnv() {
	e := os.ExpandEnv
	c.Services.Mount.Device = e(c.Services.Mount.Device)
	c.Services.Mount.Target = e(c.Services.Mount.Target)
	c.Services.Mount.FsType = e(c.Services.Mount.FsType)
	c.Services.Mount.Options = e(c.Services.Mount.Options)

	c.Services.SMB.BinPath = e(c.Services.SMB.BinPath)
	c.Services.SMB.Listen = e(c.Services.SMB.Listen)
	c.Services.SMB.ShareName = e(c.Services.SMB.ShareName)
	c.Services.SMB.SharePath = e(c.Services.SMB.SharePath)
	c.Services.SMB.User = e(c.Services.SMB.User)
	c.Services.SMB.Password = e(c.Services.SMB.Password)

	c.Services.PXE.Listen = e(c.Services.PXE.Listen)
	c.Services.PXE.TFTPRoot = e(c.Services.PXE.TFTPRoot)
	c.Services.PXE.DefaultImage = e(c.Services.PXE.DefaultImage)
	c.Services.PXE.BootFile = e(c.Services.PXE.BootFile)
	for k, v := range c.Services.PXE.MacImages {
		c.Services.PXE.MacImages[k] = e(v)
	}

	c.Services.ExtrasFile = e(c.Services.ExtrasFile)
}

// ApplyExtras merges ExtrasConfig into the loaded config.
func (c *Config) ApplyExtras(extras *ExtrasConfig) {
	if extras == nil {
		return
	}
	for mac, ip := range extras.Reservations {
		parsedIP := net.ParseIP(ip)
		if parsedIP == nil {
			continue
		}
		parsedIP = parsedIP.To4()
		if parsedIP == nil {
			continue
		}
		if c.LAN.Address != "" {
			if _, subnet, err := net.ParseCIDR(c.LAN.Address); err == nil && subnet.Contains(parsedIP) {
				if c.LAN.DHCP.Reservations == nil {
					c.LAN.DHCP.Reservations = make(map[string]string)
				}
				c.LAN.DHCP.Reservations[normalizeMAC(mac)] = ip
			}
		}
		for i := range c.VLANs {
			if _, subnet, err := net.ParseCIDR(c.VLANs[i].Address); err == nil && subnet.Contains(parsedIP) {
				if c.VLANs[i].DHCP.Reservations == nil {
					c.VLANs[i].DHCP.Reservations = make(map[string]string)
				}
				c.VLANs[i].DHCP.Reservations[normalizeMAC(mac)] = ip
			}
		}
	}
	for mac, img := range extras.MacImages {
		if c.Services.PXE.MacImages == nil {
			c.Services.PXE.MacImages = make(map[string]string)
		}
		c.Services.PXE.MacImages[normalizeMAC(mac)] = img
	}
	if extras.DefaultImage != "" {
		c.Services.PXE.DefaultImage = extras.DefaultImage
	}
	if extras.PXEBootFile != "" {
		for i := range c.VLANs {
			c.VLANs[i].DHCP.PXEBootFile = extras.PXEBootFile
		}
		c.WiFi.DHCP.PXEBootFile = extras.PXEBootFile
		c.LAN.DHCP.PXEBootFile = extras.PXEBootFile
	}
	for id, addr := range extras.VLANAddresses {
		for i := range c.VLANs {
			if c.VLANs[i].ID == id {
				c.VLANs[i].Address = addr
				break
			}
		}
	}
}

// ExtrasFromConfig populates an ExtrasConfig from the current Config.
// Used to initialise the extras TOML file when none exists.
func ExtrasFromConfig(c *Config) *ExtrasConfig {
	e := &ExtrasConfig{}
	e.Reservations = make(map[string]string)
	for _, v := range c.VLANs {
		for mac, ip := range v.DHCP.Reservations {
			e.Reservations[mac] = ip
		}
	}
	for mac, ip := range c.LAN.DHCP.Reservations {
		e.Reservations[mac] = ip
	}
	for mac, ip := range c.WiFi.DHCP.Reservations {
		e.Reservations[mac] = ip
	}

	e.MacImages = make(map[string]string)
	for mac, img := range c.Services.PXE.MacImages {
		e.MacImages[mac] = img
	}
	e.DefaultImage = c.Services.PXE.DefaultImage
	for _, v := range c.VLANs {
		if v.DHCP.PXEBootFile != "" {
			e.PXEBootFile = v.DHCP.PXEBootFile
			break
		}
	}
	e.VLANAddresses = make(map[int]string)
	for _, v := range c.VLANs {
		e.VLANAddresses[v.ID] = v.Address
	}
	return e
}

func normalizeMAC(mac string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(mac, "-", ":")))
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
