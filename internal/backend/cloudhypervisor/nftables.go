//go:build linux

package cloudhypervisor

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"syscall"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
)

// ifNameLen is the kernel's interface-name buffer size (IFNAMSIZ): nft compares
// iifname/oifname against a 16-byte, NUL-padded name.
const ifNameLen = 16

// nftProbe verifies nf_tables is usable on this host, discriminating the two
// failure modes (Decision 8): a non-root host fails ListTables with EPERM ("needs
// root"), while a kernel without nf_tables fails with EOPNOTSUPP/ENOENT. It runs
// AFTER the netlink root probe (the bridge create), so a non-root user already saw
// one "needs root" error and never reaches a second; the discrimination keeps the
// message coherent if a sandbox somehow lets the bridge through but not nft.
func nftProbe() error {
	c, err := nftables.New()
	if err != nil {
		return classifyNFTErr(err)
	}
	if _, err := c.ListTables(); err != nil {
		return classifyNFTErr(err)
	}
	return nil
}

// installFirewall builds the per-bridge nf_tables firewall: an `ip`-family table
// (IPv4-only — Decision 2) named nftTableName(bridge), holding a NAT postrouting
// chain that masquerades the subnet's egress (Decision 6) and a filter forward
// chain (policy accept — we shadow nothing) carrying one subnet-scoped drop of
// unsolicited inbound (Decision 5). Everything is added on one connection and
// committed with a single Flush.
func installFirewall(bridge string, subnet netip.Prefix) error {
	c, err := nftables.New()
	if err != nil {
		return classifyNFTErr(err)
	}

	name := nftTableName(bridge)
	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: name})

	// nat/postrouting: ip saddr <subnet> oifname != <bridge> masquerade.
	postrouting := c.AddChain(&nftables.Chain{
		Name:     "postrouting",
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
	})
	c.AddRule(&nftables.Rule{Table: table, Chain: postrouting, Exprs: masqueradeExprs(subnet, bridge)})

	// filter/forward: policy ACCEPT (do NOT clamp — accept is non-terminal, so this
	// shadows nothing and breaks no other bridge), with the one protective drop.
	policyAccept := nftables.ChainPolicyAccept
	forward := c.AddChain(&nftables.Chain{
		Name:     "forward",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
		Policy:   &policyAccept,
	})
	c.AddRule(&nftables.Rule{Table: table, Chain: forward, Exprs: forwardDropExprs(subnet, bridge)})

	if err := c.Flush(); err != nil {
		return classifyNFTErr(err)
	}

	// google/nftables footgun (issue #170): Flush can return nil while the kernel
	// silently drops a rule whose expression it doesn't support. Read the ruleset
	// back and fail loudly unless the table, both chains, and both rules survived.
	return verifyFirewall(name)
}

// removeFirewall deletes the bridge's nft table whole (chains and rules go with
// it — Decision 9). It is list-then-delete and idempotent: a table already gone is
// not an error, and a racing concurrent delete (ENOENT on flush) is swallowed.
func removeFirewall(bridge string) error {
	name := nftTableName(bridge)
	c, err := nftables.New()
	if err != nil {
		return classifyNFTErr(err)
	}
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return classifyNFTErr(err)
	}

	found := findTable(tables, name)
	if found == nil {
		return nil // already gone
	}

	c.DelTable(found)
	if err := c.Flush(); err != nil && !errors.Is(err, syscall.ENOENT) {
		return classifyNFTErr(err)
	}
	return nil
}

// verifyFirewall reads the just-written ruleset back and confirms it survived the
// kernel: the table is present, the masquerade rule carries an expr.Masq, and the
// forward rule carries its expr.Ct match (a verdict does not round-trip as
// expr.Verdict — it returns as an immediate with the verdict stripped — so the ct
// match is the load-bearing proof that the protective rule, with its full
// expression list, was not silently dropped). Counters the issue-#170 footgun.
func verifyFirewall(name string) error {
	c, err := nftables.New()
	if err != nil {
		return classifyNFTErr(err)
	}
	tables, err := c.ListTablesOfFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return classifyNFTErr(err)
	}

	table := findTable(tables, name)
	if table == nil {
		return fmt.Errorf("nft table %s missing after install (kernel rejected it)", name)
	}

	if err := requireRuleExpr(c, table, "postrouting", "masquerade", func(e expr.Any) bool {
		_, ok := e.(*expr.Masq)
		return ok
	}); err != nil {
		return err
	}
	return requireRuleExpr(c, table, "forward", "forward drop", func(e expr.Any) bool {
		_, ok := e.(*expr.Ct)
		return ok
	})
}

// requireRuleExpr fails unless the named chain holds a rule with an expression
// matching pred — an empty chain means the kernel dropped the rule.
func requireRuleExpr(c *nftables.Conn, table *nftables.Table, chain, what string, pred func(expr.Any) bool) error {
	rules, err := c.GetRules(table, &nftables.Chain{Name: chain, Table: table})
	if err != nil {
		return fmt.Errorf("read back %s chain: %w", chain, err)
	}
	for _, r := range rules {
		if slices.ContainsFunc(r.Exprs, pred) {
			return nil
		}
	}
	return fmt.Errorf("nft %s rule missing after install (kernel rejected it)", what)
}

// masqueradeExprs renders `ip saddr <subnet> oifname != <bridge> masquerade` — the
// nft equivalent of the old `-t nat -I POSTROUTING -s <subnet> ! -o <bridge> -j
// MASQUERADE`. The source address is masked to the subnet and compared to the
// network address; the masquerade applies only when leaving a non-bridge NIC.
func masqueradeExprs(subnet netip.Prefix, bridge string) []expr.Any {
	network := subnet.Masked().Addr().As4()
	mask := net.CIDRMask(subnet.Bits(), 32)
	return []expr.Any{
		// ip saddr (network header offset 12) & mask == network
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: network[:]},
		// oifname != bridge
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: ifName(bridge)},
		// masquerade
		&expr.Masq{},
	}
}

// forwardDropExprs renders `ct state new iifname != <bridge> ip daddr <subnet>
// drop` (Decision 5): it drops a NEW connection arriving on any interface other
// than our bridge whose destination is our guest subnet — i.e. unsolicited inbound
// from the host LAN — while letting VM-originated traffic and established/related
// returns through (they are not state `new`, and bridge-ingress is excluded).
func forwardDropExprs(subnet netip.Prefix, bridge string) []expr.Any {
	network := subnet.Masked().Addr().As4()
	mask := net.CIDRMask(subnet.Bits(), 32)
	ctStateNew := binaryutil.NativeEndian.PutUint32(expr.CtStateBitNEW)
	return []expr.Any{
		// ct state & NEW != 0
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: ctStateNew, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
		// iifname != bridge
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: ifName(bridge)},
		// ip daddr (network header offset 16) & mask == network
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: network[:]},
		// drop
		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}

// ifName renders an interface name as the kernel's fixed-width, NUL-padded buffer
// for an iifname/oifname comparison.
func ifName(name string) []byte {
	b := make([]byte, ifNameLen)
	copy(b, name)
	return b
}

// findTable returns the table with the given name from a ListTables result, or
// nil if absent.
func findTable(tables []*nftables.Table, name string) *nftables.Table {
	i := slices.IndexFunc(tables, func(t *nftables.Table) bool { return t.Name == name })
	if i < 0 {
		return nil
	}
	return tables[i]
}
