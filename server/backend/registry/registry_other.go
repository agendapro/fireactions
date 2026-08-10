//go:build !linux

package registry

import (
	"fmt"
	"runtime"

	"github.com/hostinger/fireactions/server/backend"
)

// New returns an error: no VM backend is available for this GOOS yet. This
// stub exists so the neutral server package and its tests stay buildable on
// every GOOS while backend implementations are added platform by platform.
// Phase 6 replaces it for darwin with registry_darwin.go, at which point
// this file's tag becomes "!linux && !darwin".
func New(opts ...Opt) (backend.Backend, error) {
	_ = newSettings(opts...)

	return nil, fmt.Errorf("no VM backend available for GOOS=%s", runtime.GOOS)
}
