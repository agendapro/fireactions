package cache

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/rs/zerolog"
)

// guardChainName is the dedicated iptables chain fireactions installs to
// scope guest->host DROP rules. It is a var, not a const, so tests can swap
// in a disposable chain name (same convention as defaultMMDSAddress in
// agent/mmds) without touching any real host state.
var guardChainName = "FIREACTIONS-CACHE-GUARD"

// bridgeInterface is the CNI bridge name fireactions provisions for guest
// VMs. It is not operator-configurable in v1 — a real host mismatch (e.g. a
// renamed bridge) is caught by the manual verification checklist added in
// plan 01-04, not by any runtime check here.
const bridgeInterface = "fireactions-br0"

// guardProtocols returns both netfilter families the guard must cover. The
// listeners it protects are dual-stack under the default config (a host-less
// bind address like ":8080" binds [::], serving IPv4 and IPv6), and Linux
// auto-assigns an IPv6 link-local address to bridge interfaces — an
// IPv4-only guard would leave the control-plane ports reachable from guests
// over IPv6 link-local, bypassing D-07 entirely.
func guardProtocols() []iptables.Protocol {
	return []iptables.Protocol{iptables.ProtocolIPv4, iptables.ProtocolIPv6}
}

// iptablesWaitSeconds bounds how long iptables/ip6tables waits for the
// xtables lock (--wait) instead of failing immediately when another process
// on a shared host happens to hold it.
const iptablesWaitSeconds = 5

// protocolName renders a go-iptables Protocol for error messages.
func protocolName(proto iptables.Protocol) string {
	if proto == iptables.ProtocolIPv6 {
		return "IPv6"
	}

	return "IPv4"
}

// installGuardRules installs a dedicated FIREACTIONS-CACHE-GUARD chain in
// the filter table's INPUT chain — in both the IPv4 and IPv6 netfilter
// families — dropping guest->host traffic from iface to each port in ports.
// It is idempotent: calling it again (e.g. after an unclean shutdown left
// the chain behind) reuses the existing chain instead of duplicating rules.
//
// Any error here is the D-07 fail-closed signal: the caller must not bind
// its listener when this returns non-nil. A host without ip6tables fails
// closed too — a guard covering only one family is exactly the bypass this
// function exists to prevent — and a partial install (one family succeeded,
// the other failed) is rolled back so no netfilter state is left behind.
func installGuardRules(logger *zerolog.Logger, iface string, ports []string) error {
	if _, err := net.InterfaceByName(iface); err != nil {
		// The CNI bridge is created lazily on first VM boot (the same racing
		// behavior that motivated D-02). iptables accepts rules referencing
		// an interface that does not exist yet and applies them as soon as
		// it appears — failing here would make a cold-boot server
		// permanently cache-less, so we only warn and continue.
		logger.Warn().Str("interface", iface).Err(err).Msg("CNI bridge not present yet, guard rules installed ahead of it")
	}

	for _, proto := range guardProtocols() {
		if err := installGuardRulesFamily(proto, iface, ports); err != nil {
			if removeErr := removeGuardRules(); removeErr != nil {
				logger.Warn().Err(removeErr).Msg("Failed to roll back partially installed guard rules")
			}

			return err
		}
	}

	return nil
}

// installGuardRulesFamily installs the guard chain, its DROP rules, and the
// INPUT jump for a single netfilter family.
func installGuardRulesFamily(proto iptables.Protocol, iface string, ports []string) error {
	ipt, err := iptables.New(iptables.IPFamily(proto), iptables.Timeout(iptablesWaitSeconds))
	if err != nil {
		// fails closed: exec.LookPath("iptables"/"ip6tables") error surfaces here
		return fmt.Errorf("firewall: iptables unavailable (%s): %w", protocolName(proto), err)
	}

	exists, err := ipt.ChainExists("filter", guardChainName)
	if err != nil {
		return fmt.Errorf("firewall: checking chain (%s): %w", protocolName(proto), err)
	}

	if !exists {
		if err := ipt.NewChain("filter", guardChainName); err != nil {
			return fmt.Errorf("firewall: creating chain (%s): %w", protocolName(proto), err)
		}
	}

	for _, port := range ports {
		rule := []string{"-i", iface, "-p", "tcp", "--dport", port, "-j", "DROP"}
		if err := ipt.AppendUnique("filter", guardChainName, rule...); err != nil {
			return fmt.Errorf("firewall: adding drop rule for port %s (%s): %w", port, protocolName(proto), err)
		}
	}

	if err := reconcileStalePortRules(ipt, proto, ports); err != nil {
		return err
	}

	jumpRule := []string{"-j", guardChainName}
	if err := ipt.InsertUnique("filter", "INPUT", 1, jumpRule...); err != nil {
		return fmt.Errorf("firewall: installing jump rule (%s): %w", protocolName(proto), err)
	}

	return nil
}

// reconcileStalePortRules deletes guard-chain DROP rules whose destination
// port is no longer in ports. An unclean shutdown leaves the chain behind
// by design (SIGKILL bypasses removal), and a port change before the next
// start would otherwise leave the old ports' DROP rules in place forever —
// install only ever appends, and graceful removal deletes the whole chain
// only when shutdown is actually reached. Concretely harmful when an
// operator reassigns a previously guarded port to the cache's own
// bind_address: guests would silently be unable to reach the cache while
// the chain "looks correct" on inspection.
//
// Ordering is deliberate and fail-closed: the caller appends the desired
// rules before this reconciliation removes stale ones, so the chain never
// passes through a state weaker than either the old or the new config. The
// whole chain is never flushed for the same reason — a pre-existing INPUT
// jump from the unclean run would briefly point at an empty chain while
// guests may be live.
func reconcileStalePortRules(ipt *iptables.IPTables, proto iptables.Protocol, ports []string) error {
	desired := make(map[string]struct{}, len(ports))
	for _, port := range ports {
		desired[port] = struct{}{}
	}

	rules, err := ipt.List("filter", guardChainName)
	if err != nil {
		return fmt.Errorf("firewall: listing guard chain (%s): %w", protocolName(proto), err)
	}

	for _, rule := range rules {
		fields := strings.Fields(rule)
		if len(fields) < 3 || fields[0] != "-A" {
			// The "-N <chain>" header, or nothing rule-shaped.
			continue
		}

		port := dportValue(fields)
		if port == "" {
			continue
		}

		if _, ok := desired[port]; ok {
			continue
		}

		// fields[2:] is the listed rule's own rulespec (everything after
		// "-A <chain>"), so the deletion matches exactly what exists on the
		// host, whatever options iptables normalised into the listing.
		if err := ipt.Delete("filter", guardChainName, fields[2:]...); err != nil {
			return fmt.Errorf("firewall: removing stale drop rule for port %s (%s): %w", port, protocolName(proto), err)
		}
	}

	return nil
}

// dportValue extracts the value following "--dport" from a listed rule's
// fields, or "" when the rule has none.
func dportValue(fields []string) string {
	for i, field := range fields {
		if field == "--dport" && i+1 < len(fields) {
			return fields[i+1]
		}
	}

	return ""
}

// removeGuardRules removes the jump rule from INPUT and deletes the guard
// chain in both netfilter families. It is idempotent: calling it when
// nothing is installed returns nil. Deleting by chain name — rather than
// reconstructed rulespecs — is what makes cleanup survive a config change
// between start and stop. Both families are always attempted: a failure in
// one must not leave the other's chain behind.
func removeGuardRules() error {
	var errs []error

	for _, proto := range guardProtocols() {
		if err := removeGuardRulesFamily(proto); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// removeGuardRulesFamily removes the guard chain and its INPUT jump for a
// single netfilter family.
func removeGuardRulesFamily(proto iptables.Protocol) error {
	ipt, err := iptables.New(iptables.IPFamily(proto), iptables.Timeout(iptablesWaitSeconds))
	if err != nil {
		return fmt.Errorf("firewall: iptables unavailable (%s): %w", protocolName(proto), err)
	}

	_ = ipt.DeleteIfExists("filter", "INPUT", "-j", guardChainName)

	if err := ipt.ClearAndDeleteChain("filter", guardChainName); err != nil {
		return fmt.Errorf("firewall: removing chain (%s): %w", protocolName(proto), err)
	}

	return nil
}
