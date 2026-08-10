// Package registry is the only place in the codebase that chooses a VM
// backend implementation for the host platform; every other package
// depends on server/backend's interfaces alone.
package registry

import (
	"github.com/hostinger/fireactions/server/backend"
	"github.com/rs/zerolog"
)

// Opt is a functional option shared by every platform's New.
type Opt func(s *settings)

// settings holds the values every platform's New needs to construct its
// backend, before that platform's constructor-specific options are built
// from them.
type settings struct {
	logger *zerolog.Logger

	stateDirRoot string

	containerdAddress   string
	containerdNamespace string
}

// newSettings builds settings from defaults plus opts.
func newSettings(opts ...Opt) *settings {
	logger := zerolog.Nop()
	s := &settings{
		logger:       &logger,
		stateDirRoot: backend.DefaultStateDirRoot,
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// WithLogger sets the logger passed to the selected backend's constructor.
func WithLogger(logger *zerolog.Logger) Opt {
	f := func(s *settings) {
		s.logger = logger
	}

	return f
}

// WithStateDirRoot overrides the root directory the selected backend uses
// for per-pool ephemeral state.
func WithStateDirRoot(root string) Opt {
	f := func(s *settings) {
		s.stateDirRoot = root
	}

	return f
}

// WithContainerd sets the containerd socket address and namespace passed to
// backends that need one (Firecracker, on linux).
func WithContainerd(address, namespace string) Opt {
	f := func(s *settings) {
		s.containerdAddress = address
		s.containerdNamespace = namespace
	}

	return f
}
