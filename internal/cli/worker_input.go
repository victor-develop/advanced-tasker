package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/victor-develop/advanced-tasker/internal/jobs"
	"github.com/victor-develop/advanced-tasker/internal/store"
	"github.com/victor-develop/advanced-tasker/internal/threads"
)

// extends the existing newRenderCmd in render.go with the worker-input
// subcommand. We register it from the render command's constructor by
// calling AttachWorkerInput in the render command setup. To keep diffs
// small we add the subcommand here by mutating the cmd in init().
func init() {
	attachers = append(attachers, attachWorkerInput)
}

var attachers []func(opts *Options, render *cobra.Command)

// callAttachers wires extra subcommands into a render command.
func callAttachers(opts *Options, render *cobra.Command) {
	for _, fn := range attachers {
		fn(opts, render)
	}
}

func attachWorkerInput(opts *Options, render *cobra.Command) {
	render.AddCommand(&cobra.Command{
		Use:   "worker-input <J-id>",
		Short: "Render the worker prompt for a job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			st, path, err := jobs.FindJob(root, id)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			_ = st
			j, err := jobs.Read(path)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			out, err := renderWorkerInput(root, j)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			fmt.Fprint(cmd.OutOrStdout(), out)
			return nil
		},
	})

	render.AddCommand(&cobra.Command{
		Use:   "updater-input <thread-id>",
		Short: "Alias for `harness rollup render-input`",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			out, err := renderUpdaterInput(root, args[0])
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			fmt.Fprint(cmd.OutOrStdout(), out)
			return nil
		},
	})
}

// renderWorkerInput assembles the worker prompt per design/06.
func renderWorkerInput(root string, j *jobs.Job) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a %s worker for harness. Your job is to complete the task\n", j.Role)
	b.WriteString("described below and submit a structured report via:\n\n")
	fmt.Fprintf(&b, "  harness submit-report %s --file=report.yaml\n\n", j.ID)
	b.WriteString("You may NOT call other `harness` verbs that mutate state — your only\n")
	b.WriteString("side effect is `submit-report`.\n\n")
	b.WriteString("────────────────────────────────────────────────────────────────────────\n")
	b.WriteString("INSTRUCTION (from commander):\n")
	b.WriteString(strings.TrimSpace(j.Instruction))
	b.WriteString("\n\n")
	if len(j.Expects.OutcomeEnum) > 0 {
		fmt.Fprintf(&b, "REQUIRED OUTCOME — must be one of: %v\n\n", j.Expects.OutcomeEnum)
	}
	b.WriteString("────────────────────────────────────────────────────────────────────────\n")
	b.WriteString("ROLE SYSTEM PROMPT:\n")
	rolePath := filepath.Join(root, "roles", j.Role+".md")
	if rb, err := os.ReadFile(rolePath); err == nil {
		b.Write(rb)
		b.WriteString("\n")
	} else {
		fmt.Fprintf(&b, "(no role file at %s)\n", rolePath)
	}
	b.WriteString("\n────────────────────────────────────────────────────────────────────────\n")
	b.WriteString("CONTEXT (whitelisted):\n\n")

	if len(j.Context.Rollups) > 0 {
		b.WriteString("Thread rollups in scope:\n")
		for _, rid := range j.Context.Rollups {
			if body, err := threads.ReadRollup(root, rid); err == nil {
				fmt.Fprintf(&b, "--- %s/rollup.md ---\n%s\n", rid, body)
			} else {
				fmt.Fprintf(&b, "(%s: rollup not found)\n", rid)
			}
		}
	}
	if len(j.Context.Tasks) > 0 {
		b.WriteString("\nLinked tasks:\n")
		for _, tid := range j.Context.Tasks {
			if goal, err := store.GoalBody(root, tid); err == nil {
				fmt.Fprintf(&b, "--- %s/goal.md ---\n%s\n", tid, goal)
			}
			if stx, err := store.ReadStatus(root, tid); err == nil {
				out, _ := yaml.Marshal(stx)
				fmt.Fprintf(&b, "--- %s/status ---\n%s", tid, out)
			}
			if log, err := store.LogBody(root, tid); err == nil {
				fmt.Fprintf(&b, "--- %s/log.md ---\n%s\n", tid, log)
			}
		}
	}
	if len(j.Context.Files) > 0 {
		b.WriteString("\nFiles in scope:\n")
		for _, fp := range j.Context.Files {
			abs := filepath.Join(root, fp)
			if !strings.HasPrefix(abs, root) {
				continue
			}
			if data, err := os.ReadFile(abs); err == nil {
				fmt.Fprintf(&b, "--- %s ---\n%s\n", fp, data)
			} else {
				fmt.Fprintf(&b, "(%s: %v)\n", fp, err)
			}
		}
	}
	if len(j.Context.PriorReports) > 0 {
		b.WriteString("\nPrior worker reports for this task:\n")
		for _, pj := range j.Context.PriorReports {
			if _, p, err := jobs.FindJob(root, pj); err == nil {
				if jb, err := os.ReadFile(p); err == nil {
					fmt.Fprintf(&b, "--- %s ---\n%s\n", pj, jb)
				}
			}
		}
	}

	b.WriteString("\n────────────────────────────────────────────────────────────────────────\n")
	b.WriteString("REPORT SCHEMA: see design/02 + design/06. tldr ≤200 chars, next[]\n")
	b.WriteString("actions limited to the documented enum.\n")
	return b.String(), nil
}
