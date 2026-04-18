package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	configPath := flag.String("config", "/etc/gokrazy-router.json", "path to configuration file")
	flag.Parse()

	log.Printf("gokrazy-router starting, config=%s", *configPath)

	// TODO: load config
	// TODO: network setup (bridge, IPs)
	// TODO: VLAN setup
	// TODO: enable IP forwarding
	// TODO: install NAT rules
	// TODO: start DHCP server(s)

	_ = configPath

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	sig := <-ch
	log.Printf("received %v, shutting down", sig)
}
