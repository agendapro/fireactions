// Package backend defines the VM-backend seam: the Backend and Machine
// interfaces every hypervisor implementation (Firecracker on linux, VZ on
// darwin) satisfies. It carries no build tags, so it compiles under every
// GOOS — which is what keeps server/pool.go OS-agnostic.
package backend

import (
	"context"
	"path/filepath"
	"time"

	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"google.golang.org/grpc"
)

// DefaultStateDirRoot is the host directory under which backends keep
// per-pool, per-VM ephemeral state (sockets, logs, vsock paths, and any
// other on-disk bookkeeping a backend needs for the lifetime of a VM).
// server/pool.go hardcoded this path directly before the backend seam
// existed; it lives here now so every backend implementation shares one
// convention.
const DefaultStateDirRoot = "/var/lib/fireactions"

// PoolStateDir returns the state directory for a single pool: root joined
// with "pools/<pool>". This layout is shared by every backend and
// reproduces today's "/var/lib/fireactions/pools/<name>" exactly.
func PoolStateDir(root, pool string) string {
	return filepath.Join(root, "pools", pool)
}

// Image is an image known to a backend, as reported by Backend.ListImages.
// Its fields are field-for-field what convertImageToProto reads off today's
// image-catalog client.
type Image struct {
	Name      string
	Size      int64
	CreatedAt time.Time
}

// ImageRef is an opaque, backend-owned handle to an image that
// Backend.EnsureImage resolved and Backend.CreateMachine consumes. Neutral
// code never inspects an ImageRef beyond Ref, which returns the reference
// string it was resolved from.
type ImageRef interface {
	// Ref returns the reference string this ImageRef was resolved from.
	Ref() string
}

// FirecrackerSpec carries the firecracker-only knobs of a VM request,
// mirroring server/config.go's FirecrackerConfig fields of the same names.
// It is the zero value for backends that do not use Firecracker.
type FirecrackerSpec struct {
	BinaryPath      string
	KernelImagePath string
	KernelArgs      string
}

// VMSpec is everything Pool already knows when it asks for a VM; it
// carries no backend-specific type.
//
// There is deliberately no vsock identifier or socket-path field here:
// that allocation and bookkeeping stays inside the firecracker
// implementation.
type VMSpec struct {
	Pool     string
	Name     string
	Hostname string

	// JITConfig is a GitHub runner registration secret. It must never be
	// logged.
	JITConfig string

	ShutdownOnExit bool
	VCPUCount      int64
	MemSizeMib     int64

	// Metadata is the operator-supplied metadata map from pool config. The
	// backend owns deep-copying it before mutation; the delivery mechanism
	// (today's guest metadata channel, vsock later) is backend-internal —
	// VMSpec carries no delivery-specific envelope.
	Metadata map[string]interface{}

	Firecracker FirecrackerSpec
}

// Machine is the neutral per-VM handle Pool tracks.
type Machine interface {
	// Addr returns the guest's address, or "" when the backend cannot
	// report one.
	Addr() string

	// ConnectToGuestAgent dials the guest agent's gRPC server. The caller
	// closes the returned conn.
	ConnectToGuestAgent(ctx context.Context) (*grpc.ClientConn, agentv1.AgentServiceClient, error)

	// Stop stops the VM.
	Stop() error

	// Wait blocks until the VM has exited.
	Wait(ctx context.Context) error

	// Cleanup releases host-side resources held for the VM after it has
	// exited. It is called once, after Wait returns, and is where a
	// backend cancels the leases/contexts/clone files it created in
	// CreateMachine. This is the generic teardown hook every backend
	// implements.
	Cleanup(ctx context.Context) error
}

// Backend is the VM-backend seam: Pool calls only through this interface,
// never through a hypervisor-specific type.
//
// There is deliberately no Close method: nothing closes today's
// image-catalog client (server/server.go never does), and adding one
// would be new behavior in a behavior-preserving phase.
type Backend interface {
	// PrepareStateDir creates, if missing, the pool's ephemeral state
	// directory and returns its absolute path. The path follows
	// PoolStateDir; this is the only filesystem setup Pool delegates.
	PrepareStateDir(pool string) (string, error)

	// ReconcileOrphans reclaims ephemeral per-VM state a previous process
	// left behind for the pool, returning the names it reclaimed. Backends
	// that do not re-adopt or sweep orphaned state return (nil, nil); the
	// firecracker implementation is a documented no-op that preserves
	// today's restart behavior (nothing is re-adopted on restart).
	ReconcileOrphans(ctx context.Context, pool string) ([]string, error)

	// EnsureImage resolves ref to an ImageRef, pulling it if necessary.
	// pullPolicy is one of Always/Never/IfNotPresent, matching
	// server/config.go's RunnerConfig.ImagePullPolicy validation.
	EnsureImage(ctx context.Context, ref, pullPolicy string) (ImageRef, error)

	// ListImages returns the images known to the backend.
	ListImages(ctx context.Context) ([]Image, error)

	// RemoveImage removes the image identified by ref.
	RemoveImage(ctx context.Context, ref string) error

	// CreateMachine returns a started, running machine. The backend
	// derives the VM's lifetime context from ctx (the pool's context) and
	// rolls back the resource leases it took (e.g. the containerd lease)
	// if any step fails. Per-VM files under the pool state directory may
	// survive a failed call; ReconcileOrphans is the hook for reclaiming
	// them.
	CreateMachine(ctx context.Context, spec VMSpec, image ImageRef) (Machine, error)
}
