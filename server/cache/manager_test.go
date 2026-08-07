package cache

import (
	"context"
	"io"
	"net/http"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func testConfig(t *testing.T) *Config {
	t.Helper()

	return &Config{
		Enabled:     true,
		BindAddress: "127.0.0.1:0",
		MaxSize:     "1GB",
		DataDir:     t.TempDir(),
	}
}

// requireRootLinux skips the calling test unless it is running as root on
// Linux — real firewall guard install requires CAP_NET_ADMIN, which is only
// exercised there (Pitfall 2's two-tier strategy, reused from firewall_test.go).
func requireRootLinux(t *testing.T) {
	t.Helper()

	if runtime.GOOS != "linux" {
		t.Skip("skipping: requires Linux (real iptables/netfilter)")
	}

	if os.Geteuid() != 0 {
		t.Skip("skipping: requires root (CAP_NET_ADMIN) to mutate iptables")
	}
}

func TestNew_NilConfig(t *testing.T) {
	m, err := New(nil, []string{"8080"})
	assert.Error(t, err)
	assert.Nil(t, m)
}

func TestNew_NoGuardedPorts(t *testing.T) {
	cfg := testConfig(t)

	m, err := New(cfg, nil)
	assert.Error(t, err)
	assert.Nil(t, m)

	m, err = New(cfg, []string{})
	assert.Error(t, err)
	assert.Nil(t, m)
}

func TestManager_Run_BeforeStart(t *testing.T) {
	cfg := testConfig(t)

	m, err := New(cfg, []string{"8080"})
	assert.NoError(t, err)
	assert.NotNil(t, m)

	err = m.Run(context.Background())
	assert.Error(t, err)
}

// TestManager_Start_FirewallGuardFailsClosed exercises D-07 on any host
// where the guard cannot be installed (macOS dev, unprivileged Linux CI):
// Start must return a non-nil error wrapping the firewall failure, and
// Addr() must be nil afterwards — the listener is never created. Shutdown
// after the failed Start must not panic and must not attempt guard removal.
func TestManager_Start_FirewallGuardFailsClosed(t *testing.T) {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		t.Skip("skipping fail-closed test: running as root on Linux, guard install is expected to succeed")
	}

	cfg := testConfig(t)

	m, err := New(cfg, []string{"8080", "8081"})
	assert.NoError(t, err)
	assert.NotNil(t, m)

	err = m.Start(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "firewall:")
	assert.Nil(t, m.Addr())

	assert.NotPanics(t, func() { m.Shutdown(context.Background()) })
	assert.False(t, m.guardInstalled)
}

// TestManager_Start_AddressInUse exercises the pre-existing listen-failure
// path, now gated behind guard install succeeding first — requires root and
// Linux. Verifies the guard is rolled back when net.Listen fails, leaving no
// host state behind.
func TestManager_Start_AddressInUse(t *testing.T) {
	requireRootLinux(t)

	originalChainName := guardChainName
	guardChainName = "FIREACTIONS-CACHE-GUARD-TEST"
	defer func() { guardChainName = originalChainName }()

	cfg := testConfig(t)

	m1, err := New(cfg, []string{"8080"})
	assert.NoError(t, err)

	err = m1.Start(context.Background())
	assert.NoError(t, err)
	defer m1.Shutdown(context.Background())

	cfg2 := &Config{
		Enabled:     true,
		BindAddress: m1.Addr().String(),
		MaxSize:     "1GB",
		DataDir:     t.TempDir(),
	}

	m2, err := New(cfg2, []string{"8080"})
	assert.NoError(t, err)

	err = m2.Start(context.Background())
	assert.Error(t, err)
	assert.Nil(t, m2.Addr())
	assert.False(t, m2.guardInstalled)
}

// TestManager_Lifecycle exercises the full happy path end-to-end (guard
// install, real listener, /healthz, /nope 404, graceful shutdown, guard
// removal) — requires root and Linux.
func TestManager_Lifecycle(t *testing.T) {
	requireRootLinux(t)

	originalChainName := guardChainName
	guardChainName = "FIREACTIONS-CACHE-GUARD-TEST"
	defer func() { guardChainName = originalChainName }()

	cfg := testConfig(t)

	m, err := New(cfg, []string{"8080", "8081"})
	assert.NoError(t, err)
	assert.NotNil(t, m)

	err = m.Start(context.Background())
	assert.NoError(t, err)
	assert.NotNil(t, m.Addr())
	assert.True(t, m.guardInstalled)

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- m.Run(context.Background())
	}()

	addr := "http://" + m.Addr().String()

	// Bounded wait for the listener to actually start accepting connections.
	assert.Eventually(t, func() bool {
		resp, getErr := http.Get(addr + "/healthz")
		if getErr != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()

		return resp.StatusCode == http.StatusOK
	}, 2*time.Second, 10*time.Millisecond)

	resp, err := http.Get(addr + "/healthz")
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.NoError(t, err)
	assert.Equal(t, "ok", string(body))

	resp404, err := http.Get(addr + "/nope")
	assert.NoError(t, err)
	_ = resp404.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp404.StatusCode)

	m.Shutdown(context.Background())
	assert.False(t, m.guardInstalled)

	select {
	case runErr := <-runErrCh:
		assert.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Run to return after Shutdown")
	}

	respAfterShutdown, err := http.Get(addr + "/healthz")
	if respAfterShutdown != nil {
		_ = respAfterShutdown.Body.Close()
	}
	assert.Error(t, err)
}
