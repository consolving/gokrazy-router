// Command gokrazy-router-status queries the router's status API and displays
// port link states and per-client traffic counters.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
)

type PortInfo struct {
	Name    string     `json:"name"`
	Up      bool       `json:"up"`
	Carrier bool       `json:"carrier"`
	TxBytes uint64     `json:"txBytes"`
	RxBytes uint64     `json:"rxBytes"`
	TxPkts  uint64     `json:"txPackets"`
	RxPkts  uint64     `json:"rxPackets"`
	Sub     []PortInfo `json:"sub,omitempty"`
}

type ClientInfo struct {
	IP      string `json:"ip"`
	MAC     string `json:"mac"`
	Via     string `json:"via"`
	TxBytes uint64 `json:"txBytes"`
	RxBytes uint64 `json:"rxBytes"`
	TxPkts  uint64 `json:"txPackets"`
	RxPkts  uint64 `json:"rxPackets"`
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

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(s)
		return
	}

	// Ports table (wan, lan with sub-ports, wifi)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "IFACE\tRX\tTX\tRX PKTS\tTX PKTS\n")
	for _, p := range s.Ports {
		printPort(w, p, "")
	}
	w.Flush()

	if len(s.Clients) > 0 {
		fmt.Println()
		fmt.Println("CLIENTS")
		w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "VIA\tIP\tMAC\tUL\tDL\tUL PKTS\tDL PKTS\n")
		for _, c := range s.Clients {
			via := c.Via
			if via == "" {
				via = "?"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\n",
				via, c.IP, c.MAC,
				humanBytes(c.RxBytes), humanBytes(c.TxBytes),
				c.RxPkts, c.TxPkts)
		}
		w.Flush()
	} else {
		fmt.Println("\nNo clients connected.")
	}
}

func printPort(w *tabwriter.Writer, p PortInfo, prefix string) {
	fmt.Fprintf(w, "%s%s\t%s\t%s\t%d\t%d\n",
		prefix, p.Name,
		humanBytes(p.RxBytes), humanBytes(p.TxBytes),
		p.RxPkts, p.TxPkts)
	for _, sub := range p.Sub {
		printPort(w, sub, "  ")
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
