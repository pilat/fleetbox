//go:build linux

// Package cloudhypervisor implements the backend interface using
// cloud-hypervisor on Linux. It boots a stock cloud image with the pinned
// rust-hypervisor-firmware and controls the VM over cloud-hypervisor's REST API
// on a per-VM unix socket, using only the Go standard library — no cgo. It is
// the only package that knows cloud-hypervisor specifics (ADR-0002, ADR-0011).
package cloudhypervisor

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/pilat/fleetbox/internal/backend"
)

var (
	_ backend.Backend = (*Backend)(nil)
	_ backend.Network = (*chNetwork)(nil)
	_ backend.VM      = (*VM)(nil)
)

// Backend implements the cloud-hypervisor backend.
type Backend struct {
	// binDir is where the pinned cloud-hypervisor binary and firmware are cached
	// (typically ~/.fleetbox/bin); the root package injects it so this package
	// does not depend on internal/store.
	binDir string
	// netDir is where per-network write-ahead records and the ip_forward marker
	// live (typically ~/.fleetbox/networks); injected for the same reason. The
	// records let a crashed cluster's bridges/taps/rules be reclaimed (ADR-0013).
	netDir string
}

// New creates a cloud-hypervisor backend caching its binary and firmware under
// binDir and keeping network teardown records under netDir.
func New(binDir, netDir string) *Backend {
	return &Backend{binDir: binDir, netDir: netDir}
}

// NestedVirtSupported reports whether /dev/kvm exists and KVM nested
// virtualization is enabled — what consumers running KVM inside guests need.
func (b *Backend) NestedVirtSupported() bool {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return false
	}
	return kvmNestedEnabled()
}

// SupportsClustering is always true on Linux: cluster members share one bridge
// and reach each other (ADR-0011), unlike the macOS <26 NAT path.
func (b *Backend) SupportsClustering() bool {
	return true
}

// Reconcile removes the host resources (bridges, taps, iptables rules) of every
// network whose owning holder is no longer alive, and restores ip_forward once
// nothing of ours remains. It is the engine behind `fleetbox prune`; the same
// sweep runs automatically at the start of each CreateNetwork so orphans from a
// crashed holder self-heal on the next up (ADR-0013).
func (b *Backend) Reconcile() error {
	return b.reconcile(true)
}

// Create builds (but does not boot) a cloud-hypervisor VM attached to nw: it
// ensures the pinned binaries, checks /dev/kvm, and creates a tap enslaved to
// the cluster bridge. Boot happens in VM.Start.
func (b *Backend) Create(cfg backend.Config, nw backend.Network) (backend.VM, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	chNet, ok := nw.(*chNetwork)
	if !ok {
		return nil, fmt.Errorf("network is not a cloud-hypervisor network: %T", nw)
	}

	chBin, fwPath, err := ensureBinaries(b.binDir)
	if err != nil {
		return nil, fmt.Errorf("ensure binaries: %w", err)
	}

	if err := checkKVM(); err != nil {
		return nil, err
	}

	// Tap creation is the last fallible step, so an error before it leaves no
	// interface to clean up.
	tap, err := chNet.createTap()
	if err != nil {
		return nil, fmt.Errorf("create tap: %w", err)
	}

	return newVM(cfg, chNet, chBin, fwPath, tap), nil
}

func validateConfig(cfg backend.Config) error {
	if cfg.Name == "" {
		return errors.New("name is required")
	}
	if cfg.DiskPath == "" {
		return errors.New("disk path is required")
	}
	if cfg.SeedPath == "" {
		return errors.New("seed path is required")
	}
	if cfg.AssignedIP == "" {
		return errors.New("assigned IP is required (the Linux backend uses static addressing)")
	}
	if cfg.CPUs < 1 {
		return errors.New("cpus must be at least 1")
	}
	if cfg.MemoryBytes < 256*1024*1024 {
		return errors.New("memory must be at least 256 MB")
	}
	if _, err := os.Stat(cfg.DiskPath); err != nil {
		return fmt.Errorf("disk not found: %s", cfg.DiskPath)
	}
	if _, err := os.Stat(cfg.SeedPath); err != nil {
		return fmt.Errorf("seed not found: %s", cfg.SeedPath)
	}
	return nil
}

// checkKVM opens /dev/kvm so a missing or inaccessible device fails with a clear,
// actionable error rather than an opaque cloud-hypervisor launch failure.
func checkKVM() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/kvm (is KVM available and are you in the kvm group?): %w", err)
	}
	_ = f.Close()
	return nil
}

// kvmNestedEnabled reports whether the kvm_intel/kvm_amd nested parameter is on.
func kvmNestedEnabled() bool {
	for _, p := range []string{
		"/sys/module/kvm_intel/parameters/nested",
		"/sys/module/kvm_amd/parameters/nested",
	} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(data)) {
		case "Y", "1":
			return true
		}
	}
	return false
}
