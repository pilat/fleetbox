//go:build darwin && arm64

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
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
  fleetbox up [name...] [-n N] [--cpus N] [--mem GB] [--disk GB] [--image alias|URL]
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

Defaults: image=debian-12, cpus=2, mem=4, disk=20`)
}

func cmdUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	count := fs.Int("n", 1, "number of VMs to create")
	cpus := fs.Int("cpus", 2, "number of CPUs")
	mem := fs.Int("mem", 4, "memory in GB")
	disk := fs.Int("disk", 20, "disk size in GB")
	image := fs.String("image", "debian-12", "image alias or URL")
	_ = fs.Parse(args)

	names := fs.Args()
	if len(names) == 0 {
		names = []string{"default"}
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

	for _, name := range names {
		if *count > 1 {
			for i := 1; i <= *count; i++ {
				vmName := fmt.Sprintf("%s-%d", name, i)
				if err := startVM(st, vmName, opts); err != nil {
					return err
				}
			}
		} else {
			if err := startVM(st, name, opts); err != nil {
				return err
			}
		}
	}

	return nil
}

func startVM(st *store.Store, name string, opts []fleetbox.Option) error {
	fmt.Printf("Starting %s...\n", name)

	status, err := runner.Spawn(st, name, opts)
	if err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}

	fmt.Printf("  IP: %s\n", status.IP)
	fmt.Printf("  SSH: ssh fleetbox@%s\n", status.IP)
	return nil
}

func cmdDown(args []string) error {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	all := fs.Bool("all", false, "stop all VMs")
	_ = fs.Parse(args)

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
		names = fs.Args()
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
	_ = fs.Parse(args)

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
		names = fs.Args()
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
