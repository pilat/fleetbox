//go:build darwin && arm64

package vz

import "testing"

func TestParseMacOSMajor(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"26.4.1", 26},
		{"26.0", 26},
		{"26", 26},
		{"15.5", 15},
		{"13.7.6", 13},
		{"", 0},
		{"notaversion", 0},
		{".4", 0},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseMacOSMajor(tt.in); got != tt.want {
				t.Errorf("parseMacOSMajor(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestSupportsClusteringGate pins the version gate that selects SharedMode +
// clustering (26+) versus the NAT, single-VM fallback (<26) — no <26 hardware
// needed, the major version is injected directly.
func TestSupportsClusteringGate(t *testing.T) {
	tests := []struct {
		major int
		want  bool
	}{
		{0, false},
		{15, false},
		{25, false},
		{26, true},
		{27, true},
	}

	for _, tt := range tests {
		b := &Backend{macOSMajor: tt.major}
		if got := b.SupportsClustering(); got != tt.want {
			t.Errorf("Backend{macOSMajor: %d}.SupportsClustering() = %v, want %v", tt.major, got, tt.want)
		}
	}
}
