// Package runner provides the VM holder process for CLI mode.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/pilat/fleetbox"
	"github.com/pilat/fleetbox/internal/store"
)

const (
	runnerFlag = "--fleetbox-runner"

	// Runner states reported over the status socket.
	stateStarting = "starting"
	stateRunning  = "running"
	stateStopped  = "stopped"
	stateError    = "error"
)

// Status represents the state of a VM runner.
type Status struct {
	Name    string `json:"name"`
	PID     int    `json:"pid"`
	Running bool   `json:"running"`
	IP      string `json:"ip"`
	State   string `json:"state"` // "starting", "running", "error"
	Error   string `json:"error,omitempty"`
}

// IsRunner returns true if the current process is a runner.
func IsRunner() bool {
	for _, arg := range os.Args {
		if arg == runnerFlag {
			return true
		}
	}
	return false
}

// GetRunnerVMName returns the VM name from runner args.
func GetRunnerVMName() string {
	for i, arg := range os.Args {
		if arg == runnerFlag && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

// Spawn starts a new runner process for the given VM.
func Spawn(st *store.Store, name string, opts []fleetbox.Option) (*Status, error) {
	// Check if already running
	if IsRunning(st, name) {
		return GetStatus(st, name)
	}

	// Re-exec ourselves with the runner flag
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("get executable: %w", err)
	}

	args := []string{runnerFlag, name}

	// Encode options as JSON and pass via env
	optData, err := encodeOptions(opts)
	if err != nil {
		return nil, fmt.Errorf("encode options: %w", err)
	}

	// Create log file for runner output
	logPath := st.BaseDir() + "/runner-" + name + ".log"
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("create runner log: %w", err)
	}

	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "FLEETBOX_OPTS="+optData)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start runner: %w", err)
	}
	_ = logFile.Close()

	// Wait for the runner to signal ready (socket exists and has IP)
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)

		status, err := GetStatus(st, name)
		if err != nil {
			continue
		}
		if status.State == stateError {
			return nil, fmt.Errorf("runner failed: %s", status.Error)
		}
		if status.State == stateRunning && status.IP != "" {
			return status, nil
		}
	}

	return nil, errors.New("timeout waiting for VM to start")
}

// runnerState holds mutable state for the socket handler
type runnerState struct {
	mu    sync.Mutex
	name  string
	state string
	ip    string
	err   string
	vm    *fleetbox.VM
}

func (s *runnerState) getStatus() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{
		Name:    s.name,
		PID:     os.Getpid(),
		Running: s.state == stateRunning || s.state == stateStarting,
		IP:      s.ip,
		State:   s.state,
		Error:   s.err,
	}
}

func (s *runnerState) setRunning(vm *fleetbox.VM) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = stateRunning
	s.vm = vm
	if vm != nil && vm.IP() != nil {
		s.ip = vm.IP().String()
	}
}

func (s *runnerState) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = stateError
	s.err = err.Error()
}

// Run is the runner's main loop.
func Run() error {
	name := GetRunnerVMName()
	if name == "" {
		return errors.New("no VM name provided")
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	state := &runnerState{name: name, state: stateStarting}

	// Write pidfile first
	if err := WritePidfile(st, name); err != nil {
		return fmt.Errorf("write pidfile: %w", err)
	}
	defer func() { _ = RemovePidfile(st, name) }()

	// Start socket server BEFORE booting VM
	sockPath := st.SocketPath(name)
	_ = os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen socket: %w", err)
	}
	defer func() { _ = listener.Close() }()
	defer func() { _ = os.Remove(sockPath) }()

	stopCh := make(chan struct{})

	// Socket handler goroutine
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-stopCh:
					return
				default:
					continue
				}
			}
			go handleConn(conn, state, stopCh)
		}
	}()

	// Decode options from env
	opts, err := decodeOptions(os.Getenv("FLEETBOX_OPTS"))
	if err != nil {
		state.setError(err)
		return fmt.Errorf("decode options: %w", err)
	}

	// Boot the VM
	ctx := context.Background()
	vm, err := fleetbox.Start(ctx, name, opts...)
	if err != nil {
		state.setError(err)
		return fmt.Errorf("start vm: %w", err)
	}
	state.setRunning(vm)

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Wait for signal or stop command
	select {
	case <-sigCh:
	case <-stopCh:
	}

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = vm.Stop(ctx)

	return nil
}

func handleConn(conn net.Conn, state *runnerState, stopCh chan struct{}) {
	defer func() { _ = conn.Close() }()

	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return
	}

	cmd := string(buf[:n])
	switch cmd {
	case "status":
		status := state.getStatus()
		data, _ := json.Marshal(status)
		_, _ = conn.Write(data)

	case "stop":
		_, _ = conn.Write([]byte("ok"))
		close(stopCh)
	}
}

// IsRunning checks if a VM runner is alive.
func IsRunning(st *store.Store, name string) bool {
	pidfile := st.PidfilePath(name)
	data, err := os.ReadFile(pidfile)
	if err != nil {
		return false
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return false
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// GetStatus returns the status of a VM via its runner socket.
func GetStatus(st *store.Store, name string) (*Status, error) {
	if !st.Exists(name) {
		return nil, fmt.Errorf("VM %q does not exist", name)
	}

	if !IsRunning(st, name) {
		cfg, err := st.Load(name)
		if err != nil {
			return nil, fmt.Errorf("load vm config: %w", err)
		}
		return &Status{
			Name:  cfg.Name,
			State: stateStopped,
		}, nil
	}

	// Query the runner socket
	sockPath := st.SocketPath(name)
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to runner: %w", err)
	}
	defer func() { _ = conn.Close() }()

	_, _ = conn.Write([]byte("status"))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("read status: %w", err)
	}

	var status Status
	if err := json.Unmarshal(buf[:n], &status); err != nil {
		return nil, fmt.Errorf("parse status: %w", err)
	}

	return &status, nil
}

// Stop sends a shutdown signal to a running VM.
func Stop(st *store.Store, name string) error {
	if !IsRunning(st, name) {
		return nil
	}

	sockPath := st.SocketPath(name)
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect to runner: %w", err)
	}
	defer func() { _ = conn.Close() }()

	_, _ = conn.Write([]byte("stop"))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	buf := make([]byte, 64)
	_, _ = conn.Read(buf)

	// Wait for process to exit
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if !IsRunning(st, name) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return errors.New("timeout waiting for VM to stop")
}

// WritePidfile writes the current process PID.
func WritePidfile(st *store.Store, name string) error {
	pidfile := st.PidfilePath(name)
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return fmt.Errorf("write pidfile: %w", err)
	}

	return nil
}

// RemovePidfile removes the VM's pidfile.
func RemovePidfile(st *store.Store, name string) error {
	if err := os.Remove(st.PidfilePath(name)); err != nil {
		return fmt.Errorf("remove pidfile: %w", err)
	}

	return nil
}

// Options encoding
type optionsData struct {
	Image  string `json:"image,omitempty"`
	CPUs   int    `json:"cpus,omitempty"`
	MemGB  int    `json:"mem,omitempty"`
	DiskGB int    `json:"disk,omitempty"`
}

func encodeOptions(opts []fleetbox.Option) (string, error) {
	// Option funcs cannot be serialized; apply them to an Options struct
	// and serialize the resulting values instead.
	var options fleetbox.Options
	for _, opt := range opts {
		opt(&options)
	}

	data := optionsData{
		Image:  options.Image,
		CPUs:   options.CPUs,
		MemGB:  options.MemGB,
		DiskGB: options.DiskGB,
	}

	b, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("marshal options: %w", err)
	}

	return string(b), nil
}

func decodeOptions(s string) ([]fleetbox.Option, error) {
	if s == "" {
		return nil, nil
	}
	var data optionsData
	if err := json.Unmarshal([]byte(s), &data); err != nil {
		return nil, fmt.Errorf("unmarshal options: %w", err)
	}

	var opts []fleetbox.Option
	if data.Image != "" {
		opts = append(opts, fleetbox.WithImage(data.Image))
	}
	if data.CPUs > 0 {
		opts = append(opts, fleetbox.WithCPUs(data.CPUs))
	}
	if data.MemGB > 0 {
		opts = append(opts, fleetbox.WithMemoryGB(data.MemGB))
	}
	if data.DiskGB > 0 {
		opts = append(opts, fleetbox.WithDiskGB(data.DiskGB))
	}
	return opts, nil
}
