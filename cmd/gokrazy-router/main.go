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
	if cfg.WiFi.Interface != "" {
		monitorPorts = append(monitorPorts, cfg.WiFi.Interface)
	} else {
		monitorPorts = append(monitorPorts, "wlan0") // default
	}
	mon, err := status.New(monitorPorts)
	if err != nil {
		log.Printf("status monitor: %v (continuing without)", err)
	}

	// 5. Start WiFi AP (hostapd).
	var wifiAP *wifi.AP
	if cfg.WiFi.Enabled {
		ap, err := wifi.New(cfg.WiFi, cfg.LAN.Bridge)
		if err != nil {
			log.Fatalf("wifi: %v", err)
		}

		// Log WLAN client events and optionally track in status monitor.
		if mon != nil {
			ap.OnClient(func(ev wifi.ClientEvent) {
				if ev.Associated {
					log.Printf("wifi: client %s connected via WLAN", ev.MAC)
				} else {
					log.Printf("wifi: client %s disconnected from WLAN", ev.MAC)
				}
			})
		}

		if err := ap.Start(); err != nil {
			log.Fatalf("wifi: start: %v", err)
		}
		wifiAP = ap
	}

	// 6. Start DHCP server (serves both wired and wireless clients via br-lan).
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
				log.Fatalf("dhcp server: %v", err)
			}
		}()
	}

	// 7. Start status HTTP API.
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
