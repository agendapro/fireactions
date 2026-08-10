package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	serverv1 "github.com/hostinger/fireactions/proto/server/v1"
	"github.com/hostinger/fireactions/server/backend"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// fakeMachine is a hand-rolled in-memory implementation of backend.Machine.
// It requires no host, no hypervisor and no network I/O.
type fakeMachine struct {
	name string
	addr string

	stopOnce  sync.Once
	stopped   chan struct{}
	stopCalls atomic.Int32

	cleanupCalls atomic.Int32
	cleanupErr   error
}

var _ backend.Machine = (*fakeMachine)(nil)

func (m *fakeMachine) Addr() string {
	return m.addr
}

func (m *fakeMachine) ConnectToGuestAgent(_ context.Context) (*grpc.ClientConn, agentv1.AgentServiceClient, error) {
	return nil, nil, errors.New("fake machine: no guest agent")
}

func (m *fakeMachine) Stop() error {
	m.stopCalls.Add(1)
	m.stopOnce.Do(func() { close(m.stopped) })
	return nil
}

func (m *fakeMachine) Wait(ctx context.Context) error {
	select {
	case <-m.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *fakeMachine) Cleanup(_ context.Context) error {
	m.cleanupCalls.Add(1)
	return m.cleanupErr
}

// fakeImageRef is the minimal backend.ImageRef fakeBackend.EnsureImage
// resolves to.
type fakeImageRef struct {
	ref string
}

var _ backend.ImageRef = (*fakeImageRef)(nil)

func (r *fakeImageRef) Ref() string {
	return r.ref
}

// fakeBackend is a hand-rolled in-memory implementation of backend.Backend.
type fakeBackend struct {
	mu       sync.Mutex
	dir      string
	machines []*fakeMachine
	orphans  []string

	createCalls    atomic.Int32
	ensureCalls    atomic.Int32
	reconcileCalls atomic.Int32

	prepareErr error
	ensureErr  error
	createErr  error
}

var _ backend.Backend = (*fakeBackend)(nil)

func (b *fakeBackend) PrepareStateDir(_ string) (string, error) {
	if b.prepareErr != nil {
		return "", b.prepareErr
	}

	return b.dir, nil
}

func (b *fakeBackend) ReconcileOrphans(_ context.Context, _ string) ([]string, error) {
	b.reconcileCalls.Add(1)
	return b.orphans, nil
}

func (b *fakeBackend) EnsureImage(_ context.Context, ref, _ string) (backend.ImageRef, error) {
	b.ensureCalls.Add(1)
	if b.ensureErr != nil {
		return nil, b.ensureErr
	}

	return &fakeImageRef{ref: ref}, nil
}

func (b *fakeBackend) ListImages(_ context.Context) ([]backend.Image, error) {
	return nil, nil
}

func (b *fakeBackend) RemoveImage(_ context.Context, _ string) error {
	return nil
}

func (b *fakeBackend) CreateMachine(_ context.Context, spec backend.VMSpec, _ backend.ImageRef) (backend.Machine, error) {
	b.createCalls.Add(1)
	if b.createErr != nil {
		return nil, b.createErr
	}

	m := &fakeMachine{name: spec.Name, stopped: make(chan struct{})}

	b.mu.Lock()
	b.machines = append(b.machines, m)
	b.mu.Unlock()

	return m, nil
}

// lastMachine returns the most recently created machine, or nil if none.
func (b *fakeBackend) lastMachine() *fakeMachine {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.machines) == 0 {
		return nil
	}

	return b.machines[len(b.machines)-1]
}

// machineCount returns the number of machines CreateMachine has produced.
func (b *fakeBackend) machineCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.machines)
}

// fakeGitHub is a hand-rolled in-memory implementation of githubAPI.
type fakeGitHub struct {
	installationID int64
	jitConfig      string

	runnerID atomic.Int64

	findErr     error
	generateErr error
	removeErr   error

	removeCalls atomic.Int32

	mu      sync.Mutex
	removed []int64
}

var _ githubAPI = (*fakeGitHub)(nil)

func (g *fakeGitHub) FindOrganizationInstallation(_ context.Context, _ string) (int64, error) {
	if g.findErr != nil {
		return 0, g.findErr
	}

	return g.installationID, nil
}

func (g *fakeGitHub) GenerateOrgJITConfig(_ context.Context, _ int64, _, name string, _ int64, _ []string) (string, int64, string, error) {
	if g.generateErr != nil {
		return "", 0, "", g.generateErr
	}

	id := g.runnerID.Add(1)
	return g.jitConfig, id, name, nil
}

func (g *fakeGitHub) RemoveOrganizationRunner(_ context.Context, _ int64, _ string, runnerID int64) error {
	g.removeCalls.Add(1)
	if g.removeErr != nil {
		return g.removeErr
	}

	g.mu.Lock()
	g.removed = append(g.removed, runnerID)
	g.mu.Unlock()

	return nil
}

// newTestPoolConfig builds a *PoolConfig with the fields NewPool requires,
// named uniquely per test (t.Name()) so the Prometheus metrics NewPool
// registers, which are labelled by pool name, don't bleed between tests.
func newTestPoolConfig(t *testing.T) *PoolConfig {
	t.Helper()

	shutdownOnExit := true

	return &PoolConfig{
		Name:           t.Name(),
		ShutdownOnExit: &shutdownOnExit,
		Replicas:       0,
		Runner: &RunnerConfig{
			Name:            "test-runner",
			ImagePullPolicy: "IfNotPresent",
			Image:           "ghcr.io/example/runner:latest",
			Organization:    "test-org",
			GroupID:         1,
			Labels:          []string{"test"},
		},
		Firecracker: &FirecrackerConfig{
			MachineConfig: FirecrackerMachineConfig{
				VcpuCount:  1,
				MemSizeMib: 512,
			},
		},
	}
}

// newTestPool constructs a real *Pool driven entirely by fakeBackend and
// fakeGitHub — no host, no network, no privileged path. It does not start
// Run(); callers that do are responsible for their own t.Cleanup(pool.Stop).
func newTestPool(t *testing.T) (*Pool, *fakeBackend, *fakeGitHub) {
	t.Helper()

	fakeBE := &fakeBackend{dir: t.TempDir()}
	fakeGH := &fakeGitHub{installationID: 1, jitConfig: "fake-jit-config"}

	logger := zerolog.Nop()
	pool, err := NewPool(&logger, newTestPoolConfig(t), fakeGH, fakeBE)
	require.NoError(t, err)

	return pool, fakeBE, fakeGH
}

func TestPool_New_PreparesStateDirAndReconcilesOrphans(t *testing.T) {
	pool, fakeBE, _ := newTestPool(t)

	assert.Equal(t, fakeBE.dir, pool.GetDir())
	assert.Equal(t, int32(1), fakeBE.reconcileCalls.Load())
}

func TestPool_New_PrepareStateDirFailurePropagates(t *testing.T) {
	fakeBE := &fakeBackend{prepareErr: errors.New("prepare state dir: boom")}
	fakeGH := &fakeGitHub{installationID: 1, jitConfig: "fake-jit-config"}

	logger := zerolog.Nop()
	pool, err := NewPool(&logger, newTestPoolConfig(t), fakeGH, fakeBE)

	assert.Nil(t, pool)
	assert.Error(t, err)
}

func TestPool_TriggerScale_IsNonBlockingAndCoalesces(t *testing.T) {
	pool, _, _ := newTestPool(t)

	pool.TriggerScale()
	pool.TriggerScale()
	pool.TriggerScale()

	assert.Equal(t, 1, len(pool.scaleTrigger))
}

// TestPool_Scale_CreatesMachinesThroughBackend pins that scaling up creates
// machines exclusively through the backend fake: every VM Pool creates is
// observed as an EnsureImage + CreateMachine call, never a host-side effect.
func TestPool_Scale_CreatesMachinesThroughBackend(t *testing.T) {
	pool, fakeBE, _ := newTestPool(t)
	t.Cleanup(func() { pool.cancel() })

	require.NoError(t, pool.Scale(context.Background(), 2))

	require.Eventually(t, func() bool {
		return pool.GetCurrentSize() == 2 && pool.pendingCreates.Load() == 0
	}, 2*time.Second, 10*time.Millisecond)

	assert.Equal(t, int32(2), fakeBE.createCalls.Load())
	assert.Equal(t, int32(2), fakeBE.ensureCalls.Load())
}

// TestPool_Scale_IsIdempotentAtDesiredSize pins the delta == 0 early exit:
// scaling to an already-reached size must not create any further machines.
func TestPool_Scale_IsIdempotentAtDesiredSize(t *testing.T) {
	pool, fakeBE, _ := newTestPool(t)
	t.Cleanup(func() { pool.cancel() })

	// Settle on pendingCreates too: GetCurrentSize()==2 alone races the
	// deferred pendingCreates decrement in scaleUp, and a second Scale
	// landing in that window would see a stale effectiveSize.
	require.NoError(t, pool.Scale(context.Background(), 2))
	require.Eventually(t, func() bool {
		return pool.GetCurrentSize() == 2 && pool.pendingCreates.Load() == 0
	}, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, pool.Scale(context.Background(), 2))

	assert.Never(t, func() bool {
		return fakeBE.createCalls.Load() != 2
	}, 300*time.Millisecond, 20*time.Millisecond)
}

// TestPool_Scale_ScaleDownStopsAndRemovesAMachine pins that scaling down
// stops exactly one machine (deleteMachine picks an arbitrary map entry by
// design, so we assert "exactly one," never "which one").
func TestPool_Scale_ScaleDownStopsAndRemovesAMachine(t *testing.T) {
	pool, fakeBE, _ := newTestPool(t)
	t.Cleanup(func() { pool.cancel() })

	require.NoError(t, pool.Scale(context.Background(), 2))
	require.Eventually(t, func() bool {
		return pool.GetCurrentSize() == 2 && pool.pendingCreates.Load() == 0
	}, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, pool.Scale(context.Background(), 1))
	assert.Eventually(t, func() bool {
		return pool.GetCurrentSize() == 1
	}, 2*time.Second, 10*time.Millisecond)

	fakeBE.mu.Lock()
	stopped := 0
	for _, m := range fakeBE.machines {
		if m.stopCalls.Load() == 1 {
			stopped++
		}
	}
	fakeBE.mu.Unlock()

	assert.Equal(t, 1, stopped)
}

// TestPool_CreateMachine_BackendFailureDeletesTheOrphanRunner pins the
// compensation for accepted-delta 1 (05-04): JIT config is generated before
// the VM exists, so a CreateMachine failure must delete the GitHub runner
// registration it already created, or the pool leaks it on every scale tick.
func TestPool_CreateMachine_BackendFailureDeletesTheOrphanRunner(t *testing.T) {
	pool, fakeBE, fakeGH := newTestPool(t)
	t.Cleanup(func() { pool.cancel() })

	fakeBE.createErr = errors.New("backend: create machine: boom")

	require.NoError(t, pool.Scale(context.Background(), 1))

	assert.Eventually(t, func() bool {
		return pool.GetCurrentSize() == 0 && fakeGH.removeCalls.Load() == 1
	}, 2*time.Second, 10*time.Millisecond)

	fakeGH.mu.Lock()
	removed := append([]int64(nil), fakeGH.removed...)
	fakeGH.mu.Unlock()

	require.Len(t, removed, 1)
	assert.Equal(t, fakeGH.runnerID.Load(), removed[0])
}

// TestPool_CreateMachine_ImageFailureDoesNotCallGitHub pins the step order
// inside createMachine: image first, GitHub second, VM third. An image
// failure must short-circuit before either GitHub or the backend VM create
// path is reached.
func TestPool_CreateMachine_ImageFailureDoesNotCallGitHub(t *testing.T) {
	pool, fakeBE, fakeGH := newTestPool(t)
	t.Cleanup(func() { pool.cancel() })

	fakeBE.ensureErr = errors.New("backend: ensure image: boom")

	require.NoError(t, pool.Scale(context.Background(), 1))

	assert.Eventually(t, func() bool {
		return pool.pendingCreates.Load() == 0
	}, 2*time.Second, 10*time.Millisecond)

	assert.Equal(t, 0, pool.GetCurrentSize())
	assert.Equal(t, int32(0), fakeBE.createCalls.Load())
	assert.Equal(t, int32(0), fakeGH.removeCalls.Load())
}

// TestPool_MachineExit_RemovesItFromThePoolAndCleansUp simulates the guest
// exiting on its own (fakeMachine.Stop() unblocks the fake's Wait, exactly
// like a real VM shutting down) and pins that the cleanup goroutine removes
// the machine from the pool, deletes its GitHub runner registration, and
// releases the backend-side resource — each exactly once.
func TestPool_MachineExit_RemovesItFromThePoolAndCleansUp(t *testing.T) {
	pool, fakeBE, fakeGH := newTestPool(t)
	t.Cleanup(func() { pool.cancel() })

	require.NoError(t, pool.Scale(context.Background(), 1))
	require.Eventually(t, func() bool {
		return pool.GetCurrentSize() == 1 && pool.pendingCreates.Load() == 0
	}, 2*time.Second, 10*time.Millisecond)

	fm := fakeBE.lastMachine()
	require.NotNil(t, fm)

	require.NoError(t, fm.Stop())

	assert.Eventually(t, func() bool {
		return pool.GetCurrentSize() == 0 &&
			fm.cleanupCalls.Load() == 1 &&
			fakeGH.removeCalls.Load() == 1
	}, 2*time.Second, 10*time.Millisecond)
}

// TestPool_MachineExit_CleanupErrorDoesNotWedgeThePool pins that a
// Machine.Cleanup error is logged, never fatal: the machine is still
// removed from the pool's bookkeeping and the pool can scale back up
// afterwards.
func TestPool_MachineExit_CleanupErrorDoesNotWedgeThePool(t *testing.T) {
	pool, fakeBE, _ := newTestPool(t)
	t.Cleanup(func() { pool.cancel() })

	require.NoError(t, pool.Scale(context.Background(), 1))
	require.Eventually(t, func() bool {
		return pool.GetCurrentSize() == 1 && pool.pendingCreates.Load() == 0
	}, 2*time.Second, 10*time.Millisecond)

	fm := fakeBE.lastMachine()
	require.NotNil(t, fm)
	fm.cleanupErr = errors.New("cleanup: boom") // set before Stop() — no concurrent access

	require.NoError(t, fm.Stop())

	assert.Eventually(t, func() bool {
		return pool.GetCurrentSize() == 0
	}, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, pool.Scale(context.Background(), 1))
	assert.Eventually(t, func() bool {
		return fakeBE.createCalls.Load() == 2
	}, 2*time.Second, 10*time.Millisecond)
}

// TestPool_Run_ScalesToDesiredReplicasAndStopsCleanly drives Run() through a
// live goroutine: initial scale-to-replicas on start, a SetReplicas bump,
// then a clean Stop() that lets Run() return.
func TestPool_Run_ScalesToDesiredReplicasAndStopsCleanly(t *testing.T) {
	fakeBE := &fakeBackend{dir: t.TempDir()}
	fakeGH := &fakeGitHub{installationID: 1, jitConfig: "fake-jit-config"}

	cfg := newTestPoolConfig(t)
	cfg.Replicas = 1

	logger := zerolog.Nop()
	pool, err := NewPool(&logger, cfg, fakeGH, fakeBE)
	require.NoError(t, err)

	runDone := make(chan struct{})
	go func() {
		pool.Run()
		close(runDone)
	}()

	assert.Eventually(t, func() bool {
		return pool.GetCurrentSize() == 1
	}, 2*time.Second, 10*time.Millisecond)

	pool.SetReplicas(2)
	assert.Eventually(t, func() bool {
		return pool.GetCurrentSize() == 2
	}, 2*time.Second, 10*time.Millisecond)

	pool.Stop()

	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for Run to return after Stop")
	}
}

// TestPool_Stop_StopsEveryMachineAndDrainsCleanupBeforeReturning is the
// cleanupWg-drained-before-return proof (T-05-21): asserting "eventually"
// here would pass even if Stop returned while cleanup goroutines were still
// running, which is exactly the bug worth catching. Every assertion below
// runs synchronously, immediately after Stop() returns.
func TestPool_Stop_StopsEveryMachineAndDrainsCleanupBeforeReturning(t *testing.T) {
	fakeBE := &fakeBackend{dir: t.TempDir()}
	fakeGH := &fakeGitHub{installationID: 1, jitConfig: "fake-jit-config"}

	cfg := newTestPoolConfig(t)
	cfg.Replicas = 2

	logger := zerolog.Nop()
	pool, err := NewPool(&logger, cfg, fakeGH, fakeBE)
	require.NoError(t, err)

	runDone := make(chan struct{})
	go func() {
		pool.Run()
		close(runDone)
	}()

	// Setup wait (not the terminal-state proof below) — require.Eventually
	// per TESTING.md's "require for setup that must abort" convention.
	// Every assertion after Stop() returns is synchronous below, the only
	// formulation that can catch a Stop() that returns before its cleanup
	// goroutines finish.
	require.Eventually(t, func() bool {
		return pool.GetCurrentSize() == 2
	}, 2*time.Second, 10*time.Millisecond, "pool did not reach 2 replicas before Stop")

	pool.Stop()

	fakeBE.mu.Lock()
	machines := append([]*fakeMachine(nil), fakeBE.machines...)
	fakeBE.mu.Unlock()

	require.Len(t, machines, 2)
	for _, m := range machines {
		assert.GreaterOrEqual(t, m.stopCalls.Load(), int32(1))
		assert.Equal(t, int32(1), m.cleanupCalls.Load())
	}
	assert.Equal(t, 0, pool.GetCurrentSize())

	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for Run to return after Stop")
	}
}

// TestPool_PauseResume_TogglesActiveState is a pure state test with no Run
// goroutine, so it stays outside D-06's isActive race entirely. It reads
// the state through convertPoolToProto, exercising the same field the gRPC
// surface reports rather than poking the raw struct field.
func TestPool_PauseResume_TogglesActiveState(t *testing.T) {
	pool, _, _ := newTestPool(t)
	t.Cleanup(func() { pool.cancel() })

	ctx := context.Background()

	assert.Equal(t, serverv1.PoolState_POOL_STATE_ACTIVE, convertPoolToProto(ctx, pool).State)

	pool.Pause()
	assert.Equal(t, serverv1.PoolState_POOL_STATE_PAUSED, convertPoolToProto(ctx, pool).State)

	pool.Pause() // no-op
	assert.Equal(t, serverv1.PoolState_POOL_STATE_PAUSED, convertPoolToProto(ctx, pool).State)

	pool.Resume()
	assert.Equal(t, serverv1.PoolState_POOL_STATE_ACTIVE, convertPoolToProto(ctx, pool).State)

	pool.Resume() // no-op
	assert.Equal(t, serverv1.PoolState_POOL_STATE_ACTIVE, convertPoolToProto(ctx, pool).State)
}

// TestPool_Run_PausedPoolDoesNotScale pauses the pool before starting Run()
// (the only ordering that keeps isActive race-free per D-06), then proves
// a paused pool ignores TriggerScale with a bounded negative wait. Resume()
// is never called while Run is alive.
func TestPool_Run_PausedPoolDoesNotScale(t *testing.T) {
	fakeBE := &fakeBackend{dir: t.TempDir()}
	fakeGH := &fakeGitHub{installationID: 1, jitConfig: "fake-jit-config"}

	cfg := newTestPoolConfig(t)
	cfg.Replicas = 1

	logger := zerolog.Nop()
	pool, err := NewPool(&logger, cfg, fakeGH, fakeBE)
	require.NoError(t, err)

	pool.Pause() // before Run starts

	runDone := make(chan struct{})
	go func() {
		pool.Run()
		close(runDone)
	}()

	pool.TriggerScale()

	assert.Never(t, func() bool {
		return fakeBE.createCalls.Load() != 0 || pool.GetCurrentSize() != 0
	}, 300*time.Millisecond, 20*time.Millisecond)

	pool.Stop()

	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for Run to return after Stop")
	}
}
