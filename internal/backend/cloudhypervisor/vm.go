//go:build linux

package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/pilat/fleetbox/internal/backend"
)

// VM is a cloud-hypervisor virtual machine: a child process given its full
// configuration on the command line (so it boots on launch) and controlled
// afterwards over the REST API on its unix socket.
type VM struct {
	name         string
	chBin        string
	fwPath       string
	diskPath     string
	seedPath     string
	fixturePaths []string
	serialPath   string
	cpus         int
	memBytes     uint64
	mac          string
	assignedIP   string
	apiSocket    string
	tap          string
	network      *chNetwork

	mu     sync.Mutex
	cmd    *exec.Cmd
	exited chan struct{}
	state  backend.State
}

// newVM assembles a VM from the backend config, the tap it will use, and the
// resolved binary/firmware paths. The api socket and serial log live in the VM's
// store directory, derived from the disk path (the store owns the layout).
func newVM(cfg backend.Config, nw *chNetwork, chBin, fwPath, tap string) *VM {
	vmDir := filepath.Dir(cfg.DiskPath)

	return &VM{
		name:         cfg.Name,
		chBin:        chBin,
		fwPath:       fwPath,
		diskPath:     cfg.DiskPath,
		seedPath:     cfg.SeedPath,
		fixturePaths: cfg.FixturePaths,
		// cloud-hypervisor opens the serial file itself (--serial file=PATH), so the
		// path crosses straight through; no Go-side handle to own (Decision 7).
		serialPath: cfg.SerialLogPath,
		cpus:       cfg.CPUs,
		memBytes:   cfg.MemoryBytes,
		mac:        cfg.MAC,
		assignedIP: cfg.AssignedIP,
		apiSocket:  filepath.Join(vmDir, "ch.sock"),
		tap:        tap,
		network:    nw,
		state:      backend.StateStopped,
	}
}

// Start launches cloud-hypervisor, which boots the VM immediately because the
// whole configuration is on the command line, then waits for the REST API to
// answer (confirming a live VM) or for the process to exit (a boot failure).
func (v *VM) Start(ctx context.Context) error {
	_ = os.Remove(v.apiSocket) // a stale socket from a prior boot would block the bind

	args, err := v.buildArgs()
	if err != nil {
		return fmt.Errorf("build cloud-hypervisor args: %w", err)
	}
	cmd := exec.Command(v.chBin, args...)
	// Tie the VM's life to the holder's: if the holder dies — even via SIGKILL,
	// OOM, or a panic that no Close can catch — the kernel SIGKILLs this child too,
	// so a crashed holder never leaves a VM running with a vanished NIC (ADR-0013).
	// The holder pins its main goroutine to one OS thread (runtime.LockOSThread) so
	// this parent-death signal has a stable parent thread to fire on.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start cloud-hypervisor: %w", err)
	}

	v.mu.Lock()
	v.cmd = cmd
	v.state = backend.StateRunning
	v.exited = make(chan struct{})
	exited := v.exited
	v.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		v.mu.Lock()
		v.state = backend.StateStopped
		v.mu.Unlock()
		close(exited)
	}()

	if err := v.waitAPIReady(ctx, 30*time.Second); err != nil {
		return fmt.Errorf("cloud-hypervisor did not become ready: %w", err)
	}
	return nil
}

// Stop asks the guest to shut down over the REST API, escalating to SIGTERM then
// SIGKILL if it does not exit, and always removes the VM's tap and socket.
func (v *VM) Stop(ctx context.Context) error {
	v.mu.Lock()
	cmd := v.cmd
	exited := v.exited
	v.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = v.shutdownREST(ctx)
		if !waitClosed(exited, 5*time.Second) {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			if !waitClosed(exited, 5*time.Second) {
				_ = cmd.Process.Kill()
				waitClosed(exited, 2*time.Second)
			}
		}
	}

	v.cleanup()
	return nil
}

// State reports the VM's current state (running until the process exits).
func (v *VM) State() backend.State {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.state
}

// Wait blocks until the cloud-hypervisor process exits or ctx is done.
func (v *VM) Wait(ctx context.Context) error {
	v.mu.Lock()
	exited := v.exited
	v.mu.Unlock()
	if exited == nil {
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-exited:
		return nil
	}
}

// WaitForIP returns the statically assigned address once TCP port 22 on it is
// reachable. The address is known up front (allocated by the orchestrator), so
// this only confirms the guest's network is up.
func (v *VM) WaitForIP(ctx context.Context) (string, error) {
	if v.assignedIP == "" {
		return "", errors.New("no assigned IP for cloud-hypervisor VM")
	}

	v.mu.Lock()
	exited := v.exited
	v.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-exited:
			// CH crashed after the API came up but before the guest was reachable
			// (e.g. a cloud-init failure) — fail fast instead of spinning.
			return "", errors.New("cloud-hypervisor exited before the guest became reachable")
		default:
		}

		if reachableSSH(v.assignedIP) {
			return v.assignedIP, nil
		}
		time.Sleep(time.Second)
	}
}

// buildArgs renders the full cloud-hypervisor command line: the boot config
// (arch-specific — firmware on x86_64, direct kernel on arm64, via bootArgs), the
// raw disk, the read-only seed ISO and any read-only fixture images, cpu/memory,
// the tap NIC with the VM's MAC, and serial to the log file. All block devices
// share one --disk flag (cloud-hypervisor takes multiple space-separated values);
// the guest mounts fixtures by LABEL, so their order after the seed does not
// matter (ADR-0015).
func (v *VM) buildArgs() ([]string, error) {
	disks := make([]string, 0, 2+len(v.fixturePaths))
	disks = append(disks, "path="+v.diskPath, "path="+v.seedPath+",readonly=on")
	for _, p := range v.fixturePaths {
		disks = append(disks, "path="+p+",readonly=on")
	}

	// How the guest kernel is booted differs by arch (bootArgs lives in
	// boot_{amd64,arm64}.go): x86_64 chain-loads via rust-hypervisor-firmware,
	// arm64 boots the extracted kernel directly (ADR-0024).
	boot, err := v.bootArgs()
	if err != nil {
		return nil, err
	}

	args := []string{"--api-socket", v.apiSocket}
	args = append(args, boot...)
	args = append(args, "--disk")
	args = append(args, disks...)
	args = append(args,
		"--cpus", "boot="+strconv.Itoa(v.cpus),
		"--memory", "size="+strconv.FormatUint(v.memBytes/(1024*1024), 10)+"M",
		"--net", "tap="+v.tap+",mac="+v.mac,
		"--console", "off",
	)
	if v.serialPath != "" {
		args = append(args, "--serial", "file="+v.serialPath)
	} else {
		args = append(args, "--serial", "off")
	}
	return args, nil
}

// waitAPIReady polls vm.info until cloud-hypervisor answers, the process exits,
// or the deadline/ctx fires.
func (v *VM) waitAPIReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := v.httpClient()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-v.exited:
			return errors.New("process exited during boot")
		default:
		}

		if v.vmInfoOK(ctx, client) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("timeout waiting for api socket")
}

func (v *VM) vmInfoOK(ctx context.Context, client *http.Client) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/api/v1/vm.info", http.NoBody)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (v *VM) shutdownREST(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost/api/v1/vm.shutdown", http.NoBody)
	if err != nil {
		return fmt.Errorf("build shutdown request: %w", err)
	}
	resp, err := v.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("shutdown request: %w", err)
	}
	_ = resp.Body.Close()
	return nil
}

// httpClient returns an HTTP client that dials cloud-hypervisor's unix api
// socket regardless of the request URL's host.
func (v *VM) httpClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", v.apiSocket)
			},
		},
	}
}

// cleanup removes the VM's tap and api socket. It is idempotent: a second call
// (Stop then Destroy) finds the tap already cleared and no-ops.
func (v *VM) cleanup() {
	v.mu.Lock()
	tap := v.tap
	v.tap = ""
	v.mu.Unlock()

	if tap != "" && v.network != nil {
		_ = v.network.deleteTap(tap)
	}
	_ = os.Remove(v.apiSocket)
}

// reachableSSH reports whether TCP port 22 on ip accepts a connection promptly.
func reachableSSH(ip string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "22"), 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// waitClosed reports whether ch closed within timeout (a nil channel counts as
// already closed).
func waitClosed(ch chan struct{}, timeout time.Duration) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}
