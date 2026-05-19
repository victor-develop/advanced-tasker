package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/state"
)

func newInitCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create state/ skeleton and initialize its git repo",
		Long:  "Creates every directory required by the design/02 layout, writes a default config.yaml, seeds role prompt stubs, runs `git init` inside state/, and produces the initial commit.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := state.Resolve(opts.StateRoot)
			if err != nil {
				return errf(ExitUsage, "resolve state root: %v", err)
			}
			if opts.DryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would init state at %s\n", root)
				return nil
			}
			if err := state.Init(root); err != nil {
				return errf(ExitGit, "%v", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "initialized harness state at %s\n", root)
			return nil
		},
	}
}

func newVersionCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print harness version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), Version)
			return nil
		},
	}
}
