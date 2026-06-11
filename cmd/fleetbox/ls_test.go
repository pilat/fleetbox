package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pilat/fleetbox/internal/store"
)

func TestLSEntriesJSONShape(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	created := time.Date(2026, 6, 11, 1, 0, 0, 0, time.UTC)
	if err := st.Create(&store.VM{
		Name:      "web-1",
		CPUs:      2,
		MemoryMB:  4096,
		DiskMB:    20480,
		Image:     "debian-12",
		CreatedAt: created,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	entries := lsEntries(st, []string{"web-1"})
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}

	// Marshal then unmarshal into the named struct to pin the JSON field names,
	// then assert the concrete values for a known, stopped VM.
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got []lsEntry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d unmarshaled entries, want 1", len(got))
	}

	e := got[0]
	if e.Name != "web-1" {
		t.Errorf("name = %q, want web-1", e.Name)
	}
	if e.State != "stopped" {
		t.Errorf("state = %q, want stopped (not running)", e.State)
	}
	if e.IP != "" {
		t.Errorf("ip = %q, want empty for stopped VM", e.IP)
	}
	if e.CPUs != 2 {
		t.Errorf("cpus = %d, want 2", e.CPUs)
	}
	if e.MemoryMB != 4096 {
		t.Errorf("memory_mb = %d, want 4096 (MB verbatim, not GB)", e.MemoryMB)
	}
	if e.DiskMB != 20480 {
		t.Errorf("disk_mb = %d, want 20480", e.DiskMB)
	}
	if e.Image != "debian-12" {
		t.Errorf("image = %q, want debian-12", e.Image)
	}
	if e.CreatedAt != "2026-06-11T01:00:00Z" {
		t.Errorf("created_at = %q, want 2026-06-11T01:00:00Z (RFC3339)", e.CreatedAt)
	}
}

// TestLSEntriesRawJSONKeys asserts the on-wire key names directly (the machine
// contract), independent of the Go struct tags.
func TestLSEntriesRawJSONKeys(t *testing.T) {
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("store.NewAt: %v", err)
	}
	vm := &store.VM{
		Name:      "db",
		CPUs:      1,
		MemoryMB:  1024,
		DiskMB:    10240,
		Image:     "debian-12",
		CreatedAt: time.Unix(0, 0).UTC(),
	}
	if err := st.Create(vm); err != nil {
		t.Fatalf("create: %v", err)
	}

	data, err := json.Marshal(lsEntries(st, []string{"db"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("got %d entries, want 1", len(raw))
	}
	for _, key := range []string{"name", "ip", "state", "cpus", "memory_mb", "disk_mb", "image", "created_at"} {
		if _, ok := raw[0][key]; !ok {
			t.Errorf("missing JSON key %q", key)
		}
	}
	if _, ok := raw[0]["age"]; ok {
		t.Error("JSON unexpectedly contains an age field")
	}
}
