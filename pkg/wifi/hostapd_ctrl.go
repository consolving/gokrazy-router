// Package wifi - hostapd control socket client for querying station info.
//
// The hostapd control interface uses a Unix datagram socket. The client
// connects a local socket, sends text commands, and reads text replies.
// We use this to query per-station signal strength and bitrate without
// going through nl80211 (which conflicts with the RTL8192CU driver).
//
// Protocol reference: hostapd/src/common/wpa_ctrl.c
package wifi

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	hostapdCtrlDir = "/var/run/hostapd"
	ctrlTimeout    = 2 * time.Second
)

// StationInfo holds per-station data from hostapd.
type StationInfo struct {
	MAC        string
	Signal     int // dBm
	TxBitrate  int // Kbps (as reported by hostapd)
	RxBitrate  int // Kbps
	Connected  time.Duration
}

// CtrlClient communicates with hostapd via its control socket.
type CtrlClient struct {
	conn    *net.UnixConn
	localPath string
}

// NewCtrlClient connects to the hostapd control socket for the given interface.
func NewCtrlClient(iface string) (*CtrlClient, error) {
	remotePath := hostapdCtrlDir + "/" + iface

	// Create a temporary local socket for receiving replies.
	localPath := fmt.Sprintf("/tmp/hostapd_ctrl_%d", os.Getpid())
	os.Remove(localPath) // clean up any stale socket

	local, err := net.ResolveUnixAddr("unixgram", localPath)
	if err != nil {
		return nil, err
	}
	remote, err := net.ResolveUnixAddr("unixgram", remotePath)
	if err != nil {
		return nil, err
	}

	conn, err := net.DialUnix("unixgram", local, remote)
	if err != nil {
		os.Remove(localPath)
		return nil, fmt.Errorf("connect to hostapd ctrl %s: %w", remotePath, err)
	}

	return &CtrlClient{conn: conn, localPath: localPath}, nil
}

// Close closes the connection and removes the local socket.
func (c *CtrlClient) Close() error {
	c.conn.Close()
	os.Remove(c.localPath)
	return nil
}

// request sends a command and returns the response.
func (c *CtrlClient) request(cmd string) (string, error) {
	c.conn.SetDeadline(time.Now().Add(ctrlTimeout))
	if _, err := c.conn.Write([]byte(cmd)); err != nil {
		return "", err
	}
	buf := make([]byte, 4096)
	n, err := c.conn.Read(buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// Stations returns info for all connected stations by iterating STA-FIRST / STA-NEXT.
func (c *CtrlClient) Stations() ([]StationInfo, error) {
	var stations []StationInfo

	resp, err := c.request("STA-FIRST")
	if err != nil {
		return nil, err
	}

	for {
		resp = strings.TrimSpace(resp)
		if resp == "" || resp == "FAIL" {
			break
		}

		si := parseStationResponse(resp)
		if si.MAC != "" {
			stations = append(stations, si)
		}

		// Get next station.
		resp, err = c.request("STA-NEXT " + si.MAC)
		if err != nil {
			break
		}
	}

	return stations, nil
}

// parseStationResponse parses a STA/STA-FIRST/STA-NEXT response.
// The first line is the MAC address, followed by key=value pairs.
//
// Example:
//
//	aa:bb:cc:dd:ee:ff
//	flags=[AUTH][ASSOC][AUTHORIZED]
//	aid=1
//	capability=0x431
//	listen_interval=10
//	supported_rates=...
//	timeout_next=NULLFUNC POLL
//	signal=-42
//	connected_time=123
//	tx_bitrate=720
//	rx_bitrate=650
func parseStationResponse(resp string) StationInfo {
	lines := strings.Split(resp, "\n")
	if len(lines) < 1 {
		return StationInfo{}
	}

	si := StationInfo{MAC: strings.TrimSpace(lines[0])}

	for _, line := range lines[1:] {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "signal":
			si.Signal, _ = strconv.Atoi(val)
		case "tx_bitrate":
			si.TxBitrate = parseBitrate(val)
		case "rx_bitrate":
			si.RxBitrate = parseBitrate(val)
		case "connected_time":
			secs, _ := strconv.Atoi(val)
			si.Connected = time.Duration(secs) * time.Second
		}
	}

	return si
}

// parseBitrate parses a bitrate value from hostapd. The value may be a plain
// integer (in 100 Kbps units in some versions) or a string like "72.2 MBit/s".
// We return the value in Kbps.
func parseBitrate(s string) int {
	s = strings.TrimSpace(s)

	// Try "XX.X MBit/s" format first.
	if strings.Contains(s, "MBit/s") {
		s = strings.TrimSuffix(s, " MBit/s")
		s = strings.Fields(s)[0] // take first field in case of "72.2 MBit/s MCS 7 40MHz"
		f, err := strconv.ParseFloat(s, 64)
		if err == nil {
			return int(f * 1000) // MBit/s -> Kbps
		}
	}

	// Plain integer — hostapd reports in 100 Kbps units.
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v * 100
}
