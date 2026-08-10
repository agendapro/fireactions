package server

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hostinger/fireactions/helper/stringid"
	"github.com/hostinger/fireactions/server/backend"
	"github.com/rs/zerolog"
)

// Pool represents a pool of VMs that are used to run GitHub Actions jobs.
type Pool struct {
	config         *PoolConfig
	backend        backend.Backend
	github         githubAPI
	pendingCreates atomic.Int32
	pendingDeletes atomic.Int32
	machinesMu     *sync.Mutex
	machines       map[string]*Machine
	installationID atomic.Int64
	logger         *zerolog.Logger
	replicas       atomic.Int32
	isActive       bool
	scaleTrigger   chan struct{}
	stopCh         chan struct{}
	doneCh         chan struct{}
	cleanupWg      sync.WaitGroup
	ctx            context.Context
	cancel         context.CancelFunc
	dir            string
	l              *sync.Mutex
}

// PoolConfig represents the configuration of a Pool.
type PoolConfig struct {
	Name           string             `yaml:"name" validate:"required"`
	ShutdownOnExit *bool              `yaml:"shutdown_on_exit"`
	Replicas       int                `yaml:"replicas" validate:"min=0"`
	Runner         *RunnerConfig      `yaml:"runner" validate:"required"`
	Firecracker    *FirecrackerConfig `yaml:"firecracker" validate:"required"`
}

// UnmarshalYAML implements custom unmarshaling to set defaults.
func (p *PoolConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type poolConfigAlias PoolConfig
	defaults := poolConfigAlias{
		ShutdownOnExit: func() *bool { b := true; return &b }(),
	}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	*p = PoolConfig(defaults)
	return nil
}

// NewPool creates a new Pool.
func NewPool(logger *zerolog.Logger, config *PoolConfig, github githubAPI, b backend.Backend) (*Pool, error) {
	l := logger.With().Str("pool", config.Name).Logger()

	ctx, cancel := context.WithCancel(context.Background())

	p := &Pool{
		config:       config,
		l:            &sync.Mutex{},
		machinesMu:   &sync.Mutex{},
		machines:     make(map[string]*Machine),
		isActive:     true,
		backend:      b,
		github:       github,
		logger:       &l,
		scaleTrigger: make(chan struct{}, 1),
		stopCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
	}

	p.replicas.Store(int32(config.Replicas))

	dir, err := b.PrepareStateDir(config.Name)
	if err != nil {
		return nil, err
	}
	p.dir = dir

	p.logger.Debug().Msgf("Pool directory created at %s", dir)

	orphans, err := b.ReconcileOrphans(ctx, config.Name)
	if err != nil {
		return nil, fmt.Errorf("reconciling orphaned machine state: %w", err)
	}

	if len(orphans) > 0 {
		p.logger.Info().Msgf("Reconciled %d orphaned machines", len(orphans))
	}

	metricPoolRunnersCurrent.
		WithLabelValues(p.config.Name, p.config.Runner.Organization).Set(float64(p.GetCurrentSize()))
	metricPoolRunnersDesired.
		WithLabelValues(p.config.Name, p.config.Runner.Organization).Set(float64(p.config.Replicas))
	metricPoolStatus.
		WithLabelValues(p.config.Name).Set(1)

	metricPoolsTotal.Inc()

	return p, nil
}

// Run starts the pool. Starting the pool will start the scaling process.
func (p *Pool) Run() {
	defer close(p.doneCh) // Signal that Run() has exited

	// Trigger initial scale
	p.TriggerScale()

	for {
		select {
		case <-p.scaleTrigger:
		case <-time.After(2 * time.Second):
		case <-p.stopCh:
			return
		case <-p.ctx.Done():
			return
		}

		// Check if we should stop before scaling (non-blocking check)
		select {
		case <-p.ctx.Done():
			return
		case <-p.stopCh:
			return
		default:
		}

		curSize := p.GetCurrentSize()
		desiredReplicas := p.GetReplicas()
		pendingCreates := int(p.pendingCreates.Load())
		pendingDeletes := int(p.pendingDeletes.Load())
		netPending := pendingCreates - pendingDeletes
		metricPoolRunnersCurrent.
			WithLabelValues(p.config.Name, p.config.Runner.Organization).Set(float64(curSize))
		metricPoolRunnersDesired.
			WithLabelValues(p.config.Name, p.config.Runner.Organization).Set(float64(desiredReplicas))
		metricPoolRunnersPending.
			WithLabelValues(p.config.Name, p.config.Runner.Organization).Set(float64(netPending))

		if !p.isActive {
			p.logger.Debug().Msgf("Pool %s is paused, skipping scaling", p.config.Name)
			continue
		}

		// Scale to desired replicas
		if err := p.Scale(p.ctx, desiredReplicas); err != nil {
			// Don't log errors if context was cancelled (pool is stopping)
			if p.ctx.Err() == nil {
				p.logger.Error().Err(err).Msg("Failed to scale pool")
			}
		}
	}
}

// Stop stops the pool. Stopping the pool will stop all the VMs in the pool.
func (p *Pool) Stop() {
	p.logger.Debug().Msgf("Stopping pool %s", p.config.Name)
	p.cancel()

	// Signal the Start() loop to exit (non-blocking)
	select {
	case p.stopCh <- struct{}{}:
	default:
		// Channel already has a value or Start() already exited
	}

	// Wait for Run() loop to exit cleanly with a timeout
	select {
	case <-p.doneCh:
	case <-time.After(5 * time.Second):
		p.logger.Warn().Msg("Timeout waiting for Run() to exit")
	}

	p.logger.Debug().Msgf("Stopping %d machines in pool %s", len(p.machines), p.config.Name)

	type namedMachine struct {
		name    string
		machine *Machine
	}

	p.machinesMu.Lock()
	machines := make([]namedMachine, 0, len(p.machines))
	for name, machine := range p.machines {
		machines = append(machines, namedMachine{name: name, machine: machine})
	}
	p.machinesMu.Unlock()

	// Stop all machines - cleanup goroutines will handle the rest
	for _, nm := range machines {
		err := nm.machine.Stop()
		if err != nil {
			p.logger.Error().Err(err).Msgf("Failed to stop VM %s", nm.name)
		}

		p.logger.Debug().Msgf("Stopped VM %s", nm.name)
	}

	cleanupDone := make(chan struct{})
	go func() {
		p.cleanupWg.Wait()
		close(cleanupDone)
	}()

	select {
	case <-cleanupDone:
	case <-time.After(35 * time.Second):
		p.logger.Warn().Msg("Timeout waiting for cleanup goroutines to finish")
	}

	p.logger.Debug().Msgf("Pool %s stopped", p.config.Name)
}

// GetDir returns the directory where the backend keeps this pool's per-VM state.
func (p *Pool) GetDir() string {
	return p.dir
}

// Scale scales the pool to the desired size.
func (p *Pool) Scale(ctx context.Context, desiredReplicas int) error {
	p.l.Lock()
	defer p.l.Unlock()

	select {
	case <-p.ctx.Done():
		return p.ctx.Err()
	default:
	}

	curSize := p.GetCurrentSize()
	pendingCreates := int(p.pendingCreates.Load())
	pendingDeletes := int(p.pendingDeletes.Load())

	// Calculate effective size accounting for in-flight operations
	effectiveSize := curSize + pendingCreates - pendingDeletes
	delta := desiredReplicas - effectiveSize

	if delta == 0 {
		return nil
	}

	if delta > 0 {
		p.scaleUp(
			ctx, delta, desiredReplicas, curSize, pendingCreates, pendingDeletes)
	} else {
		p.scaleDown(
			ctx, -delta, desiredReplicas, curSize, pendingCreates, pendingDeletes)
	}

	return nil
}

func (p *Pool) scaleUp(ctx context.Context, count, desiredReplicas, curSize, pendingCreates, pendingRemovals int) {
	p.logger.Debug().Msgf("Scaling up by %d VMs (target: %d, current: %d, pending creates: %d, pending removals: %d)",
		count, desiredReplicas, curSize, pendingCreates, pendingRemovals)

	for i := 0; i < count; i++ {
		p.pendingCreates.Add(1)

		go func() {
			defer p.pendingCreates.Add(-1)

			select {
			case <-p.ctx.Done():
				return
			default:
			}

			start := time.Now()
			if err := p.createMachine(ctx); err != nil {
				metricScaleOperations.WithLabelValues(p.config.Name, p.config.Runner.Organization, "up", "failure").Inc()
				p.logger.Error().Err(err).Msg("Failed to create machine")
				return
			}

			duration := time.Since(start).Seconds()
			metricScaleOperations.WithLabelValues(p.config.Name, p.config.Runner.Organization, "up", "success").Inc()
			metricScaleDuration.WithLabelValues(p.config.Name, p.config.Runner.Organization, "up").Observe(duration)
		}()
	}
}

func (p *Pool) scaleDown(ctx context.Context, count, desiredReplicas, curSize, pendingCreates, pendingDeletes int) {
	if count > curSize {
		count = curSize
	}

	p.logger.Debug().Msgf("Scaling down by %d VMs (target: %d, current: %d, pending creates: %d, pending deletes: %d)",
		count, desiredReplicas, curSize, pendingCreates, pendingDeletes)

	for i := 0; i < count; i++ {
		p.pendingDeletes.Add(1)

		go func() {
			defer p.pendingDeletes.Add(-1)

			select {
			case <-p.ctx.Done():
				return
			default:
			}

			start := time.Now()
			if err := p.deleteMachine(ctx); err != nil {
				metricScaleOperations.WithLabelValues(p.config.Name, p.config.Runner.Organization, "down", "failure").Inc()
				p.logger.Error().Err(err).Msg("Failed to delete machine")
				return
			}

			duration := time.Since(start).Seconds()
			metricScaleOperations.WithLabelValues(p.config.Name, p.config.Runner.Organization, "down", "success").Inc()
			metricScaleDuration.WithLabelValues(p.config.Name, p.config.Runner.Organization, "down").Observe(duration)
		}()
	}
}

// Pause pauses the pool. Pausing the pool will prevent the pool from scaling.
func (p *Pool) Pause() {
	if !p.isActive {
		return
	}

	p.logger.Debug().Msgf("Pool %s state changed to paused", p.config.Name)
	p.isActive = false
}

// Resume resumes the pool. Resuming the pool will allow the pool to scale.
func (p *Pool) Resume() {
	if p.isActive {
		return
	}

	p.logger.Debug().Msgf("Pool %s state changed to active", p.config.Name)
	p.isActive = true
}

// SetReplicas updates the desired replica count for the pool in a thread-safe manner.
func (p *Pool) SetReplicas(replicas int) {
	p.replicas.Store(int32(replicas))
	p.TriggerScale()
}

// TriggerScale sends a non-blocking notification to trigger scaling.
func (p *Pool) TriggerScale() {
	select {
	case p.scaleTrigger <- struct{}{}:
	default:
	}
}

// GetReplicas returns the desired replica count for the pool in a thread-safe manner.
func (p *Pool) GetReplicas() int {
	return int(p.replicas.Load())
}

// GetCurrentSize returns the current size of the pool.
func (p *Pool) GetCurrentSize() int {
	p.machinesMu.Lock()
	defer p.machinesMu.Unlock()
	return len(p.machines)
}

func (p *Pool) ListMachines(ctx context.Context) ([]*Machine, error) {
	p.machinesMu.Lock()
	defer p.machinesMu.Unlock()

	machines := make([]*Machine, 0, len(p.machines))
	for _, machine := range p.machines {
		machines = append(machines, machine)
	}

	return machines, nil
}

func (p *Pool) GetMachine(name string) (*Machine, error) {
	p.machinesMu.Lock()
	defer p.machinesMu.Unlock()

	machine, ok := p.machines[name]
	if !ok {
		return nil, fmt.Errorf("machine not found: %s", name)
	}

	return machine, nil
}

func (p *Pool) createMachine(ctx context.Context) error {
	image, err := p.backend.EnsureImage(ctx, p.config.Runner.Image, p.config.Runner.ImagePullPolicy)
	if err != nil {
		return fmt.Errorf("ensuring image: %w", err)
	}

	runnerName := fmt.Sprintf("%s-%s", p.config.Runner.Name, stringid.New())

	installationID := p.installationID.Load()
	if installationID == 0 {
		id, err := p.github.FindOrganizationInstallation(ctx, p.config.Runner.Organization)
		if err != nil {
			return fmt.Errorf("github: %w", err)
		}
		installationID = id

		p.installationID.Store(installationID)
	}

	jitConfig, runnerID, jitRunnerName, err := p.github.GenerateOrgJITConfig(ctx, installationID, p.config.Runner.Organization, runnerName, p.config.Runner.GroupID, p.config.Runner.Labels)
	if err != nil {
		return fmt.Errorf("github: %w", err)
	}

	spec := backend.VMSpec{
		Pool:           p.config.Name,
		Name:           runnerName,
		Hostname:       runnerName,
		JITConfig:      jitConfig,
		ShutdownOnExit: *p.config.ShutdownOnExit,
		VCPUCount:      p.config.Firecracker.MachineConfig.VcpuCount,
		MemSizeMib:     p.config.Firecracker.MachineConfig.MemSizeMib,
		Metadata:       p.config.Firecracker.Metadata,
		Firecracker: backend.FirecrackerSpec{
			BinaryPath:      p.config.Firecracker.BinaryPath,
			KernelImagePath: p.config.Firecracker.KernelImagePath,
			KernelArgs:      p.config.Firecracker.KernelArgs,
		},
	}

	// The backend already wraps CreateMachine errors with its own subsystem
	// prefix (e.g. "firecracker: creating machine: %w"), so returning err
	// unwrapped here keeps the message byte-identical to today's.
	bm, err := p.backend.CreateMachine(p.ctx, spec, image)
	if err != nil {
		p.deleteGitHubRunner(runnerName, runnerID)
		return err
	}

	p.logger.Info().Msgf("Successfully created VM %s", runnerName)

	machine := &Machine{
		Machine:   bm,
		Name:      jitRunnerName,
		RunnerID:  runnerID,
		Pool:      p.config.Name,
		CreatedAt: time.Now().UTC(),
	}

	p.machinesMu.Lock()
	p.machines[runnerName] = machine
	p.machinesMu.Unlock()

	// Start cleanup goroutine
	p.cleanupWg.Add(1)
	go func() {
		defer p.cleanupWg.Done()

		waitDone := make(chan error, 1)
		go func() {
			waitDone <- machine.Wait(context.Background())
		}()

		select {
		case <-waitDone:
			// Machine exited normally
		case <-p.ctx.Done():
			// Pool is stopping, wait up to 30s for machine to fully exit
			select {
			case <-waitDone:
			case <-time.After(30 * time.Second):
				p.logger.Warn().Msgf("Timeout waiting for machine %s to exit during pool shutdown", runnerName)
			}
		}

		p.machinesMu.Lock()
		_, exists := p.machines[runnerName]
		if exists {
			delete(p.machines, runnerName)
		}
		p.machinesMu.Unlock()

		p.deleteGitHubRunner(runnerName, machine.RunnerID)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := machine.Cleanup(ctx); err != nil {
			p.logger.Error().Err(err).Msgf("Failed to clean up machine %s", runnerName)
		}

		p.logger.Info().Msgf("Successfully cleaned up exited VM %s", runnerName)
	}()

	return nil
}

// removeMachine removes a single machine from the pool.
func (p *Pool) deleteMachine(_ context.Context) error {
	p.machinesMu.Lock()

	// Find a machine to remove (pick the first one)
	var targetMachine *Machine
	var targetName string
	for name, machine := range p.machines {
		targetMachine = machine
		targetName = name
		break
	}

	if targetMachine == nil {
		p.machinesMu.Unlock()
		return fmt.Errorf("no machines available to scale down")
	}

	// Remove from map immediately to prevent selecting the same machine multiple times
	delete(p.machines, targetName)
	p.machinesMu.Unlock()

	err := targetMachine.Stop()
	if err != nil {
		p.logger.Warn().Err(err).Msgf("Failed to stop VM %s", targetName)
		return err
	}

	p.logger.Info().Msgf("Successfully removed VM %s", targetName)
	return nil
}

// deleteGitHubRunner removes a runner from GitHub Actions
func (p *Pool) deleteGitHubRunner(runnerName string, runnerID int64) {
	if runnerID == 0 {
		p.logger.Debug().Msgf("No GitHub runner ID found for %s, skipping deletion", runnerName)
		return
	}

	if p.installationID.Load() == 0 {
		p.logger.Warn().Msgf("No installation ID available, cannot delete runner %s", runnerName)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := p.github.RemoveOrganizationRunner(ctx, p.installationID.Load(), p.config.Runner.Organization, runnerID)
	if err != nil {
		p.logger.Error().Err(err).Msgf("Failed to delete GitHub runner %s (ID: %d)", runnerName, runnerID)
		return
	}

	p.logger.Debug().Msgf("Successfully deleted GitHub runner %s (ID: %d)", runnerName, runnerID)
}
