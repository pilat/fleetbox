package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/pilat/fleetbox/internal/control"
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

	// Rewrite the remote side's path with the actual IP.
	if strings.Contains(src, ":") {
		parts := strings.SplitN(src, ":", 2)
		src = fmt.Sprintf("fleetbox@%s:%s", status.IP, parts[1])
	}
	if strings.Contains(dst, ":") {
		parts := strings.SplitN(dst, ":", 2)
		dst = fmt.Sprintf("fleetbox@%s:%s", status.IP, parts[1])
	}

	scpArgs := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=" + devNull,
		"-i", st.SSHKeyPath(),
		src, dst,
	}

	scpCmd := exec.Command("scp", scpArgs...)
	scpCmd.Stdin = os.Stdin
	scpCmd.Stdout = os.Stdout
	scpCmd.Stderr = os.Stderr

	if err := scpCmd.Run(); err != nil {
		// Propagate scp's own exit code without an extra "error:" line (scp wrote
		// its own stderr) — the shared cliExit scheme (Task 1).
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &cliExit{code: exitErr.ExitCode(), silent: true}
		}
		return fmt.Errorf("scp: %w", err)
	}

	return nil
}
