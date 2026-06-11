package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newVersionCmd builds the `version` command: print the build version string.
// It mirrors the root's --version flag (kept for parity with ADR-0021).
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Println(versionString())
			return nil
		},
	}
}
