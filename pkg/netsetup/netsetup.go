// Package netsetup handles bridge creation, interface enslaving, and IP
// assignment via netlink. It also enables IPv4 forwarding.
package netsetup

import (
	"fmt"
	"log"
	"net"
	"os"

	"github.com/vishvananda/netlink"
)

// Setup creates the LAN bridge, enslaves the given interfaces, assigns the
// provided address (CIDR notation), brings everything up, and enables IP
// forwarding.
func Setup(bridgeName string, lanIfaces []string, addr string) (netlink.Link, error) {
	// Parse address first so we fail early on bad input.
	ipNet, err := netlink.ParseAddr(addr)
	if err != nil {
		return nil, fmt.Errorf("parse address %q: %w", addr, err)
	}

	// Create bridge if it doesn't already exist.
	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: bridgeName,
		},
	}
	if err := netlink.LinkAdd(br); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("create bridge %s: %w", bridgeName, err)
	}

	// Re-fetch to get a valid index.
	brLink, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return nil, fmt.Errorf("get bridge %s: %w", bridgeName, err)
	}

	// Enslave LAN interfaces.
	for _, name := range lanIfaces {
		iface, err := netlink.LinkByName(name)
		if err != nil {
			log.Printf("netsetup: skipping %s: %v", name, err)
			continue
		}
		if err := netlink.LinkSetMaster(iface, brLink); err != nil {
			return nil, fmt.Errorf("enslave %s to %s: %w", name, bridgeName, err)
		}
		if err := netlink.LinkSetUp(iface); err != nil {
			return nil, fmt.Errorf("bring up %s: %w", name, err)
		}
		log.Printf("netsetup: enslaved %s into %s", name, bridgeName)
	}

	// Assign IP address to bridge.
	if err := netlink.AddrAdd(brLink, ipNet); err != nil {
		// If address already exists, that's fine.
		if !os.IsExist(err) {
			return nil, fmt.Errorf("assign %s to %s: %w", addr, bridgeName, err)
		}
	}

	// Bring up the bridge.
	if err := netlink.LinkSetUp(brLink); err != nil {
		return nil, fmt.Errorf("bring up %s: %w", bridgeName, err)
	}

	log.Printf("netsetup: %s up with %s", bridgeName, addr)
	return brLink, nil
}

// EnableForwarding enables IPv4 packet forwarding via /proc.
func EnableForwarding() error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	log.Printf("netsetup: IPv4 forwarding enabled")
	return nil
}

// BridgeAddIface adds an additional interface (e.g. wlan0) to an existing bridge.
func BridgeAddIface(bridgeName, ifaceName string) error {
	br, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("get bridge %s: %w", bridgeName, err)
	}
	iface, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("get interface %s: %w", ifaceName, err)
	}
	if err := netlink.LinkSetMaster(iface, br); err != nil {
		return fmt.Errorf("enslave %s to %s: %w", ifaceName, bridgeName, err)
	}
	if err := netlink.LinkSetUp(iface); err != nil {
		return fmt.Errorf("bring up %s: %w", ifaceName, err)
	}
	log.Printf("netsetup: added %s to %s", ifaceName, bridgeName)
	return nil
}

// ParseCIDR is a helper that returns the IP and network from a CIDR string.
func ParseCIDR(cidr string) (net.IP, *net.IPNet, error) {
	return net.ParseCIDR(cidr)
}
