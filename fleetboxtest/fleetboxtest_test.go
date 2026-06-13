package fleetboxtest

import (
	"testing"
	"time"
)

func TestSafeName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"TestSimple", "testsimple"},
		{"TestFoo/Bar", "testfoo-bar"},
		{"Test_With_Underscores", "test-with-underscores"},
		{"Test123", "test123"},
		{"Test!@#Special", "testspecial"},
		{"", "test"},
		{"---", "test"},
		{"-test-", "test"},
		{"UPPERCASE", "uppercase"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := safeName(tt.input)
			if got != tt.want {
				t.Errorf("safeName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSafeNameTruncation(t *testing.T) {
	longName := "TestVeryLongTestNameThatExceedsTheFiftyCharacterLimitForHostnames"
	got := safeName(longName)
	if len(got) > 50 {
		t.Errorf("safeName should truncate to 50 chars, got %d: %q", len(got), got)
	}
}

func TestBootTimeout(t *testing.T) {
	// Unset: the default is n*5min (n treated as at least 1).
	t.Run("unset defaults to n*5min", func(t *testing.T) {
		t.Setenv("FLEETBOX_IP_WAIT_TIMEOUT", "")
		if got := BootTimeout(1); got != 5*time.Minute {
			t.Errorf("BootTimeout(1) = %v, want 5m", got)
		}
		if got := BootTimeout(3); got != 15*time.Minute {
			t.Errorf("BootTimeout(3) = %v, want 15m", got)
		}
		if got := BootTimeout(0); got != 5*time.Minute {
			t.Errorf("BootTimeout(0) = %v, want 5m (n clamped to 1)", got)
		}
	})

	// Set and parseable: that duration wins regardless of n.
	t.Run("valid duration overrides default", func(t *testing.T) {
		t.Setenv("FLEETBOX_IP_WAIT_TIMEOUT", "20m")
		if got := BootTimeout(1); got != 20*time.Minute {
			t.Errorf("BootTimeout(1) = %v, want 20m", got)
		}
		if got := BootTimeout(4); got != 20*time.Minute {
			t.Errorf("BootTimeout(4) = %v, want 20m (env wins)", got)
		}
	})

	// Garbage: fall back to the default silently.
	t.Run("unparseable falls back to default", func(t *testing.T) {
		t.Setenv("FLEETBOX_IP_WAIT_TIMEOUT", "not-a-duration")
		if got := BootTimeout(2); got != 10*time.Minute {
			t.Errorf("BootTimeout(2) = %v, want 10m (garbage ignored)", got)
		}
	})

	// Zero/negative: parses cleanly but is useless, so fall back to the default.
	t.Run("non-positive duration falls back to default", func(t *testing.T) {
		t.Setenv("FLEETBOX_IP_WAIT_TIMEOUT", "0s")
		if got := BootTimeout(1); got != 5*time.Minute {
			t.Errorf("BootTimeout(1) = %v, want 5m (0s ignored)", got)
		}
		t.Setenv("FLEETBOX_IP_WAIT_TIMEOUT", "-3m")
		if got := BootTimeout(2); got != 10*time.Minute {
			t.Errorf("BootTimeout(2) = %v, want 10m (negative ignored)", got)
		}
	})
}
