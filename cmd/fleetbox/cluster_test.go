package main

import (
	"slices"
	"strings"
	"testing"
)

func TestClusterMembers(t *testing.T) {
	cases := []struct {
		name        string
		names       []string
		count       int
		want        []string
		wantErrPart string
	}{
		{name: "prefix with n=3", names: []string{"x"}, count: 3, want: []string{"x-1", "x-2", "x-3"}},
		{name: "prefix with n=2", names: []string{"x"}, count: 2, want: []string{"x-1", "x-2"}},
		{name: "explicit names as-is", names: []string{"a", "b", "c"}, count: 1, want: []string{"a", "b", "c"}},
		{name: "no names defaults", names: nil, count: 1, want: []string{"default"}},
		{name: "count below 1", names: nil, count: 0, wantErrPart: "at least 1"},
		{name: "prefix with count 0", names: []string{"x"}, count: 0, wantErrPart: "at least 1"},
		{name: "multiple names with n", names: []string{"a", "b"}, count: 3, wantErrPart: "exactly one name"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := clusterMembers(tc.names, tc.count)
			if tc.wantErrPart != "" {
				if err == nil {
					t.Fatalf("clusterMembers(%v, %d) = nil error, want error", tc.names, tc.count)
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Errorf("error %q does not contain %q", err, tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("clusterMembers(%v, %d): %v", tc.names, tc.count, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("clusterMembers(%v, %d) = %v, want %v", tc.names, tc.count, got, tc.want)
			}
		})
	}
}
