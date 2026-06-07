// Package runner provides the VM holder process for CLI mode.
//
// A single holder process owns one fleetbox.Cluster — its shared vmnet network
// and every VM on it — so a CLI-launched cluster gets the same VM↔VM
// connectivity the library StartN gives (ADR-0008). The holder serves one
// control socket and one pidfile per member name, so the per-name CLI commands
// (ls/ssh/down/rm/status) work unchanged: each addresses a member by talking to
// its socket, unaware that several members may live in one process. Members can
// be stopped individually (the process survives until the last one leaves) and
// added at runtime (a stopped node re-joins the live cluster's network).
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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pilat/fleetbox"
	"github.com/pilat/fleetbox/internal/store"
)

const (
	runnerFlag = "--fleetbox-runner"

	// Runner states reported over the status socket. stateDownloading is the
	// one-time pull of the image + VMM binaries that precedes booting; it is
	// reported separately so the CLI's readiness wait does not charge that
	// (potentially multi-GB) download against the per-boot budget (ADR-0013).
	stateDownloading = "downloading"
	stateStarting    = "starting"
	stateRunning     = "running"
	stateStopped     = "stopped"
	stateError       = "error"

	// failedClusterLinger is how long a holder keeps serving error status after
	// a failed initial boot, so the spawning CLI can read the error before the
	// process tears down. The CLI polls every 500ms, so this is generous.
	failedClusterLinger = 30 * time.Second

	// maxDownloadWait bounds the stateDownloading phase: while a member is
	// pulling, waitForMembers keeps the boot deadline ahead of it, but a stuck
	// download must still fail rather than hang the CLI forever.
	maxDownloadWait = 30 * time.Minute
)

// Status represents the state of a VM member.
type Status struct {
	Name    string `json:"name"`
	PID     int    `json:"pid"`
	Running bool   `json:"running"`
	IP      string `json:"ip"`
	State   string `json:"state"` // "starting", "running", "stopped", "error"
	Error   string `json:"error,omitempty"`
}

// IsRunner returns true if the current process is a holder.
func IsRunner() bool {
	return slices.Contains(os.Args, runnerFlag)
}

// GetRunnerVMNames returns the member names this holder was launched for.
func GetRunnerVMNames() []string {
	for i, arg := range os.Args {
		if arg == runnerFlag && i+1 < len(os.Args) {
			return strings.Split(os.Args[i+1], ",")
		}
	}
	return nil
}

// Spawn launches a fresh holder process for the given member names and waits for
// every one to come up. It assumes none of the names is currently running — the
// caller (idempotency, partial re-join) is handled in the CLI. It returns the
// per-name status on success, or the first member's boot error.
func Spawn(st *store.Store, names []string, opts []fleetbox.Option) (map[string]*Status, error) {
	if len(names) == 0 {
		return nil, errors.New("no VM names provided")
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("get executable: %w", err)
	}

	optData, err := encodeOptions(opts)
	if err != nil {
		return nil, fmt.Errorf("encode options: %w", err)
	}

	logPath := st.BaseDir() + "/runner-" + names[0] + ".log"
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("create runner log: %w", err)
	}

	cmd := exec.Command(exe, runnerFlag, strings.Join(names, ","))
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

	return waitForMembers(st, names, 5*time.Minute)
}

// AddMember asks a running holder — reached through a live sibling's socket — to
// boot name onto its existing shared network, then waits for it to come up. This
// is how a stopped node re-joins a live cluster instead of getting its own,
// isolated network.
func AddMember(st *store.Store, sibling, name string) error {
	sockPath := st.SocketPath(sibling)
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect to holder via %s: %w", sibling, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("addmember " + name)); err != nil {
		return fmt.Errorf("send addmember: %w", err)
	}
	// The holder replies only after the member has booted, which can take a
	// while, so allow generous time for the reply.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))

	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	reply := strings.TrimSpace(string(buf[:n]))
	if after, ok := strings.CutPrefix(reply, "err:"); ok {
		return errors.New(strings.TrimSpace(after))
	}

	_, err = waitForMembers(st, []string{name}, 5*time.Minute)
	return err
}

// waitForMembers polls per-member status until every name is running with an IP,
// returning early with an error if any member enters the error state.
func waitForMembers(st *store.Store, names []string, timeout time.Duration) (map[string]*Status, error) {
	result := make(map[string]*Status, len(names))
	pending := append([]string(nil), names...)

	bootDeadline := time.Now().Add(timeout)
	hardDeadline := time.Now().Add(maxDownloadWait)
	announcedPull := false
	for len(pending) > 0 {
		time.Sleep(500 * time.Millisecond)

		var still []string
		downloading := false
		for _, name := range pending {
			status, err := GetStatus(st, name)
			if err != nil {
				still = append(still, name)
				continue
			}
			if status.State == stateError {
				return nil, fmt.Errorf("%s failed: %s", name, status.Error)
			}
			if status.State == stateRunning && status.IP != "" {
				result[name] = status
				continue
			}
			if status.State == stateDownloading {
				downloading = true
			}
			still = append(still, name)
		}
		pending = still

		now := time.Now()
		if now.After(hardDeadline) {
			break
		}
		// The one-time image/binary pull must not consume the per-boot budget:
		// while any member is still downloading, keep the boot deadline ahead of
		// it. hardDeadline still bounds a stuck download (ADR-0013).
		if downloading {
			if !announcedPull {
				fmt.Fprintln(os.Stderr, "Pulling image and VMM binaries (first run, this can take a few minutes)...")
				announcedPull = true
			}
			bootDeadline = now.Add(timeout)
			continue
		}
		if now.After(bootDeadline) {
			break
		}
	}

	if len(pending) > 0 {
		return nil, fmt.Errorf("timeout waiting for: %s", strings.Join(pending, ", "))
	}
	return result, nil
}

// member is one VM held by the process: its reported state plus the socket that
// serves it.
type member struct {
	mu    sync.Mutex
	name  string
	state string
	ip    string
	err   string
	vm    *fleetbox.VM

	ln         net.Listener
	gone       chan struct{}
	retireOnce sync.Once
}

func (m *member) status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{
		Name:    m.name,
		PID:     os.Getpid(),
		Running: m.state == stateRunning || m.state == stateStarting || m.state == stateDownloading,
		IP:      m.ip,
		State:   m.state,
		Error:   m.err,
	}
}

// holder owns the cluster and the member registry for one process.
type holder struct {
	st *store.Store

	mu       sync.Mutex
	cluster  *fleetbox.Cluster
	members  map[string]*member
	running  int // members currently starting or running
	done     chan struct{}
	doneOnce sync.Once
}

// Run is the holder's main loop.
func Run() error {
	// Pin this goroutine to its OS thread for the holder's lifetime. Every VM is
	// booted from here (bootMember runs synchronously below), and each VM's child
	// process carries a PR_SET_PDEATHSIG so it dies if its parent thread dies. A
	// stable, long-lived parent thread makes that signal fire only when the holder
	// actually exits, not when Go happens to retire the launching thread (ADR-0013).
	runtime.LockOSThread()

	names := GetRunnerVMNames()
	if len(names) == 0 {
		return errors.New("no VM names provided")
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	opts, err := decodeOptions(os.Getenv("FLEETBOX_OPTS"))
	if err != nil {
		return fmt.Errorf("decode options: %w", err)
	}

	h := &holder{st: st, members: make(map[string]*member), done: make(chan struct{})}

	// On any exit — clean shutdown, signal, or panic — release the cluster's
	// shared network so a Linux bridge and its egress rules don't leak. Runs
	// after stopAll (deferred LIFO), so members are down first. No-op on macOS.
	defer func() {
		h.mu.Lock()
		c := h.cluster
		h.mu.Unlock()
		if c != nil {
			_ = c.Close()
		}
	}()

	// Register every member as "starting" and start serving its socket BEFORE
	// booting, so the spawning CLI sees status the moment it polls.
	for _, name := range names {
		if err := h.register(name); err != nil {
			return fmt.Errorf("register %s: %w", name, err)
		}
	}

	// NewCluster performs the one-time, cluster-wide pull of the image and VMM
	// binaries before any VM exists. Mark members "downloading" around it so the
	// CLI's readiness wait does not charge that (potentially multi-GB) download
	// against the per-boot budget (ADR-0013); flip back to "starting" once the
	// pull is done and the per-VM boot begins.
	for _, name := range names {
		h.setState(name, stateDownloading)
	}
	ctx := context.Background()
	cluster, err := fleetbox.NewCluster(opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create cluster: %v\n", err)
		for _, name := range names {
			h.markError(h.memberByName(name), err)
		}
	} else {
		h.mu.Lock()
		h.cluster = cluster
		h.mu.Unlock()
		for _, name := range names {
			h.setState(name, stateStarting)
		}
		for _, name := range names {
			h.bootMember(ctx, name)
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	if h.allRunning(names) {
		// Healthy cluster: serve until a signal or until every member is
		// stopped individually (the last `stop` closes done).
		select {
		case <-sigCh:
		case <-h.done:
		}
	} else {
		// Initial boot was all-or-nothing and at least one member failed. Keep
		// serving the error briefly so the CLI reads it, then tear the whole
		// cluster down — no half-up clusters from a single `up`.
		select {
		case <-sigCh:
		case <-time.After(failedClusterLinger):
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h.stopAll(shutdownCtx)
	return nil
}

// register opens a member's control socket and pidfile and marks it starting.
func (h *holder) register(name string) error {
	sockPath := h.st.SocketPath(name)
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen socket: %w", err)
	}

	if err := WritePidfile(h.st, name); err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return err
	}

	m := &member{name: name, state: stateStarting, ln: ln, gone: make(chan struct{})}
	h.mu.Lock()
	h.members[name] = m
	h.running++
	h.mu.Unlock()

	go h.serve(m)
	return nil
}

// bootMember brings a registered member up on the shared network.
func (h *holder) bootMember(ctx context.Context, name string) {
	m := h.memberByName(name)
	if m == nil {
		return
	}
	vm, err := h.cluster.Add(ctx, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boot %s: %v\n", name, err)
		h.markError(m, err)
		return
	}
	h.markRunning(m, vm)
}

// addMember registers and boots a brand-new member on the existing cluster
// network. Unlike the initial boot it is independent: its failure does not tear
// down the rest of the cluster.
func (h *holder) addMember(ctx context.Context, name string) error {
	h.mu.Lock()
	_, exists := h.members[name]
	cluster := h.cluster
	h.mu.Unlock()
	if exists {
		return nil // already a member — idempotent
	}
	if cluster == nil {
		return errors.New("cluster not ready")
	}

	// register starts the new member's accept loop, which serves independent
	// per-connection commands on their own background contexts — not derived
	// from this request's ctx.
	if err := h.register(name); err != nil { //nolint:contextcheck // server accept loop owns its context
		return err
	}
	m := h.memberByName(name)
	vm, err := cluster.Add(ctx, name)
	if err != nil {
		h.markError(m, err)
		h.retire(m)
		return fmt.Errorf("boot member %s: %w", name, err)
	}
	h.markRunning(m, vm)
	return nil
}

// stopMember gracefully shuts down one member and retires its socket/pidfile;
// the process survives for its siblings until the last member is gone.
func (h *holder) stopMember(ctx context.Context, name string) {
	m := h.memberByName(name)
	if m == nil {
		return
	}
	m.mu.Lock()
	vm := m.vm
	m.mu.Unlock()
	if vm != nil {
		_ = vm.Stop(ctx)
	}
	h.markStopped(m)
	h.retire(m)
}

// stopAll tears down every member (running, errored, or otherwise) and cleans up
// its socket and pidfile.
func (h *holder) stopAll(ctx context.Context) {
	h.mu.Lock()
	ms := make([]*member, 0, len(h.members))
	for _, m := range h.members {
		ms = append(ms, m)
	}
	h.mu.Unlock()

	for _, m := range ms {
		m.mu.Lock()
		vm := m.vm
		m.mu.Unlock()
		if vm != nil {
			_ = vm.Stop(ctx)
		}
		h.retire(m)
	}
}

func (h *holder) markRunning(m *member, vm *fleetbox.VM) {
	m.mu.Lock()
	m.state = stateRunning
	m.vm = vm
	if vm != nil && vm.IP() != nil {
		m.ip = vm.IP().String()
	}
	m.mu.Unlock()
}

// setState transitions a member between two "active" states (the
// downloading->starting flip around the cluster-wide pull). Unlike
// markRunning/markError/markStopped it does not touch the running counter, since
// both downloading and starting already count as active.
func (h *holder) setState(name, state string) {
	m := h.memberByName(name)
	if m == nil {
		return
	}
	m.mu.Lock()
	m.state = state
	m.mu.Unlock()
}

func (h *holder) markError(m *member, err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	wasActive := m.state == stateStarting || m.state == stateRunning || m.state == stateDownloading
	m.state = stateError
	m.err = err.Error()
	m.mu.Unlock()
	if wasActive {
		h.decRunning()
	}
}

func (h *holder) markStopped(m *member) {
	m.mu.Lock()
	wasActive := m.state == stateStarting || m.state == stateRunning || m.state == stateDownloading
	m.state = stateStopped
	m.mu.Unlock()
	if wasActive {
		h.decRunning()
	}
}

func (h *holder) decRunning() {
	h.mu.Lock()
	h.running--
	zero := h.running == 0
	h.mu.Unlock()
	if zero {
		h.doneOnce.Do(func() { close(h.done) })
	}
}

// retire closes a member's socket and removes its pidfile and registry entry. It
// is idempotent — stop and teardown may both reach the same member.
func (h *holder) retire(m *member) {
	m.retireOnce.Do(func() {
		close(m.gone)
		_ = m.ln.Close()
		_ = os.Remove(h.st.SocketPath(m.name))
		_ = RemovePidfile(h.st, m.name)
		h.mu.Lock()
		delete(h.members, m.name)
		h.mu.Unlock()
	})
}

func (h *holder) memberByName(name string) *member {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.members[name]
}

// allRunning reports whether every named member reached the running state.
func (h *holder) allRunning(names []string) bool {
	for _, name := range names {
		m := h.memberByName(name)
		if m == nil {
			return false
		}
		m.mu.Lock()
		ok := m.state == stateRunning
		m.mu.Unlock()
		if !ok {
			return false
		}
	}
	return true
}

func (h *holder) serve(m *member) {
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			select {
			case <-m.gone:
				return
			default:
				continue
			}
		}
		go h.handleConn(m, conn)
	}
}

func (h *holder) handleConn(m *member, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return
	}

	fields := strings.Fields(string(buf[:n]))
	if len(fields) == 0 {
		return
	}

	switch fields[0] {
	case "status":
		data, _ := json.Marshal(m.status())
		_, _ = conn.Write(data)

	case "stop":
		_, _ = conn.Write([]byte("ok"))
		h.stopMember(context.Background(), m.name)

	case "addmember":
		if len(fields) < 2 {
			_, _ = conn.Write([]byte("err: missing member name"))
			return
		}
		if err := h.addMember(context.Background(), fields[1]); err != nil {
			_, _ = conn.Write([]byte("err: " + err.Error()))
			return
		}
		_, _ = conn.Write([]byte("ok"))
	}
}

// IsRunning checks if the holder serving a member is alive.
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

// GetStatus returns the status of a member. A live holder is authoritative — even
// for a member still downloading or booting, whose config.json does not exist yet
// (it is written during boot) — so the holder socket is consulted first. Only
// when no holder serves the name do we fall back to on-disk state: stopped if the
// VM exists, otherwise an error that it is absent.
func GetStatus(st *store.Store, name string) (*Status, error) {
	if IsRunning(st, name) {
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

	if !st.Exists(name) {
		return nil, fmt.Errorf("VM %q does not exist", name)
	}
	cfg, err := st.Load(name)
	if err != nil {
		return nil, fmt.Errorf("load vm config: %w", err)
	}
	return &Status{
		Name:  cfg.Name,
		State: stateStopped,
	}, nil
}

// Stop gracefully shuts down a single member (the holder keeps running for its
// siblings).
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

	// Wait for the member's pidfile to disappear (the holder retires it once
	// stopped, even though the process may live on for siblings).
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if !IsRunning(st, name) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return errors.New("timeout waiting for VM to stop")
}

// WritePidfile writes the current process PID for a member.
func WritePidfile(st *store.Store, name string) error {
	pidfile := st.PidfilePath(name)
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return fmt.Errorf("write pidfile: %w", err)
	}

	return nil
}

// RemovePidfile removes a member's pidfile.
func RemovePidfile(st *store.Store, name string) error {
	if err := os.Remove(st.PidfilePath(name)); err != nil {
		return fmt.Errorf("remove pidfile: %w", err)
	}

	return nil
}

// Options encoding
type optionsData struct {
	Image  string      `json:"image,omitempty"`
	CPUs   int         `json:"cpus,omitempty"`
	MemGB  int         `json:"mem,omitempty"`
	DiskGB int         `json:"disk,omitempty"`
	Mounts []mountData `json:"mounts,omitempty"`
}

// mountData carries a mount across the holder process boundary. Only the host
// and guest paths cross — the host path is already absolute (resolved by the
// CLI); tags are not serialized because they are assigned at first-create in the
// library (ADR-0010).
type mountData struct {
	HostPath  string `json:"host_path"`
	GuestPath string `json:"guest_path"`
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
	for _, m := range options.Mounts {
		data.Mounts = append(data.Mounts, mountData{HostPath: m.HostPath, GuestPath: m.GuestPath})
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
	for _, m := range data.Mounts {
		opts = append(opts, fleetbox.WithMount(m.HostPath, m.GuestPath))
	}
	return opts, nil
}
