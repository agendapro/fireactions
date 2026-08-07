package cache

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/coreos/go-iptables/iptables"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFirewall_InstallGuardRules_FailsClosed exercises the D-07 fail-closed
// branch. On macOS, iptables.New() fails outright (no binary, exec.LookPath
// error). On unprivileged Linux, the binary exists but the first real rule
// operation fails with a permission error. Both are the same fail-closed
// branch, so this test is portable and requires no privilege.
func TestFirewall_InstallGuardRules_FailsClosed(t *testing.T) {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		t.Skip("skipping fail-closed test: running as root on Linux, install is expected to succeed")
	}

	logger := zerolog.Nop()
	err := installGuardRules(&logger, "lo", []string{"65500"})
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "firewall: "), "expected error to be wrapped with the firewall: prefix, got: %v", err)
}

// TestFirewall_GuardRules_InstallIdempotentAndRemove exercises the happy
// path: real chain creation, idempotent reinstall, and clean removal.
// Requires root and Linux (real netfilter), so it is skipped everywhere
// else.
func TestFirewall_GuardRules_InstallIdempotentAndRemove(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping: requires Linux (real iptables/netfilter)")
	}
	if os.Geteuid() != 0 {
		t.Skip("skipping: requires root (CAP_NET_ADMIN) to mutate iptables")
	}

	originalChainName := guardChainName
	guardChainName = "FIREACTIONS-CACHE-GUARD-TEST"
	defer func() { guardChainName = originalChainName }()
	defer func() { _ = removeGuardRules() }()

	logger := zerolog.Nop()
	iface := "lo"
	port := "65500"

	err := installGuardRules(&logger, iface, []string{port})
	require.NoError(t, err)

	ipt, err := iptables.New()
	require.NoError(t, err)

	dropRule := []string{"-i", iface, "-p", "tcp", "--dport", port, "-j", "DROP"}
	dropExists, err := ipt.Exists("filter", guardChainName, dropRule...)
	require.NoError(t, err)
	assert.True(t, dropExists, "expected DROP rule to exist in guard chain")

	jumpExists, err := ipt.Exists("filter", "INPUT", "-j", guardChainName)
	require.NoError(t, err)
	assert.True(t, jumpExists, "expected jump rule to exist in INPUT")

	// Re-installing must not duplicate rules (SIGKILL-recovery idempotency).
	err = installGuardRules(&logger, iface, []string{port})
	require.NoError(t, err)

	rules, err := ipt.List("filter", guardChainName)
	require.NoError(t, err)
	dropRuleCount := 0
	for _, r := range rules {
		if strings.Contains(r, "DROP") && strings.Contains(r, port) {
			dropRuleCount++
		}
	}
	assert.Equal(t, 1, dropRuleCount, "expected exactly one DROP rule for the port after reinstall")

	inputRules, err := ipt.List("filter", "INPUT")
	require.NoError(t, err)
	jumpRuleCount := 0
	for _, r := range inputRules {
		if strings.Contains(r, guardChainName) {
			jumpRuleCount++
		}
	}
	assert.Equal(t, 1, jumpRuleCount, "expected exactly one jump rule in INPUT after reinstall")

	err = removeGuardRules()
	require.NoError(t, err)

	chainExists, err := ipt.ChainExists("filter", guardChainName)
	require.NoError(t, err)
	assert.False(t, chainExists, "expected guard chain to be removed")

	// Once the chain is gone, ipt.Exists("filter", "INPUT", "-j",
	// guardChainName) can no longer be used to check for the jump rule's
	// absence: iptables -C must resolve the jump target to validate the
	// rulespec, so it errors with "chain does not exist" (exit 2) instead of
	// the ordinary "rule not found" (exit 1) once that target chain is gone.
	// List + substring search sidesteps this rulespec-validation requirement.
	inputRulesAfterRemove, err := ipt.List("filter", "INPUT")
	require.NoError(t, err)
	for _, r := range inputRulesAfterRemove {
		assert.NotContains(t, r, guardChainName, "expected jump rule to be removed from INPUT")
	}

	// Removing again is idempotent.
	err = removeGuardRules()
	assert.NoError(t, err)
}
