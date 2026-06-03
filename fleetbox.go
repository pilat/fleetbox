//go:build darwin && arm64

// Package fleetbox provides Linux VMs as Go test fixtures on macOS (Apple Silicon).
//
// Fleetbox boots stock Linux cloud images via Apple Virtualization.framework,
// configures them with cloud-init, and provides SSH access for testing.
//
// Basic usage:
//
//	vm, err := fleetbox.Start(ctx, "myvm")
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer vm.Stop(ctx)
//
//	out, err := vm.SSH(ctx, "uname -a")
package fleetbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/dhcp"
	"github.com/pilat/fleetbox/internal/image"
	"github.com/pilat/fleetbox/internal/seed"
	"github.com/pilat/fleetbox/internal/sshkey"
	"github.com/pilat/fleetbox/internal/store"
)

const (
	defaultImage  = "debian-12"
	defaultCPUs   = 2
	defaultMemGB  = 4
	defaultDiskGB = 20
	defaultUser   = "fleetbox"
)

// Image aliases for common distributions.
const (
	Debian12   = "debian-12"
	Ubuntu2404 = "ubuntu-24.04"
)

// VM represents a running virtual machine.
type VM struct {
	name      string
	ip        net.IP
	store     *store.Store
	sshMgr    *sshkey.Manager
	backend   backend.VM
	config    *store.VM
	serialLog *os.File
}

// Options configures VM creation.
type Options struct {
	Image  string
	CPUs   int
	MemGB  int
	DiskGB int
}

// Option is a functional option for configuring a VM.
type Option func(*Options)

// WithImage sets the image to use (alias or URL).
func WithImage(img string) Option {
	return func(o *Options) { o.Image = img }
}

// WithCPUs sets the number of CPUs.
func WithCPUs(n int) Option {
	return func(o *Options) { o.CPUs = n }
}

// WithMemoryGB sets the memory in gigabytes.
func WithMemoryGB(n int) Option {
	return func(o *Options) { o.MemGB = n }
}

// WithDiskGB sets the disk size in gigabytes.
func WithDiskGB(n int) Option {
	return func(o *Options) { o.DiskGB = n }
}

// Start creates and boots a new VM with the given name.
// If the VM already exists, it boots the existing VM.
func Start(ctx context.Context, name string, opts ...Option) (*VM, error) {
	options := &Options{
		Image:  defaultImage,
		CPUs:   defaultCPUs,
		MemGB:  defaultMemGB,
		DiskGB: defaultDiskGB,
	}
	for _, opt := range opts {
		opt(options)
	}

	// Initialize store
	st, err := store.New()
	if err != nil {
		return nil, fmt.Errorf("init store: %w", err)
	}

	// Ensure SSH key
	sshMgr := sshkey.NewManager(st.SSHKeyPath())
	pubKey, err := sshMgr.EnsureKey()
	if err != nil {
		return nil, fmt.Errorf("ensure ssh key: %w", err)
	}

	// Ensure image
	imagePath, err := image.Ensure(st.ImagesDir(), options.Image)
	if err != nil {
		return nil, fmt.Errorf("ensure image: %w", err)
	}

	var vmConfig *store.VM
	if st.Exists(name) {
		// Load existing VM config
		vmConfig, err = st.Load(name)
		if err != nil {
			return nil, fmt.Errorf("load vm config: %w", err)
		}
	} else {
		// Create new VM
		vmConfig = &store.VM{
			Name:      name,
			MAC:       backend.GenerateMAC(name),
			CPUs:      options.CPUs,
			MemoryMB:  options.MemGB * 1024,
			DiskMB:    options.DiskGB * 1024,
			Image:     options.Image,
			CreatedAt: time.Now(),
		}
		if err := st.Create(vmConfig); err != nil {
			return nil, fmt.Errorf("create vm store: %w", err)
		}

		// Copy disk image
		diskPath := st.DiskPath(name)
		diskSize := int64(options.DiskGB) * 1024 * 1024 * 1024
		if err := image.CopyDisk(imagePath, diskPath, diskSize); err != nil {
			return nil, fmt.Errorf("copy disk: %w", err)
		}

		// Create seed ISO
		seedPath := st.SeedPath(name)
		seedCfg := seed.Config{
			Hostname: name,
			User:     defaultUser,
			SSHKey:   pubKey,
		}
		if err := seed.Create(seedPath, seedCfg); err != nil {
			return nil, fmt.Errorf("create seed: %w", err)
		}
	}

	// Create serial log file (owned by VM, closed in Stop/Destroy)
	serialLog, err := os.OpenFile(st.SerialLogPath(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open serial log: %w", err)
	}

	// Create backend VM
	vzBackend := newBackend()
	backendCfg := backend.Config{
		Name:        name,
		DiskPath:    st.DiskPath(name),
		SeedPath:    st.SeedPath(name),
		EFIPath:     st.EFIPath(name),
		MAC:         vmConfig.MAC,
		CPUs:        vmConfig.CPUs,
		MemoryBytes: uint64(vmConfig.MemoryMB) * 1024 * 1024,
		SerialOut:   serialLog,
	}
	backendVM, err := vzBackend.Create(backendCfg)
	if err != nil {
		return nil, fmt.Errorf("create backend vm: %w", err)
	}

	// Boot the VM
	if err := backendVM.Start(ctx); err != nil {
		return nil, fmt.Errorf("start vm: %w", err)
	}

	vm := &VM{
		name:      name,
		store:     st,
		sshMgr:    sshMgr,
		backend:   backendVM,
		config:    vmConfig,
		serialLog: serialLog,
	}

	// Wait for IP
	ip, err := vm.waitForIP(ctx, 2*time.Minute)
	if err != nil {
		_ = backendVM.Stop(ctx)
		return nil, fmt.Errorf("wait for ip: %w", err)
	}
	vm.ip = ip

	// Wait for SSH
	if err := vm.waitForSSH(ctx, 2*time.Minute); err != nil {
		_ = backendVM.Stop(ctx)
		return nil, fmt.Errorf("wait for ssh: %w", err)
	}

	return vm, nil
}

// Name returns the VM name.
func (v *VM) Name() string {
	return v.name
}

// IP returns the VM's IP address.
func (v *VM) IP() net.IP {
	return v.ip
}

// SSH executes a command on the VM via SSH and returns the output.
func (v *VM) SSH(ctx context.Context, cmd string) (string, error) {
	client, err := v.sshMgr.DialIP(v.ip, defaultUser, 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = client.Close() }()

	out, err := client.Run(cmd)
	if err != nil {
		return out, fmt.Errorf("run %q: %w", cmd, err)
	}

	return out, nil
}

// Stop gracefully shuts down the VM. The disk is preserved.
func (v *VM) Stop(ctx context.Context) error {
	err := v.backend.Stop(ctx)
	if v.serialLog != nil {
		_ = v.serialLog.Close()
		v.serialLog = nil
	}
	if err != nil {
		return fmt.Errorf("stop vm: %w", err)
	}

	return nil
}

// Destroy stops the VM and removes all its files.
func (v *VM) Destroy(ctx context.Context) error {
	_ = v.backend.Stop(ctx)

	if v.serialLog != nil {
		_ = v.serialLog.Close()
		v.serialLog = nil
	}

	if err := v.store.Delete(v.name); err != nil {
		return fmt.Errorf("delete vm files: %w", err)
	}

	return nil
}

// State returns the current VM state.
func (v *VM) State() string {
	return v.backend.State().String()
}

func (v *VM) waitForIP(ctx context.Context, timeout time.Duration) (net.IP, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		ipStr, err := dhcp.LookupByHostname(v.name)
		if err == nil && ipStr != "" {
			if ip := net.ParseIP(ipStr); ip != nil {
				if isReachable(ip) {
					return ip, nil
				}
			}
		}

		time.Sleep(time.Second)
	}
	return nil, errors.New("timeout waiting for IP")
}

func isReachable(ip net.IP) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), "22"), 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (v *VM) waitForSSH(ctx context.Context, timeout time.Duration) error {
	addr := net.JoinHostPort(v.ip.String(), "22")
	if err := v.sshMgr.WaitForSSH(addr, defaultUser, timeout); err != nil {
		return fmt.Errorf("wait for ssh: %w", err)
	}

	return nil
}

// StartN creates and boots N VMs with the given prefix (prefix-1, prefix-2, ...).
func StartN(ctx context.Context, prefix string, n int, opts ...Option) ([]*VM, error) {
	vms := make([]*VM, 0, n)
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("%s-%d", prefix, i)
		vm, err := Start(ctx, name, opts...)
		if err != nil {
			// Clean up already started VMs
			for _, started := range vms {
				_ = started.Destroy(ctx)
			}
			return nil, fmt.Errorf("start %s: %w", name, err)
		}
		vms = append(vms, vm)
	}
	return vms, nil
}

// NestedVirtSupported returns true if nested virtualization is available.
// Requires M3+ and macOS 15+.
func NestedVirtSupported() bool {
	return nestedVirtSupported()
}
