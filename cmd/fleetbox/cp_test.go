package main

import (
	"path/filepath"
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

func TestResolveLocalDest(t *testing.T) {
	cases := []struct {
		name     string
		dst      string
		srcBase  string
		dstIsDir bool
		want     string
	}{
		{name: "dot copies into cwd", dst: ".", srcBase: "x", want: filepath.Join(".", "x")},
		{name: "dotdot copies into parent", dst: "..", srcBase: "x", want: filepath.Join("..", "x")},
		{name: "trailing slash copies inside", dst: "out/", srcBase: "x", want: filepath.Join("out", "x")},
		{
			name:     "existing dir copies inside",
			dst:      "existing",
			srcBase:  "x",
			dstIsDir: true,
			want:     filepath.Join("existing", "x"),
		},
		{name: "explicit name passes through", dst: "renamed.txt", srcBase: "x", want: "renamed.txt"},
		{name: "explicit path passes through", dst: "sub/renamed.txt", srcBase: "x", want: "sub/renamed.txt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveLocalDest(tc.dst, tc.srcBase, tc.dstIsDir); got != tc.want {
				t.Errorf("resolveLocalDest(%q, %q, %v) = %q, want %q",
					tc.dst, tc.srcBase, tc.dstIsDir, got, tc.want)
			}
		})
	}
}
