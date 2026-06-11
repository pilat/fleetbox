package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/pilat/fleetbox/internal/control"
	"github.com/pilat/fleetbox/internal/store"
)

// VM run states as ls reports them: a VM whose holder answers is running; one
// with no reachable holder (stopped, crashed, or never booted) reads as stopped.
const (
	stateStopped = "stopped"
	stateRunning = "running"
)

// lsEntry is the machine-readable shape of a VM in `ls -o json`. It is the pinned
// contract scripts depend on: field names and units are fixed (memory_mb/disk_mb
// are megabytes, verbatim from the stored config; created_at is RFC3339). Keys
// are snake_case to match config.json and the control protocol — one consistent
// case across all fleetbox JSON. There is deliberately no age field — age is a
// display-only derivation of created_at.
type lsEntry struct {
	Name      string `json:"name"`
	IP        string `json:"ip"`
	State     string `json:"state"`
	CPUs      int    `json:"cpus"`
	MemoryMB  int    `json:"memory_mb"`
	DiskMB    int    `json:"disk_mb"`
	Image     string `json:"image"`
	CreatedAt string `json:"created_at"`
}

// newLsCmd builds the `ls` command: list VMs as a human table (default), bare
// names (-q), or a JSON array (-o json).
func newLsCmd() *cobra.Command {
	var (
		quiet  bool
		output string
	)
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List VMs",
		Long: `List VMs as a human-readable table (default), as bare names one per
line (-q, for scripting), or as a JSON array (-o json, the machine contract).`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runLs(quiet, output)
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only VM names, one per line")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table or json")
	// -q and -o select different renderings; setting both is ambiguous.
	cmd.MarkFlagsMutuallyExclusive("quiet", "output")
	return cmd
}

func runLs(quiet bool, output string) error {
	if output != "table" && output != "json" {
		return fmt.Errorf("invalid --output %q: want \"table\" or \"json\"", output)
	}

	st, err := store.New()
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	names, err := st.List()
	if err != nil {
		return fmt.Errorf("list vms: %w", err)
	}

	switch {
	case quiet:
		for _, name := range names {
			fmt.Println(name)
		}
		return nil
	case output == "json":
		return lsJSON(st, names)
	default:
		return lsTable(st, names)
	}
}

// lsEntries loads each VM's config and live status into the machine-readable
// shape, skipping any whose config cannot be read (matching the table path). A
// stopped or unreachable VM reports state "stopped" and an empty IP.
func lsEntries(st *store.Store, names []string) []lsEntry {
	entries := make([]lsEntry, 0, len(names))
	for _, name := range names {
		vmCfg, err := st.Load(name)
		if err != nil {
			continue
		}

		ip := ""
		state := stateStopped
		if status, err := control.GetStatus(st, name); err == nil && status.Running {
			state = stateRunning
			ip = status.IP
		}

		entries = append(entries, lsEntry{
			Name:      vmCfg.Name,
			IP:        ip,
			State:     state,
			CPUs:      vmCfg.CPUs,
			MemoryMB:  vmCfg.MemoryMB,
			DiskMB:    vmCfg.DiskMB,
			Image:     vmCfg.Image,
			CreatedAt: vmCfg.CreatedAt.Format(time.RFC3339),
		})
	}
	return entries
}

func lsJSON(st *store.Store, names []string) error {
	data, err := json.MarshalIndent(lsEntries(st, names), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	fmt.Println(string(data))
	return nil
}

func lsTable(st *store.Store, names []string) error {
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
		state := stateStopped

		status, err := control.GetStatus(st, name)
		if err == nil && status.Running {
			state = stateRunning
			ip = status.IP
		}

		age := time.Since(vmCfg.CreatedAt).Round(time.Second)

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%dGB\t%dGB\t%s\n",
			vmCfg.Name, ip, state, vmCfg.CPUs, vmCfg.MemoryMB/1024, vmCfg.DiskMB/1024, age)
	}

	_ = w.Flush()
	return nil
}
