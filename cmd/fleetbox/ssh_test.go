package main

import (
	"slices"
	"strings"
	"testing"
)

func TestParseSSHArgs(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantName   string
		wantRemote []string
		wantErr    string // substring; empty means no error
	}{
		{name: "interactive shell", args: []string{"web"}, wantName: "web"},
		{
			name:       "command after dash",
			args:       []string{"web", "--", "uname", "-a"},
			wantName:   "web",
			wantRemote: []string{"uname", "-a"},
		},
		{name: "dash with empty command", args: []string{"web", "--"}, wantName: "web", wantRemote: []string{}},
		{name: "trailing args without dash", args: []string{"web", "uname", "-a"}, wantErr: "fleetbox ssh"},
		{name: "dash first missing name", args: []string{"--", "x"}, wantErr: "missing VM name"},
		{name: "no args", args: nil, wantErr: "usage"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, remote, err := parseSSHArgs(tc.args)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseSSHArgs(%v) = nil error, want error containing %q", tc.args, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSSHArgs(%v): %v", tc.args, err)
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if !slices.Equal(remote, tc.wantRemote) {
				t.Errorf("remote = %v, want %v", remote, tc.wantRemote)
			}
		})
	}
}
