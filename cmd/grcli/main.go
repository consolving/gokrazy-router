// Command grcli queries the router's HTTP API for status, MAC export,
// and netboot image management.
//
// Usage:
//
//	grcli [flags] [command]
//
// Commands:
//
//	status              Show port and client status (default)
//	netboot list        List netboot images
//	netboot upload      Upload a netboot image (tar archive or single file)
//	netboot delete      Delete a netboot image or file
//
// Flags:
//
//	--host string       Router API address (default "10.0.0.1:8080")
//	--json              Output raw JSON
//
// Status flags:
//
//	--export-toml       Export known MACs as TOML mac-vlan-map
//	--merge string      Merge new MACs into existing TOML file
//
// Netboot upload flags:
//
//	--name string       Image name (required)
//	--file string       Path to tar/tar.gz archive or single file
//	--dest string       Destination path within image (for single file upload)
//
// Netboot delete flags:
//
//	--name string       Image name (required)
//	--path string       File path within image (omit to delete entire image)
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
	host := flag.String("host", "10.0.0.1:8080", "router API address")
	jsonOut := flag.Bool("json", false, "output raw JSON")
	exportTOML := flag.Bool("export-toml", false, "export known MACs as TOML mac-vlan-map")
	mergeFile := flag.String("merge", "", "merge new MACs into existing TOML file (use with --export-toml)")
	// Netboot flags.
	nbName := flag.String("name", "", "netboot image name")
	nbFile := flag.String("file", "", "path to tar/tar.gz archive or single file (netboot upload)")
	nbDest := flag.String("dest", "", "destination path within image (single file upload)")
	nbPath := flag.String("path", "", "file path within image (netboot delete)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: grcli [flags] [command]

Commands:
  status              Show port and client status (default)
  netboot list        List netboot images
  netboot upload      Upload a netboot image (tar archive or single file)
  netboot delete      Delete a netboot image or file

Flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	cmd := "status"
	if len(args) >= 1 {
		cmd = args[0]
	}

	switch cmd {
	case "status":
		runStatus(*host, *jsonOut, *exportTOML, *mergeFile)
	case "netboot":
		subcmd := ""
		if len(args) >= 2 {
			subcmd = args[1]
		}
		switch subcmd {
		case "list":
			runNetbootList(*host, *jsonOut)
		case "upload":
			if *nbName == "" || *nbFile == "" {
				fmt.Fprintln(os.Stderr, "error: --name and --file are required for netboot upload")
				os.Exit(1)
			}
			runNetbootUpload(*host, *nbName, *nbFile, *nbDest)
		case "delete":
			if *nbName == "" {
				fmt.Fprintln(os.Stderr, "error: --name is required for netboot delete")
				os.Exit(1)
			}
			runNetbootDelete(*host, *nbName, *nbPath)
		default:
			fmt.Fprintln(os.Stderr, "error: unknown netboot subcommand (use: list, upload, delete)")
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "error: unknown command %q\n", cmd)
		flag.Usage()
		os.Exit(1)
	}
}

func runStatus(host string, jsonOut, exportTOML bool, mergeFile string) {
	resp, err := http.Get(fmt.Sprintf("http://%s/status", host))
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

	if exportTOML {
		exportMACMap(s, mergeFile)
		return
	}

	if jsonOut {
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

// runNetbootList fetches and displays the list of netboot images.
func runNetbootList(host string, jsonOut bool) {
	resp, err := http.Get(fmt.Sprintf("http://%s/netboot/images", host))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "error: server returned %s\n", resp.Status)
		os.Exit(1)
	}

	var result struct {
		Images []struct {
			Name  string `json:"name"`
			Files int    `json:"files"`
			Size  uint64 `json:"size"`
		} `json:"images"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "error decoding response: %v\n", err)
		os.Exit(1)
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(result)
		return
	}

	if len(result.Images) == 0 {
		fmt.Println("No netboot images found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "NAME\tFILES\tSIZE\n")
	for _, img := range result.Images {
		fmt.Fprintf(w, "%s\t%d\t%s\n", img.Name, img.Files, humanBytes(img.Size))
	}
	w.Flush()
}

// runNetbootUpload uploads a tar archive or single file as a netboot image.
func runNetbootUpload(host, name, file, dest string) {
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading %s: %v\n", file, err)
		os.Exit(1)
	}

	var url string
	var method string
	var contentType string

	if dest != "" {
		// Single file upload.
		url = fmt.Sprintf("http://%s/netboot/images/%s/%s", host, name, dest)
		method = http.MethodPut
		contentType = "application/octet-stream"
	} else {
		// Tar archive upload.
		url = fmt.Sprintf("http://%s/netboot/images/%s", host, name)
		method = http.MethodPost
		contentType = "application/gzip"
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "error: server returned %s: %s\n", resp.Status, body)
		os.Exit(1)
	}

	if dest != "" {
		fmt.Printf("Uploaded %s to image %s/%s (%s)\n", file, name, dest, humanBytes(uint64(len(data))))
	} else {
		fmt.Printf("Uploaded image %s from %s (%s)\n", name, file, humanBytes(uint64(len(data))))
	}
}

// runNetbootDelete deletes a netboot image or a single file within it.
func runNetbootDelete(host, name string, path string) {
	var url string
	if path != "" {
		url = fmt.Sprintf("http://%s/netboot/images/%s/%s", host, name, path)
	} else {
		url = fmt.Sprintf("http://%s/netboot/images/%s", host, name)
	}

	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "error: server returned %s: %s\n", resp.Status, body)
		os.Exit(1)
	}

	if path != "" {
		fmt.Printf("Deleted %s/%s\n", name, path)
	} else {
		fmt.Printf("Deleted image %s\n", name)
	}
}
