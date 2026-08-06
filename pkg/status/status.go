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
	Name    string     `json:"name"`
	MAC     string     `json:"mac,omitempty"`
	Up      bool       `json:"up"`
	Carrier bool       `json:"carrier"`
	Speed   int        `json:"speed,omitempty"`  // negotiated link speed in Mbps, 0 if unknown
	Duplex  string     `json:"duplex,omitempty"` // "full", "half", or ""
	TxBytes uint64     `json:"txBytes"`
	RxBytes uint64     `json:"rxBytes"`
	TxPkts  uint64     `json:"txPackets"`
	RxPkts  uint64     `json:"rxPackets"`
	Sub     []PortInfo `json:"sub,omitempty"` // sub-ports (e.g. lan1-4 under lan)
}

// ClientInfo describes a connected client with traffic counters.
type ClientInfo struct {
	IP        string `json:"ip"`
	IP6       string `json:"ip6,omitempty"`
	MAC       string `json:"mac"`
	Via       string `json:"via"` // "L" = LAN, "W" = WiFi, "G" = Gateway (router itself)
	Connected bool   `json:"connected"`

	// Live counters: current session only (reset on reconnect).
	TxBytes uint64 `json:"txBytes"` // bytes sent TO client (download)
	RxBytes uint64 `json:"rxBytes"` // bytes sent FROM client (upload)
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
	FirstSeen string `json:"firstSeen"`
	LastSeen  string `json:"lastSeen"`
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
	table6     *nftables.Table
	chainRx    *nftables.Chain        // IPv4 traffic FROM clients (src match)
	chainTx    *nftables.Chain        // IPv4 traffic TO clients (dst match)
	chainRx6   *nftables.Chain        // IPv6 traffic FROM clients (src match)
	chainTx6   *nftables.Chain        // IPv6 traffic TO clients (dst match)
	clients    map[string]clientEntry // IP -> entry
	gatewayIPs map[string]bool        // router's own IPs (to mark as "G")
	wanIface   string
	lanIface   string   // bridge name (br-lan)
	lanPorts   []string // individual LAN ports (lan1-lan4)
	wifiIface  string
	scanIfaces []string // extra interfaces scanned for IPv6 neighbors (e.g. VLAN bridges)

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
	IP6       net.IP
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

	table6 := conn.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv6,
		Name:   "gokrazy_stats6",
	})

	chainRx6 := conn.AddChain(&nftables.Chain{
		Name:     "client_rx6",
		Table:    table6,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
	})

	chainTx6 := conn.AddChain(&nftables.Chain{
		Name:     "client_tx6",
		Table:    table6,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
	})

	if err := conn.Flush(); err != nil {
		return nil, fmt.Errorf("status: create nftables table: %w", err)
	}

	// Collect the router's own IPs (v4 and v6) to identify gateway entries.
	gwIPs := make(map[string]bool)
	for _, ifname := range []string{lanIface, wifiIface, wanIface} {
		link, err := netlink.LinkByName(ifname)
		if err != nil {
			continue
		}
		addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
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
		table6:       table6,
		chainRx:      chainRx,
		chainTx:      chainTx,
		chainRx6:     chainRx6,
		chainTx6:     chainTx6,
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

	ipStr := ip.String()

	// Mark router's own IPs as gateway.
	if m.gatewayIPs[ipStr] {
		via = "G"
	}

	now := time.Now()

	// Attach IPv4 to an existing IPv6-discovered client with the same MAC.
	if mac != "" {
		for key, entry := range m.clients {
			if key == ipStr || !entry.Connected || entry.MAC != mac {
				continue
			}
			entry.IP = ip.To4()
			entry.LastSeen = now
			m.clients[key] = entry
			m.addCounterRulesLocked(ip, false)
			if err := m.conn.Flush(); err != nil {
				return fmt.Errorf("status: add counter rules for %s: %w", ipStr, err)
			}
			return nil
		}
	}

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

	m.addCounterRulesLocked(ip, false)

	if err := m.conn.Flush(); err != nil {
		return fmt.Errorf("status: add counter rules for %s: %w", ipStr, err)
	}

	log.Printf("status: tracking %s (%s) via %s", ipStr, mac, via)
	return nil
}

// addCounterRulesLocked adds rx/tx counter rules for a client address in the
// appropriate address family. Caller must hold m.mu.
func (m *Monitor) addCounterRulesLocked(ip net.IP, isV6 bool) {
	rxChain, txChain := m.chainRx, m.chainTx
	table := m.table
	var offset, dstOffset uint32
	n := uint32(4) // IPv4 src/dst length
	suffix := ""
	data := ip.To4()
	if isV6 {
		rxChain, txChain = m.chainRx6, m.chainTx6
		table = m.table6
		offset, dstOffset, n = 8, 24, 16 // IPv6 src/dst
		suffix = "6"
		data = ip.To16()
	}
	ipStr := ip.String()

	// Count traffic FROM this client (upload): match src IP
	m.conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: rxChain,
		Exprs: []expr.Any{
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseNetworkHeader,
				Offset:       offset,
				Len:          n,
			},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     data,
			},
			&expr.Counter{},
		},
		UserData: []byte(ipStr + "/rx" + suffix),
	})

	// Count traffic TO this client (download): match dst IP
	m.conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: txChain,
		Exprs: []expr.Any{
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseNetworkHeader,
				Offset:       dstOffset,
				Len:          n,
			},
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     data,
			},
			&expr.Counter{},
		},
		UserData: []byte(ipStr + "/tx" + suffix),
	})
}

// DiscoverIPv6Neighbors scans the kernel IPv6 neighbor table on the LAN and
// WiFi interfaces and adds counter rules for any new global addresses. This
// is how SLAAC and DHCPv6 clients are discovered, since clients pick their own
// addresses.
func (m *Monitor) DiscoverIPv6Neighbors() {
	m.mu.Lock()
	defer m.mu.Unlock()

	seen := make(map[string]bool)
	for _, ifname := range append([]string{m.lanIface, m.wifiIface}, m.scanIfaces...) {
		link, err := netlink.LinkByName(ifname)
		if err != nil {
			continue
		}
		neighs, err := netlink.NeighList(link.Attrs().Index, netlink.FAMILY_V6)
		if err != nil {
			log.Printf("status: ipv6 neighbor list on %s: %v", ifname, err)
			continue
		}
		via := "L"
		if ifname == m.wifiIface {
			via = "W"
		}
		for _, n := range neighs {
			if n.IP == nil || n.State&(netlink.NUD_NONE|netlink.NUD_INCOMPLETE|netlink.NUD_FAILED) != 0 {
				continue
			}
			if n.IP.IsLinkLocalUnicast() || n.IP.IsLoopback() {
				continue
			}
			ipStr := n.IP.String()
			seen[ipStr] = true
			m.ensureClient6Locked(n.IP, n.HardwareAddr.String(), via)
		}
	}
	m.pruneStaleIPv6Locked(seen)
}

// ensureClient6Locked registers a discovered IPv6 neighbor and adds counter
// rules for it. Caller must hold m.mu.
func (m *Monitor) ensureClient6Locked(ip net.IP, mac, via string) {
	ipStr := ip.String()
	if m.gatewayIPs[ipStr] {
		via = "G"
	}
	now := time.Now()

	// Attach IPv6 to an existing connected client with the same MAC.
	if mac != "" {
		for key, entry := range m.clients {
			if key == ipStr || !entry.Connected || entry.MAC != mac {
				continue
			}
			entry.IP6 = ip.To16()
			entry.LastSeen = now
			m.clients[key] = entry
			m.addCounterRulesLocked(ip, true)
			return
		}
	}

	if entry, ok := m.clients[ipStr]; ok {
		if entry.Connected {
			entry.LastSeen = now
			m.clients[ipStr] = entry
			return
		}
		entry.Connected = true
		entry.IP6 = ip.To16()
		entry.LastSeen = now
		m.clients[ipStr] = entry
	} else {
		m.clients[ipStr] = clientEntry{
			MAC:       mac,
			IP6:       ip.To16(),
			Via:       via,
			Connected: true,
			FirstSeen: now,
			LastSeen:  now,
		}
	}
	m.addCounterRulesLocked(ip, true)
	log.Printf("status: tracking ipv6 %s (%s) via %s", ipStr, mac, via)
}

// pruneStaleIPv6Locked removes counter rules and entries for IPv6 neighbors
// that have disappeared from the neighbor table. Caller must hold m.mu.
func (m *Monitor) pruneStaleIPv6Locked(seen map[string]bool) {
	const staleTimeout = 3 * time.Minute
	now := time.Now()
	changed := false
	for key, entry := range m.clients {
		if entry.IP6 == nil || !entry.Connected {
			continue
		}
		ip6 := entry.IP6.String()
		if seen[ip6] || now.Sub(entry.LastSeen) < staleTimeout {
			continue
		}
		rxBytes, rxPkts := m.readAndDeleteRules(m.chainRx6, ip6+"/rx6")
		txBytes, txPkts := m.readAndDeleteRules(m.chainTx6, ip6+"/tx6")
		entry.HistRxBytes += rxBytes
		entry.HistRxPkts += rxPkts
		entry.HistTxBytes += txBytes
		entry.HistTxPkts += txPkts
		if entry.IP == nil {
			delete(m.clients, key)
		} else {
			entry.IP6 = nil
			entry.LastSeen = now
			m.clients[key] = entry
		}
		changed = true
		log.Printf("status: ipv6 neighbor %s gone, counters removed", ip6)
	}
	if changed {
		m.conn.Flush()
	}
}

// RemoveClient marks a client as disconnected. The current nftables counter
// values are read and accumulated into the historical totals, then the
// nftables rules are deleted so that a future reconnect starts fresh counters.
func (m *Monitor) RemoveClient(ip net.IP) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ipStr := ip.String()
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

	// If the client still has an active IPv6 session, keep the entry alive so
	// IPv6 counters keep working; only the IPv4 session ends here.
	if entry.IP6 != nil {
		entry.IP = nil
		entry.LastSeen = time.Now()
		m.clients[ipStr] = entry
		if err := m.conn.Flush(); err != nil {
			return fmt.Errorf("status: flush after removing rules for %s: %w", ipStr, err)
		}
		log.Printf("status: client %s (%s) ipv4 disconnected, ipv6 session continues", ipStr, entry.MAC)
		return nil
	}

	entry.Connected = false
	entry.LastSeen = time.Now()
	m.clients[ipStr] = entry
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
	var ip, ip6 net.IP
	for _, entry := range m.clients {
		if entry.MAC == mac && entry.Connected {
			ip = entry.IP
			ip6 = entry.IP6
			break
		}
	}
	m.mu.Unlock()

	if ip == nil && ip6 == nil {
		return nil // unknown MAC
	}
	if ip != nil {
		return m.RemoveClient(ip)
	}

	// IPv6-only client: remove its v6 counters directly.
	m.mu.Lock()
	defer m.mu.Unlock()
	ipStr := ip6.String()
	if entry, ok := m.clients[ipStr]; ok {
		rxBytes, rxPkts := m.readAndDeleteRules(m.chainRx6, ipStr+"/rx6")
		txBytes, txPkts := m.readAndDeleteRules(m.chainTx6, ipStr+"/tx6")
		entry.HistRxBytes += rxBytes
		entry.HistRxPkts += rxPkts
		entry.HistTxBytes += txBytes
		entry.HistTxPkts += txPkts
		delete(m.clients, ipStr)
		m.conn.Flush()
	}
	return nil
}

// readAndDeleteRules reads the counter values from nftables rules matching
// the given userData tag, deletes them, and returns the total bytes and packets.
// Must be called with m.mu held.
func (m *Monitor) readAndDeleteRules(chain *nftables.Chain, tag string) (bytes, packets uint64) {
	table := m.table
	if chain.Table != nil {
		table = chain.Table
	}
	rules, err := m.conn.GetRules(table, chain)
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
			m.DiscoverIPv6Neighbors()
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
	rxRules6, _ := m.conn.GetRules(m.table6, m.chainRx6)
	txRules6, _ := m.conn.GetRules(m.table6, m.chainTx6)

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
	for _, r := range rxRules6 {
		ip := extractIP(r.UserData, "/rx6")
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
	for _, r := range txRules6 {
		ip := extractIP(r.UserData, "/tx6")
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

// AddScanInterfaces registers extra interfaces whose IPv6 neighbor tables
// should be scanned for client addresses (e.g. per-VLAN bridges in VLAN mode).
func (m *Monitor) AddScanInterfaces(names []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range names {
		if n != "" {
			m.scanIfaces = append(m.scanIfaces, n)
		}
	}
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
	rxRules6, _ := m.conn.GetRules(m.table6, m.chainRx6)
	txRules6, _ := m.conn.GetRules(m.table6, m.chainTx6)

	// Build maps: IP -> counter values [bytes, packets]
	rxCounters := make(map[string][2]uint64)
	txCounters := make(map[string][2]uint64)
	forEachCounter(rxRules, "/rx", func(ip string, c *expr.Counter) {
		rxCounters[ip] = [2]uint64{c.Bytes, c.Packets}
	})
	forEachCounter(txRules, "/tx", func(ip string, c *expr.Counter) {
		txCounters[ip] = [2]uint64{c.Bytes, c.Packets}
	})
	forEachCounter(rxRules6, "/rx6", func(ip string, c *expr.Counter) {
		rxCounters[ip] = [2]uint64{c.Bytes, c.Packets}
	})
	forEachCounter(txRules6, "/tx6", func(ip string, c *expr.Counter) {
		txCounters[ip] = [2]uint64{c.Bytes, c.Packets}
	})

	for _, entry := range m.clients {
		ci := ClientInfo{
			MAC:       entry.MAC,
			Via:       entry.Via,
			Connected: entry.Connected,
			FirstSeen: entry.FirstSeen.Format(time.RFC3339),
			LastSeen:  entry.LastSeen.Format(time.RFC3339),
		}
		if entry.IP != nil {
			ci.IP = entry.IP.String()
		}
		if entry.IP6 != nil {
			ci.IP6 = entry.IP6.String()
		}

		// Live counters from nftables (v4 + v6, merged by address).
		var liveRxBytes, liveRxPkts, liveTxBytes, liveTxPkts uint64
		for _, ip := range []string{ci.IP, ci.IP6} {
			if ip == "" {
				continue
			}
			if rx, ok := rxCounters[ip]; ok {
				liveRxBytes += rx[0]
				liveRxPkts += rx[1]
			}
			if tx, ok := txCounters[ip]; ok {
				liveTxBytes += tx[0]
				liveTxPkts += tx[1]
			}
		}

		ci.RxBytes = liveRxBytes
		ci.RxPkts = liveRxPkts
		ci.TxBytes = liveTxBytes
		ci.TxPkts = liveTxPkts

		// Throughput from background sampler.
		for _, ip := range []string{ci.IP, ci.IP6} {
			if rate, ok := m.rates[ip]; ok {
				ci.RxRate += rate.RxRate
				ci.TxRate += rate.TxRate
			}
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

// forEachCounter iterates the Counter expressions in rules whose UserData
// matches the given suffix, calling fn with the extracted IP and counter.
func forEachCounter(rules []*nftables.Rule, suffix string, fn func(ip string, c *expr.Counter)) {
	for _, r := range rules {
		ip := extractIP(r.UserData, suffix)
		if ip == "" {
			continue
		}
		for _, e := range r.Exprs {
			if c, ok := e.(*expr.Counter); ok {
				fn(ip, c)
			}
		}
	}
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
