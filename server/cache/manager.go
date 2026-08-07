package cache

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
)

// Manager manages the lifecycle of the cache component: the single
// path-routed HTTP listener that serves all cache kinds, backed by a
// bounded disk store and guarded by a dedicated firewall chain that
// blocks guest access to fireactions' own ports.
type Manager struct {
	config         *Config
	guardedPorts   []string
	logger         *zerolog.Logger
	httpServer     *http.Server
	listener       net.Listener
	mux            *http.ServeMux
	store          *Store
	guardInstalled bool
	sweepInterval  time.Duration
	maxSizeBytes   int64
}

// Opt is a functional option for Manager.
type Opt func(m *Manager)

// WithLogger sets the logger for the Manager.
func WithLogger(logger *zerolog.Logger) Opt {
	f := func(m *Manager) {
		m.logger = logger
	}

	return f
}

// defaultSweepInterval is how often the background sweep runs when
// WithSweepInterval is not supplied. It is intentionally not exposed in the
// YAML schema: D-09 accepts temporary overshoot between sweeps, and adding a
// config field would widen the operator surface for no demonstrated need.
const defaultSweepInterval = 5 * time.Minute

// WithSweepInterval overrides the default sweep interval
// (defaultSweepInterval). Tests use this to drive the sweep loop on a short
// interval instead of waiting for the real 5-minute default.
func WithSweepInterval(d time.Duration) Opt {
	f := func(m *Manager) {
		m.sweepInterval = d
	}

	return f
}

// New creates a new Manager. It performs no I/O — fallible setup (firewall
// install, binding the listener) happens in Start.
//
// guardedPorts must be non-empty: a Manager that guards nothing must be
// unconstructible. Start uses guardedPorts to install a firewall rule that
// blocks guest access to fireactions' own gRPC/metrics ports from the CNI
// bridge before the cache listener starts.
func New(config *Config, guardedPorts []string, opts ...Opt) (*Manager, error) {
	if config == nil {
		return nil, fmt.Errorf("cache: config is required")
	}

	if len(guardedPorts) == 0 {
		return nil, fmt.Errorf("cache: at least one guarded port is required")
	}

	logger := zerolog.Nop()

	m := &Manager{
		config:         config,
		guardedPorts:   guardedPorts,
		logger:         &logger,
		httpServer:     nil,
		listener:       nil,
		mux:            nil,
		store:          nil,
		guardInstalled: false,
		sweepInterval:  defaultSweepInterval,
		maxSizeBytes:   0,
	}

	for _, opt := range opts {
		opt(m)
	}

	m.mux = m.buildMux()
	m.httpServer = &http.Server{
		Handler:           m.mux,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		// WriteTimeout is deliberately left at zero: later phases stream
		// multi-hundred-megabyte cache artifacts through this listener, and a
		// write deadline would truncate them mid-transfer.
	}

	return m, nil
}

// Start performs the fallible setup required before the cache can serve
// traffic. The config is re-validated first — it is already validated at
// load time, so this is a belt-and-braces check for callers that construct
// a Config directly, covering every invariant (bind_address shape, max_size
// positivity) rather than a partial subset — and max_size is parsed and
// cached off the hot path before any I/O happens. The disk store is created
// next — it is the cheapest and least destructive I/O failure to check, so
// a bad data_dir never causes netfilter mutations on the host. The firewall
// guard is installed after that and gates everything following it: if it
// cannot be installed, the cache never binds its listener (D-07).
func (m *Manager) Start(_ context.Context) error {
	if err := m.config.Validate(); err != nil {
		return fmt.Errorf("cache: invalid config: %w", err)
	}

	maxSizeBytes, err := m.config.MaxSizeBytes()
	if err != nil {
		return fmt.Errorf("cache: parsing max_size: %w", err)
	}

	m.maxSizeBytes = maxSizeBytes

	store, err := NewStore(m.logger, m.config.DataDir)
	if err != nil {
		return fmt.Errorf("cache: creating store: %w", err)
	}

	m.store = store

	if err := installGuardRules(m.logger, bridgeInterface, m.guardedPorts); err != nil {
		return fmt.Errorf("cache: installing firewall guard: %w", err)
	}

	m.guardInstalled = true

	listener, err := net.Listen("tcp", m.config.BindAddress)
	if err != nil {
		// A start that did not complete must not leave netfilter state
		// behind on a host fireactions doesn't own exclusively.
		if removeErr := removeGuardRules(); removeErr != nil {
			m.logger.Warn().Err(removeErr).Msg("Failed to remove firewall guard after failed listen")
		}

		m.guardInstalled = false

		return fmt.Errorf("cache: listen on %s: %w", m.config.BindAddress, err)
	}

	m.listener = listener
	m.logger.Info().Str("address", listener.Addr().String()).Msg("Cache listening")

	metricCacheUp.Set(1)

	return nil
}

// Addr returns the address the cache is listening on, or nil if Start has
// not been called yet, or failed.
func (m *Manager) Addr() net.Addr {
	if m.listener == nil {
		return nil
	}

	return m.listener.Addr()
}

// Run serves the cache HTTP listener and blocks until it is shut down. It
// also drives the periodic sweep that keeps disk usage under the operator's
// max_size cap: one sweep runs immediately, then one on every tick of
// sweepInterval, until the listener is drained. Run does not return until
// the sweep goroutine has stopped, so a caller can rely on Run returning as
// the signal that nothing is still touching data_dir. It returns an error
// if Start has not run yet.
func (m *Manager) Run(ctx context.Context) error {
	if m.listener == nil {
		return fmt.Errorf("cache: Run called before Start")
	}

	sweepCtx, cancelSweep := context.WithCancel(ctx)
	defer cancelSweep()

	var wg sync.WaitGroup

	wg.Add(1)

	go m.runSweep(sweepCtx, &wg)

	err := m.httpServer.Serve(m.listener)

	cancelSweep()
	wg.Wait()

	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		// The cache is dead from here on (listener gone, sweep stopped) and
		// nothing restarts it — the error may sit in the caller's errgroup
		// until process shutdown, so the transition must be visible now:
		// cache_up exists precisely to detect this condition, and leaving it
		// at 1 would defeat its one job.
		metricCacheUp.Set(0)
		m.logger.Error().Err(err).Msg("Cache listener terminated unexpectedly")

		return fmt.Errorf("cache: serve: %w", err)
	}

	return nil
}

// Shutdown gracefully drains the cache HTTP listener, then removes the
// firewall guard if Start installed one. It tolerates being called when
// Start never ran, or failed before installing the guard. The HTTP server
// is drained before the guard is removed so no request is in flight while
// the control-plane ports become reachable again.
func (m *Manager) Shutdown(ctx context.Context) {
	if m.httpServer != nil {
		if err := m.httpServer.Shutdown(ctx); err != nil {
			m.logger.Warn().Err(err).Msg("Failed to gracefully shut down cache")
		}
	}

	metricCacheUp.Set(0)

	if !m.guardInstalled {
		return
	}

	if err := removeGuardRules(); err != nil {
		m.logger.Warn().Err(err).Msg("Failed to remove firewall guard")
	}

	m.guardInstalled = false
}

func (m *Manager) buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Reserved prefixes for the registry, Go module proxy, npm and friends
	// stay unregistered here (D-05): this generic route must not shadow
	// the real cache-kind path prefixes Phases 2-4 add.
	mux.HandleFunc("GET /cache/{kind}/{key...}", m.handleCacheGet)

	return mux
}

// handleCacheGet serves content out of the disk store. The response
// headers are always set to a fixed, non-sniffable content type before
// the body is written, so a cached artifact that happens to parse as
// HTML can never be sniffed and rendered by a browser-like client.
func (m *Manager) handleCacheGet(w http.ResponseWriter, r *http.Request) {
	if m.store == nil {
		// The mux is built in New, before Start creates the store — a
		// request cannot normally arrive this early, but the handler must
		// not dereference a nil store if one somehow does.
		http.Error(w, "cache not ready", http.StatusServiceUnavailable)

		return
	}

	kind := r.PathValue("kind")
	key := r.PathValue("key")

	f, fi, err := m.store.Open(kind, key)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidKey):
			http.Error(w, "invalid key", http.StatusBadRequest)
		case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
			// ENOTDIR is a guest-shaped miss, not a server fault: any request
			// whose path extends an existing file (…/hello.txt/extra) fails
			// with it, and mapping it to 500 would let an untrusted guest
			// flood host logs at Error level with ordinary requests.
			http.Error(w, "not found", http.StatusNotFound)
		default:
			m.logger.Error().Err(err).Str("kind", kind).Str("key", key).Msg("cache: serving content failed")
			http.Error(w, "internal error", http.StatusInternalServerError)
		}

		return
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// runSweep runs one sweep immediately, then one on every tick of
// sweepInterval, until ctx is cancelled. A sweep error is logged and the
// loop continues — a failing sweep must never take down the listener it
// shares Run with.
func (m *Manager) runSweep(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	ticker := time.NewTicker(m.sweepInterval)
	defer ticker.Stop()

	m.sweepOnce()

	for {
		select {
		case <-ticker.C:
			m.sweepOnce()
		case <-ctx.Done():
			return
		}
	}
}

// sweepOnce runs a single Store.Sweep pass and publishes its result to the
// cache_disk_bytes_used gauge and cache_evictions_total counter.
func (m *Manager) sweepOnce() {
	result, err := m.store.Sweep(m.maxSizeBytes)
	if err != nil {
		m.logger.Error().Err(err).Msg("cache: sweep failed")

		return
	}

	metricCacheDiskBytesUsed.Set(float64(result.BytesAfter))
	metricCacheEvictionsTotal.Add(float64(result.Evicted))

	if result.Evicted > 0 {
		m.logger.Info().
			Int64("bytes_before", result.BytesBefore).
			Int64("bytes_after", result.BytesAfter).
			Int("evicted", result.Evicted).
			Int("temp_reaped", result.TempReaped).
			Msg("cache: sweep complete")

		return
	}

	m.logger.Debug().
		Int64("bytes_before", result.BytesBefore).
		Int64("bytes_after", result.BytesAfter).
		Int("evicted", result.Evicted).
		Int("temp_reaped", result.TempReaped).
		Msg("cache: sweep complete")
}
