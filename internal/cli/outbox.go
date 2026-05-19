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
	"github.com/victor-develop/advanced-tasker/internal/ids"
	"github.com/victor-develop/advanced-tasker/internal/outbox"
	"github.com/victor-develop/advanced-tasker/internal/state"
)

// newOutboxCmd implements `harness outbox send|approve|revoke|reject|ls|flush`.
func newOutboxCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "outbox", Short: "Inspect and manage the outbox"}
	c.AddCommand(
		outboxSendCmd(opts),
		outboxApproveCmd(opts),
		outboxRejectCmd(opts),
		outboxRevokeCmd(opts),
		outboxLsCmd(opts),
		outboxFlushCmd(opts),
	)
	return c
}

func outboxSendCmd(opts *Options) *cobra.Command {
	var to, thread, bodyArg, risk, inReplyTo string
	c := &cobra.Command{
		Use:   "send",
		Short: "Enqueue an outbox item (risk-classified)",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			if to == "" || thread == "" || risk == "" {
				return errf(ExitUsage, "--to, --thread, --risk are required")
			}
			if !outbox.IsValidRisk(risk) {
				return errf(ExitValidation, "invalid --risk %q (want low|normal|high)", risk)
			}
			body, err := readBodyArg(bodyArg, cmd.InOrStdin())
			if err != nil {
				return errf(ExitUsage, "%v", err)
			}
			return state.WithStateLock(root, func() error {
				id, err := ids.NewOutboxID()
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				lim, _ := outbox.LoadLimits(root)
				it := &outbox.Item{
					ID:           id,
					CreatedAt:    time.Now().UTC(),
					CreatedBy:    valueOr(os.Getenv("HARNESS_AGENT_ID"), "commander"),
					To:           to,
					Ref:          outbox.Ref{Thread: thread, InReplyTo: inReplyTo},
					BodyFile:     bodyArg,
					Body:         body,
					Risk:         outbox.Risk(risk),
					RevokeWindow: lim.RevokeWindow.String(),
				}
				// Anti-spam: refuse duplicate within 10 minutes.
				if err := outbox.DuplicateCheck(root, it, time.Now().UTC()); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				dest := outbox.StatePending
				if it.Risk != outbox.RiskLow {
					dest = outbox.StateAwaitingHuman
				}
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would queue %s to outbox/%s\n", id, dest)
					return nil
				}
				if err := outbox.Write(outbox.PathFor(root, dest, id), it); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), id)
				return nil
			})
		},
	}
	c.Flags().StringVar(&to, "to", "", "channel: slack|github-pr-comment|github-pr-review")
	c.Flags().StringVar(&thread, "thread", "", "thread id")
	c.Flags().StringVar(&bodyArg, "body", "", "body file path or - for stdin")
	c.Flags().StringVar(&risk, "risk", "", "low|normal|high")
	c.Flags().StringVar(&inReplyTo, "in-reply-to", "", "optional source event id")
	return c
}

func readBodyArg(p string, stdin io.Reader) (string, error) {
	if p == "" {
		return "", nil
	}
	if p == "-" {
		b, err := io.ReadAll(stdin)
		return string(b), err
	}
	b, err := os.ReadFile(p)
	return string(b), err
}

func outboxApproveCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "approve <O-id>",
		Short: "Approve an awaiting-human item (human-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.EqualFold(os.Getenv("HARNESS_AGENT_KIND"), "llm") {
				return errf(ExitValidation, "outbox approve is human-only")
			}
			id := args[0]
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			return state.WithStateLock(root, func() error {
				st, path, err := outbox.Find(root, id)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if st != outbox.StateAwaitingHuman {
					return errf(ExitValidation, "outbox %s not in awaiting-human/", id)
				}
				it, err := outbox.Read(path)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if _, err := outbox.Move(root, it, outbox.StateAwaitingHuman, outbox.StatePending); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "approved %s\n", id)
				return nil
			})
		},
	}
}

func outboxRejectCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "reject <O-id>",
		Short: "Reject (delete) an awaiting-human item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			return state.WithStateLock(root, func() error {
				_, path, err := outbox.Find(root, id)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if err := os.Remove(path); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "rejected %s\n", id)
				return nil
			})
		},
	}
}

func outboxRevokeCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <O-id>",
		Short: "Revoke (or delete) an outbox item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			return state.WithStateLock(root, func() error {
				st, path, err := outbox.Find(root, id)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				switch st {
				case outbox.StatePending, outbox.StateAwaitingHuman:
					if err := os.Remove(path); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "revoked (deleted) %s\n", id)
					return nil
				case outbox.StateSent:
					it, err := outbox.Read(path)
					if err != nil {
						return errf(ExitValidation, "%v", err)
					}
					win := 5 * time.Minute
					if d, err := time.ParseDuration(it.RevokeWindow); err == nil {
						win = d
					}
					if time.Since(it.SentAt) > win {
						return errf(ExitValidation, "revoke window expired for %s — manual cleanup required", id)
					}
					// We do NOT actually call provider delete APIs in this
					// process (sender does that). We mark intent.
					it.SenderResponse = map[string]any{"revoked_at": time.Now().UTC().Format(time.RFC3339)}
					if err := outbox.Write(path, it); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					r := gitops.Repo{Dir: root}
					_ = r.Add(filepath.Join("outbox", "sent", id+".yaml"))
					if _, err := r.Commit(fmt.Sprintf("outbox %s: mark revoked", id)); err != nil && !errors.Is(err, gitops.ErrNothingToCommit) {
						return errf(ExitGit, "%v", err)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "marked %s revoked (sender will call provider delete)\n", id)
					return nil
				case outbox.StateFailed:
					if err := os.Remove(path); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					return nil
				}
				return errf(ExitValidation, "unknown state %s", st)
			})
		},
	}
}

func outboxLsCmd(opts *Options) *cobra.Command {
	var stateFilter string
	c := &cobra.Command{
		Use:   "ls",
		Short: "List outbox items",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			states := outbox.AllStates
			if stateFilter != "" {
				states = []outbox.State{outbox.State(stateFilter)}
			}
			for _, st := range states {
				ids, err := outbox.ListByState(root, st)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "── %s (%d) ──\n", st, len(ids))
				for _, id := range ids {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", id)
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&stateFilter, "state", "", "pending|awaiting-human|sent|failed")
	return c
}

func outboxFlushCmd(opts *Options) *cobra.Command {
	var dryRun bool
	c := &cobra.Command{
		Use:   "flush",
		Short: "Run one sender pass (manual mode); --dry-run prints intended sends",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			return state.WithStateLock(root, func() error {
				ids, err := outbox.ListByState(root, outbox.StatePending)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				lim, _ := outbox.LoadLimits(root)
				for _, id := range ids {
					src := outbox.PathFor(root, outbox.StatePending, id)
					it, err := outbox.Read(src)
					if err != nil {
						continue
					}
					if err := outbox.RateCheck(root, it, lim, time.Now().UTC()); err != nil {
						fmt.Fprintf(cmd.OutOrStdout(), "skip %s: %v\n", id, err)
						continue
					}
					if err := outbox.DuplicateCheck(root, it, time.Now().UTC()); err != nil {
						fmt.Fprintf(cmd.OutOrStdout(), "skip %s: %v\n", id, err)
						continue
					}
					if dryRun || opts.DryRun {
						fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would send %s to=%s thread=%s risk=%s\n", id, it.To, it.Ref.Thread, it.Risk)
						continue
					}
					// Real send is performed by the autopilot outbox-sender
					// daemon (see internal/daemon). Manual flush only
					// validates; instruct user to run autopilot for sends.
					fmt.Fprintf(cmd.OutOrStdout(), "ready %s (run autopilot for actual send)\n", id)
				}
				return nil
			})
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print intended sends without sending")
	return c
}
