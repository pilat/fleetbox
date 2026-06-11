package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/pilat/fleetbox"
	"github.com/pilat/fleetbox/internal/control"
	"github.com/pilat/fleetbox/internal/orchestrator"
	"github.com/pilat/fleetbox/internal/store"
)

// upFlags holds the parsed flags for `up`. The values are bound on the command
// in newUpCmd and passed into runUp explicitly, so there is no shared mutable
// state between invocations.
type upFlags struct {
	count    int
	cpus     int
	mem      int
	disk     int
	image    string
	fixtures []string
}

// newUpCmd builds the `up` command: create and boot one VM or an interconnected
// cluster, idempotently. An empty name uses the VM "default".
func newUpCmd() *cobra.Command {
	f := &upFlags{}
	cmd := &cobra.Command{
		Use:     "up [name...]",
		Aliases: []string{"start"},
		Short:   "Create and boot VM(s)",
		Long: `Create and boot one VM or an interconnected cluster. up is idempotent:
re-running it boots stopped members and leaves running ones in place. Disks
survive across reboots; only rm destroys them.

With no name, the VM "default" is used. Create-time options (--cpus/--mem/--disk/
--image/--fixture) are frozen at creation and ignored when a VM already exists;
change them by rm + up.`,
		Example: `  fleetbox up                      boot the "default" VM
  fleetbox up web                  boot a single VM named web
  fleetbox up web -n 3             boot a 3-node cluster web-1, web-2, web-3
  fleetbox up db cache             boot db and cache on one shared network
  fleetbox up dev --fixture ./src:/work   pack ./src into the guest at /work`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUp(cmd, args, f)
		},
	}

	// -n is registered as a long name "n" with shorthand "n" so `-n 3` parses
	// exactly as before and no `--count` surface is added. Do NOT collapse to
	// IntVar(&count, "n", ...): pflag would then treat -n as an unknown shorthand.
	cmd.Flags().IntVarP(&f.count, "n", "n", 1, "cluster size (boots <name>-1 .. <name>-N, interconnected)")
	cmd.Flags().IntVar(&f.cpus, "cpus", 2, "number of CPUs")
	cmd.Flags().IntVar(&f.mem, "mem", 4, "memory in GB")
	cmd.Flags().IntVar(&f.disk, "disk", 20, "disk size in GB")
	cmd.Flags().StringVar(&f.image, "image", "debian-12", "image alias or URL")
	// StringArrayVar appends each --fixture verbatim (no comma splitting), so a
	// host path containing a comma survives — unlike StringSliceVar.
	cmd.Flags().StringArrayVar(&f.fixtures, "fixture", nil,
		"pack a host dir into the guest read-only (host:guest, repeatable)")
	return cmd
}

func runUp(cmd *cobra.Command, names []string, f *upFlags) error {
	// up creates the bridge/taps via the root holder, so it needs root: re-exec
	// under sudo (interactive) or print the command (non-interactive). No-op on
	// macOS and when already root (ADR-0023).
	if err := ensurePrivileged(); err != nil {
		return err
	}

	members, err := clusterMembers(names, f.count)
	if err != nil {
		return err
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	// Create-time options are frozen at creation (ADR-0015); they are silently
	// ignored when a member already exists. Tell the user rather than letting them
	// believe a re-up resized the VM. The flags still take effect for any member
	// being created fresh in the same call.
	if createFlagsChanged(cmd) {
		for _, m := range members {
			if st.Exists(m) {
				fmt.Fprintf(os.Stderr,
					"note: %s already exists; --cpus/--mem/--disk/--image/--fixture ignored (rm + up to change)\n", m)
			}
		}
	}

	options := []fleetbox.Option{
		fleetbox.WithImage(f.image),
		fleetbox.WithCPUs(f.cpus),
		fleetbox.WithMemoryGB(f.mem),
		fleetbox.WithDiskGB(f.disk),
	}

	// Resolve host paths to absolute here, against the CLI's cwd, before they
	// cross into the holder process (which may not share the cwd).
	for _, fv := range f.fixtures {
		host, guest, err := parseFixture(fv)
		if err != nil {
			return err
		}
		options = append(options, fleetbox.WithFixture(host, guest))
	}

	return upMembers(st, members, options)
}

// createFlagsChanged reports whether any of the create-only flags were set on
// the command line. They are frozen at first create (ADR-0015), so a true result
// on an existing VM means the values are ignored — used only to warn the user.
func createFlagsChanged(cmd *cobra.Command) bool {
	return slices.ContainsFunc(
		[]string{"cpus", "mem", "disk", "image", "fixture"}, cmd.Flags().Changed)
}

// parseFixture splits a --fixture value into an absolute host path and a guest
// path. The split is on the LAST colon: guest paths are absolute and colon-free,
// and host paths effectively never contain colons, so this keeps the absolute
// guest path unambiguous. A value missing a colon, host, or guest — or a
// non-absolute guest path — is an error, surfaced here (before any image
// download) rather than later in the orchestrator.
func parseFixture(v string) (host, guest string, err error) {
	idx := strings.LastIndex(v, ":")
	if idx < 0 {
		return "", "", fmt.Errorf("invalid --fixture %q: expected host:guest", v)
	}
	host, guest = v[:idx], v[idx+1:]
	if host == "" || guest == "" {
		return "", "", fmt.Errorf("invalid --fixture %q: expected host:guest", v)
	}
	if !filepath.IsAbs(guest) {
		return "", "", fmt.Errorf("invalid --fixture %q: guest path %q must be absolute", v, guest)
	}
	abs, err := filepath.Abs(host)
	if err != nil {
		return "", "", fmt.Errorf("resolve fixture host path %q: %w", host, err)
	}
	return abs, guest, nil
}

// clusterMembers resolves CLI args into the concrete member names to bring up:
//
//	up <name>          -> [name]                  (single VM)
//	up <prefix> -n N   -> [prefix-1 .. prefix-N]  (interconnected cluster)
//	up a b c           -> [a, b, c]               (interconnected cluster)
func clusterMembers(names []string, count int) ([]string, error) {
	switch {
	case count < 1:
		return nil, errors.New("-n must be at least 1")
	case count > 1:
		if len(names) != 1 {
			return nil, errors.New("with -n, give exactly one name to use as the cluster prefix")
		}
		members := make([]string, count)
		for i := 1; i <= count; i++ {
			members[i-1] = fmt.Sprintf("%s-%d", names[0], i)
		}
		return members, nil
	case len(names) == 0:
		return []string{"default"}, nil
	default:
		return names, nil
	}
}

// upMembers brings the requested members up as one interconnected cluster: a
// fresh holder process when none is running, or — when some members already run
// in one holder — adding the missing members to that live network so a re-upped
// node re-joins the cluster instead of getting an isolated network of its own.
func upMembers(st *store.Store, members []string, options []fleetbox.Option) error {
	var running, missing []string
	for _, m := range members {
		if control.IsRunning(st, m) {
			running = append(running, m)
		} else {
			missing = append(missing, m)
		}
	}

	if len(missing) == 0 {
		return printMembers(st, members)
	}

	if len(running) == 0 {
		fmt.Printf("Starting %s...\n", strings.Join(missing, ", "))
		// Client-side orchestration drives a fresh detached helper (ADR-0020): the
		// CLI resolves the image, builds disks/seeds/fixtures, and drives boot over
		// the protocol; the helper persists after this command exits.
		if err := orchestrator.StartClusterDetached(context.Background(), missing, options...); err != nil {
			return fmt.Errorf("start cluster: %w", err)
		}
		return printMembers(st, members)
	}

	// Some members already run. They must share one holder for the added members
	// to land on the same network; AddMember drives the live helper through a
	// running sibling's socket (reserve + boot-member on the existing network).
	sibling, err := soleHolder(st, running)
	if err != nil {
		return err
	}
	for _, m := range missing {
		fmt.Printf("Adding %s to the cluster...\n", m)
		if err := orchestrator.AddMember(context.Background(), sibling, m, options...); err != nil {
			return fmt.Errorf("add %s: %w", m, err)
		}
	}
	return printMembers(st, members)
}

// soleHolder returns a running member whose holder owns all the others, or an
// error if the running members are split across processes (their separate
// networks cannot be merged).
func soleHolder(st *store.Store, running []string) (string, error) {
	pid := -1
	for _, m := range running {
		status, err := control.GetStatus(st, m)
		if err != nil {
			return "", fmt.Errorf("status %s: %w", m, err)
		}
		if pid == -1 {
			pid = status.PID
		} else if status.PID != pid {
			return "", errors.New(
				"the running members are on separate networks, so the new member can't " +
					"join them; bring the cluster up together (down the members, then up " +
					"them in one command) or boot the new member on its own",
			)
		}
	}
	return running[0], nil
}

func printMembers(st *store.Store, members []string) error {
	for _, m := range members {
		status, err := control.GetStatus(st, m)
		if err != nil {
			// printMembers runs only after a successful start/add, so the holder is
			// up and a status failure is a real broken-control-path signal, not the
			// "booted but no IP yet" case below — don't paper over it as "(no IP)".
			return fmt.Errorf("status %s: %w", m, err)
		}
		if status.IP == "" {
			fmt.Printf("  %s: (no IP)\n", m)
			continue
		}
		fmt.Printf("  %s  IP: %s  SSH: ssh fleetbox@%s\n", m, status.IP, status.IP)
	}
	return nil
}
