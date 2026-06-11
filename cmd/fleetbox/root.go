package main

import (
	"github.com/spf13/cobra"
)

// newRootCmd assembles the full command tree in one place. Every subcommand is
// built by its own newXxxCmd constructor and added here; there are no
// package-level command globals and no init() — the root is created, wired, and
// returned by this function alone (per the cobra-written-clean constraints).
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "fleetbox",
		Short: "Linux VMs as test fixtures (macOS Apple Silicon + Linux)",
		Long: `fleetbox runs real Linux VMs as Go test fixtures — a real kernel, real
systemd, real KVM — on macOS (Apple Silicon) and Linux behind one API.

Clusters (interconnected, VMs reach each other by IP):
  fleetbox up web -n 3      boots web-1, web-2, web-3 on one shared network
  fleetbox up db cache      boots db and cache on one shared network
  down/ssh/rm address each member by name (e.g. fleetbox ssh web-2)

Fixtures (read-only host dir copied into the guest, set at creation, repeatable):
  fleetbox up dev --fixture ./src:/work   packs ./src into the guest at /work
  Fixtures are read-only and world-readable; the set is frozen at creation
  (change it by rm + up), but the content is re-snapshotted on every boot. Host
  paths must not contain colons (the value is split on the last colon).

Defaults: image=debian-12, cpus=2, mem=4, disk=20`,
		// A command error prints our own "error: %v" line (see main); cobra must
		// not also dump usage or re-print the error.
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       versionString(),
	}

	// `fleetbox --version` prints exactly versionString(); the default template
	// would prepend "fleetbox version ".
	root.SetVersionTemplate("{{.Version}}\n")

	root.AddCommand(
		newUpCmd(),
		newDownCmd(),
		newLsCmd(),
		newSSHCmd(),
		newCpCmd(),
		newSSHConfigCmd(),
		newRmCmd(),
		newVersionCmd(),
	)
	return root
}
