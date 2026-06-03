//go:build darwin && arm64

// Package vz implements the backend interface using Apple Virtualization.framework.
package vz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/Code-Hex/vz/v3/vmnet"

	"github.com/pilat/fleetbox/internal/backend"
)

// reservedSubnets records every /24 handed out by detectFreeIPv4Subnet in this
// process. The host bridge interface for a vmnet network does not appear until
// a VM boots on it, so two networks created back-to-back cannot be told apart
// by scanning net.Interfaces() alone — the reservation set closes that race.
var (
	reservedSubnetsMu sync.Mutex
	reservedSubnets   = map[netip.Prefix]struct{}{}
)

var (
	_ backend.Backend = (*Backend)(nil)
	_ backend.Network = (*vzNetwork)(nil)
)

// Backend implements the VZ backend.
type Backend struct{}

// New creates a new VZ backend.
func New() *Backend {
	return &Backend{}
}

// NestedVirtSupported returns true if nested virtualization is available.
func (b *Backend) NestedVirtSupported() bool {
	return vz.IsNestedVirtualizationSupported()
}

// CreateNetwork creates a vmnet SharedMode logical network. Every VM attached
// to it reaches the host, the internet (via NAT44), and the other VMs on the
// same network — VM↔VM connectivity that VZ NAT did not provide (ADR-0008).
// It requires macOS 26 or newer; on older releases the underlying vmnet API
// returns an error, wrapped here as the single canonical message.
func (b *Backend) CreateNetwork() (backend.Network, error) {
	cfg, err := vmnet.NewNetworkConfiguration(vmnet.SharedMode)
	if err != nil {
		return nil, fmt.Errorf("create vmnet configuration (fleetbox networking requires macOS 26+): %w", err)
	}

	subnet, err := detectFreeIPv4Subnet()
	if err != nil {
		return nil, fmt.Errorf("detect free subnet: %w", err)
	}
	if err := cfg.SetIPv4Subnet(subnet); err != nil {
		return nil, fmt.Errorf("set vmnet subnet %s: %w", subnet, err)
	}

	network, err := vmnet.NewNetwork(cfg)
	if err != nil {
		return nil, fmt.Errorf("create vmnet network: %w", err)
	}

	return &vzNetwork{network: network}, nil
}

// vzNetwork wraps a vmnet logical network as an opaque backend.Network.
type vzNetwork struct {
	network *vmnet.Network
}

// Close is a no-op. The vmnet network is released by the Go runtime
// (runtime.AddCleanup inside the vmnet package) once every VM holding a
// reference to it is unreferenced. Phase 1 never calls Close — releasing a
// network that a cluster's other VMs still share would break them (ADR-0008,
// R3). It exists for explicit whole-cluster teardown / Phase 2.
func (n *vzNetwork) Close() error {
	return nil
}

// detectFreeIPv4Subnet returns a /24 inside 192.168.0.0/16 that overlaps no
// host interface and has not already been handed out in this process. The
// chosen subnet is reserved (see reservedSubnets) so a later call picks a
// different one even before this network's host bridge interface appears.
func detectFreeIPv4Subnet() (netip.Prefix, error) {
	occupied, err := occupiedPrivatePrefixes()
	if err != nil {
		return netip.Prefix{}, err
	}

	reservedSubnetsMu.Lock()
	defer reservedSubnetsMu.Unlock()

	for octet := range 256 {
		candidate := netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, byte(octet), 0}), 24)
		if _, taken := reservedSubnets[candidate]; taken {
			continue
		}
		if slices.ContainsFunc(occupied, candidate.Overlaps) {
			continue
		}
		reservedSubnets[candidate] = struct{}{}
		return candidate, nil
	}

	return netip.Prefix{}, errors.New("no free /24 subnet available in 192.168.0.0/16")
}

// occupiedPrivatePrefixes returns the host interface prefixes that fall within
// 192.168.0.0/16.
func occupiedPrivatePrefixes() ([]netip.Prefix, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}

	target := netip.MustParsePrefix("192.168.0.0/16")
	var occupied []netip.Prefix
	for i := range ifaces {
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			prefix, err := netip.ParsePrefix(ipNet.String())
			if err != nil {
				continue
			}
			if target.Overlaps(prefix) {
				occupied = append(occupied, prefix)
			}
		}
	}

	return occupied, nil
}

// Create creates a new VM with the given configuration, attached to nw.
// nw must be a Network returned by this backend's CreateNetwork.
func (b *Backend) Create(cfg backend.Config, nw backend.Network) (backend.VM, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	// Create or load EFI variable store
	var efiStore *vz.EFIVariableStore
	var err error
	if _, statErr := os.Stat(cfg.EFIPath); os.IsNotExist(statErr) {
		efiStore, err = vz.NewEFIVariableStore(cfg.EFIPath, vz.WithCreatingEFIVariableStore())
	} else {
		efiStore, err = vz.NewEFIVariableStore(cfg.EFIPath)
	}
	if err != nil {
		return nil, fmt.Errorf("create efi store: %w", err)
	}

	// Create bootloader
	bootloader, err := vz.NewEFIBootLoader(vz.WithEFIVariableStore(efiStore))
	if err != nil {
		return nil, fmt.Errorf("create bootloader: %w", err)
	}

	// Create VM configuration
	vmConfig, err := vz.NewVirtualMachineConfiguration(bootloader, uint(cfg.CPUs), cfg.MemoryBytes)
	if err != nil {
		return nil, fmt.Errorf("create vm config: %w", err)
	}

	// Platform configuration with nested virtualization
	platform, err := vz.NewGenericPlatformConfiguration()
	if err != nil {
		return nil, fmt.Errorf("create platform config: %w", err)
	}
	if vz.IsNestedVirtualizationSupported() {
		if err := platform.SetNestedVirtualizationEnabled(true); err != nil {
			return nil, fmt.Errorf("enable nested virt: %w", err)
		}
	}
	vmConfig.SetPlatformVirtualMachineConfiguration(platform)

	vm := &VM{name: cfg.Name}

	// Serial console
	if cfg.SerialOut != nil {
		serialRead, serialWrite, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("create serial pipe: %w", err)
		}
		vm.serialRead = serialRead
		vm.serialWrite = serialWrite

		go func() { _, _ = io.Copy(cfg.SerialOut, serialRead) }()

		serialAttachment, err := vz.NewFileHandleSerialPortAttachment(os.Stdin, serialWrite)
		if err != nil {
			return nil, fmt.Errorf("create serial attachment: %w", err)
		}
		serialConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAttachment)
		if err != nil {
			return nil, fmt.Errorf("create serial config: %w", err)
		}
		vmConfig.SetSerialPortsVirtualMachineConfiguration(
			[]*vz.VirtioConsoleDeviceSerialPortConfiguration{serialConfig},
		)
	}

	// Disk
	diskAttachment, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
		cfg.DiskPath, false,
		vz.DiskImageCachingModeAutomatic,
		vz.DiskImageSynchronizationModeFsync,
	)
	if err != nil {
		return nil, fmt.Errorf("create disk attachment: %w", err)
	}
	diskConfig, err := vz.NewVirtioBlockDeviceConfiguration(diskAttachment)
	if err != nil {
		return nil, fmt.Errorf("create disk config: %w", err)
	}

	// Seed ISO
	seedAttachment, err := vz.NewDiskImageStorageDeviceAttachment(cfg.SeedPath, true)
	if err != nil {
		return nil, fmt.Errorf("create seed attachment: %w", err)
	}
	seedConfig, err := vz.NewVirtioBlockDeviceConfiguration(seedAttachment)
	if err != nil {
		return nil, fmt.Errorf("create seed config: %w", err)
	}

	vmConfig.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{diskConfig, seedConfig})

	// Network: attach to the shared vmnet SharedMode logical network (ADR-0008).
	vzNet, ok := nw.(*vzNetwork)
	if !ok {
		return nil, fmt.Errorf("network is not a vz network: %T", nw)
	}
	vm.network = vzNet // retain for the VM's lifetime so GC keeps the network (R3)
	vmnetAttachment, err := vz.NewVmnetNetworkDeviceAttachment(vzNet.network)
	if err != nil {
		return nil, fmt.Errorf("create vmnet attachment: %w", err)
	}
	netConfig, err := vz.NewVirtioNetworkDeviceConfiguration(vmnetAttachment)
	if err != nil {
		return nil, fmt.Errorf("create net config: %w", err)
	}
	macBytes, err := parseMACBytes(cfg.MAC)
	if err != nil {
		return nil, fmt.Errorf("parse mac: %w", err)
	}
	macAddr, err := vz.NewMACAddress(net.HardwareAddr(macBytes))
	if err != nil {
		return nil, fmt.Errorf("create mac address: %w", err)
	}
	netConfig.SetMACAddress(macAddr)
	vmConfig.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netConfig})

	// Entropy
	entropyConfig, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("create entropy config: %w", err)
	}
	vmConfig.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropyConfig})

	// Validate
	if valid, err := vmConfig.Validate(); !valid || err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	// Create VM
	vzVM, err := vz.NewVirtualMachine(vmConfig)
	if err != nil {
		return nil, fmt.Errorf("create vm: %w", err)
	}

	vm.vm = vzVM
	return vm, nil
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
	if cfg.EFIPath == "" {
		return errors.New("efi path is required")
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

// VM represents a VZ virtual machine.
type VM struct {
	name string
	vm   *vz.VirtualMachine
	// network is retained for the VM's whole lifetime so the shared vmnet
	// network is not released by GC while the VM (or a cluster sibling) runs
	// (ADR-0008, R3).
	network     *vzNetwork
	serialRead  *os.File
	serialWrite *os.File
}

var _ backend.VM = (*VM)(nil)

// Start boots the VM.
func (v *VM) Start(ctx context.Context) error {
	if err := v.vm.Start(); err != nil {
		return fmt.Errorf("start vm: %w", err)
	}

	// Wait for running state
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		state := v.vm.State()
		if state == vz.VirtualMachineStateRunning {
			return nil
		}
		if state == vz.VirtualMachineStateError {
			return errors.New("vm entered error state")
		}
		time.Sleep(100 * time.Millisecond)
	}

	return errors.New("timeout waiting for vm to start")
}

// Stop gracefully shuts down the VM via ACPI.
// Returns nil if the VM is already stopped.
func (v *VM) Stop(ctx context.Context) error {
	// Already stopped? Not an error.
	if v.vm.State() == vz.VirtualMachineStateStopped {
		return nil
	}

	if !v.vm.CanRequestStop() {
		return fmt.Errorf("vm cannot be stopped (state: %v)", v.vm.State())
	}

	stopped, err := v.vm.RequestStop()
	if err != nil {
		return fmt.Errorf("request stop: %w", err)
	}
	if !stopped {
		return errors.New("stop request failed")
	}

	// Wait for stopped state
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		state := v.vm.State()
		if state == vz.VirtualMachineStateStopped {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	return errors.New("timeout waiting for vm to stop")
}

// State returns the current VM state.
func (v *VM) State() backend.State {
	switch v.vm.State() {
	case vz.VirtualMachineStateStopped:
		return backend.StateStopped
	case vz.VirtualMachineStateRunning:
		return backend.StateRunning
	case vz.VirtualMachineStatePaused:
		return backend.StatePaused
	case vz.VirtualMachineStateError:
		return backend.StateError
	case vz.VirtualMachineStateStarting:
		return backend.StateStarting
	case vz.VirtualMachineStatePausing:
		return backend.StatePausing
	case vz.VirtualMachineStateResuming:
		return backend.StateResuming
	case vz.VirtualMachineStateStopping:
		return backend.StateStopping
	case vz.VirtualMachineStateSaving, vz.VirtualMachineStateRestoring:
		// Save/restore is not supported by fleetbox; treat as unknown.
		return backend.StateUnknown
	default:
		return backend.StateUnknown
	}
}

// Wait blocks until the VM stops.
func (v *VM) Wait(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		state := v.vm.State()
		if state == vz.VirtualMachineStateStopped || state == vz.VirtualMachineStateError {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func parseMACBytes(mac string) ([]byte, error) {
	parts := strings.Split(mac, ":")
	if len(parts) != 6 {
		return nil, fmt.Errorf("invalid MAC address: %s", mac)
	}
	result := make([]byte, 6)
	for i, p := range parts {
		val, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return nil, fmt.Errorf("invalid MAC address octet %q: %w", p, err)
		}
		result[i] = byte(val)
	}
	return result, nil
}
