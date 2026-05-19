package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/gitops"
	"github.com/victor-develop/advanced-tasker/internal/ids"
	"github.com/victor-develop/advanced-tasker/internal/state"
	"github.com/victor-develop/advanced-tasker/internal/store"
)

// newGoalCmd implements `harness goal create`. A goal is a top-level
// task (no parent). It returns the assigned T-<n>.
func newGoalCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{
		Use:   "goal",
		Short: "Manage top-level goals (parent-less tasks)",
	}
	c.AddCommand(&cobra.Command{
		Use:   "create <title>",
		Short: "Create a new top-level goal (parent-less task)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCreateGoal(opts, cmd, args[0])
		},
	})
	return c
}

func runCreateGoal(opts *Options, cmd *cobra.Command, title string) error {
	root, err := requireInitialized(opts)
	if err != nil {
		return err
	}
	return state.WithStateLock(root, func() error {
		id, err := ids.NextTaskID(idsTasksRoot(root))
		if err != nil {
			return errf(ExitGit, "%v", err)
		}
		now := time.Now().UTC()
		st := store.Status{
			ID:            id,
			State:         store.StateReady,
			Priority:      store.PriorityNormal,
			BlockedOn:     []string{},
			LinkedThreads: []string{},
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		body := fmt.Sprintf("# %s — %s\n\n%s\n", id, title, title)
		if opts.DryRun {
			fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would create %s\n", id)
			return nil
		}
		if err := store.CreateTask(root, st, body); err != nil {
			return errf(ExitValidation, "%v", err)
		}
		if err := commitTaskMutation(root, fmt.Sprintf("create goal %s: %s", id, title), id); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), id)
		return nil
	})
}

// newTaskCmd assembles `harness task <verb>`.
func newTaskCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{
		Use:   "task",
		Short: "Create and manipulate tasks",
	}
	c.AddCommand(
		taskCreateCmd(opts),
		taskUpdateCmd(opts),
		taskKillCmd(opts),
		taskDeferCmd(opts),
		taskSplitCmd(opts),
		taskMergeCmd(opts),
		taskRestateGoalCmd(opts),
		taskLsCmd(opts),
		taskShowCmd(opts),
	)
	return c
}

func taskCreateCmd(opts *Options) *cobra.Command {
	var parent, priority, due string
	c := &cobra.Command{
		Use:   "create <title>",
		Short: "Create a new task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			title := args[0]
			return state.WithStateLock(root, func() error {
				id, err := ids.NextTaskID(idsTasksRoot(root))
				if err != nil {
					return errf(ExitGit, "%v", err)
				}
				now := time.Now().UTC()
				st := store.Status{
					ID:            id,
					State:         store.StateReady,
					Priority:      store.PriorityNormal,
					BlockedOn:     []string{},
					LinkedThreads: []string{},
					CreatedAt:     now,
					UpdatedAt:     now,
				}
				if parent != "" {
					pn, err := ids.NormalizeTaskID(parent)
					if err != nil {
						return errf(ExitValidation, "--parent: %v", err)
					}
					if _, err := store.ReadStatus(root, pn); err != nil {
						return errf(ExitValidation, "parent %s not found", pn)
					}
					st.ParentGoal = pn
				}
				if priority != "" {
					st.Priority = store.Priority(priority)
				}
				if due != "" {
					t, err := time.Parse(time.RFC3339, due)
					if err != nil {
						return errf(ExitValidation, "--due must be RFC3339: %v", err)
					}
					st.DueAt = &t
				}
				body := fmt.Sprintf("# %s — %s\n\n%s\n", id, title, title)
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would create %s\n", id)
					return nil
				}
				if err := store.CreateTask(root, st, body); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if err := commitTaskMutation(root, fmt.Sprintf("create task %s: %s", id, title), id); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), id)
				return nil
			})
		},
	}
	c.Flags().StringVar(&parent, "parent", "", "parent task id (T-<n>)")
	c.Flags().StringVar(&priority, "priority", "", "low|normal|high")
	c.Flags().StringVar(&due, "due", "", "due time (RFC3339)")
	return c
}

func taskUpdateCmd(opts *Options) *cobra.Command {
	var stateF, priority, due, assignee string
	c := &cobra.Command{
		Use:   "update <T-n>",
		Short: "Update a task's state / priority / due / assignee",
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
			return state.WithStateLock(root, func() error {
				st, err := store.ReadStatus(root, id)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if stateF != "" {
					st.State = store.TaskState(stateF)
				}
				if priority != "" {
					st.Priority = store.Priority(priority)
				}
				if due != "" {
					t, err := time.Parse(time.RFC3339, due)
					if err != nil {
						return errf(ExitValidation, "--due must be RFC3339: %v", err)
					}
					st.DueAt = &t
				}
				if assignee != "" {
					st.Assignee = assignee
				}
				st.UpdatedAt = time.Now().UTC()
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would update %s\n", id)
					return nil
				}
				if err := store.WriteStatus(root, st); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				_ = store.AppendLog(root, id, "cli", fmt.Sprintf("update state=%s priority=%s", st.State, st.Priority))
				if err := commitTaskMutation(root, fmt.Sprintf("update task %s: state=%s", id, st.State), id); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "updated %s\n", id)
				return nil
			})
		},
	}
	c.Flags().StringVar(&stateF, "state", "", "ready|in-progress|blocked|deferred|done|killed")
	c.Flags().StringVar(&priority, "priority", "", "low|normal|high")
	c.Flags().StringVar(&due, "due", "", "due time (RFC3339)")
	c.Flags().StringVar(&assignee, "assignee", "", "assignee identifier")
	return c
}

func taskKillCmd(opts *Options) *cobra.Command {
	var reason string
	c := &cobra.Command{
		Use:   "kill <T-n>",
		Short: "Kill a task (terminal state)",
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
			return state.WithStateLock(root, func() error {
				st, err := store.ReadStatus(root, id)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				st.State = store.StateKilled
				st.KilledReason = reason
				st.UpdatedAt = time.Now().UTC()
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would kill %s\n", id)
					return nil
				}
				if err := store.WriteStatus(root, st); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				_ = store.AppendLog(root, id, "cli", fmt.Sprintf("killed: %s", reason))
				if err := commitTaskMutation(root, fmt.Sprintf("kill task %s: %s", id, reason), id); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "killed %s\n", id)
				return nil
			})
		},
	}
	c.Flags().StringVar(&reason, "reason", "", "reason for killing")
	return c
}

func taskDeferCmd(opts *Options) *cobra.Command {
	var reason, until string
	c := &cobra.Command{
		Use:   "defer <T-n>",
		Short: "Defer a task (state=deferred)",
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
			return state.WithStateLock(root, func() error {
				st, err := store.ReadStatus(root, id)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				st.State = store.StateDeferred
				st.DeferredReason = reason
				if until != "" {
					t, err := time.Parse(time.RFC3339, until)
					if err != nil {
						return errf(ExitValidation, "--until must be RFC3339: %v", err)
					}
					st.DeferredUntil = &t
				}
				st.UpdatedAt = time.Now().UTC()
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would defer %s\n", id)
					return nil
				}
				if err := store.WriteStatus(root, st); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				_ = store.AppendLog(root, id, "cli", fmt.Sprintf("deferred: %s (until=%s)", reason, until))
				if err := commitTaskMutation(root, fmt.Sprintf("defer task %s: %s", id, reason), id); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "deferred %s\n", id)
				return nil
			})
		},
	}
	c.Flags().StringVar(&reason, "reason", "", "reason for deferral")
	c.Flags().StringVar(&until, "until", "", "deferred until (RFC3339)")
	return c
}

func taskSplitCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "split <T-n> <child-title> [<child-title>...]",
		Short: "Split a task into N child tasks (parent kept as parent_goal)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			parent, err := ids.NormalizeTaskID(args[0])
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			titles := args[1:]
			return state.WithStateLock(root, func() error {
				// Confirm parent exists.
				pst, err := store.ReadStatus(root, parent)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would split %s into %d children\n", parent, len(titles))
					return nil
				}
				created := []string{}
				for _, title := range titles {
					id, err := ids.NextTaskID(idsTasksRoot(root))
					if err != nil {
						return errf(ExitGit, "%v", err)
					}
					now := time.Now().UTC()
					child := store.Status{
						ID:            id,
						State:         store.StateReady,
						Priority:      pst.Priority,
						ParentGoal:    parent,
						BlockedOn:     []string{},
						LinkedThreads: []string{},
						CreatedAt:     now,
						UpdatedAt:     now,
					}
					body := fmt.Sprintf("# %s — %s\n\n%s\n\n(split from %s)\n", id, title, title, parent)
					if err := store.CreateTask(root, child, body); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					created = append(created, id)
				}
				_ = store.AppendLog(root, parent, "cli", fmt.Sprintf("split into: %s", strings.Join(created, ", ")))
				msg := fmt.Sprintf("split task %s into %d children", parent, len(created))
				if err := commitTaskMutation(root, msg, append([]string{parent}, created...)...); err != nil {
					return err
				}
				for _, id := range created {
					fmt.Fprintln(cmd.OutOrStdout(), id)
				}
				return nil
			})
		},
	}
}

func taskMergeCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "merge <T-keep> <T-absorb>",
		Short: "Absorb T-absorb into T-keep (T-absorb is killed with a pointer)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			keep, err := ids.NormalizeTaskID(args[0])
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			absorb, err := ids.NormalizeTaskID(args[1])
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			if keep == absorb {
				return errf(ExitValidation, "cannot merge a task into itself")
			}
			return state.WithStateLock(root, func() error {
				keepSt, err := store.ReadStatus(root, keep)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				absorbSt, err := store.ReadStatus(root, absorb)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would merge %s into %s\n", absorb, keep)
					return nil
				}
				// Move absorb's linked threads onto keep.
				for _, t := range absorbSt.LinkedThreads {
					keepSt.LinkedThreads = appendUnique(keepSt.LinkedThreads, t)
				}
				// Move absorb's blocked_on onto keep (deduped, skipping keep itself).
				for _, b := range absorbSt.BlockedOn {
					if b == keep {
						continue
					}
					keepSt.BlockedOn = appendUnique(keepSt.BlockedOn, b)
				}
				keepSt.UpdatedAt = time.Now().UTC()
				absorbSt.State = store.StateKilled
				absorbSt.KilledReason = fmt.Sprintf("merged into %s", keep)
				absorbSt.UpdatedAt = time.Now().UTC()
				if err := store.WriteStatus(root, keepSt); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if err := store.WriteStatus(root, absorbSt); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				// Any task that was blocked on `absorb` now blocks on `keep`.
				all, err := store.LoadAll(root)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				touched := []string{keep, absorb}
				for _, s := range all {
					if s.ID == keep || s.ID == absorb {
						continue
					}
					changed := false
					for i, b := range s.BlockedOn {
						if b == absorb {
							s.BlockedOn[i] = keep
							changed = true
						}
					}
					if changed {
						s.UpdatedAt = time.Now().UTC()
						if err := store.WriteStatus(root, s); err != nil {
							return errf(ExitValidation, "%v", err)
						}
						touched = append(touched, s.ID)
					}
				}
				_ = store.AppendLog(root, keep, "cli", fmt.Sprintf("merged %s into this task", absorb))
				_ = store.AppendLog(root, absorb, "cli", fmt.Sprintf("merged into %s", keep))
				msg := fmt.Sprintf("merge tasks %s+%s: %s absorbed", keep, absorb, absorb)
				if err := commitTaskMutation(root, msg, touched...); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "merged %s into %s\n", absorb, keep)
				return nil
			})
		},
	}
}

func taskRestateGoalCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "restate-goal <T-n> <new-goal>",
		Short: "Rewrite goal.md for a task",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			id, err := ids.NormalizeTaskID(args[0])
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			body := args[1]
			return state.WithStateLock(root, func() error {
				st, err := store.ReadStatus(root, id)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				st.UpdatedAt = time.Now().UTC()
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would restate goal for %s\n", id)
					return nil
				}
				p := store.PathsFor(root, id)
				newBody := fmt.Sprintf("# %s\n\n%s\n", id, body)
				if err := writeFile(p.Goal, newBody); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if err := store.WriteStatus(root, st); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				_ = store.AppendLog(root, id, "cli", "restated goal")
				if err := commitTaskMutation(root, fmt.Sprintf("restate goal %s", id), id); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "restated %s\n", id)
				return nil
			})
		},
	}
}

func taskLsCmd(opts *Options) *cobra.Command {
	var stateFilter, parentFilter string
	var blockedOnly bool
	c := &cobra.Command{
		Use:   "ls",
		Short: "List tasks",
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
			for _, s := range all {
				if stateFilter != "" && string(s.State) != stateFilter {
					continue
				}
				if parentFilter != "" {
					p, err := ids.NormalizeTaskID(parentFilter)
					if err != nil {
						return errf(ExitValidation, "--parent: %v", err)
					}
					if s.ParentGoal != p {
						continue
					}
				}
				if blockedOnly && len(s.BlockedOn) == 0 {
					continue
				}
				goalSummary := firstLine(readFile(store.PathsFor(root, s.ID).Goal))
				fmt.Fprintf(cmd.OutOrStdout(), "%-6s %-12s %-10s %s\n", s.ID, s.State, s.Priority, goalSummary)
			}
			return nil
		},
	}
	c.Flags().StringVar(&stateFilter, "state", "", "filter by state")
	c.Flags().StringVar(&parentFilter, "parent", "", "filter by parent goal")
	c.Flags().BoolVar(&blockedOnly, "blocked", false, "only show tasks with blockers")
	return c
}

func taskShowCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "show <T-n>",
		Short: "Show full status, goal, and log for a task",
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
			st, err := store.ReadStatus(root, id)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			goal, _ := store.GoalBody(root, id)
			log, _ := store.LogBody(root, id)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s  state=%s  priority=%s\n", st.ID, st.State, st.Priority)
			if st.ParentGoal != "" {
				fmt.Fprintf(out, "parent: %s\n", st.ParentGoal)
			}
			if len(st.BlockedOn) > 0 {
				fmt.Fprintf(out, "blocked_on: %s\n", strings.Join(st.BlockedOn, ", "))
			}
			if len(st.LinkedThreads) > 0 {
				fmt.Fprintf(out, "linked_threads: %s\n", strings.Join(st.LinkedThreads, ", "))
			}
			fmt.Fprintf(out, "\n--- GOAL ---\n%s\n--- LOG ---\n%s\n", goal, log)
			return nil
		},
	}
}

// ---------- helpers ----------

func idsTasksRoot(stateRoot string) string {
	return ids.TasksRoot(stateRoot)
}

func commitTaskMutation(stateRoot, msg string, taskIDs ...string) error {
	r := gitops.Repo{Dir: stateRoot}
	for _, id := range taskIDs {
		if err := r.Add(fmt.Sprintf("tasks/%s", id)); err != nil {
			return errf(ExitGit, "%v", err)
		}
	}
	if _, err := r.Commit(msg); err != nil && !errors.Is(err, gitops.ErrNothingToCommit) {
		return errf(ExitGit, "%v", err)
	}
	return nil
}

func appendUnique(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// firstLine returns the first non-blank line of s with any leading
// Markdown header markers stripped, plus the "T-<n> — " prefix that
// CreateTask injects so listings stay terse.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "# "))
		if line == "" {
			continue
		}
		if idx := strings.Index(line, " — "); idx > 0 {
			return line[idx+len(" — "):]
		}
		return line
	}
	return ""
}
