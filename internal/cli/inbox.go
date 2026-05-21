package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/gitops"
	"github.com/victor-develop/advanced-tasker/internal/ids"
	"github.com/victor-develop/advanced-tasker/internal/inbox"
	"github.com/victor-develop/advanced-tasker/internal/state"
	"github.com/victor-develop/advanced-tasker/internal/store"
	"github.com/victor-develop/advanced-tasker/internal/threads"
)

// jsonMarshalIndent is the same as encoding/json.MarshalIndent — wrapped
// to make the inbox handler's use of it explicit.
func jsonMarshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// newInboxCmd implements `harness inbox ls|show`.
func newInboxCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{
		Use:   "inbox",
		Short: "Inspect inbox buckets",
	}
	var bucket string
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List inbox items, optionally filtered by --bucket",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			buckets := inbox.AllBuckets
			if bucket != "" {
				ok := false
				for _, b := range inbox.AllBuckets {
					if string(b) == bucket {
						buckets = []inbox.Bucket{b}
						ok = true
					}
				}
				if !ok {
					return errf(ExitUsage, "unknown bucket %q", bucket)
				}
			}
			for _, b := range buckets {
				names, err := inbox.List(root, b)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "── %s (%d) ──\n", b, len(names))
				for _, n := range names {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", n)
				}
			}
			return nil
		},
	}
	ls.Flags().StringVar(&bucket, "bucket", "", "new|updates|human|agent-reports|anomalies")

	show := &cobra.Command{
		Use:   "show <inbox-id>",
		Short: "Print the JSON body of an inbox item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			_, path, err := inbox.Find(root, args[0])
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		},
	}
	c.AddCommand(ls, show)
	return c
}

// newTriageCmd implements `harness triage <inbox-id> --action=...`.
func newTriageCmd(opts *Options) *cobra.Command {
	var action, thread, title, parent string
	c := &cobra.Command{
		Use:   "triage <inbox-id>",
		Short: "Apply a triage action to an inbox item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			if action == "" {
				return errf(ExitUsage, "--action is required (drop|track|attach|task)")
			}
			return state.WithStateLock(root, func() error {
				_, path, err := inbox.Find(root, id)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				switch action {
				case "drop":
					if opts.DryRun {
						fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would drop %s\n", id)
						return nil
					}
					if err := os.Remove(path); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "dropped %s\n", id)
					return nil
				case "track":
					it, err := inbox.ReadJSON(path)
					if err != nil {
						return errf(ExitValidation, "%v", err)
					}
					threadID := strings.TrimSuffix(strings.TrimSuffix(it.ID, ".json"), ".md")
					if it.ID == "" {
						threadID = strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".json"), ".md")
					}
					meta := &threads.Meta{
						ID:            threadID,
						Source:        it.Source,
						LastEventAt:   it.ReceivedAt,
						CreatedAt:     it.ReceivedAt,
						TrackingSince: time.Now().UTC(),
					}
					if opts.DryRun {
						fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would track %s\n", threadID)
						return nil
					}
					if err := threads.WriteMeta(root, meta); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					// Per design/02 §"Raw event location": promote raw_inline
					// (if present) into threads/<id>/raw/<event>.json so the
					// rollup updater can read it on the next pass.
					if it.RawInline != nil {
						rawBody, _ := jsonMarshalIndent(it.RawInline)
						eventName := fmt.Sprintf("promoted-%d.json", time.Now().UnixNano())
						rp := filepath.Join(threads.RawDir(root, threadID), eventName)
						if err := os.MkdirAll(filepath.Dir(rp), 0o755); err == nil {
							_ = os.WriteFile(rp, rawBody, 0o644)
						}
					}
					_ = threads.MarkDirty(root, threadID)
					_ = os.Remove(path)
					r := gitops.Repo{Dir: root}
					_ = r.Add(filepath.Join("threads", threadID))
					if _, err := r.Commit(fmt.Sprintf("triage %s: track %s", id, threadID)); err != nil && !errors.Is(err, gitops.ErrNothingToCommit) {
						return errf(ExitGit, "%v", err)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "tracked %s\n", threadID)
					return nil
				case "attach":
					if thread == "" {
						return errf(ExitUsage, "attach requires --thread <id>")
					}
					it, _ := inbox.ReadJSON(path)
					body := fmt.Sprintf("attached from inbox %s: %s", id, ifThen(it != nil, it.Summary))
					rawPath := filepath.Join(threads.RawDir(root, thread), fmt.Sprintf("attach-%d.json", time.Now().UnixNano()))
					if opts.DryRun {
						fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would attach %s to %s\n", id, thread)
						return nil
					}
					if err := os.MkdirAll(filepath.Dir(rawPath), 0o755); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					_ = os.WriteFile(rawPath, []byte(body), 0o644)
					_ = threads.MarkDirty(root, thread)
					_ = os.Remove(path)
					fmt.Fprintf(cmd.OutOrStdout(), "attached %s → %s\n", id, thread)
					return nil
				case "task":
					if title == "" {
						return errf(ExitUsage, "task requires --title \"...\"")
					}
					it, _ := inbox.ReadJSON(path)
					_ = it
					nextID, err := ids.NextTaskID(ids.TasksRoot(root))
					if err != nil {
						return errf(ExitValidation, "%v", err)
					}
					now := time.Now().UTC()
					stx := store.Status{
						ID:            nextID,
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
							return errf(ExitValidation, "%v", err)
						}
						stx.ParentGoal = pn
					}
					body := fmt.Sprintf("# %s — %s\n\nfrom inbox %s\n", nextID, title, id)
					if opts.DryRun {
						fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would create %s\n", nextID)
						return nil
					}
					if err := store.CreateTask(root, stx, body); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					_ = os.Remove(path)
					if err := commitTaskMutation(root, fmt.Sprintf("triage %s: create %s", id, nextID), nextID); err != nil {
						return err
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s\n", nextID)
					return nil
				default:
					return errf(ExitUsage, "unknown --action %q", action)
				}
			})
		},
	}
	c.Flags().StringVar(&action, "action", "", "drop|track|attach|task")
	c.Flags().StringVar(&thread, "thread", "", "(attach) target thread id")
	c.Flags().StringVar(&title, "title", "", "(task) new task title")
	c.Flags().StringVar(&parent, "parent", "", "(task) parent task id")
	return c
}

func ifThen(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}
