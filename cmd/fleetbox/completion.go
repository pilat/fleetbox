package main

import (
	"github.com/spf13/cobra"

	"github.com/pilat/fleetbox/internal/control"
	"github.com/pilat/fleetbox/internal/store"
)

// completeVMNames provides dynamic shell completion for a VM-name argument. With
// runningOnly it offers only members whose holder reports running (ssh/cp/down),
// otherwise every member (rm). It opens its own store — shell completion runs in
// a fresh process with no shared command state — and never offers file
// completion. Any error yields no suggestions rather than a completion error.
//
// The `completion` subcommand itself is provided by cobra automatically; this
// helper only feeds the per-command ValidArgsFunction.
func completeVMNames(runningOnly bool) ([]string, cobra.ShellCompDirective) {
	st, err := store.New()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names, err := st.List()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	if !runningOnly {
		return names, cobra.ShellCompDirectiveNoFileComp
	}

	running := make([]string, 0, len(names))
	for _, name := range names {
		if control.IsRunning(st, name) {
			running = append(running, name)
		}
	}
	return running, cobra.ShellCompDirectiveNoFileComp
}
