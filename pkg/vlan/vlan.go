// Package vlan handles 802.1Q VLAN sub-interface creation and VLAN-aware
// bridge configuration for the BPI-R1's BCM53125 DSA switch.
//
// For DSA switch ports (lan1-lan4, wan), VLAN membership is configured via
// the Linux bridge VLAN API. Each VLAN gets its own bridge with the
// appropriate ports enslaved and their PVID set.
//
// For WiFi, hostapd creates VLAN sub-interfaces (e.g. wlan0.10) dynamically
// when clients are assigned to VLANs. These sub-interfaces are then added
// to the corresponding VLAN bridge by the router daemon.
package vlan

import (
	"fmt"
	"log"
	"net"
	"os"

	"github.com/consolving/gokrazy-router/pkg/config"
	"github.com/vishvananda/netlink"
)

// VLANBridge holds the result of setting up a single VLAN.
type VLANBridge struct {
	ID     int
	Name   string
	Bridge netlink.Link
	Subnet *net.IPNet
}

// Setup configures all VLANs defined in the config. For each VLAN it:
//  1. Creates a VLAN-filtering bridge (br-vlan<ID>)
//  2. Enslaves the configured DSA ports
//  3. Sets the PVID and untagged flags on each port for this VLAN
//  4. Assigns the IP address to the bridge
//  5. Brings everything up
//
// Returns a slice of VLANBridge for use by the caller (DHCP, NAT, etc).
func Setup(vlans []config.VLANConfig) ([]VLANBridge, error) {
	var result []VLANBridge

	for _, vc := range vlans {
		vb, err := setupOne(vc)
		if err != nil {
			return nil, fmt.Errorf("vlan %d (%s): %w", vc.ID, vc.Name, err)
		}
		result = append(result, *vb)
	}

	return result, nil
}

func setupOne(vc config.VLANConfig) (*VLANBridge, error) {
	bridgeName := fmt.Sprintf("br-vlan%d", vc.ID)

	// Parse address.
	ipNet, err := netlink.ParseAddr(vc.Address)
	if err != nil {
		return nil, fmt.Errorf("parse address %q: %w", vc.Address, err)
	}

	// Create bridge. Each VLAN gets its own bridge with dedicated ports,
	// so VLAN filtering is not needed — port isolation is achieved by
	// having each port on a separate bridge.
	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: bridgeName,
		},
	}
	if err := netlink.LinkAdd(br); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("create bridge %s: %w", bridgeName, err)
	}

	brLink, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return nil, fmt.Errorf("get bridge %s: %w", bridgeName, err)
	}

	// Enslave ports to this bridge.
	for _, portName := range vc.Ports {
		port, err := netlink.LinkByName(portName)
		if err != nil {
			log.Printf("vlan: skipping port %s: %v", portName, err)
			continue
		}

		// Enslave port into the VLAN bridge.
		if err := netlink.LinkSetMaster(port, brLink); err != nil {
			return nil, fmt.Errorf("enslave %s to %s: %w", portName, bridgeName, err)
		}

		if err := netlink.LinkSetUp(port); err != nil {
			return nil, fmt.Errorf("bring up %s: %w", portName, err)
		}

		log.Printf("vlan: port %s -> %s (vlan %d)", portName, bridgeName, vc.ID)
	}

	// Assign IP address.
	if err := netlink.AddrAdd(brLink, ipNet); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("assign %s to %s: %w", vc.Address, bridgeName, err)
	}

	// Bring up the bridge.
	if err := netlink.LinkSetUp(brLink); err != nil {
		return nil, fmt.Errorf("bring up %s: %w", bridgeName, err)
	}

	_, subnet, _ := net.ParseCIDR(vc.Address)

	log.Printf("vlan: %s (vlan %d) up with %s, ports: %v", bridgeName, vc.ID, vc.Address, vc.Ports)

	return &VLANBridge{
		ID:     vc.ID,
		Name:   vc.Name,
		Bridge: brLink,
		Subnet: subnet,
	}, nil
}

// AddInterface adds an interface (e.g. a hostapd-created wlan0.10) to the
// bridge for the given VLAN ID. This is used to add WiFi VLAN sub-interfaces
// after hostapd creates them.
func AddInterface(vlanID int, ifaceName string) error {
	bridgeName := fmt.Sprintf("br-vlan%d", vlanID)
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

	log.Printf("vlan: added %s to %s (vid %d)", ifaceName, bridgeName, vlanID)
	return nil
}

// BridgeName returns the bridge name for a VLAN ID.
func BridgeName(vlanID int) string {
	return fmt.Sprintf("br-vlan%d", vlanID)
}
