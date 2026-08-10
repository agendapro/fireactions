package firecracker

import (
	"context"
	"fmt"
	"net"

	"github.com/containerd/errdefs"
	fcsdk "github.com/firecracker-microvm/firecracker-go-sdk"
	fcvsock "github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	agentv1 "github.com/hostinger/fireactions/proto/agent/v1"
	"github.com/hostinger/fireactions/server/backend"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// machine is the Firecracker implementation of backend.Machine: a thin
// wrapper around the Firecracker SDK's own machine handle plus the
// host-side bookkeeping needed to tear it down.
type machine struct {
	fc *fcsdk.Machine

	// vsockCID is the guest CID allocated for this machine. It is not read
	// by any method here today; it is kept as per-VM bookkeeping that must
	// stay internal to this package, never surfacing on backend.Machine, so
	// no future backend implementation can be handed a CID it does not own.
	vsockCID uint32

	vsockPath   string
	leaseCancel func(context.Context) error // containerd lease cancel function
	vmmCancel   context.CancelFunc
}

var _ backend.Machine = (*machine)(nil)

// Addr returns the guest's address, or "" when the machine has no network
// interface configured.
func (m *machine) Addr() string {
	addr := ""
	if len(m.fc.Cfg.NetworkInterfaces) > 0 {
		addr = m.fc.Cfg.NetworkInterfaces[0].StaticConfiguration.IPConfiguration.IPAddr.IP.String()
	}

	return addr
}

// ConnectToGuestAgent dials the guest agent's gRPC server over vsock.
func (m *machine) ConnectToGuestAgent(ctx context.Context) (*grpc.ClientConn, agentv1.AgentServiceClient, error) {
	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		return fcvsock.DialContext(ctx, m.vsockPath, 9001)
	}

	// Create gRPC client with VSOCK transport
	// Use "passthrough:" resolver to bypass name resolution and pass directly to dialer
	conn, err := grpc.NewClient(
		"passthrough:vsock",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("grpc dial: %w", err)
	}

	client := agentv1.NewAgentServiceClient(conn)
	return conn, client, nil
}

// Stop stops the VM.
func (m *machine) Stop() error {
	return m.fc.StopVMM()
}

// Wait blocks until the VM has exited.
func (m *machine) Wait(ctx context.Context) error {
	return m.fc.Wait(ctx)
}

// Cleanup releases the host-side resources held for the VM: it cancels the
// VMM context and removes the containerd lease taken for the VM's image
// snapshot. It is called once, after Wait returns. A NotFound error from
// the lease removal is swallowed, matching today's cleanup-goroutine
// behavior in server/pool.go.
func (m *machine) Cleanup(ctx context.Context) error {
	m.vmmCancel()

	err := m.leaseCancel(ctx)
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("containerd: removing lease: %w", err)
	}

	return nil
}
