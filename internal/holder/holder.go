// Package holder is the server half of the holder protocol: one process owns one
// cluster — its shared network and every VM on it — so a cluster gets the same
// VM↔VM connectivity the library StartN gives (ADR-0008). Since ADR-0020 it is a
// thin backend-server: it links the real backend (vz/cloud-hypervisor) and does
// nothing else. It does NOT resolve images, build disks/seeds/fixtures, or manage
// the store — all of that runs client-side in the orchestrator, which drives this
// process over the control protocol. The helper receives ready paths and boots
// what the client tells it to.
//
// It serves one control socket and one pidfile per member name, so the per-name
// CLI commands (ls/ssh/down/rm/status) address a member by talking to its socket,
// unaware that several members may live in one process. The cluster-level RPCs
// (createnetwork/reserve/boot-member) travel over the primary member's socket and
// are dispatched by name. Members can be stopped individually (the process
// survives until the last one leaves) and added at runtime (a stopped node
// re-joins the live cluster's network with a fresh reserve + boot-member).
//
// It runs in two lifetime modes (ADR-0017, R4). DETACHED: a persistent session
// leader whose VMs outlive the spawning CLI (cattle-with-persistence). BOUND: an
// attached child of a test process, selected by FLEETBOX_PARENT_PID; it watches
// that parent (reparent poll) and a long-lived control connection (EOF), and
// reaps itself and its VMs the moment the test process is gone. On darwin this
// package is compiled only into cmd/fleetbox-helper (which links vz); on linux the
// CLI/test binary re-execs itself into Run.
package holder

import (
	"context"
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

	"github.com/pilat/fleetbox/internal/backend"
	"github.com/pilat/fleetbox/internal/control"
	"github.com/pilat/fleetbox/internal/store"
)

// ipWaitTimeout bounds the helper-side wait for a freshly booted member's IP
// (vz discovers it from dhcpd_leases; cloud-hypervisor probes its static address).
// boot-member is synchronous, so this caps how long it blocks before replying.
const ipWaitTimeout = 2 * time.Minute

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

// IsReconcile reports whether this process was launched as a one-shot reconcile
// helper (the Prune path).
func IsReconcile() bool {
	return slices.Contains(os.Args, control.ReconcileFlag)
}

// RunReconcile reclaims orphaned host network state and returns, serving no
// member. It is the helper's Prune entrypoint: it builds the real backend and runs
// its Reconcile (Linux bridges/taps/iptables/ip_forward; a no-op on vz/fake).
func RunReconcile() error {
	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}
	b, err := newRealBackend(st)
	if err != nil {
		return fmt.Errorf("init backend: %w", err)
	}
	if err := b.Reconcile(); err != nil {
		return fmt.Errorf("reconcile: %w", err)
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
	vm    backend.VM

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

// holder owns the shared network, the per-member reservations, and the member
// registry for one process. It links the real backend and translates the
// control-protocol RPCs into backend calls.
type holder struct {
	st      *store.Store
	backend backend.Backend

	mu           sync.Mutex
	network      backend.Network
	reservations map[string]control.Reservation
	members      map[string]*member
	running      int // members currently starting or running
	done         chan struct{}
	doneOnce     sync.Once

	// boundShutdown is closed by the parent-pid poll or the control-socket EOF
	// when this holder is bound to a test process that has gone away (R4). It is
	// nil in detached mode.
	boundShutdown chan struct{}
	boundOnce     sync.Once
}

// Run is the holder's main loop.
func Run() error {
	// Pin this goroutine to its OS thread for the holder's lifetime. Every VM is
	// booted from a connection-handling goroutine, and on Linux each VM's
	// cloud-hypervisor child carries PR_SET_PDEATHSIG so it dies if the holder
	// dies. A stable, long-lived main thread makes that signal fire only when the
	// holder actually exits, not when Go retires a transient thread (ADR-0013).
	runtime.LockOSThread()

	names := GetRunnerVMNames()
	if len(names) == 0 {
		return errors.New("no VM names provided")
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	b, err := newRealBackend(st)
	if err != nil {
		return fmt.Errorf("init backend: %w", err)
	}

	// Reclaim any host network state orphaned by a predecessor that crashed before
	// teardown (no-op on vz/fake; on Linux it removes stale bridges/taps and
	// restores ip_forward if ours). Owned by the helper now, keyed on its PID
	// (ADR-0013/0020).
	_ = b.Reconcile()

	h := &holder{
		st:           st,
		backend:      b,
		reservations: make(map[string]control.Reservation),
		members:      make(map[string]*member),
		done:         make(chan struct{}),
	}

	// On any exit — clean shutdown, signal, or panic — release the shared network
	// so a Linux bridge and its egress rules don't leak. Runs after stopAll
	// (deferred LIFO), so members are down first. No-op on macOS.
	defer func() {
		h.mu.Lock()
		nw := h.network
		h.mu.Unlock()
		if nw != nil {
			_ = nw.Close()
		}
	}()

	// Register every spawn-name member as "starting" and serve its socket BEFORE
	// any boot, so the client can address it (and drive its boot) the moment it
	// connects. Runtime-added members register lazily in bootMember.
	for _, name := range names {
		if err := h.register(name); err != nil {
			return fmt.Errorf("register %s: %w", name, err)
		}
	}

	// Bound mode: begin watching the parent process and the control connection now,
	// before any boot, so the client's bind dial connects promptly and a parent
	// death during boot is still caught (R4).
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

	// Serve until a signal, until every member has been stopped individually (the
	// last stop closes done), or until the bound parent goes away. Members boot on
	// demand via boot-member RPCs, so there is no synchronous boot loop here. In
	// detached mode boundShutdown is nil, so that select case never fires.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-sigCh:
	case <-h.done:
	case <-h.boundShutdown:
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
	chmodClientSocket(sockPath)
	return ln
}

// chmodClientSocket loosens a holder-created unix socket to 0666 when the holder
// runs as root, so a non-root client (`ls`/`ssh`/`cp` and the library's bound
// control connection) can connect — a unix-socket connect needs WRITE permission,
// and a root umask leaves the socket 0755 → EACCES for the user (ADR-0023).
// Best-effort and a no-op off-root; 0666 on a local-only dev socket is an accepted
// tradeoff (noted in ADR-0023).
func chmodClientSocket(path string) {
	if os.Geteuid() == 0 {
		_ = os.Chmod(path, 0o666)
	}
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
// chokepoint for both boot paths — the initial spawn-name loop and bootMember's
// runtime add — so the directory is guaranteed for a brand-new member too.
func (h *holder) register(name string) error {
	if err := h.st.EnsureDir(name); err != nil {
		return fmt.Errorf("ensure member dir: %w", err)
	}
	// If the subsequent boot fails, this dir is intentionally left in place: a
	// re-joining member keeps its persistent disk, and an empty dir from a
	// never-retried failed boot is benign (List/cmdList skip a configless member
	// and the next `up` reuses the dir). Deleting it here would risk wiping a real
	// disk on a transient boot failure — cattle with persistence.

	sockPath := h.st.SocketPath(name)
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen socket: %w", err)
	}
	chmodClientSocket(sockPath)

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

// createNetwork creates the cluster's shared network on first request and returns
// its subnet (empty on the DHCP/vz path). It is idempotent: a repeated call
// returns the existing network's subnet.
func (h *holder) createNetwork() (string, error) {
	h.mu.Lock()
	if h.network != nil {
		subnet := h.network.Subnet()
		h.mu.Unlock()
		return subnet, nil
	}
	h.mu.Unlock()

	// backend.CreateNetwork is slow on Linux (ip/iptables system calls + a
	// reconcile sweep); do NOT hold h.mu across it, or a concurrent status/stop/
	// reserve RPC would stall for the full duration. Double-check after.
	nw, err := h.backend.CreateNetwork()
	if err != nil {
		return "", fmt.Errorf("create network: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.network != nil {
		_ = nw.Close() // another goroutine raced us to it; drop ours
		return h.network.Subnet(), nil
	}
	h.network = nw
	return nw.Subnet(), nil
}

// reserve allocates a member's address on the live network (helper-side, the
// successor to the orchestrator's allocateIP) and remembers it for the member's
// boot-member, so the NIC and the client's seed agree on the MAC/IP (Decisions
// 5, 6).
func (h *holder) reserve(name, ipHint string) (control.Reservation, error) {
	// Hold h.mu across the whole reservation so it is atomic AND idempotent: a
	// repeated reserve for the same member returns the first reservation instead of
	// allocating (and leaking) a second address. nw.Reserve is in-memory and fast,
	// so holding the lock is cheap (unlike createNetwork's system calls).
	h.mu.Lock()
	defer h.mu.Unlock()
	if res, ok := h.reservations[name]; ok {
		return res, nil
	}
	if h.network == nil {
		return control.Reservation{}, errors.New("network not created")
	}
	ip, mac, err := h.network.Reserve(name, ipHint)
	if err != nil {
		return control.Reservation{}, fmt.Errorf("reserve %s: %w", name, err)
	}
	res := control.Reservation{IP: ip, MAC: mac}
	h.reservations[name] = res
	return res, nil
}

// bootMember creates and starts a member's VM on the shared network from a
// resolved spec plus the address reserved for it, then waits for its IP. It is
// synchronous: it returns only once the VM is up (IP known) or has failed, so the
// client's boot-member RPC reply is the authoritative boot result. A runtime-added
// member (not a spawn name) is registered here first.
func (h *holder) bootMember(ctx context.Context, spec control.MemberSpec) error {
	h.mu.Lock()
	nw := h.network
	res, reserved := h.reservations[spec.Name]
	b := h.backend
	h.mu.Unlock()
	if nw == nil {
		return errors.New("network not created")
	}
	if !reserved {
		return fmt.Errorf("member %s was not reserved", spec.Name)
	}

	if h.memberByName(spec.Name) == nil {
		// register starts the member's accept loop, whose handlers run on their own
		// background contexts — not derived from this boot's ctx.
		if err := h.register(spec.Name); err != nil { //nolint:contextcheck // server accept loop owns its context
			return fmt.Errorf("register %s: %w", spec.Name, err)
		}
	}
	m := h.memberByName(spec.Name)
	if m == nil {
		return fmt.Errorf("member %s not registered", spec.Name)
	}
	// Refuse a duplicate boot for an already-running member: re-running b.Create
	// would fail and, via markError, wrongly decrement the running counter — which
	// for a solo member would close `done` and exit the holder out from under the
	// live VM. Reject without touching the counter.
	m.mu.Lock()
	already := m.vm != nil
	m.mu.Unlock()
	if already {
		return fmt.Errorf("member %s is already running", spec.Name)
	}

	cfg := backend.Config{
		Name:          spec.Name,
		DiskPath:      spec.DiskPath,
		SeedPath:      spec.SeedPath,
		EFIPath:       spec.EFIPath,
		MAC:           res.MAC,
		CPUs:          spec.CPUs,
		MemoryBytes:   spec.MemoryBytes,
		SerialLogPath: spec.SerialLogPath,
		FixturePaths:  spec.FixturePaths,
		AssignedIP:    res.IP,
	}

	vm, err := b.Create(cfg, nw)
	if err != nil {
		h.markError(m, err)
		return fmt.Errorf("create %s: %w", spec.Name, err)
	}
	if err := vm.Start(ctx); err != nil {
		_ = vm.Stop(ctx) // release any tap/process/serial a partial boot left behind
		h.markError(m, err)
		return fmt.Errorf("start %s: %w", spec.Name, err)
	}

	ipCtx, cancel := context.WithTimeout(ctx, ipWaitTimeout)
	ip, err := vm.WaitForIP(ipCtx)
	cancel()
	if err != nil {
		_ = vm.Stop(ctx)
		h.markError(m, err)
		return fmt.Errorf("wait for ip %s: %w", spec.Name, err)
	}

	if !h.markRunning(m, vm, ip) {
		// The member was stopped concurrently while we were booting it (a stop RPC,
		// or teardown). Reap the VM we just started so it does not outlive its
		// retired member.
		_ = vm.Stop(ctx)
		return fmt.Errorf("member %s was stopped during boot", spec.Name)
	}
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

// markRunning records a member as running with its VM and IP, unless the member
// was stopped concurrently during boot — in which case it returns false WITHOUT
// adopting the VM, so the caller reaps the orphan. (It does not touch the running
// counter: register counted the member, and a concurrent stop already decremented
// it via markStopped.)
func (h *holder) markRunning(m *member, vm backend.VM, ip string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == control.StateStopped {
		return false
	}
	m.state = control.StateRunning
	m.vm = vm
	m.ip = ip
	return true
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

// handleConn serves one NDJSON request on a member's socket. status/stop are
// member-scoped (they act on m); createnetwork/reserve/boot-member are
// cluster-level (the client routes them through the primary member's socket and
// they act on the holder by name, ignoring which socket carried them).
func (h *holder) handleConn(m *member, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	var req control.Request
	if err := control.ReadMessage(conn, &req); err != nil {
		return
	}

	switch req.Cmd {
	case control.CmdStatus:
		s := m.status()
		_ = control.WriteMessage(conn, control.Response{Status: &s})

	case control.CmdStop:
		_ = control.WriteMessage(conn, control.Response{})
		h.stopMember(context.Background(), m.name)

	case control.CmdCreateNetwork:
		subnet, err := h.createNetwork()
		if err != nil {
			_ = control.WriteMessage(conn, control.Response{Error: err.Error()})
			return
		}
		_ = control.WriteMessage(conn, control.Response{Subnet: subnet})

	case control.CmdReserve:
		if req.Name == "" {
			_ = control.WriteMessage(conn, control.Response{Error: "missing member name"})
			return
		}
		res, err := h.reserve(req.Name, req.IPHint)
		if err != nil {
			_ = control.WriteMessage(conn, control.Response{Error: err.Error()})
			return
		}
		_ = control.WriteMessage(conn, control.Response{Reservation: &res})

	case control.CmdBootMember:
		if req.Spec == nil || req.Spec.Name == "" {
			_ = control.WriteMessage(conn, control.Response{Error: "missing or invalid spec"})
			return
		}
		// boot-member runs on a fresh background context (the member outlives this
		// connection), not the per-request ctx.
		err := h.bootMember(context.Background(), *req.Spec)
		if err != nil {
			_ = control.WriteMessage(conn, control.Response{Error: err.Error()})
			return
		}
		_ = control.WriteMessage(conn, control.Response{})
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
