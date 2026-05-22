package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/dag"
	"github.com/victor-develop/advanced-tasker/internal/ids"
	"github.com/victor-develop/advanced-tasker/internal/state"
	"github.com/victor-develop/advanced-tasker/internal/store"
)

// newLinkCmd implements: harness link T-<a> blocked-on T-<b>
func newLinkCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "link <T-from> blocked-on <T-to>",
		Short: "Add a blocked-on dependency edge (T-from depends on T-to)",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[1] != "blocked-on" {
				return errf(ExitUsage, "second arg must be 'blocked-on'")
			}
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			from, err := ids.NormalizeTaskID(args[0])
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			to, err := ids.NormalizeTaskID(args[2])
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			return state.WithStateLock(root, func() error {
				all, err := store.LoadAll(root)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if _, err := store.ReadStatus(root, from); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if _, err := store.ReadStatus(root, to); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				g := dag.FromStatuses(all)
				cycle, path, _ := g.AddEdge(from, to)
				if cycle {
					return errf(ExitValidation, "would create dependency cycle: %s", dag.FormatCycle(path))
				}
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would link %s blocked-on %s\n", from, to)
					return nil
				}
				fromSt, _ := store.ReadStatus(root, from)
				if store.AddBlockedOn(fromSt, to) {
					if err := store.WriteStatus(root, fromSt); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					_ = store.AppendLog(root, from, "cli", fmt.Sprintf("link blocked-on %s", to))
				}
				if err := commitTaskMutation(root, fmt.Sprintf("link %s blocked-on %s", from, to), from); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "linked %s blocked-on %s\n", from, to)
				return nil
			})
		},
	}
}

// newUnlinkCmd implements: harness unlink T-<a> T-<b>
func newUnlinkCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "unlink <T-from> <T-to>",
		Short: "Remove a blocked-on dependency edge",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			from, err := ids.NormalizeTaskID(args[0])
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			to, err := ids.NormalizeTaskID(args[1])
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			return state.WithStateLock(root, func() error {
				fromSt, err := store.ReadStatus(root, from)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would unlink %s blocked-on %s\n", from, to)
					return nil
				}
				if !store.RemoveBlockedOn(fromSt, to) {
					return errf(ExitValidation, "edge %s blocked-on %s not present", from, to)
				}
				if err := store.WriteStatus(root, fromSt); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				_ = store.AppendLog(root, from, "cli", fmt.Sprintf("unlink blocked-on %s", to))
				if err := commitTaskMutation(root, fmt.Sprintf("unlink %s blocked-on %s", from, to), from); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "unlinked %s blocked-on %s\n", from, to)
				return nil
			})
		},
	}
}

// newDepsCmd implements: harness deps show / harness deps cycles
func newDepsCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{
		Use:   "deps",
		Short: "Inspect the dependency graph",
	}
	c.AddCommand(
		&cobra.Command{
			Use:   "show <T-n>",
			Short: "Show upstream and downstream dependencies",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				id, err := ids.NormalizeTaskID(args[0])
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				all, err := store.LoadAll(root)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				g := dag.FromStatuses(all)
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "task: %s\n", id)
				fmt.Fprintf(out, "upstream (blocks me):   %v\n", g.Upstream(id))
				fmt.Fprintf(out, "downstream (I block):   %v\n", g.Downstream(id))
				return nil
			},
		},
		&cobra.Command{
			Use:   "cycles",
			Short: "Report any cycles in the dependency graph",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				all, err := store.LoadAll(root)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				g := dag.FromStatuses(all)
				cycles := g.FindCycles()
				if len(cycles) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "no cycles")
					return nil
				}
				for _, c := range cycles {
					fmt.Fprintln(cmd.OutOrStdout(), dag.FormatCycle(c))
				}
				return errf(ExitValidation, "%d cycle(s) detected", len(cycles))
			},
		},
	)
	return c
}
