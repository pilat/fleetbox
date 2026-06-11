package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/pilat/fleetbox"
	"github.com/pilat/fleetbox/internal/control"
	"github.com/pilat/fleetbox/internal/store"
)

// newDownCmd builds the `down` command: gracefully shut VMs down, preserving
// their disks. Names may be VM names or cluster prefixes; --all stops everything.
func newDownCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "down <name>... | --all",
		Aliases: []string{"stop", "halt"},
		Short:   "Gracefully shut down VM(s), disk preserved",
		Long: `Gracefully shut down one or more VMs (or whole clusters by prefix),
preserving their disks. Bring them back with up. Use --all to stop every VM.`,
		ValidArgsFunction: func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			return completeVMNames(true)
		},
		RunE: func(_ *cobra.Command, args []string) error {
			return runDown(args, all)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "stop all VMs")
	return cmd
}

func runDown(positional []string, all bool) error {
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

	// Sweep up anything a crashed holder left behind (orphaned bridges, taps,
	// firewall rules) before stopping — cleanup is automatic on down as well as
	// up, never the user's job (ADR-0013). Best-effort: a failure must not block
	// the shutdown the user asked for. No-op on macOS.
	if err := fleetbox.Prune(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cleanup of orphaned resources failed: %v\n", err)
	}

	// Best-effort: attempt every target, report per-target, never abort on the
	// first wedged holder. A stopped member is not a failure; a failed Stop or an
	// unknown pattern is, and makes the command exit non-zero.
	failed := false
	for _, name := range targets {
		if !control.IsRunning(st, name) {
			fmt.Printf("%s is not running\n", name)
			continue
		}
		fmt.Printf("Stopping %s...\n", name)
		if err := control.Stop(st, name); err != nil {
			fmt.Fprintf(os.Stderr, "%s: stop failed: %v\n", name, err)
			failed = true
			continue
		}
		fmt.Printf("  stopped\n")
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
