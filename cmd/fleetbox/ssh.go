package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/pilat/fleetbox/internal/control"
	"github.com/pilat/fleetbox/internal/store"
)

// devNull is where ssh points UserKnownHostsFile: a VM is cattle whose host key
// changes every boot, so fleetbox never records it in the user's known_hosts.
// Shared by ssh and ssh-config so the bit bucket is named in one place. (cp no
// longer needs it — it dials via the in-process copy primitive, which ignores host
// keys directly.)
const devNull = "/dev/null"

// newSSHCmd builds the `ssh` command: open an interactive shell on a VM, or run
// a command on it after `--` (kubectl/multipass style).
func newSSHCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ssh <name> [-- cmd...]",
		Aliases: []string{"shell"},
		Short:   "SSH into a VM",
		Long: `Open an interactive shell on a VM, or run a command on it after --:

  fleetbox ssh web              interactive shell
  fleetbox ssh web -- uname -a  run "uname -a" on web and exit

The remote command must follow --; trailing args without it are rejected rather
than silently dropped. The VM's exit code is propagated.`,
		// Complete only the VM name (the first positional); the remote command
		// after it runs in the guest and cannot be completed locally.
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) != 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return completeVMNames(true)
		},
		RunE: func(_ *cobra.Command, args []string) error {
			return runSSH(args)
		},
	}
	// Stop pflag at the first positional so trailing tokens (including dashed
	// flags and a literal --) are captured verbatim for the remote command
	// instead of being parsed as fleetbox flags. runSSH then splits on -- itself
	// (ArgsLenAtDash is unusable with interspersing off).
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// parseSSHArgs splits the ssh positional args into the VM name and an optional
// remote command. The command must be separated by a literal "--"; trailing args
// without it are an error (the silent-drop footgun), not an interactive shell. It
// scans the slice for "--" rather than relying on cobra's ArgsLenAtDash, which
// stays -1 once interspersing is disabled.
func parseSSHArgs(args []string) (name string, remote []string, err error) {
	dash := -1
	for i, a := range args {
		if a == "--" {
			dash = i
			break
		}
	}

	if dash >= 0 {
		switch dash {
		case 0:
			return "", nil, errors.New("missing VM name")
		case 1:
			return args[0], args[dash+1:], nil
		default:
			// Tokens between the name and -- would be silently dropped; the contract
			// is exactly one name, then -- <cmd>.
			return "", nil, errors.New("to run a command, use: fleetbox ssh <name> -- <cmd>")
		}
	}

	switch len(args) {
	case 0:
		return "", nil, errors.New("usage: fleetbox ssh <name> [-- cmd]")
	case 1:
		return args[0], nil, nil
	default:
		return "", nil, errors.New("to run a command, use: fleetbox ssh <name> -- <cmd>")
	}
}

func runSSH(args []string) error {
	name, remote, err := parseSSHArgs(args)
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

	sshArgs := make([]string, 0, 7+len(remote))
	sshArgs = append(sshArgs,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile="+devNull,
		"-i", st.SSHKeyPath(),
		"fleetbox@"+status.IP,
	)
	sshArgs = append(sshArgs, remote...)

	sshCmd := exec.Command("ssh", sshArgs...)
	sshCmd.Stdin = os.Stdin
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr

	if err := sshCmd.Run(); err != nil {
		// Surface the child's real exit code without an extra "error:" line — ssh
		// already wrote its own stderr (the shared cliExit scheme, Task 1).
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &cliExit{code: exitErr.ExitCode(), silent: true}
		}
		return fmt.Errorf("ssh: %w", err)
	}

	return nil
}
