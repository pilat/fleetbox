// Package holder is the server half of the holder protocol: one process owns one
// orchestrator.Cluster — its shared network and every VM on it — so a cluster
// gets the same VM↔VM connectivity the library StartN gives (ADR-0008). It serves
// one control socket and one pidfile per member name, so the per-name CLI
// commands (ls/ssh/down/rm/status) address a member by talking to its socket,
// unaware that several members may live in one process. Members can be stopped
// individually (the process survives until the last one leaves) and added at
// runtime (a stopped node re-joins the live cluster's network).
//
// It runs in two lifetime modes (ADR-0017, R4). DETACHED: a persistent session
// leader whose VMs outlive the spawning CLI (cattle-with-persistence). BOUND: an
// attached child of a test process, selected by FLEETBOX_PARENT_PID; it watches
// that parent (reparent poll) and a long-lived control connection (EOF), and
// reaps itself and its in-process VMs the moment the test process is gone. On
// darwin this package is compiled only into cmd/fleetbox-helper (which links vz);
// on linux the CLI re-execs itself into Run.
package holder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pilat/fleetbox/internal/control"
	"github.com/pilat/fleetbox/internal/opts"
	"github.com/pilat/fleetbox/internal/orchestrator"
	"github.com/pilat/fleetbox/internal/store"
)

// failedClusterLinger is how long a holder keeps serving error status after a
// failed initial boot, so the spawning client can read the error before the
// process tears down. The client polls every 500ms, so this is generous.
const failedClusterLinger = 30 * time.Second

// IsRunner returns true if the current process is a holder.
func IsRunner() bool {
	return slices.Contains(os.Args, control.RunnerFlag)
}

// GetRunnerVMNames returns the member names this holder was launched for.
func GetRunnerVMNames() []string {
	for i, arg := range os.Args {
		if arg == control.RunnerFlag && i+1 < len(os.Args) {
			return strings.Split(os.Args[i+1], ",")
		}
	}
	return nil
}

// member is one VM held by the process: its reported state plus the socket that
// serves it.
type member struct {
	mu    sync.Mutex
	name  string
	state string
	ip    string
	err   string
	vm    *orchestrator.VM

	ln         net.Listener
	gone       chan struct{}
	retireOnce sync.Once
}

func (m *member) status() control.Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return control.Status{
		Name: m.name,
		PID:  os.Getpid(),
		Running: m.state == control.StateRunning || m.state == control.StateStarting ||
			m.state == control.StateDownloading,
		IP:    m.ip,
		State: m.state,
		Error: m.err,
	}
}

// holder owns the cluster and the member registry for one process.
type holder struct {
	st *store.Store

	mu       sync.Mutex
	cluster  *orchestrator.Cluster
	members  map[string]*member
	running  int // members currently starting or running
	done     chan struct{}
	doneOnce sync.Once

	// boundShutdown is closed by the parent-pid poll or the control-socket EOF
	// when this holder is bound to a test process that has gone away (R4). It is
	// nil in detached mode.
	boundShutdown chan struct{}
	boundOnce     sync.Once
}

// Run is the holder's main loop.
func Run() error {
	// Pin this goroutine to its OS thread for the holder's lifetime. Every VM is
	// booted from here (bootMember runs synchronously below), and on Linux each
	// VM's cloud-hypervisor child carries a PR_SET_PDEATHSIG so it dies if its
	// parent thread dies. A stable, long-lived parent thread makes that signal
	// fire only when the holder actually exits, not when Go happens to retire the
	// launching thread (ADR-0013).
	runtime.LockOSThread()

	names := GetRunnerVMNames()
	if len(names) == 0 {
		return errors.New("no VM names provided")
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	options, err := opts.Decode(os.Getenv(control.EnvOpts))
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
	// booting, so the spawning client sees status the moment it polls.
	for _, name := range names {
		if err := h.register(name); err != nil {
			return fmt.Errorf("register %s: %w", name, err)
		}
	}

	// Bound mode: begin watching the parent process and the control connection
	// now, before the (potentially slow) boot, so the client's bind dial connects
	// promptly and a parent death during boot is still caught (R4).
	if pidStr := os.Getenv(control.EnvParentPID); pidStr != "" {
		h.boundShutdown = make(chan struct{})
		if parentPID, err := strconv.Atoi(pidStr); err == nil {
			go h.watchParent(parentPID)
		}
		if ln := h.listenControl(names[0]); ln != nil {
			defer func() {
				_ = ln.Close()
				_ = os.Remove(h.st.ControlSocketPath(names[0]))
			}()
			go h.serveControl(ln)
		}
	}

	// NewCluster performs the one-time, cluster-wide pull of the image and VMM
	// binaries before any VM exists. Mark members "downloading" around it so the
	// client's readiness wait does not charge that (potentially multi-GB) download
	// against the per-boot budget (ADR-0013); flip back to "starting" once the
	// pull is done and the per-VM boot begins.
	for _, name := range names {
		h.setState(name, control.StateDownloading)
	}
	ctx := context.Background()
	cluster, err := orchestrator.NewCluster(options...)
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
			h.setState(name, control.StateStarting)
		}
		for _, name := range names {
			h.bootMember(ctx, name)
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// In bound mode, a nil channel select case never fires, so the same select
	// covers both modes: bound adds the parent/EOF teardown trigger.
	if h.allRunning(names) {
		// Healthy cluster: serve until a signal, until every member is stopped
		// individually (the last `stop` closes done), or until the bound parent
		// goes away.
		select {
		case <-sigCh:
		case <-h.done:
		case <-h.boundShutdown:
		}
	} else {
		// Initial boot was all-or-nothing and at least one member failed. Keep
		// serving the error briefly so the client reads it, then tear the whole
		// cluster down — no half-up clusters from a single `up`.
		select {
		case <-sigCh:
		case <-h.boundShutdown:
		case <-time.After(failedClusterLinger):
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h.stopAll(shutdownCtx)
	return nil
}

// watchParent triggers a bound holder's teardown when its parent test process is
// gone. An attached child is reparented (to launchd/init) the moment its parent
// dies, however the parent died — including kill -9, which delivers no signal —
// so a changed parent PID is a robust, PID-reuse-resistant death signal (the
// reparent, not the bare PID, is what we test). It is the backstop for the faster
// control-connection EOF path (R4).
func (h *holder) watchParent(parentPID int) {
	for {
		time.Sleep(time.Second)
		if os.Getppid() != parentPID {
			h.triggerBoundShutdown()
			return
		}
	}
}

// listenControl opens the holder-wide control socket the bound client connects
// to. A failure is non-fatal: the parent-pid poll still backstops teardown.
func (h *holder) listenControl(primary string) net.Listener {
	sockPath := h.st.ControlSocketPath(primary)
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen control socket: %v\n", err)
		return nil
	}
	return ln
}

// serveControl accepts connections on the holder-wide control socket and hands
// each to handleBindConn. It keeps accepting so a client whose first attempt
// failed the handshake (a dial retry) can connect again.
func (h *holder) serveControl(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed on shutdown
		}
		go h.handleBindConn(conn)
	}
}

// handleBindConn runs the server side of the bind handshake: read the bind
// command, reply with the protocol version, then wait for the client's ack. Only
// an ACKED connection arms the death-watch — its later EOF means the parent test
// process is gone, the faster counterpart to watchParent (R4). A connection that
// closes before acking (a dial retry, a stray probe) is dropped WITHOUT a
// teardown, so it cannot be mistaken for the parent dying.
func (h *holder) handleBindConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	buf := make([]byte, 64)

	// Bounded handshake: bind command, then ack. A client that never completes it
	// is not a real bind, so just drop the connection.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Read(buf); err != nil { // bind command
		return
	}
	if _, err := conn.Write([]byte(control.ProtocolVersion)); err != nil {
		return
	}
	if _, err := conn.Read(buf); err != nil { // ack — confirms the client is committing
		return
	}

	// Established: the client holds this connection for the helper's lifetime.
	// Its EOF (any later read returning) means the parent is gone — tear down.
	_ = conn.SetReadDeadline(time.Time{})
	for {
		if _, err := conn.Read(buf); err != nil {
			h.triggerBoundShutdown()
			return
		}
	}
}

func (h *holder) triggerBoundShutdown() {
	h.boundOnce.Do(func() {
		if h.boundShutdown != nil {
			close(h.boundShutdown)
		}
	})
}

// register opens a member's control socket and pidfile and marks it starting.
// It ensures the member directory exists first, because the pidfile lives inside
// it but the VM (and thus store.Create) boots later. This is the single
// chokepoint for both boot paths — the initial loop and addMember's runtime
// re-join — so the directory is guaranteed for a brand-new member too.
func (h *holder) register(name string) error {
	if err := h.st.EnsureDir(name); err != nil {
		return fmt.Errorf("ensure member dir: %w", err)
	}
	// If the subsequent boot fails, this dir is intentionally left in place: a
	// re-joining member keeps its persistent disk, and an empty dir from a
	// never-retried failed boot is benign (List/cmdList skip a configless member
	// and the next `up` reuses the dir). Deleting it here would risk wiping a
	// real disk on a transient boot failure — cattle with persistence.

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

	m := &member{name: name, state: control.StateStarting, ln: ln, gone: make(chan struct{})}
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

func (h *holder) markRunning(m *member, vm *orchestrator.VM) {
	m.mu.Lock()
	m.state = control.StateRunning
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
	wasActive := m.state == control.StateStarting || m.state == control.StateRunning ||
		m.state == control.StateDownloading
	m.state = control.StateError
	m.err = err.Error()
	m.mu.Unlock()
	if wasActive {
		h.decRunning()
	}
}

func (h *holder) markStopped(m *member) {
	m.mu.Lock()
	wasActive := m.state == control.StateStarting || m.state == control.StateRunning ||
		m.state == control.StateDownloading
	m.state = control.StateStopped
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
		ok := m.state == control.StateRunning
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
	case control.CmdStatus:
		data, _ := json.Marshal(m.status())
		_, _ = conn.Write(data)

	case control.CmdStop:
		_, _ = conn.Write([]byte("ok"))
		h.stopMember(context.Background(), m.name)

	case control.CmdAddMember:
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
