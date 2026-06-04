//go:build darwin && arm64

package main

import (
	"path/filepath"
	"testing"
)

func TestParseMountValid(t *testing.T) {
	host, guest, err := parseMount("/abs/src:/work")
	if err != nil {
		t.Fatalf("parseMount: %v", err)
	}
	if host != "/abs/src" {
		t.Errorf("host = %q, want /abs/src", host)
	}
	if guest != "/work" {
		t.Errorf("guest = %q, want /work", guest)
	}
}

func TestParseMountAbsolutizesRelativeHost(t *testing.T) {
	host, _, err := parseMount("./src:/work")
	if err != nil {
		t.Fatalf("parseMount: %v", err)
	}
	if !filepath.IsAbs(host) {
		t.Errorf("host %q not absolutized", host)
	}
}

func TestParseMountLastColon(t *testing.T) {
	// Only the LAST colon separates host from guest, so a value with more than
	// one colon keeps everything before the last as the host path.
	host, guest, err := parseMount("/a:b:/work")
	if err != nil {
		t.Fatalf("parseMount: %v", err)
	}
	if host != "/a:b" || guest != "/work" {
		t.Errorf("got host=%q guest=%q, want host=/a:b guest=/work", host, guest)
	}
}

func TestParseMountErrors(t *testing.T) {
	for _, v := range []string{"noColon", ":/work", "/host:", ""} {
		if _, _, err := parseMount(v); err == nil {
			t.Errorf("parseMount(%q) = nil error, want error", v)
		}
	}
}
