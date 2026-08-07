package cache

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// Manager manages the lifecycle of the cache component: the single
// path-routed HTTP listener that serves all cache kinds, guarded by a
// dedicated firewall chain that blocks guest access to fireactions' own
// ports. A later phase adds a bounded disk store on top of this same
// Manager.
type Manager struct {
	config         *Config
	guardedPorts   []string
	logger         *zerolog.Logger
	httpServer     *http.Server
	listener       net.Listener
	mux            *http.ServeMux
	guardInstalled bool
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
		guardInstalled: false,
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
// traffic. The firewall guard is installed first and gates everything
// after it: if it cannot be installed, the cache never binds its listener
// (D-07). Structured so that a later phase's disk store creation can be
// inserted above the listen call without reordering: nothing may bind a
// guest-reachable port before that step succeeds either.
func (m *Manager) Start(_ context.Context) error {
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
// returns an error if Start has not run yet.
func (m *Manager) Run(_ context.Context) error {
	if m.listener == nil {
		return fmt.Errorf("cache: Run called before Start")
	}

	err := m.httpServer.Serve(m.listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
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

	return mux
}
