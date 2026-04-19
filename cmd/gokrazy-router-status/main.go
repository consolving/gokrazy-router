// Command gokrazy-router-status queries the router's status API and displays
// port link states and per-client traffic counters.
//
// It can also export the list of known MAC addresses as a TOML file for
// VLAN assignment, and merge new clients into an existing mapping file.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/consolving/gokrazy-router/pkg/macmap"
)

type PortInfo struct {
	Name    string     `json:"name"`
	MAC     string     `json:"mac,omitempty"`
	Up      bool       `json:"up"`
	Carrier bool       `json:"carrier"`
	Speed   int        `json:"speed,omitempty"`
	Duplex  string     `json:"duplex,omitempty"`
	TxBytes uint64     `json:"txBytes"`
	RxBytes uint64     `json:"rxBytes"`
	TxPkts  uint64     `json:"txPackets"`
	RxPkts  uint64     `json:"rxPackets"`
	Sub     []PortInfo `json:"sub,omitempty"`
}

type ClientInfo struct {
	IP           string  `json:"ip"`
	MAC          string  `json:"mac"`
	Via          string  `json:"via"`
	Connected    bool    `json:"connected"`
	TxBytes      uint64  `json:"txBytes"`
	RxBytes      uint64  `json:"rxBytes"`
	TxPkts       uint64  `json:"txPackets"`
	RxPkts       uint64  `json:"rxPackets"`
	TxRate       float64 `json:"txRate"`
	RxRate       float64 `json:"rxRate"`
	LinkTxRate   int     `json:"linkTxRate,omitempty"`
	LinkRxRate   int     `json:"linkRxRate,omitempty"`
	Signal       int     `json:"signal,omitempty"`
	TotalTxBytes uint64  `json:"totalTxBytes"`
	TotalRxBytes uint64  `json:"totalRxBytes"`
	TotalTxPkts  uint64  `json:"totalTxPackets"`
	TotalRxPkts  uint64  `json:"totalRxPackets"`
	FirstSeen    string  `json:"firstSeen"`
	LastSeen     string  `json:"lastSeen"`
}

type SummaryInfo struct {
	Name    string `json:"name"`
	TxBytes uint64 `json:"txBytes"`
	RxBytes uint64 `json:"rxBytes"`
	TxPkts  uint64 `json:"txPackets"`
	RxPkts  uint64 `json:"rxPackets"`
}

type Status struct {
	Summary []SummaryInfo `json:"summary"`
	Ports   []PortInfo    `json:"ports"`
	Clients []ClientInfo  `json:"clients"`
}

func main() {
	host := flag.String("host", "10.0.0.1:8080", "router status API address")
	jsonOut := flag.Bool("json", false, "output raw JSON")
	exportTOML := flag.Bool("export-toml", false, "export known MACs as TOML mac-vlan-map")
	mergeFile := flag.String("merge", "", "merge new MACs into existing TOML file (use with --export-toml)")
	flag.Parse()

	resp, err := http.Get(fmt.Sprintf("http://%s/status", *host))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var s Status
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		fmt.Fprintf(os.Stderr, "error decoding response: %v\n", err)
		os.Exit(1)
	}

	if *exportTOML {
		exportMACMap(s, *mergeFile)
		return
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(s)
		return
	}

	// Ports table (wan, lan1-4, wifi)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "IFACE\tMAC\tSPEED\tRX\tTX\tRX PKTS\tTX PKTS\n")
	for _, p := range s.Ports {
		if len(p.Sub) > 0 {
			// Show sub-ports directly (e.g. lan1-lan4 instead of lan)
			for _, sub := range p.Sub {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\n",
					sub.Name, sub.MAC, formatSpeed(sub.Speed, sub.Duplex),
					humanBytes(sub.RxBytes), humanBytes(sub.TxBytes),
					sub.RxPkts, sub.TxPkts)
			}
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\n",
				p.Name, p.MAC, formatSpeed(p.Speed, p.Duplex),
				humanBytes(p.RxBytes), humanBytes(p.TxBytes),
				p.RxPkts, p.TxPkts)
		}
	}
	w.Flush()

	if len(s.Clients) > 0 {
		// Separate connected and disconnected clients.
		var connected, disconnected []ClientInfo
		for _, c := range s.Clients {
			if c.Connected {
				connected = append(connected, c)
			} else {
				disconnected = append(disconnected, c)
			}
		}

		fmt.Println()
		fmt.Println("CONNECTED CLIENTS")
		if len(connected) == 0 {
			fmt.Println("  (none)")
		} else {
			w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "VIA\tIP\tMAC\tUL RATE\tDL RATE\tLINK\tSIGNAL\tUL\tDL\tTOTAL UL\tTOTAL DL\n")
			hasWiFi := false
			for _, c := range connected {
				via := c.Via
				if via == "W" {
					hasWiFi = true
				} else if via == "" {
					via = "?"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					via, c.IP, c.MAC,
					humanRate(c.RxRate), humanRate(c.TxRate),
					formatLinkRate(c.LinkTxRate, c.LinkRxRate),
					formatSignal(c.Signal),
					humanBytes(c.RxBytes), humanBytes(c.TxBytes),
					humanBytes(c.TotalRxBytes), humanBytes(c.TotalTxBytes))
			}
			w.Flush()

			if hasWiFi {
				fmt.Println()
				fmt.Println("NOTE: WiFi is in routed mode (separate subnet). LAN and WiFi")
				fmt.Println("      clients can reach each other — no inter-subnet firewall")
				fmt.Println("      rules are configured.")
			}
		}

		if len(disconnected) > 0 {
			fmt.Println()
			fmt.Println("DISCONNECTED CLIENTS (historical)")
			w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "VIA\tIP\tMAC\tTOTAL UL\tTOTAL DL\tLAST SEEN\n")
			for _, c := range disconnected {
				via := c.Via
				if via == "" {
					via = "?"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					via, c.IP, c.MAC,
					humanBytes(c.TotalRxBytes), humanBytes(c.TotalTxBytes),
					c.LastSeen)
			}
			w.Flush()
		}
	} else {
		fmt.Println("\nNo clients connected.")
	}
}

func humanBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func formatSpeed(speed int, duplex string) string {
	if speed <= 0 {
		return "-"
	}
	s := fmt.Sprintf("%d Mbps", speed)
	if duplex != "" {
		s += "/" + duplex
	}
	return s
}

func humanRate(bytesPerSec float64) string {
	if bytesPerSec < 1 {
		return "0 B/s"
	}
	switch {
	case bytesPerSec >= 1<<30:
		return fmt.Sprintf("%.1f GiB/s", bytesPerSec/float64(1<<30))
	case bytesPerSec >= 1<<20:
		return fmt.Sprintf("%.1f MiB/s", bytesPerSec/float64(1<<20))
	case bytesPerSec >= 1<<10:
		return fmt.Sprintf("%.1f KiB/s", bytesPerSec/float64(1<<10))
	default:
		return fmt.Sprintf("%.0f B/s", bytesPerSec)
	}
}

func formatLinkRate(txKbps, rxKbps int) string {
	if txKbps <= 0 && rxKbps <= 0 {
		return "-"
	}
	// Link rates from hostapd are in Kbps, display in Mbps.
	tx := float64(txKbps) / 1000
	rx := float64(rxKbps) / 1000
	if tx == rx {
		return fmt.Sprintf("%.1f Mbps", tx)
	}
	return fmt.Sprintf("%.1f/%.1f Mbps", tx, rx)
}

func formatSignal(dBm int) string {
	if dBm == 0 {
		return "-"
	}
	return fmt.Sprintf("%d dBm", dBm)
}

// exportMACMap builds a TOML mac-vlan-map from the status API's client list.
// If mergeFile is set, it loads the existing file and adds only new MACs.
func exportMACMap(s Status, mergeFile string) {
	// Build a MapFile from the current status.
	current := &macmap.MapFile{}
	for _, c := range s.Clients {
		if c.MAC == "" {
			continue
		}
		client := macmap.Client{
			MAC:  c.MAC,
			VLAN: 0, // unassigned
		}
		// Use IP as a hint in the name field.
		if c.IP != "" {
			via := c.Via
			if via == "W" {
				via = "wifi"
			} else if via == "L" {
				via = "lan"
			}
			client.Name = fmt.Sprintf("%s (%s)", c.IP, via)
		}
		current.Clients = append(current.Clients, client)
	}

	if mergeFile != "" {
		// Load existing file, merge new MACs into it.
		existing, err := macmap.Load(mergeFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error loading %s: %v\n", mergeFile, err)
			os.Exit(1)
		}
		existing.Merge(current)
		current = existing
	} else {
		// Fresh export -- set a sensible default.
		current.DefaultVLAN = 1
		fmt.Fprintln(os.Stderr, "# Exported MAC addresses from router status API.")
		fmt.Fprintln(os.Stderr, "# Edit VLAN assignments, then deploy to the router.")
		fmt.Fprintln(os.Stderr, "# Use --merge to add new clients to an existing file.")
	}

	data, err := current.Encode()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error encoding TOML: %v\n", err)
		os.Exit(1)
	}
	os.Stdout.Write(data)
}
