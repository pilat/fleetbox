package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/pilat/fleetbox/internal/control"
	"github.com/pilat/fleetbox/internal/store"
)

// newRmCmd builds the `rm` command: destroy VMs completely, deleting their
// disks. Names may be VM names or cluster prefixes; --all removes everything.
func newRmCmd() *cobra.Command {
	var (
		all   bool
		force bool
	)
	cmd := &cobra.Command{
		Use:     "rm <name>... | --all",
		Aliases: []string{"remove", "destroy", "delete"},
		Short:   "Destroy VM(s) completely",
		Long: `Destroy one or more VMs (or whole clusters by prefix), deleting their
disks. This is the only destructive command, so it confirms before removing
anything. Use --all to remove every VM and -f/--force to skip the prompt.`,
		ValidArgsFunction: func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			return completeVMNames(false)
		},
		RunE: func(_ *cobra.Command, args []string) error {
			return runRm(args, all, force)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "remove all VMs")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "force removal without confirmation")
	return cmd
}

func runRm(positional []string, all, force bool) error {
	// --all means "every VM"; combining it with explicit names is contradictory and
	// silently widening scope to all is a footgun, so reject it.
	if all && len(positional) > 0 {
		return errors.New("cannot combine --all with explicit VM names")
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	var targets, unknown []string
	if all {
		targets, err = st.List()
		if err != nil {
			return fmt.Errorf("list vms: %w", err)
		}
	} else {
		if len(positional) == 0 {
			return errors.New("no VMs specified")
		}
		targets, unknown, err = resolveTargets(st, positional)
		if err != nil {
			return err
		}
	}

	if len(targets) == 0 && len(unknown) == 0 {
		return errors.New("no VMs specified")
	}

	// rm is the only destructive command, so confirm any non-empty removal —
	// even a single VM, whose disk is deleted — unless --force/-f. A purely
	// unknown request (no resolved targets) skips the prompt and just reports.
	if !force && len(targets) > 0 {
		fmt.Printf("Will delete %d VMs: %s\n", len(targets), strings.Join(targets, ", "))
		fmt.Print("Continue? [y/N] ")
		var response string
		_, _ = fmt.Scanln(&response)
		if response != "y" && response != "Y" {
			return errors.New("aborted")
		}
	}

	// Best-effort: attempt every target, report per-target, never abort on the
	// first failure. A failed stop/delete or an unknown pattern makes the command
	// exit non-zero.
	failed := false
	for _, name := range targets {
		fmt.Printf("Removing %s...\n", name)
		// Stop holder if running.
		if control.IsRunning(st, name) {
			if err := control.Stop(st, name); err != nil {
				fmt.Fprintf(os.Stderr, "%s: stop failed: %v\n", name, err)
				failed = true
				continue
			}
		}
		// Delete the member directory (its pid/sock live inside it now, so
		// st.Delete's RemoveAll sweeps them; the empty cluster dir is dropped too).
		if err := st.Delete(name); err != nil {
			fmt.Fprintf(os.Stderr, "%s: delete failed: %v\n", name, err)
			failed = true
			continue
		}
	}
	for _, pattern := range unknown {
		fmt.Fprintf(os.Stderr, "VM %q not found\n", pattern)
		failed = true
	}

	if failed {
		return &cliExit{code: 1, silent: true}
	}
	return nil
}
