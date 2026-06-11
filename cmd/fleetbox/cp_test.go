package main

import (
	"strings"
	"testing"
)

func TestCpRemoteSide(t *testing.T) {
	cases := []struct {
		name     string
		src, dst string
		wantName string
		wantErr  string // substring; empty means no error
	}{
		{name: "upload (dst remote)", src: "./app", dst: "web:/srv/app", wantName: "web"},
		{name: "download (src remote)", src: "web:/var/log/x", dst: ".", wantName: "web"},
		{name: "vm to vm rejected", src: "a:/x", dst: "b:/y", wantErr: "VM-to-VM"},
		{name: "neither remote", src: "./a", dst: "./b", wantErr: "name:/path"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, err := cpRemoteSide(tc.src, tc.dst)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("cpRemoteSide(%q, %q) = nil error, want error containing %q", tc.src, tc.dst, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("cpRemoteSide(%q, %q): %v", tc.src, tc.dst, err)
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
		})
	}
}
