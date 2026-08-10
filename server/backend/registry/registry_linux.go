package registry

import (
	"fmt"

	"github.com/hostinger/fireactions/server/backend"
	"github.com/hostinger/fireactions/server/backend/firecracker"
)

// New creates the VM backend for this platform. On linux that is the
// Firecracker/containerd backend.
func New(opts ...Opt) (backend.Backend, error) {
	s := newSettings(opts...)

	b, err := firecracker.New(
		firecracker.WithLogger(s.logger),
		firecracker.WithStateDirRoot(s.stateDirRoot),
		firecracker.WithContainerd(s.containerdAddress, s.containerdNamespace),
	)
	if err != nil {
		return nil, fmt.Errorf("firecracker: %w", err)
	}

	return b, nil
}
