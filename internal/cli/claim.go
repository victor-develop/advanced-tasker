package cli

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/inbox"
	"github.com/victor-develop/advanced-tasker/internal/jobs"
	"github.com/victor-develop/advanced-tasker/internal/state"
	"github.com/victor-develop/advanced-tasker/internal/tick"
)

// newClaimCmd implements `harness claim commander|job ...`.
func newClaimCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "claim", Short: "Atomically claim a slot (commander or job)"}
	var asAgent, ttl string
	var pid int
	commander := &cobra.Command{
		Use:   "commander",
		Short: "Claim the commander lock",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			if asAgent == "" {
				return errf(ExitUsage, "--as is required")
			}
			d, err := time.ParseDuration(valueOr(ttl, "10m"))
			if err != nil {
				return errf(ExitUsage, "--ttl: %v", err)
			}
			eff := pid
			if eff == 0 {
				eff = os.Getpid()
			}
			lease, err := tick.Claim(root, asAgent, eff, d)
			if errors.Is(err, tick.ErrContended) {
				return errf(ExitLock, "commander already claimed")
			}
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "claimed by %s until %s\n", lease.HeldBy, lease.LeaseUntil.Format(time.RFC3339))
			return nil
		},
	}
	commander.Flags().StringVar(&asAgent, "as", "", "claiming agent id")
	commander.Flags().StringVar(&ttl, "ttl", "10m", "lease duration (Go duration)")
	commander.Flags().IntVar(&pid, "pid", 0, "PID (defaults to current process)")

	job := &cobra.Command{
		Use:   "job <J-id>",
		Short: "Claim a pending job (moves to in-flight/)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			if asAgent == "" {
				return errf(ExitUsage, "--as is required")
			}
			d, err := time.ParseDuration(valueOr(ttl, "30m"))
			if err != nil {
				return errf(ExitUsage, "--ttl: %v", err)
			}
			eff := pid
			if eff == 0 {
				eff = os.Getpid()
			}
			return state.WithStateLock(root, func() error {
				st, path, err := jobs.FindJob(root, id)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				j, err := jobs.Read(path)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				now := time.Now().UTC()
				if st == jobs.StateInFlight {
					// PID-aware stale-lock detection.
					if j.LeaseUntil.After(now) {
						if j.ClaimedPID != 0 && !pidLive(j.ClaimedPID) {
							_, _ = inbox.AppendAnomaly(root, fmt.Sprintf("job:%s", id), fmt.Sprintf("stale-lock pid=%d reclaim", j.ClaimedPID))
							// fall through to claim
						} else {
							return errf(ExitLock, "job %s claimed by %s until %s", id, j.ClaimedBy, j.LeaseUntil.Format(time.RFC3339))
						}
					}
				}
				j.ClaimedBy = asAgent
				j.ClaimedPID = eff
				j.LeaseUntil = now.Add(d)
				if _, err := jobs.Move(root, j, st, jobs.StateInFlight); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "claimed %s by %s\n", id, asAgent)
				return nil
			})
		},
	}
	job.Flags().StringVar(&asAgent, "as", "", "claiming agent id")
	job.Flags().StringVar(&ttl, "ttl", "30m", "lease duration")
	job.Flags().IntVar(&pid, "pid", 0, "PID")

	c.AddCommand(commander, job)
	return c
}

// newReleaseCmd implements `harness release commander|job ...`.
func newReleaseCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "release", Short: "Release a claimed slot"}
	c.AddCommand(
		&cobra.Command{
			Use:   "commander",
			Short: "Release the commander lock",
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				if err := tick.Release(root); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "released")
				return nil
			},
		},
		&cobra.Command{
			Use:   "job <J-id>",
			Short: "Release a job (moves in-flight → pending)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id := args[0]
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				return state.WithStateLock(root, func() error {
					st, path, err := jobs.FindJob(root, id)
					if err != nil {
						return errf(ExitValidation, "%v", err)
					}
					if st != jobs.StateInFlight {
						return errf(ExitValidation, "job %s is in %s, expected in-flight", id, st)
					}
					j, err := jobs.Read(path)
					if err != nil {
						return errf(ExitValidation, "%v", err)
					}
					j.ClaimedBy = ""
					j.ClaimedPID = 0
					j.LeaseUntil = time.Time{}
					if _, err := jobs.Move(root, j, st, jobs.StatePending); err != nil {
						return errf(ExitValidation, "%v", err)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "released %s\n", id)
					return nil
				})
			},
		},
	)
	return c
}

// pidLive reports whether a PID is reachable by signal 0.
func pidLive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}
