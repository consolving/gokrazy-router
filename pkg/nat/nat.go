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
	conn     *nftables.Conn
	table    *nftables.Table
	chain    *nftables.Chain
	fwdChain *nftables.Chain // forward chain for isolation rules (created on demand)
	outIface string
	isV6     bool
}

// Setup creates the IPv4 NAT masquerade rules: traffic from srcNet going out
// outIface gets masqueraded. Returns a Manager that can be used for cleanup.
func Setup(outIface string, srcNet *net.IPNet) (*Manager, error) {
	return setup(outIface, srcNet, false, "gokrazy_nat")
}

// Setup6 creates the IPv6 NAT66 masquerade rules: traffic from srcNet going
// out outIface gets masqueraded.
func Setup6(outIface string, srcNet *net.IPNet) (*Manager, error) {
	return setup(outIface, srcNet, true, "gokrazy_nat6")
}

func setup(outIface string, srcNet *net.IPNet, isV6 bool, tableName string) (*Manager, error) {
	conn := &nftables.Conn{}

	// Get outbound interface index for the meta match.
	iface, err := net.InterfaceByName(outIface)
	if err != nil {
		return nil, fmt.Errorf("lookup interface %s: %w", outIface, err)
	}

	// Create table.
	family := nftables.TableFamilyIPv4
	if isV6 {
		family = nftables.TableFamilyIPv6
	}
	table := conn.AddTable(&nftables.Table{
		Family: family,
		Name:   tableName,
	})

	// Create postrouting chain with NAT hook.
	chain := conn.AddChain(&nftables.Chain{
		Name:     "postrouting",
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
	})

	// Build rule: ip saddr <srcNet> oifname <outIface> masquerade.
	// We match on the source address in srcNet and the output interface,
	// then apply masquerade.
	ones, _ := srcNet.Mask.Size()
	conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: srcMatchExprs(srcNet, isV6, outIface),
	})

	if err := conn.Flush(); err != nil {
		return nil, fmt.Errorf("nftables flush: %w", err)
	}

	log.Printf("nat: masquerade %s/%d via %s (ifindex %d)", srcNet.IP, ones, outIface, iface.Index)

	return &Manager{conn: conn, table: table, chain: chain, outIface: outIface, isV6: isV6}, nil
}

// srcMatchExprs builds the expression chain that matches packets whose source
// address lies in srcNet and whose output interface equals outIface.
func srcMatchExprs(srcNet *net.IPNet, isV6 bool, outIface string) []expr.Any {
	addrLen := len(srcNet.Mask) // 4 for IPv4, 16 for IPv6
	var srcOffset uint32
	if isV6 {
		srcOffset = 8 // IPv6 source address
	} else {
		srcOffset = 12 // IPv4 source address
	}
	data := srcNet.IP.To4()
	if isV6 {
		data = srcNet.IP.To16()
	}
	return []expr.Any{
		// Load source IP into register 1
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       srcOffset,
			Len:          uint32(addrLen),
		},
		// Bitwise AND with mask for CIDR matching
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            uint32(addrLen),
			Mask:           srcNet.Mask,
			Xor:            make([]byte, addrLen),
		},
		// Compare with network address
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     data,
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
	}
}

// AddSource adds an additional source network to the masquerade rules.
func (m *Manager) AddSource(srcNet *net.IPNet) error {
	ones, _ := srcNet.Mask.Size()
	m.conn.AddRule(&nftables.Rule{
		Table: m.table,
		Chain: m.chain,
		Exprs: srcMatchExprs(srcNet, m.isV6, m.outIface),
	})

	if err := m.conn.Flush(); err != nil {
		return fmt.Errorf("nftables flush: %w", err)
	}

	log.Printf("nat: added masquerade for %s/%d via %s", srcNet.IP, ones, m.outIface)
	return nil
}

// AddSource6 adds an additional IPv6 source network to the NAT66 rules.
func (m *Manager) AddSource6(srcNet *net.IPNet) error {
	return m.AddSource(srcNet)
}

// AddIsolation adds IPv4 forward-chain rules that drop traffic from an isolated
// VLAN bridge to any other VLAN bridge. Traffic to the WAN (outIface) is
// still allowed because the forward chain has a default policy of accept
// and we only drop inter-bridge forwarding.
//
// The rule matches: iifname == isolatedBridge AND oifname == otherBridge → drop.
func (m *Manager) AddIsolation(isolatedBridge string, otherBridges []string) error {
	return m.addIsolation(isolatedBridge, otherBridges)
}

// AddIsolation6 is AddIsolation for the IPv6 nftables table. Both must be
// installed to isolate a VLAN for both address families.
func (m *Manager) AddIsolation6(isolatedBridge string, otherBridges []string) error {
	return m.addIsolation(isolatedBridge, otherBridges)
}

func (m *Manager) addIsolation(isolatedBridge string, otherBridges []string) error {
	// Ensure we have a forward chain in the same table.
	if m.fwdChain == nil {
		m.fwdChain = m.conn.AddChain(&nftables.Chain{
			Name:     "forward",
			Table:    m.table,
			Type:     nftables.ChainTypeFilter,
			Hooknum:  nftables.ChainHookForward,
			Priority: nftables.ChainPriorityFilter,
		})
	}

	for _, other := range otherBridges {
		// Drop: isolatedBridge -> otherBridge
		m.conn.AddRule(&nftables.Rule{
			Table: m.table,
			Chain: m.fwdChain,
			Exprs: []expr.Any{
				&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(isolatedBridge)},
				&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(other)},
				&expr.Verdict{Kind: expr.VerdictDrop},
			},
		})
		// Drop: otherBridge -> isolatedBridge
		m.conn.AddRule(&nftables.Rule{
			Table: m.table,
			Chain: m.fwdChain,
			Exprs: []expr.Any{
				&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(other)},
				&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(isolatedBridge)},
				&expr.Verdict{Kind: expr.VerdictDrop},
			},
		})
		log.Printf("nat: isolation: drop %s <-> %s", isolatedBridge, other)
	}

	if err := m.conn.Flush(); err != nil {
		return fmt.Errorf("nftables flush isolation rules: %w", err)
	}
	return nil
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
