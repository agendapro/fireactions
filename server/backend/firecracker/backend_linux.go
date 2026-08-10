package firecracker

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	fcsdk "github.com/firecracker-microvm/firecracker-go-sdk"
	fcmodels "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/hostinger/fireactions/helper/deepcopy"
	"github.com/hostinger/fireactions/server/backend"
	"github.com/opencontainers/image-spec/identity"
	"github.com/rs/zerolog"
	"github.com/sirupsen/logrus"
)

// var _ backend.Backend proves *Backend satisfies the backend.Backend
// interface at compile time.
var _ backend.Backend = (*Backend)(nil)

// Backend is the Firecracker/containerd implementation of backend.Backend.
// One instance is created per server process and shared by every pool.
type Backend struct {
	containerd   *containerd.Client
	imageManager *imageManager
	stateDirRoot string

	containerdAddress   string
	containerdNamespace string

	nextCID atomic.Uint32

	logger *zerolog.Logger
}

// Opt is a functional option for Backend.
type Opt func(b *Backend)

// WithLogger sets the logger for the Backend.
func WithLogger(logger *zerolog.Logger) Opt {
	f := func(b *Backend) {
		b.logger = logger
	}

	return f
}

// WithStateDirRoot overrides the root directory the Backend uses for
// per-pool ephemeral state. Defaults to backend.DefaultStateDirRoot.
func WithStateDirRoot(root string) Opt {
	f := func(b *Backend) {
		b.stateDirRoot = root
	}

	return f
}

// WithContainerd sets the containerd socket address and namespace New
// dials.
func WithContainerd(address, namespace string) Opt {
	f := func(b *Backend) {
		b.containerdAddress = address
		b.containerdNamespace = namespace
	}

	return f
}

// New creates a new Backend: it dials containerd and constructs the image
// manager. Callers typically supply WithContainerd, WithStateDirRoot and
// WithLogger.
func New(opts ...Opt) (*Backend, error) {
	logger := zerolog.Nop()
	b := &Backend{
		logger:       &logger,
		stateDirRoot: backend.DefaultStateDirRoot,
	}

	for _, opt := range opts {
		opt(b)
	}

	// CID 2 is reserved for host, start at 3
	b.nextCID.Store(2)

	containerdClient, err := containerd.New(b.containerdAddress,
		containerd.WithTimeout(5*time.Second), containerd.WithDefaultNamespace(b.containerdNamespace))
	if err != nil {
		return nil, fmt.Errorf("containerd: creating client: %w", err)
	}

	b.containerd = containerdClient
	b.imageManager = newImageManager(b.logger, b.containerd)

	return b, nil
}

// PrepareStateDir creates, if missing, the pool's ephemeral state directory
// and returns its absolute path.
func (b *Backend) PrepareStateDir(pool string) (string, error) {
	dir := backend.PoolStateDir(b.stateDirRoot, pool)

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("creating pool directory: %w", err)
		}
	}

	return dir, nil
}

// ReconcileOrphans is a documented no-op: the Firecracker backend does not
// reclaim any per-VM state a previous process left behind for the pool.
// This preserves today's real behavior — nothing in server/pool.go ever
// reads the pool state directory back on restart; it is only ever written
// to (the machine log file, the vsock path, and the Firecracker process's
// own log), never read. This is a pre-existing gap in the upstream
// behavior being documented here, not fixed (D-03): the VZ backend
// implements real reconciliation in Phase 7 (LNX-06).
func (b *Backend) ReconcileOrphans(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

// imageRef is the Firecracker backend's backend.ImageRef: a thin handle
// around a containerd.Image.
type imageRef struct {
	image containerd.Image
}

// Ref returns the reference string this imageRef was resolved from.
func (r *imageRef) Ref() string {
	return r.image.Name()
}

// EnsureImage, ListImages and RemoveImage below return imageManager's
// errors unwrapped, on purpose: server/rpc.go formats them straight into
// gRPC statuses today, and those strings must not change.

// EnsureImage resolves ref to an ImageRef, pulling it if necessary.
func (b *Backend) EnsureImage(ctx context.Context, ref, pullPolicy string) (backend.ImageRef, error) {
	img, err := b.imageManager.ensureImage(ctx, ref, pullPolicy)
	if err != nil {
		return nil, err
	}

	return &imageRef{image: img}, nil
}

// ListImages returns the images known to the backend.
func (b *Backend) ListImages(ctx context.Context) ([]backend.Image, error) {
	images, err := b.imageManager.listImages(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]backend.Image, len(images))
	for i, img := range images {
		size, _ := img.Size(ctx)
		result[i] = backend.Image{
			Name:      img.Name(),
			Size:      size,
			CreatedAt: img.Metadata().CreatedAt,
		}
	}

	return result, nil
}

// RemoveImage removes the image identified by ref.
func (b *Backend) RemoveImage(ctx context.Context, ref string) error {
	return b.imageManager.removeImage(ctx, ref)
}

// createSnapshot creates a snapshot of the specified image.
func (b *Backend) createSnapshot(ctx context.Context, image containerd.Image, snapshotID string) ([]mount.Mount, error) {
	snapshotService := b.containerd.SnapshotService(defaultSnapshotter)
	snapshotExists := true
	_, err := snapshotService.Stat(ctx, snapshotID)
	if err != nil {
		if !errdefs.IsNotFound(err) {
			return nil, err
		}

		snapshotExists = false
	}

	if !snapshotExists {
		imageContent, err := image.RootFS(ctx)
		if err != nil {
			return nil, fmt.Errorf("image: rootfs: %w", err)
		}

		_, err = snapshotService.Prepare(ctx, snapshotID, identity.ChainID(imageContent).String())
		if err != nil {
			return nil, fmt.Errorf("prepare: %w", err)
		}
	}

	mounts, err := snapshotService.Mounts(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("mounts: %w", err)
	}

	return mounts, nil
}

// CreateMachine reproduces today's Firecracker VM-creation sequence:
// containerd lease -> snapshot preparation -> Firecracker process/command
// setup -> vsock allocation -> firecracker.Config construction -> MMDS
// metadata delivery -> VMM start. ctx is the pool's lifetime context; the
// VM's own VMM context is derived from it, matching today's
// context.WithCancel(p.ctx) (Pool.Scale is only ever called with p.ctx). On
// any failure the containerd lease taken for this VM is rolled back; per-VM
// files already created under the pool state directory (the machine log,
// and after a Start failure the firecracker log, API socket and vsock UDS)
// may survive the failed call.
func (b *Backend) CreateMachine(ctx context.Context, spec backend.VMSpec, image backend.ImageRef) (backend.Machine, error) {
	img, ok := image.(*imageRef)
	if !ok {
		return nil, fmt.Errorf("firecracker: unexpected image handle %T", image)
	}

	dir := backend.PoolStateDir(b.stateDirRoot, spec.Pool)

	leaseCtx, leaseCtxCancel, err := b.containerd.WithLease(ctx,
		leases.WithID(fmt.Sprintf("fireactions/pools/%s/%s", spec.Pool, spec.Name)))
	if err != nil {
		return nil, fmt.Errorf("containerd: creating lease: %w", err)
	}

	// Track if we successfully created the machine to determine cleanup responsibility
	var machineCreated bool
	defer func() {
		if !machineCreated {
			// Clean up lease if machine creation failed
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()
			_ = leaseCtxCancel(cleanupCtx)
		}
	}()

	snapshotMounts, err := b.createSnapshot(leaseCtx, img.image, spec.Name)
	if err != nil {
		return nil, fmt.Errorf("containerd: creating snapshot: %w", err)
	}

	machineLogFile, err := os.Create(filepath.Join(dir, fmt.Sprintf("%s.log", spec.Name)))
	if err != nil {
		return nil, fmt.Errorf("creating log file: %w", err)
	}
	defer machineLogFile.Close()

	machineCmd := fcsdk.VMCommandBuilder{}.
		WithSocketPath(filepath.Join(dir, fmt.Sprintf("%s.sock", spec.Name))).
		WithStderr(machineLogFile).
		WithStdout(machineLogFile).
		WithBin(spec.Firecracker.BinaryPath).
		Build(ctx)

	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	logger.SetOutput(io.Discard)

	vsockPath := filepath.Join(dir, fmt.Sprintf("%s.vsock", spec.Name))
	vsockCID := b.nextCID.Add(1)

	fcMachine, err := fcsdk.NewMachine(ctx, fcsdk.Config{
		VMID:            spec.Name,
		SocketPath:      filepath.Join(dir, fmt.Sprintf("%s.sock", spec.Name)),
		KernelImagePath: spec.Firecracker.KernelImagePath,
		KernelArgs:      spec.Firecracker.KernelArgs,
		MachineCfg: fcmodels.MachineConfiguration{
			VcpuCount:  &spec.VCPUCount,
			MemSizeMib: &spec.MemSizeMib,
		},
		Drives: []fcmodels.Drive{{
			DriveID:      fcsdk.String("rootfs"),
			PathOnHost:   &snapshotMounts[0].Source,
			IsRootDevice: fcsdk.Bool(true),
			IsReadOnly:   fcsdk.Bool(false),
		}},
		NetworkInterfaces: []fcsdk.NetworkInterface{{
			AllowMMDS:        true,
			CNIConfiguration: &fcsdk.CNIConfiguration{NetworkName: "fireactions", IfName: "eth0", ConfDir: "/etc/cni/net.d", BinPath: []string{"/opt/cni/bin"}},
		}},
		VsockDevices:   []fcsdk.VsockDevice{{Path: vsockPath, CID: vsockCID}},
		MmdsAddress:    net.IPv4(169, 254, 169, 254),
		MmdsVersion:    fcsdk.MMDSv2,
		ForwardSignals: []os.Signal{},
		LogPath:        filepath.Join(dir, fmt.Sprintf("%s.firecracker.log", spec.Name)),
		LogLevel:       "Debug",
	}, fcsdk.WithProcessRunner(machineCmd), fcsdk.WithLogger(logrus.NewEntry(logger)))
	if err != nil {
		return nil, fmt.Errorf("firecracker: creating machine: %w", err)
	}

	// spec.JITConfig is a secret and must never be logged; it is placed only
	// into the guest metadata map delivered below.
	metadata := map[string]interface{}{"latest": map[string]interface{}{"meta-data": deepcopy.Map(spec.Metadata)}}
	metadata["latest"].(map[string]interface{})["meta-data"].(map[string]interface{})["fireactions"] = map[string]interface{}{
		"runner_id":         spec.Name,
		"runner_jit_config": spec.JITConfig,
		"hostname":          spec.Hostname,
		"shutdown_on_exit":  spec.ShutdownOnExit,
	}

	fcMachine.Handlers.FcInit = fcMachine.Handlers.FcInit.Append(fcsdk.NewSetMetadataHandler(metadata))

	vmmCtx, vmmCancel := context.WithCancel(ctx)
	if err := fcMachine.Start(vmmCtx); err != nil {
		vmmCancel()
		return nil, fmt.Errorf("firecracker: starting machine: %w", err)
	}

	// Mark machine as successfully created
	machineCreated = true

	return &machine{
		fc:          fcMachine,
		vsockCID:    vsockCID,
		vsockPath:   vsockPath,
		leaseCancel: leaseCtxCancel,
		vmmCancel:   vmmCancel,
	}, nil
}

// init silences containerd/log's own default logger, matching
// server/pool.go's init() before the split.
func init() {
	_ = log.SetLevel("panic")
}
