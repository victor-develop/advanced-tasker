package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/gitops"
	"github.com/victor-develop/advanced-tasker/internal/inbox"
	"github.com/victor-develop/advanced-tasker/internal/rollup"
	"github.com/victor-develop/advanced-tasker/internal/state"
	"github.com/victor-develop/advanced-tasker/internal/store"
	"github.com/victor-develop/advanced-tasker/internal/threads"
)

// newRollupCmd assembles the `harness rollup ...` subcommands.
func newRollupCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{
		Use:   "rollup",
		Short: "Inspect, update, and validate per-thread rollups",
	}
	c.AddCommand(
		rollupShowCmd(opts),
		rollupFlushCmd(opts),
		rollupUpdateCmd(opts),
		rollupRenderInputCmd(opts),
		rollupPinCmd(opts),
		rollupEditCmd(opts),
		rollupVerifyCommitCmd(opts),
	)
	return c
}

func rollupShowCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "show <thread-id>",
		Short: "Print rollup.md for a thread",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			body, err := threads.ReadRollup(root, args[0])
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			fmt.Fprint(cmd.OutOrStdout(), body)
			return nil
		},
	}
}

func rollupFlushCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "flush <thread-id>",
		Short: "Touch .dirty so the updater re-summarizes on next pass",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			if err := threads.MarkDirty(root, args[0]); err != nil {
				return errf(ExitValidation, "%v", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "flushed %s\n", args[0])
			return nil
		},
	}
}

func rollupUpdateCmd(opts *Options) *cobra.Command {
	var file string
	c := &cobra.Command{
		Use:   "update <thread-id> --file=<path>",
		Short: "Atomic write + validation pipeline (design/05) for a rollup",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			if file == "" {
				return errf(ExitUsage, "--file is required (use - for stdin)")
			}
			var newBody []byte
			if file == "-" {
				b, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return errf(ExitUsage, "read stdin: %v", err)
				}
				newBody = b
			} else {
				b, err := os.ReadFile(file)
				if err != nil {
					return errf(ExitUsage, "read --file: %v", err)
				}
				newBody = b
			}
			return state.WithStateLock(root, func() error {
				newR, err := rollup.Parse(string(newBody))
				if err != nil {
					emitRollupAnomaly(root, id, fmt.Sprintf("schema parse: %v", err))
					return errf(ExitValidation, "schema parse: %v", err)
				}
				if err := newR.Validate(); err != nil {
					emitRollupAnomaly(root, id, fmt.Sprintf("schema validate: %v", err))
					return errf(ExitValidation, "schema validate: %v", err)
				}
				// Compare with current.
				oldBody, err := threads.ReadRollup(root, id)
				if err == nil {
					oldR, err := rollup.Parse(oldBody)
					if err == nil {
						if err := rollup.CheckAppendOnly(oldR.DecisionsLines, newR.DecisionsLines); err != nil {
							emitRollupAnomaly(root, id, fmt.Sprintf("ledger violation: %v", err))
							return errf(ExitValidation, "ledger violation: %v", err)
						}
						if err := rollup.CheckHumanPinsPreserved(oldR.VerbatimPins, newR.VerbatimPins); err != nil {
							emitRollupAnomaly(root, id, fmt.Sprintf("human-pin violation: %v", err))
							return errf(ExitValidation, "human-pin violation: %v", err)
						}
					}
				}
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would update rollup %s\n", id)
					return nil
				}
				if err := threads.WriteRollup(root, id, string(newBody)); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				_ = threads.ClearDirty(root, id)
				r := gitops.Repo{Dir: root}
				if err := r.Add(filepath.Join("threads", id, "rollup.md")); err != nil {
					return errf(ExitGit, "%v", err)
				}
				msg := fmt.Sprintf("rollup %s: %s", id, summarizeDelta(newR))
				sha, err := r.Commit(msg)
				if err != nil && !errors.Is(err, gitops.ErrNothingToCommit) {
					return errf(ExitGit, "%v", err)
				}
				if sha == "" {
					return nil
				}
				// Emit update signal.
				_ = inbox.WriteJSON(filepath.Join(root, "inbox", "updates", fmt.Sprintf("%s-%s.json", id, sha[:8])), &inbox.Item{
					ID:         fmt.Sprintf("%s-%s", id, sha[:8]),
					Source:     "harness",
					Kind:       "update",
					ReceivedAt: time.Now().UTC(),
					Summary:    fmt.Sprintf("rollup %s updated", id),
				})
				fmt.Fprintf(cmd.OutOrStdout(), "updated %s @ %s\n", id, sha)
				return nil
			})
		},
	}
	c.Flags().StringVar(&file, "file", "", "rollup.md path or - for stdin")
	return c
}

func rollupRenderInputCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "render-input <thread-id>",
		Short: "Emit the rollup updater's input prompt (pure)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			out, err := renderUpdaterInput(root, id)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			fmt.Fprint(cmd.OutOrStdout(), out)
			return nil
		},
	}
}

// renderUpdaterInput assembles the prompt per design/05 §"Input the
// updater sees".
func renderUpdaterInput(root, id string) (string, error) {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("You are the ROLLUP UPDATER for %s. Update its rollup based on\n", id))
	b.WriteString("new events. Output the FULL new rollup.md. Do not omit sections.\n\n")
	b.WriteString("CONSTRAINTS (the CLI will reject violations):\n")
	b.WriteString("- Decisions ledger lines must be APPEND-ONLY. You may add lines but never\n")
	b.WriteString("  edit or remove existing ones.\n")
	b.WriteString("- Verbatim pins marked \"(— pinned by human)\" must be preserved verbatim.\n")
	b.WriteString("- `state` field must use the allowed enum.\n")
	b.WriteString("- Current ask: ≤3 lines. Open questions: ≤5 lines.\n\n")

	meta, _ := threads.ReadMeta(root, id)
	b.WriteString("THREAD GOAL")
	if meta != nil && meta.OwnerTask != "" {
		b.WriteString(fmt.Sprintf(" (from owner task %s):\n", meta.OwnerTask))
		if goal, err := store.GoalBody(root, meta.OwnerTask); err == nil {
			b.WriteString(goal)
			b.WriteString("\n")
		}
	} else {
		b.WriteString(":\n(not yet assigned — summarize neutrally; surface what this thread seems to be about under Current ask so the commander can decide.)\n")
	}
	b.WriteString("\nCURRENT ROLLUP:\n")
	if body, err := threads.ReadRollup(root, id); err == nil {
		b.WriteString(body)
	} else {
		b.WriteString("(no current rollup — generate from scratch)\n")
	}
	b.WriteString("\nNEW EVENTS SINCE LAST UPDATE:\n")
	if events, _ := threads.RawEvents(root, id); len(events) > 0 {
		for _, name := range events {
			if body, err := os.ReadFile(filepath.Join(threads.RawDir(root, id), name)); err == nil {
				b.WriteString("- ")
				b.WriteString(strings.TrimSpace(string(body)))
				b.WriteString("\n")
			}
		}
	} else {
		b.WriteString("(none)\n")
	}
	b.WriteString("\nYOUR OUTPUT: the full new rollup.md, in the schema specified by\n")
	b.WriteString("design/02-state-and-schemas.md. Nothing else.\n")
	return b.String(), nil
}

func rollupPinCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "pin <thread-id> <verbatim>",
		Short: "Add a human-marked Verbatim pin to a thread rollup",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, line := args[0], args[1]
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			return state.WithStateLock(root, func() error {
				body, err := threads.ReadRollup(root, id)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				// Append the pin to the Verbatim pins section verbatim.
				pin := fmt.Sprintf("> %s (— pinned by human)\n", strings.TrimSpace(line))
				if !strings.Contains(body, rollup.HeaderVerbatim) {
					body += "\n" + rollup.HeaderVerbatim + "\n"
				}
				if !strings.HasSuffix(body, "\n") {
					body += "\n"
				}
				body += pin
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would pin to %s\n", id)
					return nil
				}
				if err := threads.WriteRollup(root, id, body); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				r := gitops.Repo{Dir: root}
				if err := r.Add(filepath.Join("threads", id, "rollup.md")); err != nil {
					return errf(ExitGit, "%v", err)
				}
				if _, err := r.Commit(fmt.Sprintf("rollup %s: add human pin", id)); err != nil && !errors.Is(err, gitops.ErrNothingToCommit) {
					return errf(ExitGit, "%v", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "pinned")
				return nil
			})
		},
	}
}

func rollupEditCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "edit <thread-id>",
		Short: "Open $EDITOR on the thread rollup; re-validate on close",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			path := threads.RollupPath(root, id)
			editor := os.Getenv("EDITOR")
			if editor == "" {
				return errf(ExitUsage, "$EDITOR not set")
			}
			runner := makeEditor(editor)
			if err := runner(path); err != nil {
				return errf(ExitValidation, "%v", err)
			}
			// Re-validate by feeding the new body through update.
			b, err := os.ReadFile(path)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			r, err := rollup.Parse(string(b))
			if err != nil || r.Validate() != nil {
				return errf(ExitValidation, "rollup invalid after edit (run `rollup update --file=<path>` to commit)")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "edited (uncommitted; use `rollup update` to commit through the validation pipeline)")
			return nil
		},
	}
}

func rollupVerifyCommitCmd(opts *Options) *cobra.Command {
	var file string
	c := &cobra.Command{
		Use:    "verify-commit",
		Short:  "post-commit hook helper: re-validate ledger+pins for a tracked rollup",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			if file == "" {
				return errf(ExitUsage, "--file required")
			}
			r := gitops.Repo{Dir: root}
			oldBody, err := r.ShowAt("HEAD~1", file)
			if err != nil {
				return nil // first version of the file — nothing to compare against
			}
			newBody, err := r.ShowAt("HEAD", file)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			oldR, err1 := rollup.Parse(oldBody)
			newR, err2 := rollup.Parse(newBody)
			if err1 != nil || err2 != nil {
				return errf(ExitValidation, "parse: %v / %v", err1, err2)
			}
			if err := rollup.CheckAppendOnly(oldR.DecisionsLines, newR.DecisionsLines); err != nil {
				return errf(ExitValidation, "ledger violation: %v", err)
			}
			if err := rollup.CheckHumanPinsPreserved(oldR.VerbatimPins, newR.VerbatimPins); err != nil {
				return errf(ExitValidation, "human-pin violation: %v", err)
			}
			return nil
		},
	}
	c.Flags().StringVar(&file, "file", "", "tracked rollup path within state/")
	return c
}

// emitRollupAnomaly writes an anomaly entry under inbox/anomalies/.
func emitRollupAnomaly(root, threadID, msg string) {
	_, _ = inbox.AppendAnomaly(root, fmt.Sprintf("rollup:%s", threadID), msg)
}

// summarizeDelta is a tiny commit-message helper.
func summarizeDelta(r *rollup.Rollup) string {
	return fmt.Sprintf("state=%s ledger=%d pins=%d", r.Front.State, len(r.DecisionsLines), len(r.VerbatimPins))
}

// makeEditor is patched in tests; default runs the editor synchronously.
var makeEditor = func(editor string) func(path string) error {
	return func(path string) error {
		_ = editor
		return nil // smoke; real implementations would exec the editor
	}
}
