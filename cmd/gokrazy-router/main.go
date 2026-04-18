package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/consolving/gokrazy-router/pkg/config"
	"github.com/consolving/gokrazy-router/pkg/dhcp"
	"github.com/consolving/gokrazy-router/pkg/nat"
	"github.com/consolving/gokrazy-router/pkg/netsetup"
	"github.com/consolving/gokrazy-router/pkg/status"
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

	// 1. Create bridge, enslave LAN ports, assign IP.
	_, err = netsetup.Setup(cfg.LAN.Bridge, cfg.LAN.Interfaces, cfg.LAN.Address)
	if err != nil {
		log.Fatalf("netsetup: %v", err)
	}

	// 2. Enable IP forwarding.
	if err := netsetup.EnableForwarding(); err != nil {
		log.Fatalf("forwarding: %v", err)
	}

	// 3. Install NAT masquerade rules.
	var natMgr *nat.Manager
	if cfg.NAT.Enabled {
		_, srcNet, err := net.ParseCIDR(cfg.LAN.Address)
		if err != nil {
			log.Fatalf("parse LAN CIDR: %v", err)
		}
		natMgr, err = nat.Setup(cfg.NAT.OutInterface, srcNet)
		if err != nil {
			log.Fatalf("nat: %v", err)
		}
	}

	// 4. Start status monitor (nftables per-client counters).
	monitorPorts := append([]string{cfg.NAT.OutInterface}, cfg.LAN.Interfaces...)
	wifiIface := cfg.WiFi.Interface
	if wifiIface == "" {
		wifiIface = "wlan0"
	}
	monitorPorts = append(monitorPorts, wifiIface)
	mon, err := status.New(monitorPorts)
	if err != nil {
		log.Printf("status monitor: %v (continuing without)", err)
	}

	// 5. Start WiFi AP (hostapd).
	var wifiAP *wifi.AP
	if cfg.WiFi.Enabled {
		// Determine bridge parameter: if WiFi has its own address (routed mode),
		// don't pass bridge to hostapd. Otherwise, bridge into LAN bridge.
		wifiBridge := ""
		if cfg.WiFi.Bridge != "" && cfg.WiFi.Address == "" {
			wifiBridge = cfg.WiFi.Bridge
		}

		ap, err := wifi.New(cfg.WiFi, wifiBridge)
		if err != nil {
			log.Fatalf("wifi: %v", err)
		}

		// Log WLAN client events.
		ap.OnClient(func(ev wifi.ClientEvent) {
			if ev.Associated {
				log.Printf("wifi: client %s connected via WLAN", ev.MAC)
			} else {
				log.Printf("wifi: client %s disconnected from WLAN", ev.MAC)
			}
		})

		if err := ap.Start(); err != nil {
			log.Fatalf("wifi: start: %v", err)
		}
		wifiAP = ap

		// Routed mode: assign IP to wlan0, add NAT for WiFi subnet.
		if cfg.WiFi.Address != "" {
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

	// 6. Start DHCP server on LAN bridge.
	if cfg.LAN.DHCP.Enabled {
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
				if err := mon.AddClient(ip, mac); err != nil {
					log.Printf("status: failed to add client %s: %v", ip, err)
				}
			})
		}

		go func() {
			if err := srv.Run(); err != nil {
				log.Fatalf("dhcp server (LAN): %v", err)
			}
		}()
	}

	// 7. Start DHCP server on WiFi interface (routed mode only).
	if cfg.WiFi.Enabled && cfg.WiFi.Address != "" && cfg.WiFi.DHCP.Enabled {
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
				if err := mon.AddClient(ip, mac); err != nil {
					log.Printf("status: failed to add WiFi client %s: %v", ip, err)
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
