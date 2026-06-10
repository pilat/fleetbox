package holder

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/pilat/fleetbox/internal/control"
	"github.com/pilat/fleetbox/internal/store"
)

// These white-box tests exercise the holder's server half of the protocol with
// NO backend and NO VM boot: the per-member status framing (real client dial →
// real handleConn → member.status() JSON round-trip) and the bound-mode control
// connection's EOF teardown (real serveControl/handleBindConn arming the
// death-watch, then the connection closing). They are the first time the holder's
// goroutines run under the race detector. They deliberately do NOT drive stop or
// addmember: those call m.vm.Stop()/cluster.Add() and would nil-deref on a
// synthetic member (vm == nil) — that end-to-end path is T4's job (ADR-0018).

func TestStatusRoundTrip(t *testing.T) {
	st := shortTempStore(t)
	h := newTestHolder(st)

	const (
		name = "solo"
		ip   = "203.0.113.7"
	)
	registerSyntheticMember(t, h, name, control.StateRunning, ip)

	// Real client → real server: GetStatus dials the member's socket, the holder's
	// handleConn answers with member.status() as JSON.
	status, err := control.GetStatus(st, name)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if status.State != control.StateRunning {
		t.Errorf("status.State = %q, want %q", status.State, control.StateRunning)
	}
	if status.IP != ip {
		t.Errorf("status.IP = %q, want %q", status.IP, ip)
	}

	// WaitMembers rides the same client/server path and must resolve a member that
	// is already running with an IP.
	result, err := control.WaitMembers(st, []string{name}, 5*time.Second)
	if err != nil {
		t.Fatalf("WaitMembers: %v", err)
	}
	if got := result[name]; got == nil || got.IP != ip || got.State != control.StateRunning {
		t.Fatalf("WaitMembers[%q] = %+v, want state=%q ip=%q", name, got, control.StateRunning, ip)
	}
}

func TestControlConnEOFTriggersBoundShutdown(t *testing.T) {
	st := shortTempStore(t)
	h := newTestHolder(st)

	const primary = "solo"
	ln := h.listenControl(primary)
	if ln == nil {
		t.Fatal("listenControl returned nil")
	}
	t.Cleanup(func() { _ = ln.Close() })
	go h.serveControl(ln)

	// Mimic the client's bind handshake by hand (control.dialBind/bindHandshake are
	// unexported in package control — T3b exercises them directly). Only an ACKed
	// connection arms the EOF death-watch.
	conn, err := net.DialTimeout("unix", st.ControlSocketPath(primary), 2*time.Second)
	if err != nil {
		t.Fatalf("dial control socket: %v", err)
	}
	if _, err := conn.Write([]byte(control.CmdBind)); err != nil {
		t.Fatalf("send bind: %v", err)
	}
	buf := make([]byte, 64)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if got := string(buf[:n]); got != control.ProtocolVersion {
		t.Fatalf("server version = %q, want %q", got, control.ProtocolVersion)
	}
	if _, err := conn.Write([]byte(control.CmdBindAck)); err != nil {
		t.Fatalf("send ack: %v", err)
	}

	// Closing the acked connection is the parent-gone signal: the server's read
	// returns and it must close boundShutdown. The test never closes the channel
	// itself — that would make the assertion tautological.
	_ = conn.Close()

	select {
	case <-h.boundShutdown:
		// server tore down as expected
	case <-time.After(3 * time.Second):
		t.Fatal("boundShutdown not closed after the control connection's EOF")
	}
}

// newTestHolder builds a holder wired for the protocol tests: a real store, an
// empty member registry, and an initialized boundShutdown channel (without it the
// EOF assertion would be dead). It is a test-only constructor — kept here, not in
// a production file, so it is never compiled into the released helper.
func newTestHolder(st *store.Store) *holder {
	return &holder{
		st:            st,
		members:       make(map[string]*member),
		done:          make(chan struct{}),
		boundShutdown: make(chan struct{}),
	}
}

// registerSyntheticMember registers a member through the real holder.register
// path (which opens its socket, writes the pidfile, and starts its accept loop),
// then sets the reported state/ip. The member has vm == nil, so it serves status
// but must not be asked to stop. It is retired on cleanup.
func registerSyntheticMember(t *testing.T, h *holder, name, state, ip string) {
	t.Helper()
	if err := h.register(name); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	m := h.memberByName(name)
	if m == nil {
		t.Fatalf("member %s not registered", name)
	}
	m.mu.Lock()
	m.state = state
	m.ip = ip
	m.mu.Unlock()
	t.Cleanup(func() { h.retire(m) })
}

// shortTempStore creates a store under a short /tmp-rooted base directory rather
// than t.TempDir(): the holder's unix socket paths (run/<hash>.sock|.ctl) must
// fit the 104-byte sun_path limit, and macOS's $TMPDIR (/var/folders/...) is long
// enough to blow it. Production stays clear of this because ~/.fleetbox is short.
func shortTempStore(t *testing.T) *store.Store {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fb")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	st, err := store.NewAt(dir)
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	return st
}
