// Package status provides port link status and per-client traffic counters.
package status

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
)

// PortInfo describes the link state and traffic counters for a network port.
type PortInfo struct {
	Name    string      `json:"name"`
	MAC     string      `json:"mac,omitempty"`
	Up      bool        `json:"up"`
	Carrier bool        `json:"carrier"`
	Speed   int         `json:"speed,omitempty"`  // negotiated link speed in Mbps, 0 if unknown
	Duplex  string      `json:"duplex,omitempty"` // "full", "half", or ""
	TxBytes uint64      `json:"txBytes"`
	RxBytes uint64      `json:"rxBytes"`
	TxPkts  uint64      `json:"txPackets"`
	RxPkts  uint64      `json:"rxPackets"`
	Sub     []PortInfo  `json:"sub,omitempty"` // sub-ports (e.g. lan1-4 under lan)
}

// ClientInfo describes a connected client with traffic counters.
type ClientInfo struct {
	IP        string `json:"ip"`
	MAC       string `json:"mac"`
	Via       string `json:"via"`      // "L" = LAN, "W" = WiFi, "G" = Gateway (router itself)
	Connected bool   `json:"connected"`

	// Live counters: current session only (reset on reconnect).
	TxBytes uint64 `json:"txBytes"`  // bytes sent TO client (download)
	RxBytes uint64 `json:"rxBytes"`  // bytes sent FROM client (upload)
	TxPkts  uint64 `json:"txPackets"`
	RxPkts  uint64 `json:"rxPackets"`

	// Current throughput in bytes/sec (computed by background sampler).
	TxRate float64 `json:"txRate"` // download rate (bytes/sec)
	RxRate float64 `json:"rxRate"` // upload rate (bytes/sec)

	// WiFi link info (only for WiFi clients, from hostapd control socket).
	LinkTxRate int `json:"linkTxRate,omitempty"` // TX link rate in Kbps
	LinkRxRate int `json:"linkRxRate,omitempty"` // RX link rate in Kbps
	Signal     int `json:"signal,omitempty"`     // last RSSI in dBm

	// Historical counters: accumulated across all sessions since boot.
	TotalTxBytes uint64 `json:"totalTxBytes"`
	TotalRxBytes uint64 `json:"totalRxBytes"`
	TotalTxPkts  uint64 `json:"totalTxPackets"`
	TotalRxPkts  uint64 `json:"totalRxPackets"`

	// Timestamps
	FirstSeen    string `json:"firstSeen"`
	LastSeen     string `json:"lastSeen"`
}

// SummaryInfo provides aggregate TX/RX stats for a category.
type SummaryInfo struct {
	Name    string `json:"name"`
	TxBytes uint64 `json:"txBytes"`
	RxBytes uint64 `json:"rxBytes"`
	TxPkts  uint64 `json:"txPackets"`
	RxPkts  uint64 `json:"rxPackets"`
}

// Status is the full status response.
type Status struct {
	Summary []SummaryInfo `json:"summary"`
	Ports   []PortInfo    `json:"ports"`
	Clients []ClientInfo  `json:"clients"`
}

// WiFiStationSource provides per-station WiFi info.
type WiFiStationSource interface {
	// StationInfoAll returns info for all connected WiFi stations.
	StationInfoAll() ([]WiFiStation, error)
}

// WiFiStation holds WiFi station data as returned by a WiFiStationSource.
type WiFiStation struct {
	MAC       string
	Signal    int // dBm
	TxBitrate int // Kbps
	RxBitrate int // Kbps
}

// Monitor tracks per-client nftables counter rules and provides status.
type Monitor struct {
	mu         sync.Mutex
	conn       *nftables.Conn
	table      *nftables.Table
	chainRx    *nftables.Chain // traffic FROM clients (src match)
	chainTx    *nftables.Chain // traffic TO clients (dst match)
	clients    map[string]clientEntry // IP -> entry
	gatewayIPs map[string]bool        // router's own IPs (to mark as "G")
	wanIface   string
	lanIface   string   // bridge name (br-lan)
	lanPorts   []string // individual LAN ports (lan1-lan4)
	wifiIface  string

	// Throughput sampling state.
	prevSnapshot map[string]counterSnapshot // IP -> previous sample
	rates        map[string]clientRate      // IP -> computed rates
	stopCh       chan struct{}

	// WiFi station info (MAC -> info).
	wifiStations map[string]wifiStationInfo
	wifiSource   WiFiStationSource
}

// counterSnapshot stores a point-in-time counter reading for rate calculation.
type counterSnapshot struct {
	RxBytes uint64
	TxBytes uint64
	Time    time.Time
}

// clientRate stores the computed throughput for a client.
type clientRate struct {
	RxRate float64 // upload bytes/sec
	TxRate float64 // download bytes/sec
}

// wifiStationInfo stores per-station data from hostapd control socket.
type wifiStationInfo struct {
	LinkTxRate int // Kbps
	LinkRxRate int // Kbps
	Signal     int // dBm
}

type clientEntry struct {
	MAC       string
	IP        net.IP
	Via       string // "L" or "W"
	Connected bool
	FirstSeen time.Time
	LastSeen  time.Time

	// Historical counters accumulated from previous sessions.
	HistTxBytes uint64
	HistRxBytes uint64
	HistTxPkts  uint64
	HistRxPkts  uint64
}

// New creates a Monitor.
// wanIface is the WAN interface (e.g. "wan").
// lanIface is the LAN bridge (e.g. "br-lan").
// lanPorts are the individual LAN ports (e.g. ["lan1","lan2","lan3","lan4"]).
// wifiIface is the WiFi interface (e.g. "wlan0").
func New(wanIface, lanIface string, lanPorts []string, wifiIface string) (*Monitor, error) {
	conn := &nftables.Conn{}

	table := conn.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   "gokrazy_stats",
	})

	// Forward chain to count traffic passing through (routed traffic).
	chainRx := conn.AddChain(&nftables.Chain{
		Name:     "client_rx",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
	})

	chainTx := conn.AddChain(&nftables.Chain{
		Name:     "client_tx",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
	})

	if err := conn.Flush(); err != nil {
		return nil, fmt.Errorf("status: create nftables table: %w", err)
	}

	// Collect the router's own IPs to identify gateway entries in client list.
	gwIPs := make(map[string]bool)
	for _, ifname := range []string{lanIface, wifiIface, wanIface} {
		link, err := netlink.LinkByName(ifname)
		if err != nil {
			continue
		}
		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			gwIPs[a.IP.String()] = true
		}
	}

	m := &Monitor{
		conn:         conn,
		table:        table,
		chainRx:      chainRx,
		chainTx:      chainTx,
		clients:      make(map[string]clientEntry),
		gatewayIPs:   gwIPs,
		wanIface:     wanIface,
		lanIface:     lanIface,
		lanPorts:     lanPorts,
		wifiIface:    wifiIface,
		prevSnapshot: make(map[string]counterSnapshot),
		rates:        make(map[string]clientRate),
		wifiStations: make(map[string]wifiStationInfo),
		stopCh:       make(chan struct{}),
	}

	go m.sampleLoop()

	return m, nil
}

// AddClient adds nftables counter rules for a new DHCP client.
// via is "L" for LAN or "W" for WiFi.
func (m *Monitor) AddClient(ip net.IP, mac, via string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ipStr := ip.To4().String()

	// Mark router's own IPs as gateway.
	if m.gatewayIPs[ipStr] {
		via = "G"
	}

	now := time.Now()

	// If client already exists and is connected, nothing to do.
	if entry, exists := m.clients[ipStr]; exists {
		if entry.Connected {
			return nil
		}
		// Client is reconnecting: keep historical counters, re-add nftables rules.
		entry.Connected = true
		entry.LastSeen = now
		entry.MAC = mac // MAC may have changed (different device, same IP)
		entry.Via = via
		m.clients[ipStr] = entry
		// Fall through to add fresh nftables counter rules.
	} else {
		m.clients[ipStr] = clientEntry{
			MAC:       mac,
			IP:        ip.To4(),
			Via:       via,
			Connected: true,
			FirstSeen: now,
			LastSeen:  now,
		}
	}

	ip4 := ip.To4()

	// Count traffic FROM this client (upload): match src IP
	m.conn.AddRule(&nftables.Rule{
		Table: m.table,
		Chain: m.chainRx,
		Exprs: []expr.Any{
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseNetworkHeader,
				Offset:       12, // src IP
				Len:          4,
			},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     ip4,
			},
			&expr.Counter{},
		},
		UserData: []byte(ipStr + "/rx"),
	})

	// Count traffic TO this client (download): match dst IP
	m.conn.AddRule(&nftables.Rule{
		Table: m.table,
		Chain: m.chainTx,
		Exprs: []expr.Any{
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseNetworkHeader,
				Offset:       16, // dst IP
				Len:          4,
			},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     ip4,
			},
			&expr.Counter{},
		},
		UserData: []byte(ipStr + "/tx"),
	})

	if err := m.conn.Flush(); err != nil {
		return fmt.Errorf("status: add counter rules for %s: %w", ipStr, err)
	}

	log.Printf("status: tracking %s (%s) via %s", ipStr, mac, via)
	return nil
}

// RemoveClient marks a client as disconnected. The current nftables counter
// values are read and accumulated into the historical totals, then the
// nftables rules are deleted so that a future reconnect starts fresh counters.
func (m *Monitor) RemoveClient(ip net.IP) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ipStr := ip.To4().String()
	entry, exists := m.clients[ipStr]
	if !exists {
		return nil // unknown client, nothing to do
	}
	if !entry.Connected {
		return nil // already disconnected
	}

	// Read current counters before deleting rules.
	rxBytes, rxPkts := m.readAndDeleteRules(m.chainRx, ipStr+"/rx")
	txBytes, txPkts := m.readAndDeleteRules(m.chainTx, ipStr+"/tx")

	// Accumulate into historical totals.
	entry.HistRxBytes += rxBytes
	entry.HistRxPkts += rxPkts
	entry.HistTxBytes += txBytes
	entry.HistTxPkts += txPkts
	entry.Connected = false
	entry.LastSeen = time.Now()
	m.clients[ipStr] = entry

	if err := m.conn.Flush(); err != nil {
		return fmt.Errorf("status: flush after removing rules for %s: %w", ipStr, err)
	}

	log.Printf("status: client %s (%s) disconnected (session: rx=%d tx=%d bytes)",
		ipStr, entry.MAC, rxBytes, txBytes)
	return nil
}

// RemoveClientByMAC marks a client as disconnected by MAC address.
// This is useful for WiFi disconnect events where only the MAC is known.
func (m *Monitor) RemoveClientByMAC(mac string) error {
	m.mu.Lock()
	var ip net.IP
	for _, entry := range m.clients {
		if entry.MAC == mac && entry.Connected {
			ip = entry.IP
			break
		}
	}
	m.mu.Unlock()

	if ip == nil {
		return nil // unknown MAC
	}
	return m.RemoveClient(ip)
}

// readAndDeleteRules reads the counter values from nftables rules matching
// the given userData tag, deletes them, and returns the total bytes and packets.
// Must be called with m.mu held.
func (m *Monitor) readAndDeleteRules(chain *nftables.Chain, tag string) (bytes, packets uint64) {
	rules, err := m.conn.GetRules(m.table, chain)
	if err != nil {
		log.Printf("status: failed to get rules for chain %s: %v", chain.Name, err)
		return 0, 0
	}
	for _, r := range rules {
		if string(r.UserData) != tag {
			continue
		}
		for _, e := range r.Exprs {
			if c, ok := e.(*expr.Counter); ok {
				bytes += c.Bytes
				packets += c.Packets
			}
		}
		if err := m.conn.DelRule(r); err != nil {
			log.Printf("status: failed to delete rule %s: %v", tag, err)
		}
	}
	return bytes, packets
}

const sampleInterval = 5 * time.Second

// sampleLoop periodically reads nftables counters and computes per-client throughput.
func (m *Monitor) sampleLoop() {
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.sample()
			m.pollWiFiStations()
		case <-m.stopCh:
			return
		}
	}
}

// sample reads the current nftables counters and computes rates from the delta
// since the last sample.
func (m *Monitor) sample() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()

	rxRules, _ := m.conn.GetRules(m.table, m.chainRx)
	txRules, _ := m.conn.GetRules(m.table, m.chainTx)

	// Build current snapshot.
	current := make(map[string]counterSnapshot)
	for _, r := range rxRules {
		ip := extractIP(r.UserData, "/rx")
		if ip == "" {
			continue
		}
		for _, e := range r.Exprs {
			if c, ok := e.(*expr.Counter); ok {
				snap := current[ip]
				snap.RxBytes = c.Bytes
				snap.Time = now
				current[ip] = snap
			}
		}
	}
	for _, r := range txRules {
		ip := extractIP(r.UserData, "/tx")
		if ip == "" {
			continue
		}
		for _, e := range r.Exprs {
			if c, ok := e.(*expr.Counter); ok {
				snap := current[ip]
				snap.TxBytes = c.Bytes
				snap.Time = now
				current[ip] = snap
			}
		}
	}

	// Compute rates by comparing to previous snapshot.
	newRates := make(map[string]clientRate)
	for ip, cur := range current {
		prev, ok := m.prevSnapshot[ip]
		if !ok {
			// First sample for this client — no rate yet.
			continue
		}
		dt := cur.Time.Sub(prev.Time).Seconds()
		if dt <= 0 {
			continue
		}
		var rxDelta, txDelta uint64
		if cur.RxBytes >= prev.RxBytes {
			rxDelta = cur.RxBytes - prev.RxBytes
		}
		if cur.TxBytes >= prev.TxBytes {
			txDelta = cur.TxBytes - prev.TxBytes
		}
		newRates[ip] = clientRate{
			RxRate: float64(rxDelta) / dt,
			TxRate: float64(txDelta) / dt,
		}
	}

	m.prevSnapshot = current
	m.rates = newRates
}

// pollWiFiStations queries the configured WiFiStationSource for per-station info.
func (m *Monitor) pollWiFiStations() {
	m.mu.Lock()
	src := m.wifiSource
	m.mu.Unlock()

	if src == nil {
		return
	}

	stations, err := src.StationInfoAll()
	if err != nil {
		log.Printf("status: wifi station poll: %v", err)
		return
	}
	if stations == nil {
		return
	}

	info := make(map[string]wifiStationInfo, len(stations))
	for _, sta := range stations {
		info[sta.MAC] = wifiStationInfo{
			LinkTxRate: sta.TxBitrate,
			LinkRxRate: sta.RxBitrate,
			Signal:     sta.Signal,
		}
	}

	m.mu.Lock()
	m.wifiStations = info
	m.mu.Unlock()
}

// SetWiFiSource sets the source for WiFi station info polling.
// Must be called before the sample loop needs it (ideally right after New).
func (m *Monitor) SetWiFiSource(src WiFiStationSource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wifiSource = src
}

// Stop shuts down the background sampler.
func (m *Monitor) Stop() {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
}

// getPortInfo returns port info for a single interface.
func getPortInfo(name string) PortInfo {
	pi := PortInfo{Name: name}
	link, err := netlink.LinkByName(name)
	if err != nil {
		return pi
	}
	attrs := link.Attrs()
	if hw := attrs.HardwareAddr; len(hw) > 0 {
		pi.MAC = hw.String()
	}
	pi.Up = attrs.Flags&net.FlagUp != 0
	pi.Carrier = attrs.OperState == netlink.OperUp
	if stats := attrs.Statistics; stats != nil {
		pi.TxBytes = stats.TxBytes
		pi.RxBytes = stats.RxBytes
		pi.TxPkts = stats.TxPackets
		pi.RxPkts = stats.RxPackets
	}

	// Read negotiated link speed and duplex from sysfs.
	pi.Speed = readSysfsInt(fmt.Sprintf("/sys/class/net/%s/speed", name))
	pi.Duplex = readSysfsString(fmt.Sprintf("/sys/class/net/%s/duplex", name))

	return pi
}

// readSysfsInt reads a single integer from a sysfs file. Returns 0 on error.
func readSysfsInt(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || v < 0 {
		return 0 // kernel returns -1 for unknown speed
	}
	return v
}

// readSysfsString reads a trimmed string from a sysfs file. Returns "" on error.
func readSysfsString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if s == "unknown" {
		return ""
	}
	return s
}

// GetStatus returns the current port and client status.
func (m *Monitor) GetStatus() (*Status, error) {
	s := &Status{}

	// WAN port
	wanPort := getPortInfo(m.wanIface)
	s.Ports = append(s.Ports, wanPort)

	// LAN: bridge with sub-ports
	lanPort := getPortInfo(m.lanIface)
	lanPort.Name = "lan"
	for _, name := range m.lanPorts {
		lanPort.Sub = append(lanPort.Sub, getPortInfo(name))
	}
	s.Ports = append(s.Ports, lanPort)

	// WiFi port
	wifiPort := getPortInfo(m.wifiIface)
	wifiPort.Name = "wifi"
	s.Ports = append(s.Ports, wifiPort)

	// Summary: aggregate TX/RX per category
	s.Summary = []SummaryInfo{
		{Name: "wan", TxBytes: wanPort.TxBytes, RxBytes: wanPort.RxBytes, TxPkts: wanPort.TxPkts, RxPkts: wanPort.RxPkts},
		{Name: "lan", TxBytes: lanPort.TxBytes, RxBytes: lanPort.RxBytes, TxPkts: lanPort.TxPkts, RxPkts: lanPort.RxPkts},
		{Name: "wifi", TxBytes: wifiPort.TxBytes, RxBytes: wifiPort.RxBytes, TxPkts: wifiPort.TxPkts, RxPkts: wifiPort.RxPkts},
	}

	// Collect client counters from nftables.
	m.mu.Lock()
	defer m.mu.Unlock()

	rxRules, _ := m.conn.GetRules(m.table, m.chainRx)
	txRules, _ := m.conn.GetRules(m.table, m.chainTx)

	// Build maps: IP -> counter values
	rxCounters := make(map[string][2]uint64) // [bytes, packets]
	txCounters := make(map[string][2]uint64)

	for _, r := range rxRules {
		ip := extractIP(r.UserData, "/rx")
		if ip == "" {
			continue
		}
		for _, e := range r.Exprs {
			if c, ok := e.(*expr.Counter); ok {
				rxCounters[ip] = [2]uint64{c.Bytes, c.Packets}
			}
		}
	}
	for _, r := range txRules {
		ip := extractIP(r.UserData, "/tx")
		if ip == "" {
			continue
		}
		for _, e := range r.Exprs {
			if c, ok := e.(*expr.Counter); ok {
				txCounters[ip] = [2]uint64{c.Bytes, c.Packets}
			}
		}
	}

	for ipStr, entry := range m.clients {
		ci := ClientInfo{
			IP:        ipStr,
			MAC:       entry.MAC,
			Via:       entry.Via,
			Connected: entry.Connected,
			FirstSeen: entry.FirstSeen.Format(time.RFC3339),
			LastSeen:  entry.LastSeen.Format(time.RFC3339),
		}

		// Live counters from nftables (only present if connected).
		var liveRxBytes, liveRxPkts, liveTxBytes, liveTxPkts uint64
		if rx, ok := rxCounters[ipStr]; ok {
			liveRxBytes = rx[0]
			liveRxPkts = rx[1]
		}
		if tx, ok := txCounters[ipStr]; ok {
			liveTxBytes = tx[0]
			liveTxPkts = tx[1]
		}

		ci.RxBytes = liveRxBytes
		ci.RxPkts = liveRxPkts
		ci.TxBytes = liveTxBytes
		ci.TxPkts = liveTxPkts

		// Throughput from background sampler.
		if rate, ok := m.rates[ipStr]; ok {
			ci.RxRate = rate.RxRate
			ci.TxRate = rate.TxRate
		}

		// WiFi link info from hostapd control socket (matched by MAC).
		if entry.Via == "W" {
			if wsi, ok := m.wifiStations[entry.MAC]; ok {
				ci.LinkTxRate = wsi.LinkTxRate
				ci.LinkRxRate = wsi.LinkRxRate
				ci.Signal = wsi.Signal
			}
		}

		// Historical = accumulated from past sessions + current live session.
		ci.TotalRxBytes = entry.HistRxBytes + liveRxBytes
		ci.TotalRxPkts = entry.HistRxPkts + liveRxPkts
		ci.TotalTxBytes = entry.HistTxBytes + liveTxBytes
		ci.TotalTxPkts = entry.HistTxPkts + liveTxPkts

		s.Clients = append(s.Clients, ci)
	}

	return s, nil
}

func extractIP(userData []byte, suffix string) string {
	s := string(userData)
	if len(s) <= len(suffix) {
		return ""
	}
	if s[len(s)-len(suffix):] != suffix {
		return ""
	}
	return s[:len(s)-len(suffix)]
}

// ServeHTTP handles GET /status requests.
func (m *Monitor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s, err := m.GetStatus()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s)
}
