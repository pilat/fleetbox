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
	"syscall"
	"time"

	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/dhcp"
	"github.com/pilat/fleetbox/third_party/vz"
	"github.com/pilat/fleetbox/third_party/vz/vmnet"
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
type Backend struct {
	// macOSMajor is the host's major macOS version (e.g. 26), detected once in
	// New. It selects the network path: 26+ uses vmnet SharedMode (VM↔VM); older
	// releases fall back to VZ NAT (single, isolated VM — ADR-0012).
	macOSMajor int
}

// New creates a new VZ backend, detecting the host macOS version so the network
// path (SharedMode vs NAT) and clustering capability are fixed for its lifetime.
func New() *Backend {
	return &Backend{macOSMajor: detectMacOSMajor()}
}

// NestedVirtSupported returns true if nested virtualization is available.
func (b *Backend) NestedVirtSupported() bool {
	return vz.IsNestedVirtualizationSupported()
}

// SupportsClustering reports whether VM↔VM connectivity is available. It is
// true on macOS 26+ (vmnet SharedMode) and false on older releases, where VZ
// NAT isolates VMs from one another (ADR-0008, ADR-0012).
func (b *Backend) SupportsClustering() bool {
	return b.macOSMajor >= 26
}

// Reconcile is a no-op on macOS: vmnet owns the host network state, so there are
// no fleetbox-created bridges, taps, or firewall rules to reclaim after a crash
// (ADR-0013).
func (b *Backend) Reconcile() error {
	return nil
}

// detectMacOSMajor returns the host's major macOS version from
// kern.osproductversion. On a sysctl error it returns 0, which conservatively
// selects the NAT, single-VM path.
func detectMacOSMajor() int {
	ver, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return 0
	}
	return parseMacOSMajor(ver)
}

// parseMacOSMajor extracts the leading integer of a macOS version string
// (e.g. "26.4.1" → 26). It returns 0 for an unparseable string.
func parseMacOSMajor(ver string) int {
	major, _, _ := strings.Cut(ver, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// CreateNetwork creates the logical network VMs attach to. On macOS 26+ it is a
// vmnet SharedMode network: every VM on it reaches the host, the internet (via
// NAT44), and the other VMs — VM↔VM connectivity VZ NAT lacked (ADR-0008). On
// older releases there is no shared network object; CreateNetwork returns a
// no-op holder and the per-VM NAT attachment is applied in Create instead, which
// gives a single, isolated VM (ADR-0012).
func (b *Backend) CreateNetwork() (backend.Network, error) {
	if b.macOSMajor < 26 {
		return &vzNetwork{}, nil
	}

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

// Subnet returns the empty string: vmnet SharedMode hands out addresses via
// bootpd (DHCP), so the orchestrator allocates no static IP and emits no
// cloud-init network-config for vz VMs (ADR-0007).
func (n *vzNetwork) Subnet() string {
	return ""
}

// Reserve allocates no static IP on the DHCP/vz path — vmnet's bootpd hands out
// the address and WaitForIP discovers it post-boot (ADR-0007). It returns only
// the deterministic MAC the NIC will carry, so the helper and the client's seed
// agree on it (Decision 6).
func (n *vzNetwork) Reserve(name, _ string) (ip, mac string, err error) {
	return "", backend.GenerateMAC(name), nil
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
func (b *Backend) Create(cfg backend.Config, nw backend.Network) (_ backend.VM, retErr error) {
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

	// Serial console. The helper (this process) opens the log file itself from the
	// path the client chose — the writer cannot cross the process boundary
	// (Decision 7). If a later Create step fails, the deferred cleanup closes it so
	// a failed boot leaks no file or goroutine.
	if cfg.SerialLogPath != "" {
		if err := vm.setupSerial(cfg.SerialLogPath, vmConfig); err != nil {
			return nil, err
		}
		defer func() {
			if retErr != nil {
				vm.closeSerial()
			}
		}()
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

	storageDevices := []vz.StorageDeviceConfiguration{diskConfig, seedConfig}

	// Read-only fixture images, each attached the same way as the seed (ADR-0015).
	// The guest mounts each by its volume LABEL, so attachment order is irrelevant.
	for _, p := range cfg.FixturePaths {
		fixtureAttachment, err := vz.NewDiskImageStorageDeviceAttachment(p, true)
		if err != nil {
			return nil, fmt.Errorf("create fixture attachment %s: %w", p, err)
		}
		fixtureConfig, err := vz.NewVirtioBlockDeviceConfiguration(fixtureAttachment)
		if err != nil {
			return nil, fmt.Errorf("create fixture config %s: %w", p, err)
		}
		storageDevices = append(storageDevices, fixtureConfig)
	}

	vmConfig.SetStorageDevicesVirtualMachineConfiguration(storageDevices)

	// Network: vmnet SharedMode on macOS 26+ (ADR-0008), VZ NAT on older
	// releases (ADR-0012). The attachment differs; everything downstream
	// (MAC, config) is identical.
	vzNet, ok := nw.(*vzNetwork)
	if !ok {
		return nil, fmt.Errorf("network is not a vz network: %T", nw)
	}
	vm.network = vzNet // retain for the VM's lifetime so GC keeps the network (R3)
	attachment, err := b.networkAttachment(vzNet)
	if err != nil {
		return nil, err
	}
	netConfig, err := vz.NewVirtioNetworkDeviceConfiguration(attachment)
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

// networkAttachment builds the NIC attachment for the active macOS version:
// vmnet SharedMode on 26+ (the shared network on nw, giving VM↔VM — ADR-0008),
// or VZ NAT on older releases (a single, isolated VM — ADR-0012). On the NAT
// path nw is the no-op holder and its network field is nil.
func (b *Backend) networkAttachment(nw *vzNetwork) (vz.NetworkDeviceAttachment, error) {
	if b.macOSMajor < 26 {
		nat, err := vz.NewNATNetworkDeviceAttachment()
		if err != nil {
			return nil, fmt.Errorf("create nat attachment: %w", err)
		}
		return nat, nil
	}

	att, err := vz.NewVmnetNetworkDeviceAttachment(nw.network)
	if err != nil {
		return nil, fmt.Errorf("create vmnet attachment: %w", err)
	}
	return att, nil
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
	serialFile  *os.File
	serialRead  *os.File
	serialWrite *os.File
	serialOnce  sync.Once
}

var _ backend.VM = (*VM)(nil)

// setupSerial opens the serial log file, wires the guest's serial console to it
// through a pipe, and registers the serial port on vmConfig. The copy goroutine
// ends when closeSerial closes the pipe on Stop (or on a failed Create via the
// caller's deferred cleanup).
func (v *VM) setupSerial(path string, vmConfig *vz.VirtualMachineConfiguration) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open serial log: %w", err)
	}
	serialRead, serialWrite, err := os.Pipe()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("create serial pipe: %w", err)
	}
	v.serialFile = f
	v.serialRead = serialRead
	v.serialWrite = serialWrite

	go func() { _, _ = io.Copy(f, serialRead) }()

	serialAttachment, err := vz.NewFileHandleSerialPortAttachment(os.Stdin, serialWrite)
	if err != nil {
		return fmt.Errorf("create serial attachment: %w", err)
	}
	serialConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAttachment)
	if err != nil {
		return fmt.Errorf("create serial config: %w", err)
	}
	vmConfig.SetSerialPortsVirtualMachineConfiguration(
		[]*vz.VirtioConsoleDeviceSerialPortConfiguration{serialConfig},
	)
	return nil
}

// closeSerial closes the serial pipe and log file, ending the copy goroutine and
// flushing buffered console output. It is idempotent (Stop and a failed Create's
// deferred cleanup may both reach it). os.File methods are safe for concurrent
// use, so closing while the copy goroutine is mid-write is benign.
func (v *VM) closeSerial() {
	v.serialOnce.Do(func() {
		if v.serialWrite != nil {
			_ = v.serialWrite.Close()
		}
		if v.serialRead != nil {
			_ = v.serialRead.Close()
		}
		if v.serialFile != nil {
			_ = v.serialFile.Close()
		}
	})
}

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
	// Release the serial log file/pipe on any exit path — the VM is being torn
	// down whether or not it was already stopped (idempotent via closeSerial).
	defer v.closeSerial()

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

// WaitForIP discovers the VM's IPv4 address by looking up its hostname in
// /var/db/dhcpd_leases (ADR-0007) and returns it once TCP port 22 is reachable.
// It polls until ctx is cancelled or its deadline passes, mirroring the combined
// IP-discovery + reachability wait that previously lived in the root package.
func (v *VM) WaitForIP(ctx context.Context) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		ipStr, err := dhcp.LookupByHostname(v.name)
		if err == nil && ipStr != "" {
			if ip := net.ParseIP(ipStr); ip != nil && reachableSSH(ip.String()) {
				return ipStr, nil
			}
		}

		time.Sleep(time.Second)
	}
}

// reachableSSH reports whether TCP port 22 on ip accepts a connection within a
// short timeout. It is the readiness signal that the guest's network is up.
func reachableSSH(ip string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "22"), 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
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
