package fleetboxtest

import (
	"testing"
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
