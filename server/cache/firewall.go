package cache

import (
	"fmt"
	"net"

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

// installGuardRules installs a dedicated FIREACTIONS-CACHE-GUARD chain in
// the filter table's INPUT chain, dropping guest->host traffic from iface to
// each port in ports. It is idempotent: calling it again (e.g. after an
// unclean shutdown left the chain behind) reuses the existing chain instead
// of duplicating rules.
//
// Any error here is the D-07 fail-closed signal: the caller must not bind
// its listener when this returns non-nil.
func installGuardRules(logger *zerolog.Logger, iface string, ports []string) error {
	ipt, err := iptables.New() // fails closed: exec.LookPath("iptables") error surfaces here
	if err != nil {
		return fmt.Errorf("firewall: iptables unavailable: %w", err)
	}

	if _, err := net.InterfaceByName(iface); err != nil {
		// The CNI bridge is created lazily on first VM boot (the same racing
		// behavior that motivated D-02). iptables accepts rules referencing
		// an interface that does not exist yet and applies them as soon as
		// it appears — failing here would make a cold-boot server
		// permanently cache-less, so we only warn and continue.
		logger.Warn().Str("interface", iface).Err(err).Msg("CNI bridge not present yet, guard rules installed ahead of it")
	}

	exists, err := ipt.ChainExists("filter", guardChainName)
	if err != nil {
		return fmt.Errorf("firewall: checking chain: %w", err)
	}

	if !exists {
		if err := ipt.NewChain("filter", guardChainName); err != nil {
			return fmt.Errorf("firewall: creating chain: %w", err)
		}
	}

	for _, port := range ports {
		rule := []string{"-i", iface, "-p", "tcp", "--dport", port, "-j", "DROP"}
		if err := ipt.AppendUnique("filter", guardChainName, rule...); err != nil {
			return fmt.Errorf("firewall: adding drop rule for port %s: %w", port, err)
		}
	}

	jumpRule := []string{"-j", guardChainName}
	if err := ipt.InsertUnique("filter", "INPUT", 1, jumpRule...); err != nil {
		return fmt.Errorf("firewall: installing jump rule: %w", err)
	}

	return nil
}

// removeGuardRules removes the jump rule from INPUT and deletes the guard
// chain. It is idempotent: calling it when nothing is installed returns nil.
// Deleting by chain name — rather than reconstructed rulespecs — is what
// makes cleanup survive a config change between start and stop.
func removeGuardRules() error {
	ipt, err := iptables.New()
	if err != nil {
		return fmt.Errorf("firewall: iptables unavailable: %w", err)
	}

	_ = ipt.DeleteIfExists("filter", "INPUT", "-j", guardChainName)

	return ipt.ClearAndDeleteChain("filter", guardChainName)
}
