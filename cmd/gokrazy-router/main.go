package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/consolving/gokrazy-router/pkg/config"
	"github.com/consolving/gokrazy-router/pkg/dhcp"
	"github.com/consolving/gokrazy-router/pkg/nat"
	"github.com/consolving/gokrazy-router/pkg/netsetup"
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

	// 4. Start DHCP server.
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
		go func() {
			if err := srv.Run(); err != nil {
				log.Fatalf("dhcp server: %v", err)
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
	if natMgr != nil {
		natMgr.Cleanup()
	}
}
