package cloudhypervisor

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
)

func TestNftTableName(t *testing.T) {
	cases := map[string]string{
		"fbx-0011aabb": "fbx_0011aabb",
		"fbx-deadbeef": "fbx_deadbeef",
		"nodash":       "nodash",
	}
	for in, want := range cases {
		if got := nftTableName(in); got != want {
			t.Errorf("nftTableName(%q) = %q, want %q", in, got, want)
		}
	}
	// nft identifiers must not contain hyphens.
	if strings.Contains(nftTableName("fbx-0011aabb"), "-") {
		t.Error("nftTableName left a hyphen in the table name")
	}
}

func TestClassifyNFTErr(t *testing.T) {
	if classifyNFTErr(nil) != nil {
		t.Error("classifyNFTErr(nil) should be nil")
	}

	// EPERM is the non-root signal — must read as "needs root" and still match.
	permErr := classifyNFTErr(fmt.Errorf("list: %w", syscall.EPERM))
	if !strings.Contains(permErr.Error(), "needs root") {
		t.Errorf("EPERM classified as %q, want it to mention needs root", permErr)
	}
	if !errors.Is(permErr, syscall.EPERM) {
		t.Error("classified EPERM error no longer matches syscall.EPERM")
	}

	// A kernel without nf_tables fails with EOPNOTSUPP or ENOENT — must NOT read
	// as needs-root, and must point at the missing kernel feature.
	for _, e := range []error{syscall.EOPNOTSUPP, syscall.ENOENT} {
		got := classifyNFTErr(fmt.Errorf("list: %w", e))
		if strings.Contains(got.Error(), "needs root") {
			t.Errorf("%v misclassified as needs-root: %q", e, got)
		}
		if !strings.Contains(got.Error(), "nf_tables") {
			t.Errorf("%v classified as %q, want it to mention nf_tables", e, got)
		}
		if !errors.Is(got, e) {
			t.Errorf("classified %v error no longer matches the errno", e)
		}
	}
}

func TestUplinkName(t *testing.T) {
	resolve := func(names map[int]string) func(int) (string, error) {
		return func(idx int) (string, error) {
			if n, ok := names[idx]; ok {
				return n, nil
			}
			return "", fmt.Errorf("no interface %d", idx)
		}
	}

	// First valid index wins.
	name, err := uplinkName([]int{7, 9}, resolve(map[int]string{7: "eth0", 9: "eth1"}))
	if err != nil || name != "eth0" {
		t.Fatalf("uplinkName = (%q, %v), want (eth0, nil)", name, err)
	}

	// Non-positive indices are skipped; the first valid one is used.
	name, err = uplinkName([]int{0, -1, 3}, resolve(map[int]string{3: "wan0"}))
	if err != nil || name != "wan0" {
		t.Fatalf("uplinkName = (%q, %v), want (wan0, nil)", name, err)
	}

	// No routes (offline host) → no uplink, no error.
	name, err = uplinkName(nil, resolve(nil))
	if err != nil || name != "" {
		t.Fatalf("uplinkName(nil) = (%q, %v), want (\"\", nil)", name, err)
	}

	// A resolution failure surfaces as an error rather than an empty name.
	if _, err := uplinkName([]int{5}, resolve(nil)); err == nil {
		t.Fatal("uplinkName should error when the index cannot be resolved")
	}
}
