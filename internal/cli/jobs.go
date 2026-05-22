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
	"gopkg.in/yaml.v3"

	"github.com/victor-develop/advanced-tasker/internal/gitops"
	"github.com/victor-develop/advanced-tasker/internal/ids"
	"github.com/victor-develop/advanced-tasker/internal/inbox"
	"github.com/victor-develop/advanced-tasker/internal/jobs"
	"github.com/victor-develop/advanced-tasker/internal/outbox"
	"github.com/victor-develop/advanced-tasker/internal/state"
	"github.com/victor-develop/advanced-tasker/internal/store"
	"github.com/victor-develop/advanced-tasker/internal/threads"
)

// newDispatchCmd implements `harness dispatch T-<n> --role=<role> ...`.
func newDispatchCmd(opts *Options) *cobra.Command {
	var role, input, timeout, priority string
	var rollups, files []string
	c := &cobra.Command{
		Use:   "dispatch <T-n>",
		Short: "Queue a worker job under jobs/pending/",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			taskArg := args[0]
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			if role == "" {
				return errf(ExitUsage, "--role is required")
			}
			taskID, err := ids.NormalizeTaskID(taskArg)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			if _, err := store.ReadStatus(root, taskID); err != nil {
				return errf(ExitValidation, "%v", err)
			}
			return state.WithStateLock(root, func() error {
				id, err := ids.NewJobID()
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				instr := ""
				if input != "" {
					b, err := os.ReadFile(input)
					if err != nil {
						return errf(ExitUsage, "read --input: %v", err)
					}
					instr = string(b)
				}
				if instr == "" {
					instr = fmt.Sprintf("Work on %s as a %s.", taskID, role)
				}
				if len(instr) > 500 {
					return errf(ExitValidation, "instruction must be ≤500 chars (got %d)", len(instr))
				}
				j := &jobs.Job{
					ID:          id,
					CreatedAt:   time.Now().UTC(),
					CreatedBy:   "commander",
					Role:        role,
					TaskID:      taskID,
					Priority:    valueOr(priority, "normal"),
					Timeout:     valueOr(timeout, "30m"),
					Instruction: instr,
					Context: jobs.Context{
						Rollups:      rollups,
						Tasks:        []string{taskID},
						Files:        files,
						PriorReports: []string{},
					},
					Expects: jobs.Expects{
						OutcomeEnum: defaultOutcomeEnum(role),
					},
				}
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would dispatch %s for %s\n", id, taskID)
					return nil
				}
				dst := jobs.PathFor(root, jobs.StatePending, id)
				if err := jobs.Write(dst, j); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				_ = store.AppendLog(root, taskID, "cli", fmt.Sprintf("dispatched %s role=%s", id, role))
				fmt.Fprintln(cmd.OutOrStdout(), id)
				return nil
			})
		},
	}
	c.Flags().StringVar(&role, "role", "", "worker role (must match roles/<role>.md)")
	c.Flags().StringVar(&input, "input", "", "instruction file (≤500 chars)")
	c.Flags().StringSliceVar(&rollups, "rollups", nil, "rollups in scope (thread IDs)")
	c.Flags().StringSliceVar(&files, "files", nil, "files in scope (relative paths)")
	c.Flags().StringVar(&timeout, "timeout", "30m", "job timeout (Go duration)")
	c.Flags().StringVar(&priority, "priority", "normal", "low|normal|high")
	return c
}

// defaultOutcomeEnum returns a sensible default outcome set per role.
// Roles can override by passing custom expects via a future flag.
func defaultOutcomeEnum(role string) []string {
	switch role {
	case "pr-reviewer":
		return []string{"approve", "request-changes", "blocked"}
	case "slack-drafter":
		return []string{"draft", "blocked"}
	case "planner":
		return []string{"decomposed", "needs-input"}
	case "researcher":
		return []string{"found", "inconclusive"}
	case "summarizer":
		return []string{"updated"}
	}
	return []string{"done", "blocked"}
}

// newJobCmd implements `harness job ls|show|cancel`.
func newJobCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "job", Short: "Inspect / cancel worker jobs"}
	var stateFilter string
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List jobs (optionally filtered)",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			states := jobs.AllStates
			if stateFilter != "" {
				states = []jobs.State{jobs.State(stateFilter)}
			}
			for _, st := range states {
				names, err := jobs.ListByState(root, st)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "── %s (%d) ──\n", st, len(names))
				for _, n := range names {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", n)
				}
			}
			return nil
		},
	}
	ls.Flags().StringVar(&stateFilter, "state", "", "pending|in-flight|done|failed")
	show := &cobra.Command{
		Use:   "show <J-id>",
		Short: "Print the job YAML",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			_, path, err := jobs.FindJob(root, args[0])
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			b, _ := os.ReadFile(path)
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		},
	}
	cancel := &cobra.Command{
		Use:   "cancel <J-id>",
		Short: "Move a job to failed/",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			return state.WithStateLock(root, func() error {
				st, path, err := jobs.FindJob(root, args[0])
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				j, err := jobs.Read(path)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if _, err := jobs.Move(root, j, st, jobs.StateFailed); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "cancelled %s\n", args[0])
				return nil
			})
		},
	}
	c.AddCommand(ls, show, cancel)
	return c
}

// newSubmitReportCmd implements `harness submit-report J-<id> --file=<path>`.
func newSubmitReportCmd(opts *Options) *cobra.Command {
	var file string
	c := &cobra.Command{
		Use:   "submit-report <J-id>",
		Short: "Validate and accept a worker's report YAML",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			if file == "" {
				return errf(ExitUsage, "--file required (use - for stdin)")
			}
			body, err := readFileOrStdin(file, cmd.InOrStdin())
			if err != nil {
				return errf(ExitUsage, "%v", err)
			}
			var rpt jobs.Report
			if err := yaml.Unmarshal(body, &rpt); err != nil {
				return errf(ExitValidation, "parse report: %v", err)
			}
			if rpt.JobID == "" {
				rpt.JobID = jobID
			}
			if rpt.FinishedAt.IsZero() {
				rpt.FinishedAt = time.Now().UTC()
			}
			return state.WithStateLock(root, func() error {
				st, path, err := jobs.FindJob(root, jobID)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				j, err := jobs.Read(path)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if err := jobs.ValidateReport(j, &rpt); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				j.Report = &rpt
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would submit-report %s\n", jobID)
					return nil
				}
				if _, err := jobs.Move(root, j, st, jobs.StateDone); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				// Drop the agent-report signal.
				signal := &inbox.Item{
					ID:         jobID,
					Source:     "harness",
					Kind:       "agent-report",
					ReceivedAt: rpt.FinishedAt,
					Summary:    strings.TrimSpace(firstLineOf(rpt.TLDR)),
				}
				_ = inbox.WriteJSON(filepath.Join(root, "inbox", "agent-reports", jobID+".json"), signal)
				_ = store.AppendLog(root, j.TaskID, "worker", fmt.Sprintf("submit-report %s: outcome=%s", jobID, rpt.Outcome))
				fmt.Fprintf(cmd.OutOrStdout(), "accepted %s\n", jobID)
				return nil
			})
		},
	}
	c.Flags().StringVar(&file, "file", "", "report YAML path (or -)")
	return c
}

// readFileOrStdin reads from a file path; `-` means stdin.
func readFileOrStdin(p string, stdin io.Reader) ([]byte, error) {
	if p == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(p)
}

func firstLineOf(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// newReviewCmd implements `harness review J-<id> --action=accept|reject ...`.
func newReviewCmd(opts *Options) *cobra.Command {
	var action, only, reason, raiseRisk string
	c := &cobra.Command{
		Use:   "review <J-id>",
		Short: "Accept or reject a worker report and execute next[] actions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			if action != "accept" && action != "reject" {
				return errf(ExitUsage, "--action must be accept|reject")
			}
			return state.WithStateLock(root, func() error {
				st, path, err := jobs.FindJob(root, jobID)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if st != jobs.StateDone {
					return errf(ExitValidation, "job %s is in %s, must be in done/ to review", jobID, st)
				}
				j, err := jobs.Read(path)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if j.Report == nil {
					return errf(ExitValidation, "job %s has no report", jobID)
				}
				if action == "reject" {
					j.Report.Details += fmt.Sprintf("\n[review rejected: %s]\n", reason)
					if err := jobs.Write(path, j); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "rejected %s\n", jobID)
					return nil
				}
				// Risk override: commander may RAISE risk on outbox.send
				// items but NEVER lower it (design/07 §"Commander cannot
				// downgrade"). raiseRisk="high" only — for granular
				// per-index overrides, edit the report and resubmit.
				if raiseRisk != "" {
					if !outboxRiskValid(raiseRisk) {
						return errf(ExitValidation, "--risk: invalid %q", raiseRisk)
					}
					newRank := outboxRiskRank(raiseRisk)
					for i, n := range j.Report.Next {
						if n.Action != "outbox.send" {
							continue
						}
						if outboxRiskRank(n.Risk) > newRank {
							return errf(ExitValidation, "review cannot downgrade risk of next[%d] (was %s, --risk=%s)", i, n.Risk, raiseRisk)
						}
						j.Report.Next[i].Risk = raiseRisk
					}
				}
				picks := parseIndices(only, len(j.Report.Next))
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would accept %s (next[]=%v)\n", jobID, picks)
					return nil
				}
				out := cmd.OutOrStdout()
				for _, i := range picks {
					if i < 0 || i >= len(j.Report.Next) {
						continue
					}
					n := j.Report.Next[i]
					if err := executeNext(root, j, n); err != nil {
						return errf(ExitValidation, "next[%d] (%s): %v", i, n.Action, err)
					}
					fmt.Fprintf(out, "executed next[%d] %s\n", i, n.Action)
				}
				_ = store.AppendLog(root, j.TaskID, "cli", fmt.Sprintf("review %s accepted next=%v", jobID, picks))
				fmt.Fprintf(out, "accepted %s\n", jobID)
				return nil
			})
		},
	}
	c.Flags().StringVar(&action, "action", "", "accept|reject")
	c.Flags().StringVar(&only, "only", "", "comma-separated indices (default: all)")
	c.Flags().StringVar(&reason, "reason", "", "rejection reason")
	c.Flags().StringVar(&raiseRisk, "risk", "", "raise all outbox.send risks to low|normal|high (cannot downgrade)")
	return c
}

func outboxRiskValid(r string) bool {
	return r == "low" || r == "normal" || r == "high"
}

func outboxRiskRank(r string) int {
	switch r {
	case "low":
		return 0
	case "normal":
		return 1
	case "high":
		return 2
	}
	return -1
}

func parseIndices(s string, n int) []int {
	if s == "" {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := []int{}
	for _, p := range strings.Split(s, ",") {
		var v int
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%d", &v); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// executeNext runs one next[] action per design/06 §"Safety policy".
func executeNext(root string, j *jobs.Job, n jobs.Next) error {
	switch n.Action {
	case "task.update":
		id, _ := n.Args["id"].(string)
		idn, err := ids.NormalizeTaskID(id)
		if err != nil {
			return err
		}
		stx, err := store.ReadStatus(root, idn)
		if err != nil {
			return err
		}
		if v, ok := n.Args["state"].(string); ok {
			stx.State = store.TaskState(v)
		}
		if v, ok := n.Args["priority"].(string); ok {
			stx.Priority = store.Priority(v)
		}
		stx.UpdatedAt = time.Now().UTC()
		if err := store.WriteStatus(root, stx); err != nil {
			return err
		}
		_ = store.AppendLog(root, idn, "review", fmt.Sprintf("update via %s next[]", j.ID))
		return commitTaskMutation(root, fmt.Sprintf("review %s: task.update %s", j.ID, idn), idn)
	case "task.create":
		title, _ := n.Args["title"].(string)
		nextID, err := ids.NextTaskID(ids.TasksRoot(root))
		if err != nil {
			return err
		}
		stx := store.Status{
			ID: nextID, State: store.StateReady, Priority: store.PriorityNormal,
			BlockedOn: []string{}, LinkedThreads: []string{},
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if v, ok := n.Args["parent"].(string); ok {
			p, err := ids.NormalizeTaskID(v)
			if err != nil {
				return err
			}
			stx.ParentGoal = p
		}
		body := fmt.Sprintf("# %s — %s\n\n%s (from review of %s)\n", nextID, title, title, j.ID)
		if err := store.CreateTask(root, stx, body); err != nil {
			return err
		}
		return commitTaskMutation(root, fmt.Sprintf("review %s: task.create %s", j.ID, nextID), nextID)
	case "task.kill":
		id, _ := n.Args["id"].(string)
		reason, _ := n.Args["reason"].(string)
		idn, err := ids.NormalizeTaskID(id)
		if err != nil {
			return err
		}
		stx, err := store.ReadStatus(root, idn)
		if err != nil {
			return err
		}
		stx.State = store.StateKilled
		stx.KilledReason = reason
		stx.UpdatedAt = time.Now().UTC()
		if err := store.WriteStatus(root, stx); err != nil {
			return err
		}
		_ = store.AppendLog(root, idn, "review", fmt.Sprintf("kill via %s: %s", j.ID, reason))
		return commitTaskMutation(root, fmt.Sprintf("review %s: task.kill %s", j.ID, idn), idn)
	case "task.defer":
		id, _ := n.Args["id"].(string)
		reason, _ := n.Args["reason"].(string)
		idn, err := ids.NormalizeTaskID(id)
		if err != nil {
			return err
		}
		stx, err := store.ReadStatus(root, idn)
		if err != nil {
			return err
		}
		stx.State = store.StateDeferred
		stx.DeferredReason = reason
		stx.UpdatedAt = time.Now().UTC()
		if err := store.WriteStatus(root, stx); err != nil {
			return err
		}
		return commitTaskMutation(root, fmt.Sprintf("review %s: task.defer %s", j.ID, idn), idn)
	case "task.link":
		from, _ := n.Args["from"].(string)
		to, _ := n.Args["to"].(string)
		fromN, err := ids.NormalizeTaskID(from)
		if err != nil {
			return err
		}
		toN, err := ids.NormalizeTaskID(to)
		if err != nil {
			return err
		}
		stx, err := store.ReadStatus(root, fromN)
		if err != nil {
			return err
		}
		if store.AddBlockedOn(stx, toN) {
			if err := store.WriteStatus(root, stx); err != nil {
				return err
			}
		}
		return commitTaskMutation(root, fmt.Sprintf("review %s: task.link %s→%s", j.ID, fromN, toN), fromN)
	case "task.unlink":
		from, _ := n.Args["from"].(string)
		to, _ := n.Args["to"].(string)
		fromN, _ := ids.NormalizeTaskID(from)
		toN, _ := ids.NormalizeTaskID(to)
		stx, err := store.ReadStatus(root, fromN)
		if err != nil {
			return err
		}
		store.RemoveBlockedOn(stx, toN)
		if err := store.WriteStatus(root, stx); err != nil {
			return err
		}
		return commitTaskMutation(root, fmt.Sprintf("review %s: task.unlink %s↛%s", j.ID, fromN, toN), fromN)
	case "rollup.note":
		thread, _ := n.Args["thread"].(string)
		verbatim, _ := n.Args["verbatim"].(string)
		body, _ := threads.ReadRollup(root, thread)
		pin := fmt.Sprintf("> %s (— pinned by human)\n", strings.TrimSpace(verbatim))
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body += pin
		if err := threads.WriteRollup(root, thread, body); err != nil {
			return err
		}
		r := gitops.Repo{Dir: root}
		_ = r.Add(filepath.Join("threads", thread, "rollup.md"))
		_, err := r.Commit(fmt.Sprintf("rollup %s: pin via review %s", thread, j.ID))
		if err != nil && !errors.Is(err, gitops.ErrNothingToCommit) {
			return err
		}
		return nil
	case "outbox.send":
		thread, _ := n.Args["thread"].(string)
		bodyFile, _ := n.Args["body_file"].(string)
		risk := outbox.Risk(n.Risk)
		// Commander cannot downgrade risk vs the worker's stated risk
		// — but that check only matters if a review explicitly set
		// a lower risk than the worker. Here we accept worker's risk
		// at face value (already set on the Next entry).
		bodyText, err := readBodyForOutbox(root, j.TaskID, bodyFile)
		if err != nil {
			return err
		}
		oid, err := ids.NewOutboxID()
		if err != nil {
			return err
		}
		it := &outbox.Item{
			ID:           oid,
			CreatedAt:    time.Now().UTC(),
			CreatedBy:    j.ID,
			To:           outboxToFromThread(thread),
			Ref:          outbox.Ref{Thread: thread},
			BodyFile:     bodyFile,
			Body:         bodyText,
			Risk:         risk,
			RevokeWindow: "5m",
		}
		dest := outbox.StatePending
		if risk == outbox.RiskNormal || risk == outbox.RiskHigh {
			dest = outbox.StateAwaitingHuman
		}
		dst := outbox.PathFor(root, dest, oid)
		if err := outbox.Write(dst, it); err != nil {
			return err
		}
		return nil
	case "dispatch":
		taskRaw, _ := n.Args["task"].(string)
		role, _ := n.Args["role"].(string)
		instr, _ := n.Args["instruction"].(string)
		taskN, err := ids.NormalizeTaskID(taskRaw)
		if err != nil {
			return err
		}
		nid, err := ids.NewJobID()
		if err != nil {
			return err
		}
		nj := &jobs.Job{
			ID:          nid,
			CreatedAt:   time.Now().UTC(),
			CreatedBy:   j.ID,
			Role:        role,
			TaskID:      taskN,
			Instruction: trunc(instr, 500),
			Priority:    "normal",
			Timeout:     "30m",
			Context:     jobs.Context{Tasks: []string{taskN}, Rollups: []string{}, Files: []string{}, PriorReports: []string{j.ID}},
			Expects:     jobs.Expects{OutcomeEnum: defaultOutcomeEnum(role)},
		}
		return jobs.Write(jobs.PathFor(root, jobs.StatePending, nid), nj)
	case "ask-human":
		question, _ := n.Args["question"].(string)
		dir := filepath.Join(root, "inbox", "human")
		_ = os.MkdirAll(dir, 0o755)
		nm := fmt.Sprintf("ask-%d-%s.md", time.Now().UnixNano(), j.ID)
		body := fmt.Sprintf("---\npriority: now\nscope: global\ncreated_at: %s\ncreated_by: %s\n---\n%s\n", time.Now().UTC().Format(time.RFC3339), j.ID, question)
		return os.WriteFile(filepath.Join(dir, nm), []byte(body), 0o644)
	}
	return fmt.Errorf("unknown action %q", n.Action)
}

func outboxToFromThread(thread string) string {
	if strings.HasPrefix(thread, "slack-") {
		return "slack"
	}
	if strings.HasPrefix(thread, "github-") {
		return "github-pr-comment"
	}
	return "unknown"
}

func readBodyForOutbox(root, taskID, bodyFile string) (string, error) {
	candidate := filepath.Join(root, "tasks", taskID, "artifacts", filepath.Base(bodyFile))
	if !strings.Contains(bodyFile, string(filepath.Separator)) {
		b, err := os.ReadFile(candidate)
		if err == nil {
			return string(b), nil
		}
	}
	b, err := os.ReadFile(filepath.Join(root, bodyFile))
	if err == nil {
		return string(b), nil
	}
	// Last resort — empty body is allowed; sender will use body_file lookup.
	return "", nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func valueOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
