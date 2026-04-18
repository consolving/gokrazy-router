// Package nat configures nftables NAT/masquerade rules.
package nat

import (
	"fmt"
	"log"
	"net"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

// Manager holds the nftables connection and references to created objects
// so they can be cleaned up on shutdown.
type Manager struct {
	conn  *nftables.Conn
	table *nftables.Table
}

// Setup creates the NAT masquerade rules: traffic from srcNet going out
// outIface gets masqueraded. Returns a Manager that can be used for cleanup.
func Setup(outIface string, srcNet *net.IPNet) (*Manager, error) {
	conn := &nftables.Conn{}

	// Get outbound interface index for the meta match.
	iface, err := net.InterfaceByName(outIface)
	if err != nil {
		return nil, fmt.Errorf("lookup interface %s: %w", outIface, err)
	}

	// Create table.
	table := conn.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   "gokrazy_nat",
	})

	// Create postrouting chain with NAT hook.
	chain := conn.AddChain(&nftables.Chain{
		Name:     "postrouting",
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
	})

	// Build rule: ip saddr <srcNet> oifname <outIface> masquerade
	//
	// We match on:
	//   1. source address in srcNet
	//   2. output interface == outIface
	// Then apply masquerade.
	ones, _ := srcNet.Mask.Size()
	conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: []expr.Any{
			// Load source IP into register 1
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseNetworkHeader,
				Offset:       12,
				Len:          4,
			},
			// Bitwise AND with mask for CIDR matching
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            4,
				Mask:           srcNet.Mask,
				Xor:            net.IPv4Mask(0, 0, 0, 0),
			},
			// Compare with network address
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     srcNet.IP.To4(),
			},
			// Load output interface index into register 1
			&expr.Meta{
				Key:      expr.MetaKeyOIFNAME,
				Register: 1,
			},
			// Compare with outIface
			&expr.Cmp{
				Op:       expr.CmpOpEq,
				Register: 1,
				Data:     ifname(outIface),
			},
			// Masquerade
			&expr.Masq{},
		},
	})

	if err := conn.Flush(); err != nil {
		return nil, fmt.Errorf("nftables flush: %w", err)
	}

	log.Printf("nat: masquerade %s/%d via %s (ifindex %d)", srcNet.IP, ones, outIface, iface.Index)

	return &Manager{conn: conn, table: table}, nil
}

// Cleanup removes the NAT table and all its rules.
func (m *Manager) Cleanup() {
	if m == nil || m.conn == nil {
		return
	}
	m.conn.DelTable(m.table)
	if err := m.conn.Flush(); err != nil {
		log.Printf("nat: cleanup error: %v", err)
		return
	}
	log.Printf("nat: cleaned up rules")
}

// ifname returns a null-terminated byte slice for nftables interface name matching.
func ifname(name string) []byte {
	b := make([]byte, 16) // IFNAMSIZ
	copy(b, name)
	return b
}
