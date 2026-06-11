package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/pilat/fleetbox/internal/control"
	"github.com/pilat/fleetbox/internal/store"
)

// newSSHConfigCmd builds the `ssh-config` command: print an SSH client config
// stanza for every running VM, suitable for appending to ~/.ssh/config.
func newSSHConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh-config",
		Short: "Print SSH config for all running VMs",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runSSHConfig()
		},
	}
	return cmd
}

func runSSHConfig() error {
	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	names, err := st.List()
	if err != nil {
		return fmt.Errorf("list vms: %w", err)
	}

	for _, name := range names {
		status, err := control.GetStatus(st, name)
		if err != nil || !status.Running {
			continue // Only show running VMs
		}

		fmt.Printf("Host %s\n", name)
		fmt.Printf("  HostName %s\n", status.IP)
		fmt.Printf("  User fleetbox\n")
		fmt.Printf("  IdentityFile %s\n", st.SSHKeyPath())
		fmt.Printf("  StrictHostKeyChecking no\n")
		fmt.Printf("  UserKnownHostsFile %s\n", devNull)
		fmt.Println()
	}

	return nil
}
