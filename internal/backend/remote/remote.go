// Package remote is a pure-Go backend.Backend that drives a spawned helper over
// the control protocol instead of touching a hypervisor directly. It is the
// client half of the ADR-0020 inversion: the orchestrator links THIS backend, so
// its import graph carries no vz/cloud-hypervisor (the macOS sever, now uniform);
// the real backend lives in the helper, reached by RPC.
//
// The mapping to the wire protocol:
//   - CreateNetwork  -> createnetwork RPC (returns the subnet)
//   - Network.Reserve-> reserve RPC (returns the member's {ip, mac})
//   - Create         -> stash the resolved spec (no RPC yet)
//   - VM.Start       -> boot-member RPC (the helper does backend.Create+Start)
//   - VM.WaitForIP   -> poll the per-member status until running with an IP
//   - VM.Stop        -> stop RPC
//
// Cluster-level RPCs (createnetwork/reserve/boot-member) travel over the primary
// member's socket; per-member status/stop travel over the target's own socket.
// NestedVirtSupported/SupportsClustering are NOT routed here — the client answers
// host capability with a pure-Go heuristic before any spawn (ADR-0017, R7), so
// the orchestrator never consults them on this backend.
package remote

import (
	"context"
	"fmt"
	"time"

	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/control"
	"github.com/pilat/fleetbox/internal/store"
)

const (
	// rpcTimeout bounds the short cluster-level RPCs (createnetwork/reserve).
	rpcTimeout = 30 * time.Second
	// bootTimeout bounds the boot-member RPC: the helper may fetch its VMM binary
	// (first run) and boot the guest before replying, so it is generous.
	bootTimeout = 30 * time.Minute
	// ipPollInterval is how often WaitForIP/Wait re-read the member's status.
	ipPollInterval = 500 * time.Millisecond
)

var (
	_ backend.Backend = (*Backend)(nil)
	_ backend.Network = (*Network)(nil)
	_ backend.VM      = (*VM)(nil)
)

// Backend translates backend.Backend calls into control-protocol RPCs against a
// helper the caller has already spawned. primary is the member whose socket
// carries the cluster-level RPCs (it always exists for the cluster's lifetime).
type Backend struct {
	st      *store.Store
	primary string
}

// New creates a remote-proxy backend that drives the helper serving `primary`
// over the store's sockets.
func New(st *store.Store, primary string) *Backend {
	return &Backend{st: st, primary: primary}
}

// CreateNetwork asks the helper to create the cluster's shared network and
// returns a Network carrying the reported subnet (empty on the DHCP/vz path).
func (b *Backend) CreateNetwork() (backend.Network, error) {
	resp, err := control.SendCommand(b.st, b.primary, control.Request{Cmd: control.CmdCreateNetwork}, rpcTimeout)
	if err != nil {
		return nil, fmt.Errorf("create network: %w", err)
	}
	return &Network{st: b.st, primary: b.primary, subnet: resp.Subnet}, nil
}

// Create stashes the resolved spec and returns a VM handle; the boot-member RPC
// that actually creates and starts the guest is sent by VM.Start, matching the
// orchestrator's Create-then-Start order while keeping the helper's create+start
// atomic.
func (b *Backend) Create(cfg backend.Config, _ backend.Network) (backend.VM, error) {
	return &VM{
		st:      b.st,
		primary: b.primary,
		name:    cfg.Name,
		spec: control.MemberSpec{
			Name:          cfg.Name,
			DiskPath:      cfg.DiskPath,
			SeedPath:      cfg.SeedPath,
			FixturePaths:  cfg.FixturePaths,
			EFIPath:       cfg.EFIPath,
			CPUs:          cfg.CPUs,
			MemoryBytes:   cfg.MemoryBytes,
			SerialLogPath: cfg.SerialLogPath,
		},
	}, nil
}

// NestedVirtSupported satisfies the interface but is never consulted on the
// remote backend — the client decides host capability without the helper (R7).
func (b *Backend) NestedVirtSupported() bool { return true }

// SupportsClustering satisfies the interface but is never consulted on the remote
// backend — the root package's pure-Go host gate decides it before any spawn.
func (b *Backend) SupportsClustering() bool { return true }

// Reconcile is a no-op here: prune drives a dedicated short-lived reconcile
// helper, not the remote proxy (ADR-0013/0020).
func (b *Backend) Reconcile() error { return nil }

// Network is a handle to the helper's shared cluster network.
type Network struct {
	st      *store.Store
	primary string
	subnet  string
}

// Close is a no-op: the helper owns the live network and tears it down on its own
// teardown (death-watch or last member stopped). The client has no close RPC —
// closing the network from under live siblings would be a bug (mirrors vz GC).
func (n *Network) Close() error { return nil }

// Subnet returns the helper-reported CIDR (empty on the DHCP/vz path).
func (n *Network) Subnet() string { return n.subnet }

// Reserve asks the helper to allocate the member's address on the live network
// and returns the {ip, mac} the client bakes into the seed (Decisions 5, 6).
func (n *Network) Reserve(name, ipHint string) (ip, mac string, err error) {
	resp, err := control.SendCommand(n.st, n.primary,
		control.Request{Cmd: control.CmdReserve, Name: name, IPHint: ipHint}, rpcTimeout)
	if err != nil {
		return "", "", fmt.Errorf("reserve %s: %w", name, err)
	}
	if resp.Reservation == nil {
		return "", "", fmt.Errorf("reserve %s: helper returned no reservation", name)
	}
	return resp.Reservation.IP, resp.Reservation.MAC, nil
}

// VM is a handle to one member booted by the helper.
type VM struct {
	st      *store.Store
	primary string
	name    string
	spec    control.MemberSpec
}

// Start sends the boot-member RPC; the helper creates and starts the guest on the
// shared network using the address it reserved for this member, then replies. The
// member's runtime IP (vz DHCP) is discovered helper-side and surfaced by status,
// so callers follow Start with WaitForIP.
func (v *VM) Start(_ context.Context) error {
	_, err := control.SendCommand(v.st, v.primary,
		control.Request{Cmd: control.CmdBootMember, Spec: &v.spec}, bootTimeout)
	if err != nil {
		return fmt.Errorf("boot member %s: %w", v.name, err)
	}
	return nil
}

// Stop sends the stop RPC and waits for the member to retire (control.Stop polls
// its pidfile).
func (v *VM) Stop(_ context.Context) error {
	if err := control.Stop(v.st, v.name); err != nil {
		return fmt.Errorf("stop %s: %w", v.name, err)
	}
	return nil
}

// State reports the member's state from the helper, mapped onto backend.State. A
// missing/unreachable holder reads as stopped.
func (v *VM) State() backend.State {
	status, err := control.GetStatus(v.st, v.name)
	if err != nil {
		return backend.StateStopped
	}
	switch status.State {
	case control.StateRunning:
		return backend.StateRunning
	case control.StateStarting, control.StateDownloading:
		return backend.StateStarting
	case control.StateError:
		return backend.StateError
	default:
		return backend.StateStopped
	}
}

// Wait blocks until the member is no longer running (stopped or errored) or ctx
// is done, by polling status.
func (v *VM) Wait(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// A missing/unreachable holder means the member is gone — Wait is satisfied,
		// not failed (this mirrors State reading an absent holder as stopped).
		status, err := control.GetStatus(v.st, v.name)
		if err != nil {
			return nil //nolint:nilerr // holder gone == member stopped; Wait is done
		}
		if status.State == control.StateStopped || status.State == control.StateError {
			return nil
		}
		time.Sleep(ipPollInterval)
	}
}

// WaitForIP polls the member's status until it is running with an IP (the helper
// discovers a vz VM's DHCP address post-boot and reports it; a cloud-hypervisor
// VM reports its static address once reachable). It surfaces a helper-side boot
// error as soon as status reports it.
func (v *VM) WaitForIP(ctx context.Context) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		status, err := control.GetStatus(v.st, v.name)
		if err == nil {
			if status.State == control.StateError {
				return "", fmt.Errorf("member %s failed: %s", v.name, status.Error)
			}
			if status.State == control.StateRunning && status.IP != "" {
				return status.IP, nil
			}
		}
		time.Sleep(ipPollInterval)
	}
}
