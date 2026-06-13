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

// globalForwardingPath is the per-interface sysctl alias for the host-wide IPv4
// forwarding switch (conf.all.forwarding == net.ipv4.ip_forward). We only READ it,
// to decide whether the host already forwards; we never write it (Decision 4).
const globalForwardingPath = "/proc/sys/net/ipv4/conf/all/forwarding"

// forwardMarkerPrefix/Suffix bracket the per-uplink marker filename
// (fwd-<iface>.orig) that records an uplink's original forwarding value the first
// time fleetbox flips it on — the per-interface generalization of the old global
// ipforward.orig marker (ADR-0013, ADR-0025).
const (
	forwardMarkerPrefix = "fwd-"
	forwardMarkerSuffix = ".orig"
)

// netRecord is the persisted, write-ahead description of one bridge network and
// the host resources it owns (bridge, taps, and the uplink whose forwarding flag
// it may have flipped). It is written before the matching netlink/nftables calls
// run and deleted only after teardown is verified, so the record is always a
// superset of what exists on the host. That invariant lets a later `up` or `prune`
// clean up a network whose holder crashed before Close — orphans become
// recoverable instead of leaking (ADR-0013, ADR-0025). The egress firewall is no
// longer recorded: it is one nft table deleted whole by name (nftTableName), so a
// boolean would add nothing.
type netRecord struct {
	Bridge   string `json:"bridge"`
	Subnet   string `json:"subnet"`
	OwnerPID int    `json:"owner_pid"`
	// Uplink is the egress interface whose forwarding flag this network may have
	// flipped ("" when the host was offline or already forwarding globally).
	Uplink string `json:"uplink,omitempty"`
	// UplinkFwdOrig is the uplink's forwarding value as this holder observed it
	// before flipping ("0" = we flipped it on; "1"/"" = we did not). The
	// cross-process source of truth for restore is the first-writer-wins marker,
	// not this field, which is the write-ahead audit trail (ADR-0025).
	UplinkFwdOrig string   `json:"uplink_fwd_orig,omitempty"`
	Taps          []string `json:"taps"`
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

func (s *netStore) markerPath(uplink string) string {
	return filepath.Join(s.dir, forwardMarkerPrefix+uplink+forwardMarkerSuffix)
}

// saveForwardingOrig records an uplink's original forwarding value the first time
// fleetbox flips it on. O_EXCL makes it first-writer-wins, so concurrent holders —
// and a fresh run after a crash — agree on the true pre-fleetbox value rather than
// each recording whatever it happens to read (the cross-holder correctness the
// per-network record alone cannot provide, since a later holder reads the
// already-flipped value).
func (s *netStore) saveForwardingOrig(uplink, val string) error {
	if s.dir == "" || uplink == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create network state dir: %w", err)
	}
	f, err := os.OpenFile(s.markerPath(uplink), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil // already recorded by an earlier enable — keep that value
		}
		return fmt.Errorf("create forwarding marker: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(val); err != nil {
		return fmt.Errorf("write forwarding marker: %w", err)
	}
	return nil
}

// listForwardingOrigs returns every recorded uplink→original-forwarding-value
// marker. Empty when fleetbox never flipped an uplink (no markers).
func (s *netStore) listForwardingOrigs() map[string]string {
	if s.dir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	origs := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, forwardMarkerPrefix) || !strings.HasSuffix(name, forwardMarkerSuffix) {
			continue
		}
		uplink := strings.TrimSuffix(strings.TrimPrefix(name, forwardMarkerPrefix), forwardMarkerSuffix)
		data, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			continue
		}
		origs[uplink] = strings.TrimSpace(string(data))
	}
	return origs
}

func (s *netStore) clearForwardingOrig(uplink string) {
	if s.dir == "" || uplink == "" {
		return
	}
	_ = os.Remove(s.markerPath(uplink))
}

// forwardingPath is the per-interface IPv4 forwarding sysctl. Writing it enables
// forwarding for traffic ingressing on that one interface, leaving the global
// switch (and every other interface) untouched (Decision 4).
func forwardingPath(iface string) string {
	return "/proc/sys/net/ipv4/conf/" + iface + "/forwarding"
}

// readForwarding returns an interface's current forwarding value ("0"/"1"), or
// ok=false if it cannot be read.
func readForwarding(iface string) (string, bool) {
	data, err := os.ReadFile(forwardingPath(iface))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// writeForwarding sets an interface's forwarding value.
func writeForwarding(iface, val string) error {
	path := forwardingPath(iface)
	if err := os.WriteFile(path, []byte(val+"\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// globalForwardingOn reports whether the host already forwards globally
// (conf.all.forwarding == 1). When it does, the whole host is already a router and
// fleetbox enables nothing and restores nothing (Decision 4). A read failure is
// treated as "not on" so a fresh host still gets per-interface forwarding.
func globalForwardingOn() bool {
	data, err := os.ReadFile(globalForwardingPath)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "1"
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
