// Package fake is a dumb, instant, pure-Go implementation of the backend
// interfaces used to exercise the cross-process coordination layer (control ↔
// holder ↔ orchestrator) on a CI runner that cannot boot a real VM. It is linked
// only by internal/orchestrator's backend selector under the fleetbox_fake build
// tag, so it can never enter a normal `go build ./...` artifact (ADR-0018).
//
// The orchestrator constructs the backend internally (newBackend → fake.New) and
// never hands it back, so tests observe behavior through package-global state:
// Reset, Created, Stopped, NetworksClosed, and the FailCreate fault hook. All of
// it is mutex-guarded — the in-process tests and the fake helper's goroutines run
// under the race detector — and tests must Reset between sub-tests, because every
// orchestrator entry point builds a fresh fake.New that writes the same globals.
//
// The fake proves coordination, not that a VM boots: Start/Stop are no-ops,
// WaitForIP returns an unroutable TEST-NET-3 address, and nothing dials the
// guest. Real boot, SSH, and IP discovery stay covered by the VM-boot suites.
package fake

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"

	"github.com/pilat/fleetbox/internal/backend"
)

// EnvFailCreate names the member whose Create the fake should fail. It is the
// cross-process counterpart of FailCreate: the in-process orchestrator test sets
// the fault with FailCreate, while the spawned fake helper (a separate address
// space) reads it from this environment variable.
const EnvFailCreate = "FLEETBOX_FAKE_FAIL_CREATE"

var (
	// mu guards every package-global below. Both the in-process tests (under
	// -race) and the helper's holder goroutines read and write them.
	mu sync.Mutex
	// created records every backend.Config passed to Create, in order. Recording
	// the config is what gives real glue coverage: the orchestrator's
	// FixturePaths/MAC/AssignedIP/disk/seed threading shows up here.
	created []backend.Config
	// stopped records the name of every VM whose Stop was called, in order.
	stopped []string
	// networksClosed counts fakeNetwork.Close calls.
	networksClosed int
	// ipCounter assigns each created VM a distinct TEST-NET-3 host octet.
	ipCounter int
	// failName is the member whose Create FailCreate has armed to fail.
	failName string
)

var (
	_ backend.Backend = (*Backend)(nil)
	_ backend.Network = (*fakeNetwork)(nil)
	_ backend.VM      = (*fakeVM)(nil)
)

// Backend is the fake backend. It holds no state of its own; all observable
// effects land in the package globals so tests can inspect them.
type Backend struct{}

// New creates a fake backend.
func New() *Backend { return &Backend{} }

// CreateNetwork returns a fake network. Its Subnet is empty, mirroring the
// DHCP/vz path so the orchestrator skips static IP allocation (which is unit
// tested separately in ipalloc_test.go).
func (b *Backend) CreateNetwork() (backend.Network, error) {
	return &fakeNetwork{}, nil
}

// Create records cfg and returns a fake VM, unless this member's Create has been
// armed to fail (via FailCreate or EnvFailCreate), in which case it records the
// attempt and returns an error so the orchestrator's create-failure cleanup path
// runs. The returned VM is assigned a deterministic, unroutable TEST-NET-3 IP.
func (b *Backend) Create(cfg backend.Config, _ backend.Network) (backend.VM, error) {
	mu.Lock()
	created = append(created, cfg)
	fail := cfg.Name != "" && (cfg.Name == failName || cfg.Name == os.Getenv(EnvFailCreate))
	ipCounter++
	ip := fmt.Sprintf("203.0.113.%d", ipCounter)
	mu.Unlock()

	if fail {
		return nil, fmt.Errorf("fake backend: forced Create failure for %q", cfg.Name)
	}
	return &fakeVM{name: cfg.Name, ip: ip, state: backend.StateStopped, stopped: make(chan struct{})}, nil
}

// NestedVirtSupported always reports true: the fake never boots a guest, so the
// capability gate must not skip the coordination tests.
func (b *Backend) NestedVirtSupported() bool { return true }

// SupportsClustering always reports true so the multi-member coordination paths
// (StartN/StartCluster, Cluster.Add) are reachable.
func (b *Backend) SupportsClustering() bool { return true }

// Reconcile is a no-op: the fake owns no host resources.
func (b *Backend) Reconcile() error { return nil }

// Reset clears all recorded state and the armed fault. Tests must call it between
// sub-tests, because each orchestrator entry point builds a fresh fake.New that
// writes these same globals.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	created = nil
	stopped = nil
	networksClosed = 0
	ipCounter = 0
	failName = ""
}

// Created returns a copy of every backend.Config passed to Create, in order.
func Created() []backend.Config {
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(created)
}

// Stopped returns a copy of the names of every VM whose Stop was called, in order.
func Stopped() []string {
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(stopped)
}

// NetworksClosed returns how many times a fake network was closed.
func NetworksClosed() int {
	mu.Lock()
	defer mu.Unlock()
	return networksClosed
}

// FailCreate arms the fake so that Create for the named member returns an error.
// It latches: every Create for that member keeps failing until it is disarmed
// (pass the empty string) or cleared by Reset — matching the cross-process
// EnvFailCreate path, which stays armed for the helper's lifetime.
func FailCreate(name string) {
	mu.Lock()
	defer mu.Unlock()
	failName = name
}

// fakeNetwork is the fake's Network handle.
type fakeNetwork struct{}

// Close records that the network was released.
func (n *fakeNetwork) Close() error {
	mu.Lock()
	networksClosed++
	mu.Unlock()
	return nil
}

// Subnet returns the empty string, the signal the orchestrator uses to skip
// static IP allocation (the DHCP/vz path).
func (n *fakeNetwork) Subnet() string { return "" }

// fakeVM is the fake's VM handle. It tracks its state and records its Stop.
type fakeVM struct {
	mu       sync.Mutex
	name     string
	ip       string
	state    backend.State
	stopped  chan struct{}
	stopOnce sync.Once
}

// Start transitions the VM to running instantly.
func (v *fakeVM) Start(_ context.Context) error {
	v.mu.Lock()
	v.state = backend.StateRunning
	v.mu.Unlock()
	return nil
}

// Stop transitions the VM to stopped, records its name, and unblocks Wait.
func (v *fakeVM) Stop(_ context.Context) error {
	v.mu.Lock()
	v.state = backend.StateStopped
	name := v.name
	v.mu.Unlock()

	mu.Lock()
	stopped = append(stopped, name)
	mu.Unlock()

	v.stopOnce.Do(func() { close(v.stopped) })
	return nil
}

// State returns the VM's current state.
func (v *fakeVM) State() backend.State {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state
}

// Wait blocks until Stop is called or ctx is done.
func (v *fakeVM) Wait(ctx context.Context) error {
	select {
	case <-v.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitForIP returns the VM's deterministic, unroutable TEST-NET-3 address
// instantly. The value is the fake's own invention, so tests assert only that it
// is present, never the value itself.
func (v *fakeVM) WaitForIP(_ context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.ip, nil
}
