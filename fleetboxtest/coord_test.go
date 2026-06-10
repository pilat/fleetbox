//go:build (darwin && arm64) || linux

// coord_test.go exercises the cross-process coordination layer — the real pure-Go
// client driving a fake helper over the holder protocol — without booting a real
// VM. It is the pre-merge gate for the bound-helper teardown that the whole fake
// backend exists to protect (ADR-0018/0020). It runs on both platforms: on macOS
// the helper is a SEPARATELY-BUILT fake binary (cmd/fleetbox-helper -tags
// fleetbox_fake) reached via FLEETBOX_HELPER; on Linux it is the test binary
// itself, self-reexec'd into the fake holder by internal/holder's init()
// interceptor (no separate binary, no FLEETBOX_HELPER — helperExe is os.Executable).
//
// BANNER: this proves COORDINATION, not that a VM boots. Green here is NOT "VMs
// work" — real boot/SSH/IP discovery stay covered by `make test-vm` (M3+/26+) and
// vm-linux.yml. The fake's Stop is a no-op; only the spawn/reap/EOF logic around it
// is tested here.
//
// It is built into the fleetboxtest binary and gated at RUNTIME on
// FLEETBOX_FAKE_HELPER (set by `make test-fake`). Critically it uses the public
// fleetbox.Start directly, NOT fleetboxtest.Start — the latter skips on a host
// without nested virt, which on a hosted runner would silently turn this gate into
// a no-op (the exact failure this plan prevents).
package fleetboxtest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pilat/fleetbox"
	"github.com/pilat/fleetbox/internal/store"
)

// coordImageURL resolves in image.Ensure to the cache filename "fbtiny.raw" (URL
// basename, .qcow2/.img stripped, ".raw" appended), which shortHome pre-seeds so
// the spawned fake helper finds it cached and never downloads.
const coordImageURL = "https://invalid.test/fbtiny"

const coordBanner = `coordination test: proves the client<->helper<->holder coordination and ` +
	`teardown, NOT that a VM boots (the helper is fake; real boot is make test-vm / vm-linux.yml)`

// TestCoordHappyPath drives the full client path — fleetbox.Start → exec fake
// helper → holder → fake backend → WaitMembers — then normal teardown, asserting
// the helper's socket/pidfile come up and are retired and the helper process
// exits. It never SSHes: the fake IP is unroutable and a dial would hang.
func TestCoordHappyPath(t *testing.T) {
	requireFakeHelper(t)
	t.Log(coordBanner)

	home, st := shortHome(t)
	t.Setenv("HOME", home) // this process's fleetbox.Start (and the helper it execs) share it

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	vm, err := fleetbox.Start(ctx, "solo", fleetbox.WithImage(coordImageURL), fleetbox.WithDiskGB(1))
	if err != nil {
		t.Fatalf("fleetbox.Start: %v", err)
	}

	if vm.IP() == nil {
		t.Error("VM came up without an IP")
	}
	if got := vm.State(); got != "running" {
		t.Errorf("State() = %q, want running", got)
	}
	assertExists(t, st.PidfilePath("solo"))
	assertExists(t, st.SocketPath("solo"))
	helperPID := readPID(t, st.PidfilePath("solo"))

	// Normal teardown: Destroy stops the sole member (helper exits) and removes the
	// store files. Assert the coordination artifacts and the process are gone.
	if err := vm.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	assertPathGoneWithin(t, st.PidfilePath("solo"), 5*time.Second)
	assertPathGoneWithin(t, st.SocketPath("solo"), 5*time.Second)
	assertProcessDeadWithin(t, helperPID, 5*time.Second)
}

// TestCoordReapOnKillNine is the crown jewel: it kills the spawning process with
// SIGKILL — no cleanup runs — and asserts the bound helper reaps itself and its
// (fake) VM anyway, via its reparent poll and control-connection EOF. A leaked
// helper here is the cardinal sin for a "VMs as fixtures" library. It uses the
// standard os/exec helper-process pattern: it re-execs this test binary as a
// child (TestCoordHelperChild) that calls fleetbox.Start and blocks.
func TestCoordReapOnKillNine(t *testing.T) {
	requireFakeHelper(t)
	t.Log(coordBanner)

	home, st := shortHome(t)

	// Re-exec self as the blocking child. It shares HOME (so the helper finds the
	// pre-seeded image and writes its pidfile where we can read it) and inherits
	// FLEETBOX_HELPER / FLEETBOX_FAKE_HELPER from the make environment.
	cmd := exec.Command(os.Args[0], "-test.run=^TestCoordHelperChild$", "-test.v")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "HOME="+home)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// The pidfile carries the HELPER's PID (holder.WritePidfile), written before
	// boot — so it appears quickly once the child has spawned the helper.
	helperPID := pollHelperPID(t, st, "solo", 30*time.Second)

	// SIGKILL the child: it cannot run any cleanup, so only the helper's own
	// parent-death watch can reap it.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	_ = cmd.Wait() // reap the child and drain its output pipes

	// Window comfortably exceeds the 1s watchParent poll; the control-conn EOF is
	// faster still. If the helper survives this, teardown leaked.
	assertProcessDeadWithin(t, helperPID, 10*time.Second)
	assertPathGoneWithin(t, st.PidfilePath("solo"), 10*time.Second)
	assertPathGoneWithin(t, st.SocketPath("solo"), 10*time.Second)
	if t.Failed() {
		t.Logf("child output:\n%s", out.String())
	}
}

// TestCoordHelperChild is the re-exec'd child of TestCoordReapOnKillNine, not a
// standalone test: it runs its body only when GO_WANT_HELPER_PROCESS=1, calls
// fleetbox.Start, and blocks until the parent SIGKILLs it. In any normal run
// (including the parent's own -run TestCoord sweep) it returns immediately.
func TestCoordHelperChild(_ *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	vm, err := fleetbox.Start(context.Background(), "solo",
		fleetbox.WithImage(coordImageURL), fleetbox.WithDiskGB(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "child fleetbox.Start: %v\n", err)
		os.Exit(3)
	}
	fmt.Fprintf(os.Stderr, "child up: ip=%s\n", vm.IP())
	select {} // hold the VM (and the bound session) until the parent kills us
}

// fakeRecord mirrors one JSON line the fake backend appends to FLEETBOX_FAKE_RECORD
// (internal/backend/fake). Declared locally so the test never links the fake
// package — the contract is the JSON shape, read across the process boundary.
type fakeRecord struct {
	Op           string   `json:"op"`
	Name         string   `json:"name"`
	DiskPath     string   `json:"disk_path"`
	SeedPath     string   `json:"seed_path"`
	EFIPath      string   `json:"efi_path"`
	FixturePaths []string `json:"fixture_paths"`
	AssignedIP   string   `json:"assigned_ip"`
	MAC          string   `json:"mac"`
	IPHint       string   `json:"ip_hint"`
}

// TestCoordRecordsBackendSpec proves the client-side artifact glue end to end and
// cross-process: the client (this process) resolves the image, copies the disk,
// builds the seed and a fixture, writes config.json, then hands the helper a
// resolved spec over the protocol. The helper's fake backend appends what it
// received to FLEETBOX_FAKE_RECORD, which we read back to assert the threading —
// the in-process global-reads the deleted orchestrator_fake_test relied on are
// replaced by this record file plus the on-disk artifacts (ADR-0020, T9).
func TestCoordRecordsBackendSpec(t *testing.T) {
	requireFakeHelper(t)
	t.Log(coordBanner)

	home, st := shortHome(t)
	t.Setenv("HOME", home)
	recordPath := filepath.Join(home, "fake-record.jsonl")
	// FLEETBOX_FAKE_RECORD (internal/backend/fake.EnvFakeRecord); the spawned helper
	// inherits it via the environment and the fake writes the helper's backend args.
	t.Setenv("FLEETBOX_FAKE_RECORD", recordPath)

	hostDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostDir, "data.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatalf("write fixture source: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	vm, err := fleetbox.Start(ctx, "solo",
		fleetbox.WithImage(coordImageURL), fleetbox.WithDiskGB(1), fleetbox.WithFixture(hostDir, "/mnt/data"))
	if err != nil {
		t.Fatalf("fleetbox.Start: %v", err)
	}

	// The client built the artifacts on disk (image.CopyDisk, seed.Create,
	// fixture.BuildImage) and persisted config.json — none of which the helper does.
	assertFileNonEmpty(t, st.DiskPath("solo"))
	assertFileNonEmpty(t, st.SeedPath("solo"))
	assertFileNonEmpty(t, st.FixturePath("solo", 0))
	assertExists(t, filepath.Join(st.VMDir("solo"), "config.json"))

	// The helper's backend received the resolved spec the client built: assert the
	// reserve and create records the fake wrote across the process boundary.
	recs := readRecords(t, recordPath)
	reserve := findRecord(t, recs, "reserve", "solo")
	if reserve.MAC == "" {
		t.Error("reserve record has empty MAC")
	}
	create := findRecord(t, recs, "create", "solo")
	if create.DiskPath != st.DiskPath("solo") {
		t.Errorf("create.DiskPath = %q, want %q", create.DiskPath, st.DiskPath("solo"))
	}
	if create.SeedPath != st.SeedPath("solo") {
		t.Errorf("create.SeedPath = %q, want %q", create.SeedPath, st.SeedPath("solo"))
	}
	if create.EFIPath != st.EFIPath("solo") {
		t.Errorf("create.EFIPath = %q, want %q", create.EFIPath, st.EFIPath("solo"))
	}
	if len(create.FixturePaths) != 1 || create.FixturePaths[0] != st.FixturePath("solo", 0) {
		t.Errorf("create.FixturePaths = %v, want [%s]", create.FixturePaths, st.FixturePath("solo", 0))
	}
	// Fake's Subnet()=="" (DHCP path), so no static IP is threaded — the Linux
	// static-IP path is covered by real cloud-hypervisor in vm-linux.yml, not here.
	if create.AssignedIP != "" {
		t.Errorf("create.AssignedIP = %q, want empty (the fake mimics the DHCP path)", create.AssignedIP)
	}

	if err := vm.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	// Destroy stopped the member through the helper: a stop record appears.
	recs = readRecords(t, recordPath)
	findRecord(t, recs, "stop", "solo")
}

func requireFakeHelper(t *testing.T) {
	t.Helper()
	if os.Getenv("FLEETBOX_FAKE_HELPER") == "" {
		t.Skip("coordination test requires the fake helper; run via `make test-fake`")
	}
}

// readRecords parses the newline-delimited JSON the fake backend appended.
func readRecords(t *testing.T, path string) []fakeRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record file %s: %v", path, err)
	}
	var recs []fakeRecord
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r fakeRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("parse record %q: %v", line, err)
		}
		recs = append(recs, r)
	}
	return recs
}

// findRecord returns the first record matching op and name, failing if none.
func findRecord(t *testing.T, recs []fakeRecord, op, name string) fakeRecord {
	t.Helper()
	for _, r := range recs {
		if r.Op == op && r.Name == name {
			return r
		}
	}
	t.Fatalf("no %q record for %q in %d records", op, name, len(recs))
	return fakeRecord{}
}

// assertFileNonEmpty fails unless path exists and is non-empty.
func assertFileNonEmpty(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Size() == 0 {
		t.Fatalf("%s exists but is empty", path)
	}
}

// shortHome creates a short /tmp-rooted HOME, builds a store rooted at it, and
// pre-seeds the tiny image. The base must be short because the holder's unix
// socket paths (run/<hash>.sock|.ctl) must fit the 104-byte sun_path limit, which
// macOS's $TMPDIR (/var/folders/...) blows past. The returned store points at the
// same HOME/.fleetbox the spawned helper computes from the HOME env.
func shortHome(t *testing.T) (string, *store.Store) {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "fbhome")
	if err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	st, err := store.NewAt(filepath.Join(home, ".fleetbox"))
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	raw := filepath.Join(st.ImagesDir(), "fbtiny.raw")
	if err := os.WriteFile(raw, []byte("fleetbox-fake-tiny-image\n"), 0o644); err != nil {
		t.Fatalf("seed image %s: %v", raw, err)
	}
	return home, st
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pidfile %s: %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse pidfile %s = %q: %v", path, data, err)
	}
	return pid
}

// pollHelperPID waits for the member's pidfile to appear and returns the PID it
// holds (the helper process's PID).
func pollHelperPID(t *testing.T, st *store.Store, name string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(st.PidfilePath(name)); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pidfile for %q never appeared within %s", name, timeout)
	return 0
}

func assertProcessDeadWithin(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("helper process %d still alive after %s (teardown leaked)", pid, timeout)
}

func assertPathGoneWithin(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s still present after %s", path, timeout)
}
