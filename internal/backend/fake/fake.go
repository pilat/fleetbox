// Package fake is a dumb, instant, pure-Go implementation of the backend
// interfaces used to exercise the cross-process coordination layer (client ↔
// helper ↔ holder) on a CI runner that cannot boot a real VM. It is linked only
// by internal/holder's backend selector under the fleetbox_fake build tag, so it
// can never enter a normal `go build ./...` artifact (ADR-0018/0020).
//
// Since ADR-0020 the fake lives BEHIND the helper: a helper built -tags
// fleetbox_fake uses fake.New(), and the test drives the real client↔helper
// protocol against it. The fake runs in the helper's address space, so a test in
// the client process cannot read in-process globals. Instead, when
// FLEETBOX_FAKE_RECORD names a file, the fake appends one JSON line per backend
// call (reserve/create/stop/close) to it; the test reads that file afterwards to
// assert what the helper's backend actually received (the orchestrator's
// disk/seed/fixture/MAC/IP threading). Everything else a test needs is observable
// over the protocol (status: state/IP) and on disk (member dirs, config.json).
//
// The fake proves coordination, not that a VM boots: Start/Stop are no-ops,
// WaitForIP returns an unroutable TEST-NET-3 address, and nothing dials the guest.
// Its Subnet is empty (mimicking macOS DHCP), so the Linux static-IP allocation
// path is NOT exercised here — that stays covered by real cloud-hypervisor in
// vm-linux.yml. Real boot, SSH, and IP discovery stay covered by the VM-boot suites.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/pilat/fleetbox/internal/backend"
)

const (
	// EnvFailCreate names the member whose Create the fake should fail. The spawned
	// fake helper (a separate address space from the test) reads it from the
	// environment to inject a boot failure for the orchestrator's rollback paths.
	EnvFailCreate = "FLEETBOX_FAKE_FAIL_CREATE"

	// EnvFakeRecord names a file the fake appends one JSON record per backend call
	// to. It is the cross-process observation channel: the test sets it, drives the
	// protocol, then reads the file to assert the args the helper's backend saw.
	EnvFakeRecord = "FLEETBOX_FAKE_RECORD"
)

var (
	// mu guards ipCounter and serializes record-file appends (the helper's holder
	// goroutines run under -race).
	mu sync.Mutex
	// ipCounter assigns each created VM a distinct TEST-NET-3 host octet.
	ipCounter int
)

var (
	_ backend.Backend = (*Backend)(nil)
	_ backend.Network = (*fakeNetwork)(nil)
	_ backend.VM      = (*fakeVM)(nil)
)

// record is one line in the FLEETBOX_FAKE_RECORD file: the op plus whatever fields
// that op carries. Tests parse it with a matching local struct (no import of this
// package needed).
type record struct {
	Op           string   `json:"op"`
	Name         string   `json:"name,omitempty"`
	DiskPath     string   `json:"disk_path,omitempty"`
	SeedPath     string   `json:"seed_path,omitempty"`
	EFIPath      string   `json:"efi_path,omitempty"`
	FixturePaths []string `json:"fixture_paths,omitempty"`
	AssignedIP   string   `json:"assigned_ip,omitempty"`
	MAC          string   `json:"mac,omitempty"`
	IPHint       string   `json:"ip_hint,omitempty"`
}

// recordOp appends rec to the FLEETBOX_FAKE_RECORD file as one JSON line. It is a
// no-op when the env var is unset (the VM-boot path never records). Errors are
// swallowed: recording is a test aid, never load-bearing for a boot.
func recordOp(rec record) {
	path := os.Getenv(EnvFakeRecord)
	if path == "" {
		return
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(line, '\n'))
}

// Backend is the fake backend. It holds no state of its own; observable effects
// land in the FLEETBOX_FAKE_RECORD file and over the protocol.
type Backend struct{}

// New creates a fake backend.
func New() *Backend { return &Backend{} }

// CreateNetwork returns a fake network. Its Subnet is empty, mirroring the
// DHCP/vz path so no static IP is allocated.
func (b *Backend) CreateNetwork() (backend.Network, error) {
	return &fakeNetwork{}, nil
}

// Create records the config it received and returns a fake VM, unless this
// member's Create has been armed to fail via EnvFailCreate, in which case it
// records the attempt and returns an error so the orchestrator's create-failure
// cleanup path runs. The returned VM is assigned a deterministic, unroutable
// TEST-NET-3 IP.
func (b *Backend) Create(cfg backend.Config, _ backend.Network) (backend.VM, error) {
	recordOp(record{
		Op:           "create",
		Name:         cfg.Name,
		DiskPath:     cfg.DiskPath,
		SeedPath:     cfg.SeedPath,
		EFIPath:      cfg.EFIPath,
		FixturePaths: cfg.FixturePaths,
		AssignedIP:   cfg.AssignedIP,
		MAC:          cfg.MAC,
	})

	mu.Lock()
	ipCounter++
	ip := fmt.Sprintf("203.0.113.%d", ipCounter)
	mu.Unlock()

	if cfg.Name != "" && cfg.Name == os.Getenv(EnvFailCreate) {
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

// fakeNetwork is the fake's Network handle.
type fakeNetwork struct{}

// Close records that the network was released.
func (n *fakeNetwork) Close() error {
	recordOp(record{Op: "close"})
	return nil
}

// Subnet returns the empty string, the signal to skip static IP allocation (the
// DHCP/vz path).
func (n *fakeNetwork) Subnet() string { return "" }

// Reserve records the request and mimics the DHCP/vz path: no static IP, just the
// deterministic MAC. The fake therefore never exercises Linux static-IP
// allocation — that path is covered by real cloud-hypervisor in vm-linux.yml.
func (n *fakeNetwork) Reserve(name, ipHint string) (ip, mac string, err error) {
	mac = backend.GenerateMAC(name)
	recordOp(record{Op: "reserve", Name: name, IPHint: ipHint, MAC: mac})
	return "", mac, nil
}

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

	recordOp(record{Op: "stop", Name: name})
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
