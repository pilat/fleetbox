package control

import (
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/pilat/fleetbox/internal/store"
)

// These white-box tests drive the unexported client bind path (dialBind →
// bindHandshake) directly against an in-test fake server on a real unix socket.
// They are the only place the version-mismatch rejection (control.go's peer !=
// ProtocolVersion branch) is reachable: the real holder server always sends
// ProtocolVersion, so a mismatch cannot be produced end-to-end (ADR-0018).

func TestDialBindAcceptsMatchingVersion(t *testing.T) {
	st := shortTempStore(t)
	const primary = "solo"
	ln, err := net.Listen("unix", st.ControlSocketPath(primary))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 64)
		_, _ = conn.Read(buf)                      // bind command
		_, _ = conn.Write([]byte(ProtocolVersion)) // matching version
		_, _ = conn.Read(buf)                      // ack
		_, _ = conn.Read(buf)                      // blocks until the client closes
	}()

	conn, err := dialBind(st, primary)
	if err != nil {
		t.Fatalf("dialBind: %v", err)
	}
	if conn == nil {
		t.Fatal("dialBind returned a nil conn on the happy path")
	}
	_ = conn.Close()
	<-serverDone
}

func TestDialBindRejectsVersionMismatch(t *testing.T) {
	st := shortTempStore(t)
	const primary = "solo"
	ln, err := net.Listen("unix", st.ControlSocketPath(primary))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 64)
		_, _ = conn.Read(buf)          // bind command
		_, _ = conn.Write([]byte("2")) // a version the client does not speak
	}()

	conn, err := dialBind(st, primary)
	if err == nil {
		_ = conn.Close()
		t.Fatal("dialBind = nil error, want a version-mismatch rejection")
	}
	if conn != nil {
		t.Error("dialBind returned a non-nil conn alongside the mismatch error")
	}
	// It must be the fatal mismatch, not the non-fatal "socket never came up".
	if errors.Is(err, errBindUnavailable) {
		t.Fatalf("got errBindUnavailable, want version-mismatch error: %v", err)
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("error = %q, want it to report the protocol mismatch", err)
	}
	<-serverDone
}

// shortTempStore creates a store under a short /tmp-rooted base directory rather
// than t.TempDir(): the control socket path (run/<hash>.ctl) must fit the
// 104-byte sun_path limit, and macOS's $TMPDIR (/var/folders/...) is long enough
// to blow it. Production stays clear of this because ~/.fleetbox is short.
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
