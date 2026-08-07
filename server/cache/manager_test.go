package cache

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

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
