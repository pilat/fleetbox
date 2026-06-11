package main

import (
	"path/filepath"
	"testing"
)

func TestParseFixtureValid(t *testing.T) {
	host, guest, err := parseFixture("/abs/src:/work")
	if err != nil {
		t.Fatalf("parseFixture: %v", err)
	}
	if host != "/abs/src" {
		t.Errorf("host = %q, want /abs/src", host)
	}
	if guest != "/work" {
		t.Errorf("guest = %q, want /work", guest)
	}
}

func TestParseFixtureAbsolutizesRelativeHost(t *testing.T) {
	host, _, err := parseFixture("./src:/work")
	if err != nil {
		t.Fatalf("parseFixture: %v", err)
	}
	if !filepath.IsAbs(host) {
		t.Errorf("host %q not absolutized", host)
	}
}

func TestParseFixtureLastColon(t *testing.T) {
	// Only the LAST colon separates host from guest, so a value with more than
	// one colon keeps everything before the last as the host path.
	host, guest, err := parseFixture("/a:b:/work")
	if err != nil {
		t.Fatalf("parseFixture: %v", err)
	}
	if host != "/a:b" || guest != "/work" {
		t.Errorf("got host=%q guest=%q, want host=/a:b guest=/work", host, guest)
	}
}

func TestParseFixtureErrors(t *testing.T) {
	for _, v := range []string{"noColon", ":/work", "/host:", ""} {
		if _, _, err := parseFixture(v); err == nil {
			t.Errorf("parseFixture(%q) = nil error, want error", v)
		}
	}
}

func TestParseFixtureRelativeGuest(t *testing.T) {
	// The guest path must be absolute; reject it early (before any image
	// download) rather than letting the orchestrator catch it at boot.
	if _, _, err := parseFixture("/abs/src:work"); err == nil {
		t.Error("parseFixture with relative guest = nil error, want error")
	}
}
