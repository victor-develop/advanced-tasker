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

	"github.com/victor-develop/advanced-tasker/internal/daemon"
	"github.com/victor-develop/advanced-tasker/internal/gitops"
	"github.com/victor-develop/advanced-tasker/internal/ids"
	"github.com/victor-develop/advanced-tasker/internal/outbox"
	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
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
					// Per design/07 §Revoke, actually call the provider's
					// delete API. The polish-round-1 fix: prior to this,
					// revoke only marked intent on disk and no daemon
					// read it back, so revoked items stayed visible to
					// recipients. Now: call chat.delete (Slack) /
					// DELETE comment (GitHub TBD) before marking.
					if it.To == "slack" {
						channel, ok1 := it.SenderResponse["channel"].(string)
						messageTS, ok2 := it.SenderResponse["message_ts"].(string)
						if !ok1 || !ok2 {
							return errf(ExitValidation, "sent item missing sender_response.channel or message_ts; cannot revoke %s", id)
						}
						cfg, cerr := slackpkg.LoadConfig(filepath.Join(root, "sources", "slack", "config.yaml"))
						if cerr != nil {
							return errf(ExitValidation, "slack config: %v", cerr)
						}
						token, terr := cfg.ResolveToken()
						if terr != nil {
							return errf(ExitValidation, "slack token: %v", terr)
						}
						if derr := outbox.SlackProviderDelete(token, channel, messageTS); derr != nil {
							return errf(ExitValidation, "%v", derr)
						}
					}
					// Mark intent recorded; preserve sender_response so
					// the audit trail keeps the original message_ts.
					it.SenderResponse["revoked_at"] = time.Now().UTC().Format(time.RFC3339)
					if err := outbox.Write(path, it); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					r := gitops.Repo{Dir: root}
					_ = r.Add(filepath.Join("outbox", "sent", id+".yaml"))
					if _, err := r.Commit(fmt.Sprintf("outbox %s: revoked", id)); err != nil && !errors.Is(err, gitops.ErrNothingToCommit) {
						return errf(ExitGit, "%v", err)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "revoked %s (provider delete called)\n", id)
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
		Long: `flush is the one-shot equivalent of the autopilot outbox-sender daemon
per design/07 §"Sender daemon". With sender_enabled=false the safety
gate routes items to awaiting-human/. With sender_enabled=true and
risk=low, items are validated (rate-check + dup-check) and sent via the
provider (Slack via slack-go; GitHub TBD). --dry-run prints intended
sends without contacting any provider.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			ids, err := outbox.ListByState(root, outbox.StatePending)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			// Build a sender that uses the same provider wiring as the
			// daemon (single source of truth for send behavior).
			bus := daemon.NewBus(root, nil)
			sender := daemon.NewOutboxSender(bus)
			for _, id := range ids {
				src := outbox.PathFor(root, outbox.StatePending, id)
				it, err := outbox.Read(src)
				if err != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "skip %s: read: %v\n", id, err)
					continue
				}
				if dryRun || opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "[dry-run] would send %s to=%s thread=%s risk=%s\n", id, it.To, it.Ref.Thread, it.Risk)
					continue
				}
				if err := sender.ProcessOnce(id); err != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "error %s: %v\n", id, err)
					continue
				}
				switch {
				case fileExists(outbox.PathFor(root, outbox.StateSent, id)):
					fmt.Fprintf(cmd.OutOrStdout(), "sent %s\n", id)
				case fileExists(outbox.PathFor(root, outbox.StateAwaitingHuman, id)):
					fmt.Fprintf(cmd.OutOrStdout(), "deferred %s -> awaiting-human (sender_enabled=false)\n", id)
				case fileExists(outbox.PathFor(root, outbox.StateFailed, id)):
					fmt.Fprintf(cmd.OutOrStdout(), "failed %s\n", id)
				default:
					fmt.Fprintf(cmd.OutOrStdout(), "remains pending %s (rate-limited or dup-suppressed)\n", id)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print intended sends without sending")
	return c
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
