//go:build linux

package cloudhypervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// ipForwardPath is the sysctl that gates IPv4 forwarding host-wide.
const ipForwardPath = "/proc/sys/net/ipv4/ip_forward"

// ipForwardMarker is the filename, under the network state dir, that records the
// host's original ip_forward value the first time fleetbox turns forwarding on.
const ipForwardMarker = "ipforward.orig"

// netRecord is the persisted, write-ahead description of one bridge network and
// the host resources it owns (bridge, taps, egress rules). It is written before
// the matching `ip`/`iptables` commands run and deleted only after teardown is
// verified, so the record is always a superset of what exists on the host. That
// invariant lets a later `up` or `prune` clean up a network whose holder crashed
// before Close — orphans become recoverable instead of leaking (ADR-0013).
type netRecord struct {
	Bridge     string   `json:"bridge"`
	Subnet     string   `json:"subnet"`
	OwnerPID   int      `json:"owner_pid"`
	Masquerade bool     `json:"masquerade"`
	Taps       []string `json:"taps"`
}

// netStore persists netRecords and the ip_forward marker under dir
// (~/.fleetbox/networks). All host-resource bookkeeping the backend owns lives
// here; nothing about bridges or taps leaks into the platform-neutral per-VM
// config.json.
type netStore struct {
	dir string
}

// newNetStore returns a netStore rooted at dir. An empty dir disables
// persistence (the WAL helpers become no-ops) — used by the bare NestedVirt
// probe, which never creates a network.
func newNetStore(dir string) *netStore {
	return &netStore{dir: dir}
}

// save atomically writes rec to <dir>/<bridge>.json.
func (s *netStore) save(rec *netRecord) error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create network state dir: %w", err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	path := s.recordPath(rec.Bridge)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename record: %w", err)
	}
	return nil
}

// delete removes a bridge's record. A missing record is not an error (teardown
// is idempotent).
func (s *netStore) delete(bridge string) error {
	if s.dir == "" {
		return nil
	}
	if err := os.Remove(s.recordPath(bridge)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove record %s: %w", bridge, err)
	}
	return nil
}

// list returns every persisted record. A record that fails to read or parse is
// skipped rather than failing the whole sweep (best-effort recovery).
func (s *netStore) list() ([]*netRecord, error) {
	if s.dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read network state dir: %w", err)
	}

	var recs []*netRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var rec netRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		recs = append(recs, &rec)
	}
	return recs, nil
}

func (s *netStore) recordPath(bridge string) string {
	return filepath.Join(s.dir, bridge+".json")
}

func (s *netStore) markerPath() string {
	return filepath.Join(s.dir, ipForwardMarker)
}

// saveIPForwardOrig records the host's original ip_forward value the first time
// fleetbox enables forwarding. O_EXCL makes it first-writer-wins, so concurrent
// holders — and a fresh run after a crash — agree on the true pre-fleetbox
// value rather than each recording whatever it happens to read.
func (s *netStore) saveIPForwardOrig(val string) error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create network state dir: %w", err)
	}
	f, err := os.OpenFile(s.markerPath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil // already recorded by an earlier enable — keep that value
		}
		return fmt.Errorf("create ip_forward marker: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(val); err != nil {
		return fmt.Errorf("write ip_forward marker: %w", err)
	}
	return nil
}

// readIPForwardOrig returns the recorded original ip_forward value, or ok=false
// if fleetbox never flipped it (no marker — e.g. forwarding was already on).
func (s *netStore) readIPForwardOrig() (string, bool) {
	if s.dir == "" {
		return "", false
	}
	data, err := os.ReadFile(s.markerPath())
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

func (s *netStore) clearIPForwardOrig() {
	if s.dir == "" {
		return
	}
	_ = os.Remove(s.markerPath())
}

// readIPForward returns the current host ip_forward value ("0"/"1"), or ok=false
// if it cannot be read.
func readIPForward() (string, bool) {
	data, err := os.ReadFile(ipForwardPath)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// writeIPForward sets the host ip_forward value.
func writeIPForward(val string) error {
	if err := os.WriteFile(ipForwardPath, []byte(val+"\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", ipForwardPath, err)
	}
	return nil
}

// pidAlive reports whether pid names a live process (a signal-0 probe), the same
// test runner.IsRunning uses, so "is the owning holder still up?" is answered
// consistently across the codebase.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// cloneTaps returns a copy of taps for embedding in a record without aliasing
// the network's live slice.
func cloneTaps(taps []string) []string {
	return slices.Clone(taps)
}

// killProcsUsingTap SIGKILLs any cloud-hypervisor process whose command line
// references tap — the orphaned VM children of a holder that died before it could
// stop them. Both the binary name and the fbt-<rand> tap must match, so it cannot
// hit an unrelated process. Without this, reconcile would remove a crashed
// cluster's network yet leave its VMs burning CPU with a vanished NIC (ADR-0013).
func killProcsUsingTap(tap string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	needle := "tap=" + tap
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a /proc/<pid> entry
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		cmd := strings.ReplaceAll(string(data), "\x00", " ") // cmdline is NUL-separated
		if strings.Contains(cmd, needle) && strings.Contains(cmd, "cloud-hypervisor") {
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Signal(syscall.SIGKILL)
			}
		}
	}
}
