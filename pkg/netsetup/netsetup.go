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
// provided addresses (IPv4 and optionally IPv6, CIDR notation), brings
// everything up, and enables IP forwarding.
func Setup(bridgeName string, lanIfaces []string, addr, addr6 string) (netlink.Link, error) {
	// Parse addresses first so we fail early on bad input.
	ipNet, err := netlink.ParseAddr(addr)
	if err != nil {
		return nil, fmt.Errorf("parse address %q: %w", addr, err)
	}
	var ip6Net *netlink.Addr
	if addr6 != "" {
		ip6Net, err = netlink.ParseAddr(addr6)
		if err != nil {
			return nil, fmt.Errorf("parse address6 %q: %w", addr6, err)
		}
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

	// Assign IPv4 address to bridge.
	if err := netlink.AddrAdd(brLink, ipNet); err != nil {
		// If address already exists, that's fine.
		if !os.IsExist(err) {
			return nil, fmt.Errorf("assign %s to %s: %w", addr, bridgeName, err)
		}
	}

	// Assign optional IPv6 address to bridge.
	if ip6Net != nil {
		if err := netlink.AddrAdd(brLink, ip6Net); err != nil {
			if !os.IsExist(err) {
				return nil, fmt.Errorf("assign %s to %s: %w", addr6, bridgeName, err)
			}
		}
	}

	// Bring up the bridge.
	if err := netlink.LinkSetUp(brLink); err != nil {
		return nil, fmt.Errorf("bring up %s: %w", bridgeName, err)
	}

	log.Printf("netsetup: %s up with %s%s", bridgeName, addr, optAddr6Log(addr6))
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

// EnableIPv6Forwarding enables IPv6 packet forwarding via /proc.
func EnableIPv6Forwarding() error {
	for _, key := range []string{"net/ipv6/conf/all/forwarding", "net/ipv6/conf/default/forwarding"} {
		if err := os.WriteFile("/proc/sys/"+key, []byte("1"), 0644); err != nil {
			return fmt.Errorf("enable ipv6 forwarding (%s): %w", key, err)
		}
	}
	log.Printf("netsetup: IPv6 forwarding enabled")
	return nil
}

// EnableSLAAC configures an interface to accept Router Advertisements and
// auto-configure a global address via SLAAC. accept_ra=2 is required on
// interfaces of a router that itself has IPv6 forwarding enabled.
func EnableSLAAC(iface string) error {
	for _, key := range []string{"accept_ra", "autoconf"} {
		v := "1"
		if key == "accept_ra" {
			v = "2"
		}
		path := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/%s", iface, key)
		if err := os.WriteFile(path, []byte(v), 0644); err != nil {
			return fmt.Errorf("enable slaac %s on %s: %w", key, iface, err)
		}
	}
	log.Printf("netsetup: SLAAC enabled on %s", iface)
	return nil
}

// DisableIPv6 turns off IPv6 on an interface entirely.
func DisableIPv6(iface string) error {
	path := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/disable_ipv6", iface)
	if err := os.WriteFile(path, []byte("1"), 0644); err != nil {
		return fmt.Errorf("disable ipv6 on %s: %w", iface, err)
	}
	log.Printf("netsetup: IPv6 disabled on %s", iface)
	return nil
}

func optAddr6Log(addr6 string) string {
	if addr6 == "" {
		return ""
	}
	return " and " + addr6
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
