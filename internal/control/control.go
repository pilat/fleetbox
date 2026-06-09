// Package control is the client half of the holder protocol: it spawns a holder
// process, waits for its members to come up, and queries or stops them over the
// per-member unix sockets. It is backend-neutral — it imports only internal/opts
// and internal/store, never a hypervisor or the orchestrator — so the darwin
// library client and CLI (which both drive a holder over this protocol) stay
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
	"encoding/json"
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

	"github.com/pilat/fleetbox/internal/opts"
	"github.com/pilat/fleetbox/internal/store"
)

const (
	// RunnerFlag marks a process as a holder and is followed by the
	// comma-separated member names it owns. The holder server parses it; Spawn
	// passes it.
	RunnerFlag = "--fleetbox-runner"

	// EnvOpts carries the JSON-encoded VM options (opts.Encode) to the holder.
	EnvOpts = "FLEETBOX_OPTS"

	// EnvParentPID is set only in bound mode: it is the client PID the holder
	// watches so it can reap itself when the test process is gone (R4).
	EnvParentPID = "FLEETBOX_PARENT_PID"

	// ProtocolVersion is exchanged on the bind handshake. The client and the
	// helper bake in this constant from the same source, so a download (whose
	// filename is version-stamped) always matches; a mismatch means a stale
	// FLEETBOX_HELPER override pointing at a different build, which Spawn rejects
	// loudly rather than driving with an incompatible protocol (ADR-0017, R5).
	ProtocolVersion = "1"

	// Wire commands, shared with the holder server half (internal/holder) so the
	// two ends never drift.
	CmdStatus    = "status"
	CmdStop      = "stop"
	CmdAddMember = "addmember"
	CmdBind      = "bind"
	// CmdBindAck confirms the bind handshake. The client sends it only after it
	// has accepted the helper's version and is committing to hold the connection;
	// the helper arms its EOF death-watch only after receiving it. This keeps a
	// connection that closes mid-handshake (a dial retry, a stray probe) from
	// being mistaken for the parent dying and tearing the cluster down (R4).
	CmdBindAck = "ack"

	// Member states reported over the status socket. StateDownloading is the
	// one-time pull of the image + VMM binaries that precedes booting; it is
	// reported separately so the readiness wait does not charge that
	// (potentially multi-GB) download against the per-boot budget (ADR-0013).
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
// re-exec-self CLI). Bound selects the library lifetime mode (attached + parent
// PID + control connection); false is the CLI's detached/persistent mode.
type SpawnConfig struct {
	Exe     string
	Names   []string
	Options []opts.Option
	Bound   bool
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
			return nil, fmt.Errorf("connect to holder: %w", err)
		}
		defer func() { _ = conn.Close() }()

		_, _ = conn.Write([]byte(CmdStatus))
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
		State: StateStopped,
	}, nil
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

	_, _ = conn.Write([]byte(CmdStop))
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

	if _, err := conn.Write([]byte(CmdAddMember + " " + name)); err != nil {
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

	_, err = WaitMembers(st, []string{name}, 5*time.Minute)
	return err
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

	optData, err := opts.Encode(cfg.Options)
	if err != nil {
		return nil, fmt.Errorf("encode options: %w", err)
	}

	logPath := st.BaseDir() + "/runner-" + cfg.Names[0] + ".log"
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("create runner log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(cfg.Exe, RunnerFlag, strings.Join(cfg.Names, ","))
	cmd.Env = append(os.Environ(), EnvOpts+"="+optData)
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
