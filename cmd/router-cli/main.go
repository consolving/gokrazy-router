package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/consolving/gokrazy-router/pkg/config"
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
	IP6          string  `json:"ip6,omitempty"`
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

const defaultExtrasFile = "/mnt/data/router-extras.toml"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "status":
		runStatus(os.Args[2:])
	case "extras":
		runExtras(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: router-cli <subcommand> [options]

Subcommands:
  status      Query router status API
  extras      Manage runtime extras config (reservations, PXE images)

Run 'router-cli status --help' or 'router-cli extras --help' for details.
`)
}

// --- Status subcommand ---

func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	host := fs.String("host", "10.0.0.1:8080", "router status API address")
	jsonOut := fs.Bool("json", false, "output raw JSON")
	exportTOML := fs.Bool("export-toml", false, "export known MACs as TOML mac-vlan-map")
	mergeFile := fs.String("merge", "", "merge new MACs into existing TOML file (use with --export-toml)")
	fs.Parse(args)

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

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "IFACE\tMAC\tSPEED\tRX\tTX\tRX PKTS\tTX PKTS\n")
	for _, p := range s.Ports {
		if len(p.Sub) > 0 {
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
			fmt.Fprintf(w, "VIA\tIP\tIP6\tMAC\tUL RATE\tDL RATE\tLINK\tSIGNAL\tUL\tDL\tTOTAL UL\tTOTAL DL\n")
			hasWiFi := false
			for _, c := range connected {
				via := c.Via
				if via == "W" {
					hasWiFi = true
				} else if via == "" {
					via = "?"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					via, c.IP, c.IP6, c.MAC,
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
			fmt.Fprintf(w, "VIA\tIP\tIP6\tMAC\tTOTAL UL\tTOTAL DL\tLAST SEEN\n")
			for _, c := range disconnected {
				via := c.Via
				if via == "" {
					via = "?"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					via, c.IP, c.IP6, c.MAC,
					humanBytes(c.TotalRxBytes), humanBytes(c.TotalTxBytes),
					c.LastSeen)
			}
			w.Flush()
		}
	} else {
		fmt.Println("\nNo clients connected.")
	}
}

// --- Extras subcommand ---

func runExtras(args []string) {
	if len(args) < 1 {
		printExtrasUsage()
		os.Exit(1)
	}

	// Parse global extras flags (--file) before the subcommand.
	file := defaultExtrasFile
	if len(args) > 0 && args[0] == "--file" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "error: --file requires a path")
			os.Exit(1)
		}
		file = args[1]
		args = args[2:]
	}

	if len(args) < 1 {
		printExtrasUsage()
		os.Exit(1)
	}

	sub := args[0]
	subArgs := args[1:]

	switch sub {
	case "list":
		runExtrasList(file)
	case "set-reservation":
		runExtrasSetReservation(file, subArgs)
	case "remove-reservation":
		runExtrasRemoveReservation(file, subArgs)
	case "set-mac-image":
		runExtrasSetMacImage(file, subArgs)
	case "remove-mac-image":
		runExtrasRemoveMacImage(file, subArgs)
	default:
		fmt.Fprintf(os.Stderr, "unknown extras subcommand: %s\n", sub)
		printExtrasUsage()
		os.Exit(1)
	}
}

func printExtrasUsage() {
	fmt.Fprintf(os.Stderr, `Usage: router-cli extras [--file <path>] <command> [args]

Commands:
  list                          Show current extras config
  set-reservation <mac> <ip>    Add or update a DHCP reservation
  remove-reservation <mac>      Remove a DHCP reservation
  set-mac-image <mac> <image>   Set PXE boot image for a MAC
  remove-mac-image <mac>        Remove PXE boot image for a MAC

Default file: %s
`, defaultExtrasFile)
}

func loadExtras(path string) *config.ExtrasConfig {
	e, err := config.LoadExtras(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &config.ExtrasConfig{}
		}
		fmt.Fprintf(os.Stderr, "error loading %s: %v\n", path, err)
		os.Exit(1)
	}
	return e
}

func saveExtras(path string, e *config.ExtrasConfig) {
	if err := e.Save(path); err != nil {
		fmt.Fprintf(os.Stderr, "error saving %s: %v\n", path, err)
		os.Exit(1)
	}
}

func runExtrasList(file string) {
	e := loadExtras(file)

	if len(e.Reservations) == 0 && len(e.MacImages) == 0 && len(e.SMBUsers) == 0 {
		fmt.Println("(empty)")
		return
	}

	if len(e.Reservations) > 0 {
		fmt.Println("[reservations]")
		for mac, ip := range e.Reservations {
			fmt.Printf("  %s = %s\n", mac, ip)
		}
		fmt.Println()
	}

	if len(e.MacImages) > 0 {
		fmt.Println("[macImages]")
		for mac, img := range e.MacImages {
			fmt.Printf("  %s = %s\n", mac, img)
		}
		fmt.Println()
	}

	if len(e.SMBUsers) > 0 {
		fmt.Println("[[smbUsers]]")
		for _, u := range e.SMBUsers {
			fmt.Printf("  name = %s\n", u.Name)
		}
		fmt.Println()
	}
}

func runExtrasSetReservation(file string, args []string) {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: router-cli extras set-reservation <mac> <ip>")
		os.Exit(1)
	}
	mac, ip := args[0], args[1]
	e := loadExtras(file)
	if err := e.SetReservation(mac, ip); err == config.ErrNotModified {
		fmt.Printf("reservation for %s unchanged (%s)\n", mac, ip)
		return
	}
	saveExtras(file, e)
	fmt.Printf("reservation set: %s -> %s\n", mac, ip)
}

func runExtrasRemoveReservation(file string, args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: router-cli extras remove-reservation <mac>")
		os.Exit(1)
	}
	mac := args[0]
	e := loadExtras(file)
	if err := e.RemoveReservation(mac); err == config.ErrNotModified {
		fmt.Printf("no reservation found for %s\n", mac)
		return
	}
	saveExtras(file, e)
	fmt.Printf("reservation removed: %s\n", mac)
}

func runExtrasSetMacImage(file string, args []string) {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: router-cli extras set-mac-image <mac> <image>")
		os.Exit(1)
	}
	mac, img := args[0], args[1]
	e := loadExtras(file)
	if err := e.SetMacImage(mac, img); err == config.ErrNotModified {
		fmt.Printf("mac image for %s unchanged (%s)\n", mac, img)
		return
	}
	saveExtras(file, e)
	fmt.Printf("mac image set: %s -> %s\n", mac, img)
}

func runExtrasRemoveMacImage(file string, args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: router-cli extras remove-mac-image <mac>")
		os.Exit(1)
	}
	mac := args[0]
	e := loadExtras(file)
	if err := e.RemoveMacImage(mac); err == config.ErrNotModified {
		fmt.Printf("no mac image found for %s\n", mac)
		return
	}
	saveExtras(file, e)
	fmt.Printf("mac image removed: %s\n", mac)
}

// --- Helpers ---

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

func exportMACMap(s Status, mergeFile string) {
	current := &macmap.MapFile{}
	for _, c := range s.Clients {
		if c.MAC == "" {
			continue
		}
		client := macmap.Client{
			MAC:  c.MAC,
			VLAN: 0,
		}
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
		existing, err := macmap.Load(mergeFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error loading %s: %v\n", mergeFile, err)
			os.Exit(1)
		}
		existing.Merge(current)
		current = existing
	} else {
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
