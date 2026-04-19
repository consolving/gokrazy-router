package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/consolving/gokrazy-router/pkg/config"
	"github.com/consolving/gokrazy-router/pkg/dhcp"
	"github.com/consolving/gokrazy-router/pkg/nat"
	"github.com/consolving/gokrazy-router/pkg/netsetup"
	"github.com/consolving/gokrazy-router/pkg/status"
	"github.com/consolving/gokrazy-router/pkg/vlan"
	"github.com/consolving/gokrazy-router/pkg/wifi"
	"github.com/vishvananda/netlink"
)

func main() {
	configPath := flag.String("config", "/etc/gokrazy-router.json", "path to configuration file")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Load configuration.
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Printf("config not found (%v), using defaults", err)
		cfg = config.Default()
	}

	// 1. Network setup: either VLAN mode or flat bridge mode.
	vlanMode := len(cfg.VLANs) > 0
	var vlanBridges []vlan.VLANBridge

	if vlanMode {
		// VLAN mode: create per-VLAN bridges with DSA port membership.
		log.Printf("vlan: configuring %d VLANs", len(cfg.VLANs))
		vlanBridges, err = vlan.Setup(cfg.VLANs)
		if err != nil {
			log.Fatalf("vlan: %v", err)
		}
		_ = vlanBridges // used below for inter-VLAN isolation
	} else {
		// Flat mode: single bridge for all LAN ports.
		_, err = netsetup.Setup(cfg.LAN.Bridge, cfg.LAN.Interfaces, cfg.LAN.Address)
		if err != nil {
			log.Fatalf("netsetup: %v", err)
		}
	}

	// 2. Enable IP forwarding.
	if err := netsetup.EnableForwarding(); err != nil {
		log.Fatalf("forwarding: %v", err)
	}

	// 3. Install NAT masquerade rules.
	var natMgr *nat.Manager
	if cfg.NAT.Enabled {
		if vlanMode {
			// In VLAN mode, set up NAT for each VLAN that has nat=true.
			var initialized bool
			for _, vc := range cfg.VLANs {
				if !vc.NAT {
					continue
				}
				_, vNet, _ := net.ParseCIDR(vc.Address)
				if vNet == nil {
					continue
				}
				if !initialized {
					natMgr, err = nat.Setup(cfg.NAT.OutInterface, vNet)
					if err != nil {
						log.Fatalf("nat: %v", err)
					}
					initialized = true
				} else {
					if err := natMgr.AddSource(vNet); err != nil {
						log.Fatalf("nat: add VLAN %d source: %v", vc.ID, err)
					}
				}
			}
		} else {
			_, srcNet, err := net.ParseCIDR(cfg.LAN.Address)
			if err != nil {
				log.Fatalf("parse LAN CIDR: %v", err)
			}
			natMgr, err = nat.Setup(cfg.NAT.OutInterface, srcNet)
			if err != nil {
				log.Fatalf("nat: %v", err)
			}
		}
	}

	// 3b. Install inter-VLAN isolation rules for isolated VLANs.
	if vlanMode && natMgr != nil {
		// Collect all VLAN bridge names.
		var allBridges []string
		for _, vc := range cfg.VLANs {
			allBridges = append(allBridges, vlan.BridgeName(vc.ID))
		}
		for _, vc := range cfg.VLANs {
			if !vc.Isolated {
				continue
			}
			isolated := vlan.BridgeName(vc.ID)
			var others []string
			for _, b := range allBridges {
				if b != isolated {
					others = append(others, b)
				}
			}
			if len(others) > 0 {
				if err := natMgr.AddIsolation(isolated, others); err != nil {
					log.Fatalf("nat: isolation for VLAN %d: %v", vc.ID, err)
				}
				log.Printf("vlan: VLAN %d (%s) isolated from %d other VLANs", vc.ID, vc.Name, len(others))
			}
		}
	}

	// 4. Start status monitor (nftables per-client counters).
	wifiIface := cfg.WiFi.Interface
	if wifiIface == "" {
		wifiIface = "wlan0"
	}
	mon, err := status.New(cfg.NAT.OutInterface, cfg.LAN.Bridge, cfg.LAN.Interfaces, wifiIface)
	if err != nil {
		log.Printf("status monitor: %v (continuing without)", err)
	}

	// 5. Start WiFi AP (hostapd).
	var wifiAP *wifi.AP
	if cfg.WiFi.Enabled {
		// Wait for the WiFi interface to appear (kernel module may still be loading).
		log.Printf("wifi: waiting for %s to appear...", wifiIface)

		if err := waitForInterface(wifiIface, 120*time.Second); err != nil {
			log.Printf("wifi: %v — continuing without WiFi", err)
		} else {
			log.Printf("wifi: %s is available", wifiIface)

			// Determine bridge parameter for hostapd.
			// In VLAN mode with macMap: bridge wlan0 into the default VLAN bridge
			// so unmatched clients land there. Matched clients get VLAN sub-interfaces
			// bridged via hostapd's vlan_bridge directive.
			// In flat mode with explicit bridge: use that bridge.
			// In routed mode (address set, no macMap): no bridge.
			wifiBridge := ""
			if vlanMode && cfg.WiFi.MacMapFile != "" {
				defaultVLAN := cfg.WiFi.DefaultVLAN
				if defaultVLAN == 0 {
					defaultVLAN = 1
				}
				wifiBridge = vlan.BridgeName(defaultVLAN)
				log.Printf("wifi: VLAN mode, bridging wlan0 into %s (default VLAN %d)", wifiBridge, defaultVLAN)
			} else if cfg.WiFi.Bridge != "" && cfg.WiFi.Address == "" {
				wifiBridge = cfg.WiFi.Bridge
			}

			ap, err := wifi.New(cfg.WiFi, wifiBridge)
			if err != nil {
				log.Fatalf("wifi: %v", err)
			}

			// Log WLAN client events and update status monitor.
			ap.OnClient(func(ev wifi.ClientEvent) {
				if ev.Associated {
					log.Printf("wifi: client %s connected via WLAN", ev.MAC)
				} else {
					log.Printf("wifi: client %s disconnected from WLAN", ev.MAC)
					if mon != nil {
						if err := mon.RemoveClientByMAC(ev.MAC); err != nil {
							log.Printf("status: failed to remove WiFi client %s: %v", ev.MAC, err)
						}
					}
				}
			})

			if err := ap.Start(); err != nil {
				log.Fatalf("wifi: start: %v", err)
			}
			wifiAP = ap

			// Wire WiFi station info into status monitor via hostapd control socket.
			if mon != nil {
				mon.SetWiFiSource(&wifiStationAdapter{ap: ap})
			}

			// Routed mode: assign IP to wlan0, add NAT for WiFi subnet.
			// Skip in VLAN mode — WiFi clients get IPs from per-VLAN DHCP servers.
			if cfg.WiFi.Address != "" && wifiAP.MacMap() == nil {
				if err := assignIP(wifiIface, cfg.WiFi.Address); err != nil {
					log.Fatalf("wifi: assign IP to %s: %v", wifiIface, err)
				}
				log.Printf("wifi: %s configured with %s (routed mode)", wifiIface, cfg.WiFi.Address)

				// Add WiFi subnet to NAT masquerade.
				if natMgr != nil {
					_, wifiNet, err := net.ParseCIDR(cfg.WiFi.Address)
					if err != nil {
						log.Fatalf("parse WiFi CIDR: %v", err)
					}
					if err := natMgr.AddSource(wifiNet); err != nil {
						log.Fatalf("nat: add WiFi source: %v", err)
					}
				}
			}
		}
	}

	// 6. Start DHCP servers.
	if vlanMode {
		// Start a DHCP server on each VLAN bridge that has DHCP enabled.
		for _, vc := range cfg.VLANs {
			if !vc.DHCP.Enabled {
				continue
			}
			bridgeName := vlan.BridgeName(vc.ID)
			srv, err := dhcp.New(
				bridgeName,
				vc.Address,
				vc.DHCP.RangeStart,
				vc.DHCP.RangeEnd,
				vc.DHCP.DNS,
				vc.DHCP.ParseLeaseDuration(),
			)
			if err != nil {
				log.Fatalf("dhcp vlan %d: %v", vc.ID, err)
			}
			if mon != nil {
				vlanName := vc.Name
				if vlanName == "" {
					vlanName = fmt.Sprintf("vlan%d", vc.ID)
				}
				via := fmt.Sprintf("V%d", vc.ID)
				srv.OnLease(func(ip net.IP, mac string) {
					if err := mon.AddClient(ip, mac, via); err != nil {
						log.Printf("status: failed to add %s client %s: %v", vlanName, ip, err)
					}
				})
				srv.OnLeaseExpired(func(ip net.IP, mac string) {
					if err := mon.RemoveClient(ip); err != nil {
						log.Printf("status: failed to remove %s client %s: %v", vlanName, ip, err)
					}
				})
			}
			go func(id int) {
				if err := srv.Run(); err != nil {
					log.Fatalf("dhcp server (VLAN %d): %v", id, err)
				}
			}(vc.ID)
			log.Printf("dhcp: started on %s for VLAN %d (%s)", bridgeName, vc.ID, vc.Name)
		}
	} else if cfg.LAN.DHCP.Enabled {
		// Flat mode: single DHCP server on the LAN bridge.
		srv, err := dhcp.New(
			cfg.LAN.Bridge,
			cfg.LAN.Address,
			cfg.LAN.DHCP.RangeStart,
			cfg.LAN.DHCP.RangeEnd,
			cfg.LAN.DHCP.DNS,
			cfg.LAN.DHCP.ParseLeaseDuration(),
		)
		if err != nil {
			log.Fatalf("dhcp: %v", err)
		}

		// Register lease callback for traffic monitoring.
		if mon != nil {
			srv.OnLease(func(ip net.IP, mac string) {
				if err := mon.AddClient(ip, mac, "L"); err != nil {
					log.Printf("status: failed to add client %s: %v", ip, err)
				}
			})
			srv.OnLeaseExpired(func(ip net.IP, mac string) {
				if err := mon.RemoveClient(ip); err != nil {
					log.Printf("status: failed to remove client %s: %v", ip, err)
				}
			})
		}

		go func() {
			if err := srv.Run(); err != nil {
				log.Fatalf("dhcp server (LAN): %v", err)
			}
		}()
	}

	// 7. Start DHCP server on WiFi interface (routed mode only, not in VLAN mode).
	if wifiAP != nil && cfg.WiFi.Address != "" && cfg.WiFi.DHCP.Enabled && wifiAP.MacMap() == nil {
		srv, err := dhcp.New(
			wifiIface,
			cfg.WiFi.Address,
			cfg.WiFi.DHCP.RangeStart,
			cfg.WiFi.DHCP.RangeEnd,
			cfg.WiFi.DHCP.DNS,
			cfg.WiFi.DHCP.ParseLeaseDuration(),
		)
		if err != nil {
			log.Fatalf("dhcp wifi: %v", err)
		}

		if mon != nil {
			srv.OnLease(func(ip net.IP, mac string) {
				if err := mon.AddClient(ip, mac, "W"); err != nil {
					log.Printf("status: failed to add WiFi client %s: %v", ip, err)
				}
			})
			srv.OnLeaseExpired(func(ip net.IP, mac string) {
				if err := mon.RemoveClient(ip); err != nil {
					log.Printf("status: failed to remove WiFi client %s: %v", ip, err)
				}
			})
		}

		go func() {
			if err := srv.Run(); err != nil {
				log.Fatalf("dhcp server (WiFi): %v", err)
			}
		}()
	}

	// 8. Start status HTTP API.
	if mon != nil {
		http.Handle("/status", mon)
		go func() {
			addr := ":8080"
			log.Printf("status API listening on %s", addr)
			if err := http.ListenAndServe(addr, nil); err != nil {
				log.Printf("status API: %v", err)
			}
		}()
	}

	log.Printf("gokrazy-router running")

	// Wait for shutdown signal.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	sig := <-ch
	log.Printf("received %v, shutting down", sig)

	// Cleanup.
	if wifiAP != nil {
		wifiAP.Stop()
	}
	if mon != nil {
		mon.Stop()
	}
	if natMgr != nil {
		natMgr.Cleanup()
	}
}

// assignIP assigns a CIDR address to the named interface and brings it up.
func assignIP(ifaceName, cidr string) error {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return err
	}
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return err
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	return netlink.LinkSetUp(link)
}

// enableProxyARP enables proxy ARP on the named interface via sysctl.
// This allows the router to answer ARP requests on behalf of hosts
// reachable via a different interface on the same subnet.
func enableProxyARP(ifaceName string) {
	path := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/proxy_arp", ifaceName)
	if err := os.WriteFile(path, []byte("1"), 0644); err != nil {
		log.Printf("proxy_arp: failed to enable on %s: %v", ifaceName, err)
	}
}

// waitForInterface polls until the named network interface exists.
func waitForInterface(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := netlink.LinkByName(name); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("interface %s did not appear within %v", name, timeout)
}

// wifiStationAdapter adapts wifi.AP to status.WiFiStationSource.
type wifiStationAdapter struct {
	ap *wifi.AP
}

func (a *wifiStationAdapter) StationInfoAll() ([]status.WiFiStation, error) {
	stations, err := a.ap.StationInfoAll()
	if err != nil {
		return nil, err
	}
	result := make([]status.WiFiStation, len(stations))
	for i, s := range stations {
		result[i] = status.WiFiStation{
			MAC:       s.MAC,
			Signal:    s.Signal,
			TxBitrate: s.TxBitrate,
			RxBitrate: s.RxBitrate,
		}
	}
	return result, nil
}
