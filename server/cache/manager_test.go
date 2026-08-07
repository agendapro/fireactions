package cache

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// newTestManagerWithStore builds a Manager and attaches a real Store
// directly, bypassing Start (and therefore the firewall guard) entirely.
// This is what keeps the handler-level tests below portable: they exercise
// buildMux/handleCacheGet's real routing and store I/O through a real
// httptest.Server, without requiring CAP_NET_ADMIN anywhere.
func newTestManagerWithStore(t *testing.T) (*Manager, *Store) {
	t.Helper()

	cfg := testConfig(t)

	m, err := New(cfg, []string{"8080"})
	require.NoError(t, err)

	store, err := NewStore(m.logger, cfg.DataDir)
	require.NoError(t, err)

	m.store = store

	return m, store
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

// TestHandleCacheGet_ServesContent exercises the happy path portably
// (no Start, no firewall guard, no privilege needed): content written
// into the store via Put is retrievable at GET /cache/{kind}/{key} with
// the exact bytes and a non-sniffable content type.
func TestHandleCacheGet_ServesContent(t *testing.T) {
	m, store := newTestManagerWithStore(t)

	content := []byte("hello from the cache")
	require.NoError(t, store.Put("manual", "hello.txt", strings.NewReader(string(content))))

	srv := httptest.NewServer(m.mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/cache/manual/hello.txt")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, content, body)
}

// TestHandleCacheGet_NestedKey exercises the {key...} wildcard capturing
// a multi-segment key.
func TestHandleCacheGet_NestedKey(t *testing.T) {
	m, store := newTestManagerWithStore(t)

	content := []byte(`{"Version":"v1.0.0"}`)
	require.NoError(t, store.Put("gomod", "example.com/mod/@v/list", strings.NewReader(string(content))))

	srv := httptest.NewServer(m.mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/cache/gomod/example.com/mod/@v/list")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, content, body)
}

func TestHandleCacheGet_MissingKey(t *testing.T) {
	m, _ := newTestManagerWithStore(t)

	srv := httptest.NewServer(m.mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/cache/manual/does-not-exist.txt")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestHandleCacheGet_InvalidKind asserts an invalid key returns 400, not
// 404 or 500 — an invalid kind fails Store.Path's kindPattern check.
func TestHandleCacheGet_InvalidKind(t *testing.T) {
	m, _ := newTestManagerWithStore(t)

	srv := httptest.NewServer(m.mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/cache/Bad/x")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestHandleCacheGet_NilStore asserts the handler refuses to dereference
// a nil store: New builds the mux before Start ever creates the store, so
// a request arriving in that window must not panic.
func TestHandleCacheGet_NilStore(t *testing.T) {
	cfg := testConfig(t)

	m, err := New(cfg, []string{"8080"})
	require.NoError(t, err)

	srv := httptest.NewServer(m.mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/cache/manual/hello.txt")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// TestHandleCacheGet_HealthzUnaffected is the 01-01 regression check: the
// new /cache/{kind}/{key...} route must not shadow or otherwise disturb
// /healthz.
func TestHandleCacheGet_HealthzUnaffected(t *testing.T) {
	cfg := testConfig(t)

	m, err := New(cfg, []string{"8080"})
	require.NoError(t, err)

	srv := httptest.NewServer(m.mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestManager_CacheGet_RawEscapedTraversalRefused sends a raw,
// percent-encoded traversal attempt directly onto the socket. Both
// http.Client and http.ServeMux's path-cleaning normalise a plain ".."
// before a handler ever sees it, so a request built through either would
// never actually exercise Store.Path's own containment check — the
// escaped form (../.. spelled with %2F in place of the real slash) is
// what reaches the key parser unmodified, because Go's ServeMux (1.22+)
// matches wildcards against the escaped path. Requires root+Linux: this
// is the only way to get a real bound listener, via Start's firewall
// guard.
func TestManager_CacheGet_RawEscapedTraversalRefused(t *testing.T) {
	requireRootLinux(t)

	originalChainName := guardChainName
	guardChainName = "FIREACTIONS-CACHE-GUARD-TEST"
	defer func() { guardChainName = originalChainName }()

	cfg := testConfig(t)

	m, err := New(cfg, []string{"8080"})
	require.NoError(t, err)

	err = m.Start(context.Background())
	require.NoError(t, err)
	defer m.Shutdown(context.Background())

	go func() { _ = m.Run(context.Background()) }()

	require.NoError(t, m.store.Put("manual", "hello.txt", strings.NewReader("safe content")))

	addr := m.Addr().String()

	assert.Eventually(t, func() bool {
		conn, dialErr := net.Dial("tcp", addr)
		if dialErr != nil {
			return false
		}
		_ = conn.Close()

		return true
	}, 2*time.Second, 10*time.Millisecond)

	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	req := "GET /cache/manual/..%2F..%2F..%2Fetc%2Fpasswd HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Connection: close\r\n\r\n"

	_, err = conn.Write([]byte(req))
	require.NoError(t, err)

	respBytes, err := io.ReadAll(conn)
	require.NoError(t, err)

	resp := string(respBytes)
	statusLine := strings.SplitN(resp, "\r\n", 2)[0]

	assert.NotContains(t, statusLine, " 200 ")
	assert.NotContains(t, resp, "root:")
}

// TestManager_RestartPersistence is D-10 at the HTTP layer: content
// written before a graceful Shutdown is still served by a fresh Manager
// started over the same data_dir. Requires root+Linux for the same
// reason as the traversal test above.
func TestManager_RestartPersistence(t *testing.T) {
	requireRootLinux(t)

	originalChainName := guardChainName
	guardChainName = "FIREACTIONS-CACHE-GUARD-TEST"
	defer func() { guardChainName = originalChainName }()

	dataDir := t.TempDir()

	cfg1 := &Config{Enabled: true, BindAddress: "127.0.0.1:0", MaxSize: "1GB", DataDir: dataDir}

	m1, err := New(cfg1, []string{"8080"})
	require.NoError(t, err)

	err = m1.Start(context.Background())
	require.NoError(t, err)

	require.NoError(t, m1.store.Put("gomod", "example.com/mod/@v/v1.0.0.info", strings.NewReader("v1 content")))

	m1.Shutdown(context.Background())

	cfg2 := &Config{Enabled: true, BindAddress: "127.0.0.1:0", MaxSize: "1GB", DataDir: dataDir}

	m2, err := New(cfg2, []string{"8080"})
	require.NoError(t, err)

	err = m2.Start(context.Background())
	require.NoError(t, err)
	defer m2.Shutdown(context.Background())

	go func() { _ = m2.Run(context.Background()) }()

	addr := "http://" + m2.Addr().String()

	assert.Eventually(t, func() bool {
		resp, getErr := http.Get(addr + "/healthz")
		if getErr != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()

		return resp.StatusCode == http.StatusOK
	}, 2*time.Second, 10*time.Millisecond)

	resp, err := http.Get(addr + "/cache/gomod/example.com/mod/@v/v1.0.0.info")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "v1 content", string(body))
}

func TestNew_DefaultSweepInterval(t *testing.T) {
	cfg := testConfig(t)

	m, err := New(cfg, []string{"8080"})
	require.NoError(t, err)
	assert.Equal(t, defaultSweepInterval, m.sweepInterval)
}

func TestNew_WithSweepIntervalOption(t *testing.T) {
	cfg := testConfig(t)

	m, err := New(cfg, []string{"8080"}, WithSweepInterval(50*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, 50*time.Millisecond, m.sweepInterval)
}

// TestManager_Start_InvalidMaxSize is portable: max_size is parsed before
// any I/O (store creation, firewall install, listen), so a garbage value
// fails fast with a wrapped error and never touches the filesystem.
func TestManager_Start_InvalidMaxSize(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxSize = "not-a-size"

	m, err := New(cfg, []string{"8080"})
	require.NoError(t, err)

	err = m.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing max_size")
	assert.Nil(t, m.Addr())
}

// TestManager_Start_ZeroMaxSizeFailsClosed is the WR-01 regression check,
// portable on any host: a directly constructed Config with max_size "0"
// bypasses config-load validation, and Start's belt-and-braces re-validation
// must reject it — otherwise every sweep would run with maxBytes=0 and
// silently evict the entire cache while the listener keeps serving 404s.
func TestManager_Start_ZeroMaxSizeFailsClosed(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxSize = "0"

	m, err := New(cfg, []string{"8080"})
	require.NoError(t, err)

	err = m.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_size must be greater than zero")
	assert.Nil(t, m.Addr())
}

// TestManager_Start_EmptyBindAddressFailsClosed is the WR-02 half of the
// same belt-and-braces check, portable on any host: an enabled Config with
// an empty bind_address must never reach net.Listen("tcp", ""), which would
// bind all interfaces dual-stack on a random port.
func TestManager_Start_EmptyBindAddressFailsClosed(t *testing.T) {
	cfg := testConfig(t)
	cfg.BindAddress = ""

	m, err := New(cfg, []string{"8080"})
	require.NoError(t, err)

	err = m.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bind_address")
	assert.Nil(t, m.Addr())
}

// TestManager_Start_ParsesMaxSizeBytesBeforeGuardStep is portable on any
// host: max_size parsing happens before the firewall guard step, so
// maxSizeBytes is set regardless of whether the guard install that follows
// it succeeds (root+Linux) or fails closed (everywhere else). The chain
// name is overridden and the manager shut down unconditionally so a
// root+Linux run — where Start actually succeeds end to end — never leaves
// the production guard chain installed on the host running the test.
func TestManager_Start_ParsesMaxSizeBytesBeforeGuardStep(t *testing.T) {
	originalChainName := guardChainName
	guardChainName = "FIREACTIONS-CACHE-GUARD-TEST"
	defer func() { guardChainName = originalChainName }()

	cfg := testConfig(t)
	cfg.MaxSize = "2GB"

	m, err := New(cfg, []string{"8080"})
	require.NoError(t, err)

	_ = m.Start(context.Background())
	defer m.Shutdown(context.Background())

	assert.Equal(t, int64(2*1024*1024*1024), m.maxSizeBytes)
}

// TestManager_Run_SweepDrivesUsageUnderCap exercises the sweep really
// running from Run (not only when Store.Sweep is called directly): content
// seeded over the cap is driven back down to the low-water mark within a
// bounded wait. Requires root+Linux for a real Start.
func TestManager_Run_SweepDrivesUsageUnderCap(t *testing.T) {
	requireRootLinux(t)

	originalChainName := guardChainName
	guardChainName = "FIREACTIONS-CACHE-GUARD-TEST"
	defer func() { guardChainName = originalChainName }()

	cfg := testConfig(t)
	cfg.MaxSize = "5000" // low-water target: 4500 bytes

	m, err := New(cfg, []string{"8080"}, WithSweepInterval(50*time.Millisecond))
	require.NoError(t, err)

	err = m.Start(context.Background())
	require.NoError(t, err)

	for i := range 10 {
		require.NoError(t, m.store.Put("gomod", fmt.Sprintf("file-%02d", i), strings.NewReader(strings.Repeat("a", 1000))))
	}

	runErrCh := make(chan error, 1)

	go func() {
		runErrCh <- m.Run(context.Background())
	}()

	assert.Eventually(t, func() bool {
		usage, usageErr := m.store.Usage()

		return usageErr == nil && usage <= 4500
	}, 3*time.Second, 20*time.Millisecond)

	m.Shutdown(context.Background())

	select {
	case runErr := <-runErrCh:
		assert.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Run to return after Shutdown")
	}
}

// TestManager_Run_ImmediateSweepBeforeFirstTick uses a deliberately long
// sweep interval: if the sweep only ran on ticks, usage would never drop
// within this test's bounded wait, so a passing test proves the immediate
// pre-loop sweep runs. Requires root+Linux for a real Start.
func TestManager_Run_ImmediateSweepBeforeFirstTick(t *testing.T) {
	requireRootLinux(t)

	originalChainName := guardChainName
	guardChainName = "FIREACTIONS-CACHE-GUARD-TEST"
	defer func() { guardChainName = originalChainName }()

	cfg := testConfig(t)
	cfg.MaxSize = "5000"

	m, err := New(cfg, []string{"8080"}, WithSweepInterval(time.Hour))
	require.NoError(t, err)

	err = m.Start(context.Background())
	require.NoError(t, err)

	defer m.Shutdown(context.Background())

	for i := range 10 {
		require.NoError(t, m.store.Put("gomod", fmt.Sprintf("file-%02d", i), strings.NewReader(strings.Repeat("a", 1000))))
	}

	go func() { _ = m.Run(context.Background()) }()

	assert.Eventually(t, func() bool {
		usage, usageErr := m.store.Usage()

		return usageErr == nil && usage <= 4500
	}, 3*time.Second, 20*time.Millisecond)
}

// TestManager_Run_SweepGoroutineStopsBeforeReturn is the goroutine-quiescent
// shutdown guarantee: after Run returns, nothing is still touching
// data_dir. Proven by writing a file over the (tiny) cap after Run returns
// and confirming it is never evicted. Requires root+Linux for a real Start.
func TestManager_Run_SweepGoroutineStopsBeforeReturn(t *testing.T) {
	requireRootLinux(t)

	originalChainName := guardChainName
	guardChainName = "FIREACTIONS-CACHE-GUARD-TEST"
	defer func() { guardChainName = originalChainName }()

	cfg := testConfig(t)
	cfg.MaxSize = "1000" // tiny cap: any lingering sweep would evict a fresh write

	m, err := New(cfg, []string{"8080"}, WithSweepInterval(10*time.Millisecond))
	require.NoError(t, err)

	err = m.Start(context.Background())
	require.NoError(t, err)

	runErrCh := make(chan error, 1)

	go func() {
		runErrCh <- m.Run(context.Background())
	}()

	// Let at least one sweep tick happen before shutting down.
	time.Sleep(50 * time.Millisecond)

	m.Shutdown(context.Background())

	select {
	case runErr := <-runErrCh:
		assert.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Run to return after Shutdown")
	}

	require.NoError(t, m.store.Put("gomod", "post-shutdown", strings.NewReader(strings.Repeat("a", 2000))))

	time.Sleep(100 * time.Millisecond)

	full, err := m.store.Path("gomod", "post-shutdown")
	require.NoError(t, err)

	_, statErr := os.Stat(full)
	assert.NoError(t, statErr,
		"expected file written after Run returned to survive — sweep goroutine must be fully stopped")
}

// TestManager_Run_SweepErrorDoesNotStopServing removes data_dir out from
// under a running store so every sweep attempt errors: Run must log and
// continue, never take the listener down. Requires root+Linux for a real
// Start.
func TestManager_Run_SweepErrorDoesNotStopServing(t *testing.T) {
	requireRootLinux(t)

	originalChainName := guardChainName
	guardChainName = "FIREACTIONS-CACHE-GUARD-TEST"
	defer func() { guardChainName = originalChainName }()

	cfg := testConfig(t)

	m, err := New(cfg, []string{"8080"}, WithSweepInterval(20*time.Millisecond))
	require.NoError(t, err)

	err = m.Start(context.Background())
	require.NoError(t, err)

	require.NoError(t, os.RemoveAll(cfg.DataDir))

	runErrCh := make(chan error, 1)

	go func() {
		runErrCh <- m.Run(context.Background())
	}()

	addr := "http://" + m.Addr().String()

	assert.Eventually(t, func() bool {
		resp, getErr := http.Get(addr + "/healthz")
		if getErr != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()

		return resp.StatusCode == http.StatusOK
	}, 500*time.Millisecond, 10*time.Millisecond)

	// Give the failing sweep several ticks to prove it doesn't take down Run.
	time.Sleep(150 * time.Millisecond)

	resp, err := http.Get(addr + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	m.Shutdown(context.Background())

	select {
	case runErr := <-runErrCh:
		assert.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Run to return after Shutdown")
	}
}

// TestManager_Metrics_CacheUp is 1 while running and 0 after Shutdown.
// Requires root+Linux for a real Start.
func TestManager_Metrics_CacheUp(t *testing.T) {
	requireRootLinux(t)

	originalChainName := guardChainName
	guardChainName = "FIREACTIONS-CACHE-GUARD-TEST"
	defer func() { guardChainName = originalChainName }()

	cfg := testConfig(t)

	m, err := New(cfg, []string{"8080"})
	require.NoError(t, err)

	err = m.Start(context.Background())
	require.NoError(t, err)

	assert.InDelta(t, float64(1), testutil.ToFloat64(metricCacheUp), 0)

	m.Shutdown(context.Background())

	assert.InDelta(t, float64(0), testutil.ToFloat64(metricCacheUp), 0)
}

// TestManager_Metrics_DiskBytesUsedReflectsPostSweepUsage. Requires
// root+Linux for a real Start.
func TestManager_Metrics_DiskBytesUsedReflectsPostSweepUsage(t *testing.T) {
	requireRootLinux(t)

	originalChainName := guardChainName
	guardChainName = "FIREACTIONS-CACHE-GUARD-TEST"
	defer func() { guardChainName = originalChainName }()

	cfg := testConfig(t)
	cfg.MaxSize = "1GB" // large cap: nothing evicted, disk_bytes_used reflects the raw write

	m, err := New(cfg, []string{"8080"}, WithSweepInterval(30*time.Millisecond))
	require.NoError(t, err)

	err = m.Start(context.Background())
	require.NoError(t, err)

	defer m.Shutdown(context.Background())

	require.NoError(t, m.store.Put("gomod", "a", strings.NewReader(strings.Repeat("a", 1234))))

	go func() { _ = m.Run(context.Background()) }()

	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(metricCacheDiskBytesUsed) == 1234
	}, 3*time.Second, 20*time.Millisecond)
}

// TestManager_Metrics_EvictionsTotalIncreasesBySweepEvictions asserts a
// delta, not an absolute value: the counter is a package-level collector
// shared across sibling tests. Requires root+Linux for a real Start.
func TestManager_Metrics_EvictionsTotalIncreasesBySweepEvictions(t *testing.T) {
	requireRootLinux(t)

	originalChainName := guardChainName
	guardChainName = "FIREACTIONS-CACHE-GUARD-TEST"
	defer func() { guardChainName = originalChainName }()

	cfg := testConfig(t)
	cfg.MaxSize = "2000" // low-water target: 1800 bytes

	m, err := New(cfg, []string{"8080"}, WithSweepInterval(30*time.Millisecond))
	require.NoError(t, err)

	err = m.Start(context.Background())
	require.NoError(t, err)

	defer m.Shutdown(context.Background())

	before := testutil.ToFloat64(metricCacheEvictionsTotal)

	for i := range 5 {
		require.NoError(t, m.store.Put("gomod", fmt.Sprintf("f-%d", i), strings.NewReader(strings.Repeat("a", 1000))))
	}

	go func() { _ = m.Run(context.Background()) }()

	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(metricCacheEvictionsTotal) > before
	}, 3*time.Second, 20*time.Millisecond)
}
