package wifi

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

const apFirewallChain = "CAPSULAD_AP_PORTAL"

// installAPFirewall keeps the provisioning network from becoming a second path
// to the daemon's administrator surfaces. It is deliberately a dedicated
// chain: CAPSULAD_AP_PORTAL only permits DHCP, DNS and the portal port on the AP
// interface and drops every other inbound packet. The rules are best-effort:
// if iptables is unavailable the portal is still bound only to the AP address,
// and the daemon's Web UI default remains loopback. Cleanup removes the jump
// and chain so station mode is not filtered.
func (m *linuxManager) installAPFirewall(ctx context.Context) (func(context.Context), error) {
	if m.tools.iptables == "" {
		return nil, fmt.Errorf("iptables is not installed")
	}
	ipt := func(args ...string) {
		_, _ = m.run(ctx, 5*time.Second, m.tools.iptables, args...)
	}
	// Start from a known chain even after an unclean daemon exit.
	ipt("-N", apFirewallChain)
	ipt("-F", apFirewallChain)
	ipt("-A", apFirewallChain, "-i", m.iface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN")
	ipt("-A", apFirewallChain, "-i", m.iface, "-p", "udp", "--dport", "53", "-j", "RETURN")
	ipt("-A", apFirewallChain, "-i", m.iface, "-p", "tcp", "--dport", "53", "-j", "RETURN")
	ipt("-A", apFirewallChain, "-i", m.iface, "-p", "udp", "--dport", "67", "-j", "RETURN")
	ipt("-A", apFirewallChain, "-i", m.iface, "-p", "tcp", "--dport", strconv.Itoa(m.cfg.PortalPort), "-j", "RETURN")
	ipt("-A", apFirewallChain, "-i", m.iface, "-j", "DROP")
	// Remove an older jump before inserting exactly one. Two jumps would make
	// cleanup leave a stale reference.
	ipt("-D", "INPUT", "-j", apFirewallChain)
	if _, err := m.run(ctx, 5*time.Second, m.tools.iptables, "-I", "INPUT", "1", "-j", apFirewallChain); err != nil {
		return nil, fmt.Errorf("install AP portal firewall: %w", err)
	}
	return func(cleanup context.Context) {
		_, _ = m.run(cleanup, 5*time.Second, m.tools.iptables, "-D", "INPUT", "-j", apFirewallChain)
		_, _ = m.run(cleanup, 5*time.Second, m.tools.iptables, "-F", apFirewallChain)
		_, _ = m.run(cleanup, 5*time.Second, m.tools.iptables, "-X", apFirewallChain)
	}, nil
}
