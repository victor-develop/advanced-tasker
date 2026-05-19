package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/engine"
	"github.com/victor-develop/advanced-tasker/internal/gitops"
	"github.com/victor-develop/advanced-tasker/internal/state"
	"github.com/victor-develop/advanced-tasker/internal/telemetry"
	"github.com/victor-develop/advanced-tasker/internal/tick"
)

// newTickCmd implements `harness tick start|end`.
func newTickCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "tick", Short: "Commander tick lifecycle"}
	var asAgent, ttl string
	start := &cobra.Command{
		Use:   "start",
		Short: "Claim commander and emit the dashboard prompt",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			if asAgent == "" {
				asAgent = "agent"
			}
			d, err := time.ParseDuration(valueOr(ttl, "10m"))
			if err != nil {
				return errf(ExitUsage, "--ttl: %v", err)
			}
			lease, err := tick.Claim(root, asAgent, os.Getpid(), d)
			if errors.Is(err, tick.ErrContended) {
				return errf(ExitLock, "commander already claimed")
			}
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			path, when := tick.LogFileForNow(root)
			if err := tick.WriteFrontmatter(path, when.Format("2006-01-02T15-04-05Z"), asAgent); err != nil {
				return errf(ExitValidation, "%v", err)
			}
			// HOOK AUDIT banner: surface any pollers/daemons that look stale.
			banner := buildHookAuditBanner(root, time.Now().UTC())
			if banner != "" {
				fmt.Fprintln(cmd.OutOrStdout(), banner)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "tick %s lease=%s\n", lease.HeldBy, lease.LeaseUntil.Format(time.RFC3339))
			return nil
		},
	}
	start.Flags().StringVar(&asAgent, "as", "", "agent id")
	start.Flags().StringVar(&ttl, "ttl", "10m", "TTL")
	c.AddCommand(start)

	var idle bool
	var summary string
	var costUSD float64
	var durMS int64
	end := &cobra.Command{
		Use:   "end",
		Short: "Finalize tick log and release commander",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			return state.WithStateLock(root, func() error {
				p, ok := tick.CurrentLogPath(root)
				if !ok {
					return errf(ExitValidation, "no active tick log")
				}
				st, _ := engine.Load(root)
				engine.RecordTick(st, idle, costUSD, durMS)
				if err := engine.Save(root, st); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				if err := tick.FinalizeLog(p, durMS, costUSD, idle, st.ConsecutiveIdle, summary); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				_ = telemetry.AppendSummary(root, "tick", costUSD, durMS, false, "")
				r := gitops.Repo{Dir: root}
				_ = r.Add(strings.TrimPrefix(p, root+"/"))
				_, _ = r.Commit(fmt.Sprintf("tick end: %s", summary))
				_ = tick.Release(root)
				fmt.Fprintln(cmd.OutOrStdout(), "tick ended")
				return nil
			})
		},
	}
	end.Flags().BoolVar(&idle, "idle", false, "mark tick as no-op")
	end.Flags().StringVar(&summary, "summary", "", "tick summary text")
	end.Flags().Float64Var(&costUSD, "cost-usd", 0, "tick cost in USD")
	end.Flags().Int64Var(&durMS, "duration-ms", 0, "tick duration in ms")
	c.AddCommand(end)

	return c
}

// buildHookAuditBanner inspects daemons/pollers' last-run timestamps
// and surfaces any that look stuck. It's a soft signal — the commander
// can ignore it.
func buildHookAuditBanner(root string, now time.Time) string {
	var lines []string
	// Look for pollers' cursor files; if cursor dir mtime is older
	// than a threshold, mention it.
	for _, src := range []string{"slack", "github"} {
		info, err := os.Stat(fmt.Sprintf("%s/sources/%s/config.yaml", root, src))
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > 24*time.Hour {
			lines = append(lines, fmt.Sprintf("- poller %s: config not modified for > 24h", src))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "── HOOK AUDIT ──\n" + strings.Join(lines, "\n")
}

// newTickLogCmd implements `harness tick-log append "..."`.
func newTickLogCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "tick-log", Short: "Append to the current tick log"}
	c.AddCommand(&cobra.Command{
		Use:   "append <text>",
		Short: "Append a paragraph to the active tick log",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			p, ok := tick.CurrentLogPath(root)
			if !ok {
				return errf(ExitValidation, "no active tick log (run `harness tick start` first)")
			}
			return tick.AppendLog(p, args[0])
		},
	})
	return c
}

// newEngineCmd implements `harness engine status|idle-tick|idle-reset`.
func newEngineCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "engine", Short: "Inspect or nudge engine state"}
	c.AddCommand(
		&cobra.Command{
			Use:   "status",
			Short: "Print engine.json",
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				s, err := engine.Load(root)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "mode=%s cycles=%d consecutive_idle=%d last_tick=%s\n",
					s.Mode, s.TotalCycles, s.ConsecutiveIdle, s.LastTick.Format(time.RFC3339))
				return nil
			},
		},
		&cobra.Command{
			Use:   "idle-tick",
			Short: "Increment consecutive_idle (rare; equivalent to tick end --idle)",
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				s, _ := engine.Load(root)
				engine.RecordTick(s, true, 0, 0)
				return engine.Save(root, s)
			},
		},
		&cobra.Command{
			Use:   "idle-reset",
			Short: "Reset consecutive_idle to 0",
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				s, _ := engine.Load(root)
				s.ConsecutiveIdle = 0
				return engine.Save(root, s)
			},
		},
	)
	return c
}

// newTelemetryCmd implements `harness telemetry ls|show|cost|rotate`.
func newTelemetryCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "telemetry", Short: "Inspect captured telemetry"}
	c.AddCommand(
		&cobra.Command{
			Use:   "ls",
			Short: "List telemetry files (ticks/, workers/, audits/)",
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				items, _ := telemetry.List(root)
				for _, x := range items {
					fmt.Fprintln(cmd.OutOrStdout(), x)
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "show <rel-path>",
			Short: "Print a telemetry JSONL file",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				body, err := telemetry.Show(root, args[0])
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				fmt.Fprint(cmd.OutOrStdout(), body)
				return nil
			},
		},
	)
	var since string
	cost := &cobra.Command{
		Use:   "cost",
		Short: "Aggregate cost from telemetry/summary.log",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			var s time.Time
			if since != "" {
				t, err := time.Parse(time.RFC3339, since)
				if err != nil {
					return errf(ExitUsage, "--since: %v", err)
				}
				s = t
			}
			total, err := telemetry.CostSince(root, s)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "$%.4f\n", total)
			return nil
		},
	}
	cost.Flags().StringVar(&since, "since", "", "RFC3339 cutoff")
	c.AddCommand(cost)

	var olderThan string
	rotate := &cobra.Command{
		Use:   "rotate",
		Short: "Delete telemetry files older than --older-than",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			d := 7 * 24 * time.Hour
			if olderThan != "" {
				dx, err := time.ParseDuration(olderThan)
				if err != nil {
					return errf(ExitUsage, "%v", err)
				}
				d = dx
			}
			n, err := telemetry.Rotate(root, d)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "rotated %d file(s)\n", n)
			return nil
		},
	}
	rotate.Flags().StringVar(&olderThan, "older-than", "7d", "Go duration; default 7 days")
	c.AddCommand(rotate)
	return c
}
