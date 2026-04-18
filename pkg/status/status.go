// Package status provides port link status and per-client traffic counters.
package status

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
)

// PortInfo describes the link state and traffic counters for a network port.
type PortInfo struct {
	Name    string      `json:"name"`
	Up      bool        `json:"up"`
	Carrier bool        `json:"carrier"`
	TxBytes uint64      `json:"txBytes"`
	RxBytes uint64      `json:"rxBytes"`
	TxPkts  uint64      `json:"txPackets"`
	RxPkts  uint64      `json:"rxPackets"`
	Sub     []PortInfo  `json:"sub,omitempty"` // sub-ports (e.g. lan1-4 under lan)
}

// ClientInfo describes a connected client with traffic counters.
type ClientInfo struct {
	IP      string `json:"ip"`
	MAC     string `json:"mac"`
	Via     string `json:"via"`      // "L" = LAN, "W" = WiFi
	TxBytes uint64 `json:"txBytes"`  // bytes sent TO client (download)
	RxBytes uint64 `json:"rxBytes"`  // bytes sent FROM client (upload)
	TxPkts  uint64 `json:"txPackets"`
	RxPkts  uint64 `json:"rxPackets"`
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

// Monitor tracks per-client nftables counter rules and provides status.
type Monitor struct {
	mu        sync.Mutex
	conn      *nftables.Conn
	table     *nftables.Table
	chainRx   *nftables.Chain // traffic FROM clients (src match)
	chainTx   *nftables.Chain // traffic TO clients (dst match)
	clients   map[string]clientEntry // IP -> entry
	wanIface  string
	lanIface  string   // bridge name (br-lan)
	lanPorts  []string // individual LAN ports (lan1-lan4)
	wifiIface string
}

type clientEntry struct {
	MAC  string
	IP   net.IP
	Via  string // "L" or "W"
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

	return &Monitor{
		conn:      conn,
		table:     table,
		chainRx:   chainRx,
		chainTx:   chainTx,
		clients:   make(map[string]clientEntry),
		wanIface:  wanIface,
		lanIface:  lanIface,
		lanPorts:  lanPorts,
		wifiIface: wifiIface,
	}, nil
}

// AddClient adds nftables counter rules for a new DHCP client.
// via is "L" for LAN or "W" for WiFi.
func (m *Monitor) AddClient(ip net.IP, mac, via string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ipStr := ip.To4().String()
	if _, exists := m.clients[ipStr]; exists {
		return nil // already tracked
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

	m.clients[ipStr] = clientEntry{MAC: mac, IP: ip4, Via: via}
	log.Printf("status: tracking %s (%s) via %s", ipStr, mac, via)
	return nil
}

// getPortInfo returns port info for a single interface.
func getPortInfo(name string) PortInfo {
	pi := PortInfo{Name: name}
	link, err := netlink.LinkByName(name)
	if err != nil {
		return pi
	}
	attrs := link.Attrs()
	pi.Up = attrs.Flags&net.FlagUp != 0
	pi.Carrier = attrs.OperState == netlink.OperUp
	if stats := attrs.Statistics; stats != nil {
		pi.TxBytes = stats.TxBytes
		pi.RxBytes = stats.RxBytes
		pi.TxPkts = stats.TxPackets
		pi.RxPkts = stats.RxPackets
	}
	return pi
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
			IP:  ipStr,
			MAC: entry.MAC,
			Via: entry.Via,
		}
		if rx, ok := rxCounters[ipStr]; ok {
			ci.RxBytes = rx[0]
			ci.RxPkts = rx[1]
		}
		if tx, ok := txCounters[ipStr]; ok {
			ci.TxBytes = tx[0]
			ci.TxPkts = tx[1]
		}
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
