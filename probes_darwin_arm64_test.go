//go:build darwin && arm64

package fleetbox

import "testing"

func TestParseAppleGeneration(t *testing.T) {
	cases := []struct {
		brand string
		want  int
	}{
		{"Apple M1", 1},
		{"Apple M2", 2},
		{"Apple M2 Max", 2},
		{"Apple M3 Pro", 3},
		{"Apple M4 Pro", 4},
		{"Apple M10 Ultra", 10}, // future double-digit chip
		{"Intel(R) Core(TM) i7", 0},
		{"", 0},
		{"Apple Silicon", 0}, // no M<N> token
	}
	for _, tc := range cases {
		if got := parseAppleGeneration(tc.brand); got != tc.want {
			t.Errorf("parseAppleGeneration(%q) = %d, want %d", tc.brand, got, tc.want)
		}
	}
}

func TestNestedCapable(t *testing.T) {
	cases := []struct {
		name  string
		macOS int
		gen   int
		want  bool
	}{
		{"M3 on macOS 26", 26, 3, true},
		{"M4 on macOS 26", 26, 4, true},
		{"M1 on macOS 26", 26, 1, false},
		{"M2 on macOS 15", 15, 2, false},
		{"M3 on macOS 14 (too old)", 14, 3, false},
		{"unknown chip on macOS 26 (optimistic)", 26, 0, true},
		{"unknown chip on macOS 14 (still gated by OS)", 14, 0, false},
	}
	for _, tc := range cases {
		if got := nestedCapable(tc.macOS, tc.gen); got != tc.want {
			t.Errorf("%s: nestedCapable(%d, %d) = %v, want %v", tc.name, tc.macOS, tc.gen, got, tc.want)
		}
	}
}
