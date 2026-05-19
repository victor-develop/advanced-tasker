package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/gitops"
	"github.com/victor-develop/advanced-tasker/internal/ids"
	"github.com/victor-develop/advanced-tasker/internal/state"
	"github.com/victor-develop/advanced-tasker/internal/threads"
)

// newThreadCmd implements `harness thread track|untrack|show|link|ls`.
func newThreadCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "thread", Short: "Manage tracked threads"}
	c.AddCommand(
		&cobra.Command{
			Use:   "track <thread-id>",
			Short: "Promote an inbox/new item or freshly named thread",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id := args[0]
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				return state.WithStateLock(root, func() error {
					meta := &threads.Meta{
						ID:            id,
						Source:        sourceFromThreadID(id),
						CreatedAt:     time.Now().UTC(),
						TrackingSince: time.Now().UTC(),
					}
					if err := threads.WriteMeta(root, meta); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					r := gitops.Repo{Dir: root}
					_ = r.Add(filepath.Join("threads", id))
					if _, err := r.Commit(fmt.Sprintf("thread track %s", id)); err != nil && !errors.Is(err, gitops.ErrNothingToCommit) {
						return errf(ExitGit, "%v", err)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "tracked %s\n", id)
					return nil
				})
			},
		},
		&cobra.Command{
			Use:   "untrack <thread-id>",
			Short: "Stop tracking a thread (does not delete history by default)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				dir := threads.Dir(root, args[0])
				return os.RemoveAll(dir)
			},
		},
		&cobra.Command{
			Use:   "show <thread-id>",
			Short: "Show rollup + meta + recent raw",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if meta, err := threads.ReadMeta(root, args[0]); err == nil {
					fmt.Fprintf(out, "META: %s (source=%s, owner_task=%s, last_event=%s)\n", meta.ID, meta.Source, meta.OwnerTask, meta.LastEventAt.Format(time.RFC3339))
				}
				if body, err := threads.ReadRollup(root, args[0]); err == nil {
					fmt.Fprintln(out, "── ROLLUP ──")
					fmt.Fprint(out, body)
				}
				if events, _ := threads.RawEvents(root, args[0]); len(events) > 0 {
					fmt.Fprintln(out, "── RAW EVENTS ──")
					for _, e := range events {
						fmt.Fprintf(out, "  %s\n", e)
					}
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "link <thread-id> <T-n>",
			Short: "Set owner_task on the thread meta",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				taskID, err := ids.NormalizeTaskID(args[1])
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				meta, err := threads.ReadMeta(root, args[0])
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				meta.OwnerTask = taskID
				if err := threads.WriteMeta(root, meta); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				r := gitops.Repo{Dir: root}
				_ = r.Add(filepath.Join("threads", args[0], "meta.json"))
				if _, err := r.Commit(fmt.Sprintf("thread link %s → %s", args[0], taskID)); err != nil && !errors.Is(err, gitops.ErrNothingToCommit) {
					return errf(ExitGit, "%v", err)
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "ls",
			Short: "List tracked threads",
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				list, _ := threads.List(root)
				for _, id := range list {
					fmt.Fprintln(cmd.OutOrStdout(), id)
				}
				return nil
			},
		},
	)
	return c
}

func sourceFromThreadID(id string) string {
	if strings.HasPrefix(id, "slack-") {
		return "slack"
	}
	if strings.HasPrefix(id, "github-") {
		return "github"
	}
	return "unknown"
}
