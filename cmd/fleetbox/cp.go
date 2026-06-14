package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/pilat/fleetbox/internal/control"
	"github.com/pilat/fleetbox/internal/sshkey"
	"github.com/pilat/fleetbox/internal/store"
)

// newCpCmd builds the `cp` command: copy files to or from a VM with scp-style
// name:/path syntax. Exactly one side names a VM; the other is a local path.
func newCpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cp <src> <dst>",
		Short: "Copy files to/from a VM (scp syntax: name:/path)",
		Long: `Copy files to or from a VM. Exactly one side uses name:/path; the
other is a local path:

  fleetbox cp ./app web:/srv/app   upload ./app to web
  fleetbox cp web:/var/log/x .     download /var/log/x from web

VM-to-VM copies (both sides name:/path) are not supported.`,
		Args: cobra.ExactArgs(2),
		// Offer running VM names for either side; NoSpace so the user can append
		// the :/path to a completed name.
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) >= 2 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			names, _ := completeVMNames(true)
			return names, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
		},
		RunE: func(_ *cobra.Command, args []string) error {
			return runCp(args)
		},
	}
	return cmd
}

// cpRemoteSide determines which side of a cp names a VM. Exactly one side may
// carry the name:/path syntax: both sides remote is a VM-to-VM copy (rejected, so
// it never silently rewrites both paths to one VM's IP); neither remote means
// nothing addresses a VM.
func cpRemoteSide(src, dst string) (name string, err error) {
	srcRemote := strings.Contains(src, ":")
	dstRemote := strings.Contains(dst, ":")
	switch {
	case srcRemote && dstRemote:
		return "", errors.New("VM-to-VM copy is not supported (one side must be a local path)")
	case srcRemote:
		return vmNameBeforeColon(src)
	case dstRemote:
		return vmNameBeforeColon(dst)
	default:
		return "", errors.New("either src or dst must use name:/path syntax")
	}
}

// vmNameBeforeColon extracts the VM name from a name:/path side, rejecting an
// empty name (e.g. ":/x") at parse time rather than letting it fall through to a
// confusing `VM "" does not exist`.
func vmNameBeforeColon(side string) (string, error) {
	name := strings.SplitN(side, ":", 2)[0]
	if name == "" {
		return "", errors.New("invalid remote syntax: missing VM name before ':'")
	}
	return name, nil
}

func runCp(args []string) error {
	src, dst := args[0], args[1]

	name, err := cpRemoteSide(src, dst)
	if err != nil {
		return err
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	if !st.Exists(name) {
		return fmt.Errorf("VM %q does not exist", name)
	}

	status, err := control.GetStatus(st, name)
	if err != nil {
		return fmt.Errorf("get status: %w", err)
	}
	if !status.Running {
		return fmt.Errorf("VM %q is not running", name)
	}

	// Status.IP is a string; the copy primitive dials the parsed IP directly, the
	// same per-installation key the library uses, the same "fleetbox" user the
	// guest's cloud-init created.
	ip := net.ParseIP(status.IP)
	if ip == nil {
		return fmt.Errorf("VM %q has no usable IP (%q)", name, status.IP)
	}
	client, err := sshkey.NewManager(st.SSHKeyPath()).DialIP(ip, "fleetbox", 30*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", name, err)
	}
	defer func() { _ = client.Close() }()

	if strings.Contains(src, ":") {
		// Download: guest -> local. The CLI keeps scp's convenience of copying into
		// a directory destination; the library method itself is exact-path.
		guestPath := remotePath(src)
		// The guest root has no basename to copy into a directory destination
		// (path.Base("/") is "/"); require an explicit local target instead of
		// resolving to a surprising one.
		srcBase := path.Base(path.Clean(guestPath))
		if srcBase == "/" || srcBase == "." {
			return errors.New("copying the guest root needs an explicit local destination path")
		}
		localDst := resolveLocalDest(dst, srcBase, isExistingDir(dst))
		if err := client.CopyFrom(guestPath, localDst); err != nil {
			return fmt.Errorf("copy from %s: %w", name, err)
		}
		return nil
	}

	// Upload: local -> guest. The guest destination is exact (the CLI cannot stat
	// the guest to decide "copy into a directory"); guestPath must be absolute.
	if err := client.CopyTo(src, remotePath(dst)); err != nil {
		return fmt.Errorf("copy to %s: %w", name, err)
	}
	return nil
}

// remotePath returns the path component of a name:/path side.
func remotePath(side string) string {
	return strings.SplitN(side, ":", 2)[1]
}

// resolveLocalDest applies scp's "copy into a directory" convenience for a local
// download destination: when dst names a directory — ".", "..", a trailing
// separator, or an existing directory — the item lands inside it as
// <dst>/<srcBase>; otherwise dst is the exact destination path. It is pure (the
// "is dst a directory" decision is passed in) so the resolution is unit-testable
// without a VM.
func resolveLocalDest(dst, srcBase string, dstIsDir bool) string {
	if dst == "." || dst == ".." || dstIsDir ||
		strings.HasSuffix(dst, string(filepath.Separator)) {
		return filepath.Join(dst, srcBase)
	}
	return dst
}

// isExistingDir reports whether p exists and is a directory.
func isExistingDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
