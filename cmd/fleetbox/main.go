//go:build darwin && arm64

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pilat/fleetbox"
	"github.com/pilat/fleetbox/internal/runner"
	"github.com/pilat/fleetbox/internal/store"
)

func main() {
	// Check if we're running as a background VM holder
	if runner.IsRunner() {
		if err := runner.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "runner error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "up":
		err = cmdUp(args)
	case "down":
		err = cmdDown(args)
	case "ls", "list":
		err = cmdList(args)
	case "ssh":
		err = cmdSSH(args)
	case "cp":
		err = cmdCopy(args)
	case "ssh-config":
		err = cmdSSHConfig(args)
	case "rm", "remove":
		err = cmdRemove(args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`fleetbox - Linux VMs as test fixtures on macOS

Usage:
  fleetbox up [name...] [-n N] [--cpus N] [--mem GB] [--disk GB] [--image alias|URL] [--mount host:guest]
  fleetbox down <name>... | --all
  fleetbox ls
  fleetbox ssh <name> [-- cmd]
  fleetbox cp <src> <dst>
  fleetbox ssh-config
  fleetbox rm <name>... | --all

Commands:
  up         Create and boot VM(s)
  down       Gracefully shutdown VM(s), disk preserved
  ls         List VMs
  ssh        SSH into a VM
  cp         Copy files to/from VM (scp syntax: name:/path)
  ssh-config Print SSH config for all VMs
  rm         Destroy VM(s) completely

Clusters (interconnected, VMs reach each other by IP):
  fleetbox up web -n 3      boots web-1, web-2, web-3 on one shared network
  fleetbox up db cache      boots db and cache on one shared network
  down/ssh/rm address each member by name (e.g. fleetbox ssh web-2)

Mounts (live read-write host↔guest folders, set at creation, repeatable):
  fleetbox up dev --mount ./src:/work   shares ./src into the guest at /work
  Mounts are frozen at creation; change them by rm + up. Host paths must not
  contain colons (the value is split on the last colon).

Defaults: image=debian-12, cpus=2, mem=4, disk=20`)
}

// stringSlice is a flag.Value that accumulates a repeatable string flag (the
// stdlib flag package has no []string), used for --mount.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }

func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func cmdUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	count := fs.Int("n", 1, "cluster size (boots <name>-1 .. <name>-N, interconnected)")
	cpus := fs.Int("cpus", 2, "number of CPUs")
	mem := fs.Int("mem", 4, "memory in GB")
	disk := fs.Int("disk", 20, "disk size in GB")
	image := fs.String("image", "debian-12", "image alias or URL")
	var mounts stringSlice
	fs.Var(&mounts, "mount", "share a host dir into the guest (host:guest, repeatable)")

	// Go's flag package stops at the first positional arg, so `up test1 -n 2`
	// would treat `-n 2` as names. Parse flags and positionals interspersed.
	names, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}

	members, err := clusterMembers(names, *count)
	if err != nil {
		return err
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	opts := []fleetbox.Option{
		fleetbox.WithImage(*image),
		fleetbox.WithCPUs(*cpus),
		fleetbox.WithMemoryGB(*mem),
		fleetbox.WithDiskGB(*disk),
	}

	// Resolve host paths to absolute here, against the CLI's cwd, before they
	// cross into the holder process (which re-execs and may not share the cwd).
	for _, mv := range mounts {
		host, guest, err := parseMount(mv)
		if err != nil {
			return err
		}
		opts = append(opts, fleetbox.WithMount(host, guest))
	}

	return upMembers(st, members, opts)
}

// parseMount splits a --mount value into an absolute host path and a guest path.
// The split is on the LAST colon: guest paths are absolute and colon-free, and
// macOS host paths effectively never contain colons, so this keeps the absolute
// guest path unambiguous. A value missing a colon, host, or guest is an error.
func parseMount(v string) (host, guest string, err error) {
	idx := strings.LastIndex(v, ":")
	if idx < 0 {
		return "", "", fmt.Errorf("invalid --mount %q: expected host:guest", v)
	}
	host, guest = v[:idx], v[idx+1:]
	if host == "" || guest == "" {
		return "", "", fmt.Errorf("invalid --mount %q: expected host:guest", v)
	}
	abs, err := filepath.Abs(host)
	if err != nil {
		return "", "", fmt.Errorf("resolve mount host path %q: %w", host, err)
	}
	return abs, guest, nil
}

// parseInterspersed parses fs allowing flags and positional args in any order
// (the stdlib flag package otherwise stops at the first positional). It returns
// the collected positional args.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return nil, fmt.Errorf("parse flags: %w", err)
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
	return positional, nil
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
func upMembers(st *store.Store, members []string, opts []fleetbox.Option) error {
	var running, missing []string
	for _, m := range members {
		if runner.IsRunning(st, m) {
			running = append(running, m)
		} else {
			missing = append(missing, m)
		}
	}

	if len(missing) == 0 {
		printMembers(st, members)
		return nil
	}

	if len(running) == 0 {
		fmt.Printf("Starting %s...\n", strings.Join(missing, ", "))
		if _, err := runner.Spawn(st, missing, opts); err != nil {
			return fmt.Errorf("start cluster: %w", err)
		}
		printMembers(st, members)
		return nil
	}

	// Some members already run. They must share one holder for the added
	// members to land on the same network.
	sibling, err := soleHolder(st, running)
	if err != nil {
		return err
	}
	for _, m := range missing {
		fmt.Printf("Adding %s to the cluster...\n", m)
		if err := runner.AddMember(st, sibling, m); err != nil {
			return fmt.Errorf("add %s: %w", m, err)
		}
	}
	printMembers(st, members)
	return nil
}

// soleHolder returns a running member whose holder owns all the others, or an
// error if the running members are split across processes (their separate
// networks cannot be merged).
func soleHolder(st *store.Store, running []string) (string, error) {
	pid := -1
	for _, m := range running {
		status, err := runner.GetStatus(st, m)
		if err != nil {
			return "", fmt.Errorf("status %s: %w", m, err)
		}
		if pid == -1 {
			pid = status.PID
		} else if status.PID != pid {
			return "", errors.New(
				"requested members are running in separate processes and can't be " +
					"joined into one network; rm them and bring the cluster up together",
			)
		}
	}
	return running[0], nil
}

func printMembers(st *store.Store, members []string) {
	for _, m := range members {
		status, err := runner.GetStatus(st, m)
		if err != nil || status.IP == "" {
			fmt.Printf("  %s: (no IP)\n", m)
			continue
		}
		fmt.Printf("  %s  IP: %s  SSH: ssh fleetbox@%s\n", m, status.IP, status.IP)
	}
}

func cmdDown(args []string) error {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	all := fs.Bool("all", false, "stop all VMs")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	var names []string
	if *all {
		names, err = st.List()
		if err != nil {
			return fmt.Errorf("list vms: %w", err)
		}
	} else {
		names = positional
	}

	if len(names) == 0 {
		return errors.New("no VMs specified")
	}

	for _, name := range names {
		if !runner.IsRunning(st, name) {
			fmt.Printf("%s is not running\n", name)
			continue
		}
		fmt.Printf("Stopping %s...\n", name)
		if err := runner.Stop(st, name); err != nil {
			return fmt.Errorf("stop %s: %w", name, err)
		}
		fmt.Printf("  stopped\n")
	}

	return nil
}

func cmdList(_ []string) error {
	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	names, err := st.List()
	if err != nil {
		return fmt.Errorf("list vms: %w", err)
	}

	if len(names) == 0 {
		fmt.Println("No VMs found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tIP\tSTATE\tCPUS\tMEM\tDISK\tAGE")

	for _, name := range names {
		vmCfg, err := st.Load(name)
		if err != nil {
			continue
		}

		ip := "-"
		state := "stopped"

		status, err := runner.GetStatus(st, name)
		if err == nil && status.Running {
			state = "running"
			ip = status.IP
		}

		age := time.Since(vmCfg.CreatedAt).Round(time.Second)

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%dGB\t%dGB\t%s\n",
			vmCfg.Name, ip, state, vmCfg.CPUs, vmCfg.MemoryMB/1024, vmCfg.DiskMB/1024, age)
	}

	_ = w.Flush()
	return nil
}

func cmdSSH(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: fleetbox ssh <name> [-- cmd]")
	}

	name := args[0]
	var cmd []string
	for i, arg := range args {
		if arg == "--" {
			cmd = args[i+1:]
			break
		}
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	if !st.Exists(name) {
		return fmt.Errorf("VM %q does not exist", name)
	}

	status, err := runner.GetStatus(st, name)
	if err != nil {
		return fmt.Errorf("get status: %w", err)
	}
	if !status.Running {
		return fmt.Errorf("VM %q is not running", name)
	}

	sshArgs := make([]string, 0, 7+len(cmd))
	sshArgs = append(sshArgs,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-i", st.SSHKeyPath(),
		"fleetbox@"+status.IP,
	)
	sshArgs = append(sshArgs, cmd...)

	sshCmd := exec.Command("ssh", sshArgs...)
	sshCmd.Stdin = os.Stdin
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr

	if err := sshCmd.Run(); err != nil {
		return fmt.Errorf("ssh: %w", err)
	}

	return nil
}

func cmdCopy(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: fleetbox cp <src> <dst>")
	}

	src, dst := args[0], args[1]

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	// Parse name:/path syntax
	var name string
	if strings.Contains(src, ":") {
		parts := strings.SplitN(src, ":", 2)
		name = parts[0]
	} else if strings.Contains(dst, ":") {
		parts := strings.SplitN(dst, ":", 2)
		name = parts[0]
	} else {
		return errors.New("either src or dst must use name:/path syntax")
	}

	if !st.Exists(name) {
		return fmt.Errorf("VM %q does not exist", name)
	}

	status, err := runner.GetStatus(st, name)
	if err != nil {
		return fmt.Errorf("get status: %w", err)
	}
	if !status.Running {
		return fmt.Errorf("VM %q is not running", name)
	}

	// Rewrite paths with actual IP
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
		"-o", "UserKnownHostsFile=/dev/null",
		"-i", st.SSHKeyPath(),
		src, dst,
	}

	scpCmd := exec.Command("scp", scpArgs...)
	scpCmd.Stdin = os.Stdin
	scpCmd.Stdout = os.Stdout
	scpCmd.Stderr = os.Stderr

	if err := scpCmd.Run(); err != nil {
		return fmt.Errorf("scp: %w", err)
	}

	return nil
}

func cmdSSHConfig(_ []string) error {
	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	names, err := st.List()
	if err != nil {
		return fmt.Errorf("list vms: %w", err)
	}

	for _, name := range names {
		status, err := runner.GetStatus(st, name)
		if err != nil || !status.Running {
			continue // Only show running VMs
		}

		fmt.Printf("Host %s\n", name)
		fmt.Printf("  HostName %s\n", status.IP)
		fmt.Printf("  User fleetbox\n")
		fmt.Printf("  IdentityFile %s\n", st.SSHKeyPath())
		fmt.Printf("  StrictHostKeyChecking no\n")
		fmt.Printf("  UserKnownHostsFile /dev/null\n")
		fmt.Println()
	}

	return nil
}

func cmdRemove(args []string) error {
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	all := fs.Bool("all", false, "remove all VMs")
	force := fs.Bool("f", false, "force removal without confirmation")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	var names []string
	if *all {
		names, err = st.List()
		if err != nil {
			return fmt.Errorf("list vms: %w", err)
		}
	} else {
		names = positional
	}

	if len(names) == 0 {
		return errors.New("no VMs specified")
	}

	// Check for prefix matches
	var toDelete []string
	allNames, _ := st.List()
	for _, pattern := range names {
		found := false
		for _, name := range allNames {
			if name == pattern || strings.HasPrefix(name, pattern+"-") {
				toDelete = append(toDelete, name)
				found = true
			}
		}
		if !found {
			return fmt.Errorf("VM %q not found", pattern)
		}
	}

	if !*force && len(toDelete) > 1 {
		fmt.Printf("Will delete %d VMs: %s\n", len(toDelete), strings.Join(toDelete, ", "))
		fmt.Print("Continue? [y/N] ")
		var response string
		_, _ = fmt.Scanln(&response)
		if response != "y" && response != "Y" {
			return errors.New("aborted")
		}
	}

	for _, name := range toDelete {
		fmt.Printf("Removing %s...\n", name)
		// Stop runner if running
		if runner.IsRunning(st, name) {
			if err := runner.Stop(st, name); err != nil {
				return fmt.Errorf("stop %s: %w", name, err)
			}
		}
		// Clean up stale pid/sock files
		_ = os.Remove(st.PidfilePath(name))
		_ = os.Remove(st.SocketPath(name))
		// Delete VM directory
		if err := st.Delete(name); err != nil {
			return fmt.Errorf("delete %s: %w", name, err)
		}
	}

	return nil
}
