// Package control is the client half of the holder protocol: it spawns a holder
// process, waits for its members to come up, and drives them (createnetwork/
// reserve/boot-member/status/stop) over the per-member unix sockets. It is
// backend-neutral — it imports only internal/store, never a hypervisor or the
// orchestrator — so the client and CLI that drive a holder over this protocol stay
// pure Go and need no codesign (ADR-0017, R1).
//
// Two spawn modes share one wire protocol. The CLI spawns DETACHED (the holder
// is a session leader via Setsid and its VMs outlive the command —
// cattle-with-persistence). The library spawns BOUND/attached: the holder is a
// child of the test process, is handed the client PID, and holds one long-lived
// control connection open; when the test process dies the holder reaps itself and
// its VMs (ADR-0017, R4). The server half lives in internal/holder.
package control

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pilat/fleetbox/internal/store"
)

const (
	// RunnerFlag marks a process as a holder and is followed by the
	// comma-separated member names it owns. The holder server parses it; Spawn
	// passes it. The helper no longer receives the VM options — it is a
	// backend-server that the client drives over RPCs (ADR-0020), so the only thing
	// it needs at launch is the member names it will serve.
	RunnerFlag = "--fleetbox-runner"

	// ReconcileFlag marks a one-shot reconcile launch of the helper: it reclaims
	// orphaned host network state (Linux bridges/taps/iptables/ip_forward) and
	// exits, without serving any member. It backs Prune (ADR-0013/0020).
	ReconcileFlag = "--fleetbox-reconcile"

	// EnvParentPID is set only in bound mode: it is the client PID the holder
	// watches so it can reap itself when the test process is gone (R4).
	EnvParentPID = "FLEETBOX_PARENT_PID"

	// ProtocolVersion is exchanged on the bind handshake. The client and the
	// helper bake in this constant from the same source, so a download (whose
	// filename is version-stamped) always matches; a mismatch means a stale
	// FLEETBOX_HELPER override pointing at a different build, which Spawn rejects
	// loudly rather than driving with an incompatible protocol (ADR-0017, R5).
	//
	// "2" is the NDJSON command protocol with the resolved-member-spec payload and
	// the create-network/reserve/boot-member exchange; "1" was the fixed-256-byte
	// text protocol carrying an image alias. The bump forces a stale "1" helper to
	// be rejected at handshake rather than driven with an incompatible wire format.
	ProtocolVersion = "2"

	// Wire commands, shared with the holder server half (internal/holder) so the
	// two ends never drift. The command-socket commands (status/stop and the
	// createnetwork/reserve/boot-member set in wire.go) travel as NDJSON Request
	// objects; only bind/ack stay a raw-text handshake on the .ctl socket. Adding a
	// member to a live cluster is reserve + boot-member (no dedicated command).
	CmdStatus = "status"
	CmdStop   = "stop"
	CmdBind   = "bind"
	// CmdBindAck confirms the bind handshake. The client sends it only after it
	// has accepted the helper's version and is committing to hold the connection;
	// the helper arms its EOF death-watch only after receiving it. This keeps a
	// connection that closes mid-handshake (a dial retry, a stray probe) from
	// being mistaken for the parent dying and tearing the cluster down (R4).
	CmdBindAck = "ack"

	// Member states reported over the status socket. Since ADR-0020 the image pull
	// is client-side (before the helper is spawned), so StateDownloading now covers
	// only the helper's one-time VMM-binary fetch; it is reported separately so the
	// readiness wait does not charge that download against the per-boot budget.
	StateDownloading = "downloading"
	StateStarting    = "starting"
	StateRunning     = "running"
	StateStopped     = "stopped"
	StateError       = "error"

	// maxDownloadWait bounds the StateDownloading phase: while a member is
	// pulling, WaitMembers keeps the boot deadline ahead of it, but a stuck
	// download must still fail rather than hang forever.
	maxDownloadWait = 30 * time.Minute

	// bindDialTimeout bounds how long Spawn retries dialing the holder's
	// control socket before giving up on the fast EOF-teardown path and relying
	// on the holder's parent-pid poll alone (R4).
	bindDialTimeout = 5 * time.Second

	// sockDialWindow bounds how long dialHolder retries connecting to a holder's
	// member socket — long enough to cover a detached helper's startup before it
	// opens its sockets, short enough to fail fast on a helper that never came up.
	sockDialWindow = 10 * time.Second
)

// errBindUnavailable signals that the holder's control socket never came up
// within bindDialTimeout. It is non-fatal: Spawn proceeds without the fast
// EOF-teardown path and the holder's parent-pid poll backstops teardown.
var errBindUnavailable = errors.New("control socket unavailable")

// Status represents the state of a VM member.
type Status struct {
	Name    string `json:"name"`
	PID     int    `json:"pid"`
	Running bool   `json:"running"`
	IP      string `json:"ip"`
	State   string `json:"state"` // "starting", "running", "stopped", "error"
	Error   string `json:"error,omitempty"`
}

// SpawnConfig configures a holder launch. Exe is the binary to run (the
// downloaded fleetbox-helper on darwin, os.Executable() for the linux
// re-exec-self CLI). Names are the members the holder will serve sockets for at
// launch; the client drives their boot afterwards over RPCs. Bound selects the
// library lifetime mode (attached + parent PID + control connection); false is
// the CLI's detached/persistent mode.
type SpawnConfig struct {
	Exe   string
	Names []string
	Bound bool
}

// Session is a handle to a spawned holder. In bound mode it owns the control
// connection whose EOF triggers holder teardown and reaps the child process;
// in detached mode Close merely releases the persistent process.
type Session struct {
	cmd       *exec.Cmd
	bound     bool
	bindConn  net.Conn
	closeOnce sync.Once
}

// IsRunning reports whether the holder serving a member is alive, by reading its
// pidfile and signalling the process.
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
	return signalMeansAlive(proc.Signal(syscall.Signal(0)))
}

// signalMeansAlive classifies a kill(pid, 0) liveness probe. The process is alive
// if the probe succeeded (nil) OR failed with EPERM — EPERM means the process
// EXISTS but is owned by another user, so we are not permitted to signal it. That
// is exactly a non-root `ls`/`ssh` probing the root-owned holder an elevated `up`
// spawned (ADR-0023): the holder is alive, we just can't signal across the uid
// boundary. Only a genuinely absent process (ESRCH, or any other error) is "not
// running". Treating EPERM as dead was why non-root read-only commands reported a
// running VM as stopped. Split out as a pure function so the cross-uid case is
// unit-testable without spawning a foreign-owned process.
func signalMeansAlive(err error) bool {
	return err == nil || errors.Is(err, syscall.EPERM)
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
			return nil, fmt.Errorf("connect to holder: %w", err)
		}
		defer func() { _ = conn.Close() }()

		if err := WriteMessage(conn, Request{Cmd: CmdStatus}); err != nil {
			return nil, fmt.Errorf("send status: %w", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

		var resp Response
		if err := ReadMessage(conn, &resp); err != nil {
			return nil, fmt.Errorf("read status: %w", err)
		}
		if resp.Error != "" {
			return nil, errors.New(resp.Error)
		}
		if resp.Status == nil {
			return nil, errors.New("status reply missing status")
		}
		return resp.Status, nil
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
		State: StateStopped,
	}, nil
}

// dialHolder connects to a holder member socket, retrying until the window
// elapses, so the first RPC to a freshly spawned detached helper does not race
// the helper's socket setup.
func dialHolder(sockPath string, window time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(window)
	for {
		conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("connect to holder: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// SendCommand dials the holder socket serving member `name`, sends one NDJSON
// request, and returns the reply. It is the client transport the remote-proxy
// backend uses for the cluster-level RPCs (createnetwork/reserve/boot-member,
// routed through the primary member's socket) and for stop. A non-empty
// Response.Error is turned into a Go error. `timeout` bounds the wait for the
// reply, which a slow boot-member needs to be generous.
//
// The dial is retried for a short window: a DETACHED helper (CLI) is not bound,
// so Spawn returns before the helper has opened its member sockets, and the first
// RPC (createnetwork) would otherwise race the helper's startup. The BOUND
// (library) path is already synchronized by Spawn's bind handshake, so the retry
// is a fast no-op there.
func SendCommand(st *store.Store, name string, req Request, timeout time.Duration) (*Response, error) {
	sockPath := st.SocketPath(name)
	conn, err := dialHolder(sockPath, sockDialWindow)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	if err := WriteMessage(conn, req); err != nil {
		return nil, fmt.Errorf("send %s: %w", req.Cmd, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))

	var resp Response
	if err := ReadMessage(conn, &resp); err != nil {
		return nil, fmt.Errorf("read %s reply: %w", req.Cmd, err)
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return &resp, nil
}

// Stop gracefully shuts down a single member (the holder keeps running for its
// siblings until the last one leaves).
func Stop(st *store.Store, name string) error {
	if !IsRunning(st, name) {
		return nil
	}

	sockPath := st.SocketPath(name)
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect to holder: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := WriteMessage(conn, Request{Cmd: CmdStop}); err != nil {
		return fmt.Errorf("send stop: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	var resp Response
	_ = ReadMessage(conn, &resp) // ack is best-effort; the pidfile poll below is authoritative

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

// Spawn launches a holder for the given member names. In detached mode the
// holder is a persistent session leader; in bound mode it is an attached child
// handed the client PID, and Spawn additionally opens the holder's control
// connection so its EOF reaps the holder when the client goes away (R4). It does
// not wait for the members to boot — call WaitMembers (or use the returned
// Session) for that.
func Spawn(st *store.Store, cfg SpawnConfig) (*Session, error) {
	if len(cfg.Names) == 0 {
		return nil, errors.New("no VM names provided")
	}

	logPath := st.BaseDir() + "/runner-" + cfg.Names[0] + ".log"
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("create runner log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(cfg.Exe, RunnerFlag, strings.Join(cfg.Names, ","))
	cmd.Env = os.Environ()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil

	if cfg.Bound {
		// Attached: the holder is a child of this process so it can detect the
		// parent dying (reparenting) and reap itself + its in-process VMs.
		cmd.Env = append(cmd.Env, EnvParentPID+"="+strconv.Itoa(os.Getpid()))
	} else {
		// Detached: a new session so the holder (and its VMs) outlive the CLI.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start holder: %w", err)
	}

	sess := &Session{cmd: cmd, bound: cfg.Bound}
	if cfg.Bound {
		conn, err := dialBind(st, cfg.Names[0])
		switch {
		case errors.Is(err, errBindUnavailable):
			// The control socket never came up (a holder that died before
			// listening): WaitMembers surfaces the real boot error and the
			// parent-pid poll still backstops teardown, so proceed without it.
		case err != nil:
			// A version mismatch is fatal: stop the incompatible helper rather
			// than drive it with the wrong protocol.
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			return nil, err
		default:
			sess.bindConn = conn
		}
	}
	return sess, nil
}

// dialBind retries connecting to the holder's control socket until
// bindDialTimeout, runs the bind handshake (send bind, verify the helper's
// version, send ack), and on success returns the connection held open so its EOF
// signals the helper that the parent has gone. A connection that fails the
// handshake is closed and retried — the helper does not arm its death-watch until
// it sees the ack, so a closed mid-handshake connection never triggers a spurious
// teardown (R4). It returns errBindUnavailable if the socket never came up
// (non-fatal) and a version-mismatch error otherwise.
func dialBind(st *store.Store, primary string) (net.Conn, error) {
	sockPath := st.ControlSocketPath(primary)
	deadline := time.Now().Add(bindDialTimeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", sockPath, time.Second)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		peer, err := bindHandshake(conn)
		if err != nil {
			// Transient (socket up but handshake not ready): retry until deadline.
			_ = conn.Close()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if peer != ProtocolVersion {
			_ = conn.Close()
			return nil, fmt.Errorf(
				"fleetbox helper protocol %q does not match client %q (stale FLEETBOX_HELPER?)",
				peer, ProtocolVersion)
		}
		// Commit: ack so the helper arms its death-watch on THIS connection.
		if _, err := conn.Write([]byte(CmdBindAck)); err != nil {
			_ = conn.Close()
			return nil, errBindUnavailable
		}
		_ = conn.SetReadDeadline(time.Time{}) // clear; now hold the conn open for EOF
		return conn, nil
	}
	return nil, errBindUnavailable
}

// bindHandshake sends the bind command and reads the helper's protocol version.
func bindHandshake(conn net.Conn) (string, error) {
	if _, err := conn.Write([]byte(CmdBind)); err != nil {
		return "", fmt.Errorf("send bind: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		return "", fmt.Errorf("read version: %w", err)
	}
	return strings.TrimSpace(string(buf[:n])), nil
}

// WaitMembers polls per-member status until every name is running with an IP,
// returning early with an error if any member enters the error state. While any
// member is downloading the image/VMM binaries it announces the pull once and
// keeps the boot deadline ahead of the (bounded) download.
func WaitMembers(st *store.Store, names []string, timeout time.Duration) (map[string]*Status, error) {
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
			if status.State == StateError {
				return nil, fmt.Errorf("%s failed: %s", name, status.Error)
			}
			if status.State == StateRunning && status.IP != "" {
				result[name] = status
				continue
			}
			if status.State == StateDownloading {
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

// Close tears the session down. In bound mode it closes the control connection
// (the holder's EOF teardown trigger) and reaps the child once it exits; in
// detached mode it releases the persistent process so this client can exit
// without leaving a zombie. It is idempotent.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		if s.bindConn != nil {
			_ = s.bindConn.Close()
		}
		if s.cmd == nil || s.cmd.Process == nil {
			return
		}
		if s.bound {
			_ = s.cmd.Wait() // attached: reap once the holder exits
		} else {
			_ = s.cmd.Process.Release() // detached: stop tracking the persistent holder
		}
	})
	return nil
}
