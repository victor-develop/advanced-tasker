package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/daemon"
	"github.com/victor-develop/advanced-tasker/internal/llm"
)

// newAutopilotCmd implements `harness autopilot start|stop|pause|resume|status`.
func newAutopilotCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "autopilot", Short: "Run / control autopilot daemons"}

	var driverFlag, durationFlag string
	var dryRunOutbox bool
	start := &cobra.Command{
		Use:   "start",
		Short: "Run all daemons as goroutines in this process",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			driver, err := buildDriver(root, driverFlag)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if durationFlag != "" {
				d, err := time.ParseDuration(durationFlag)
				if err != nil {
					return errf(ExitUsage, "--duration: %v", err)
				}
				ctx2, c2 := context.WithTimeout(ctx, d)
				ctx = ctx2
				defer c2()
			} else if d := os.Getenv("HARNESS_TEST_DURATION"); d != "" {
				if dx, err := time.ParseDuration(d); err == nil {
					ctx2, c2 := context.WithTimeout(ctx, dx)
					ctx = ctx2
					defer c2()
				}
			}

			bus := daemon.NewBus(root, driver)
			bus.DryRunOutbox = dryRunOutbox || driver.Name() == "fake"

			// Mark the autopilot.lock so `autopilot status` can tell.
			_ = os.WriteFile(filepath.Join(root, "autopilot.lock"), []byte(fmt.Sprintf("pid=%d\n", os.Getpid())), 0o644)
			defer os.Remove(filepath.Join(root, "autopilot.lock"))

			var wg sync.WaitGroup
			run := func(name string, fn func(context.Context) error) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := fn(ctx); err != nil {
						bus.Log("%s: %v", name, err)
					}
				}()
			}
			run("rollup-updater", daemon.NewRollupUpdater(bus).Run)
			run("worker-runner", daemon.NewWorkerRunner(bus).Run)
			run("outbox-sender", daemon.NewOutboxSender(bus).Run)
			run("commander-scheduler", daemon.NewCommanderScheduler(bus).Run)
			run("audit-daemon", daemon.NewAuditDaemon(bus).Run)

			fmt.Fprintf(cmd.OutOrStdout(), "autopilot started (driver=%s)\n", driver.Name())
			wg.Wait()
			fmt.Fprintln(cmd.OutOrStdout(), "autopilot stopped")
			// Flush log to stderr for visibility.
			for _, line := range bus.Lines() {
				fmt.Fprintln(cmd.ErrOrStderr(), line)
			}
			return nil
		},
	}
	start.Flags().StringVar(&driverFlag, "driver", "", "claude-p|fake (override config models.driver)")
	start.Flags().StringVar(&durationFlag, "duration", "", "bound autopilot lifetime (Go duration)")
	start.Flags().BoolVar(&dryRunOutbox, "dry-run-outbox", false, "outbox-sender prints intended sends instead of executing")
	c.AddCommand(start)

	c.AddCommand(
		&cobra.Command{
			Use:   "stop",
			Short: "Signal the running autopilot to stop (best-effort)",
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				return os.Remove(filepath.Join(root, "autopilot.lock"))
			},
		},
		&cobra.Command{
			Use:   "pause",
			Short: "Pause (no-op in v1: rely on --duration to bound runs)",
			RunE: func(cmd *cobra.Command, args []string) error {
				return nil
			},
		},
		&cobra.Command{
			Use:   "resume",
			Short: "Resume (no-op in v1)",
			RunE: func(cmd *cobra.Command, args []string) error {
				return nil
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Print autopilot status",
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				b, err := os.ReadFile(filepath.Join(root, "autopilot.lock"))
				if err != nil {
					fmt.Fprintln(cmd.OutOrStdout(), "autopilot: not running")
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "autopilot: %s", string(b))
				return nil
			},
		},
	)
	return c
}

// buildDriver constructs the LLM driver based on the flag (preferred)
// or models.driver from state/config.yaml.
func buildDriver(root, flag string) (llm.Driver, error) {
	name := flag
	if name == "" {
		// Best-effort config lookup; default to fake for safety.
		name = configDriverName(root)
	}
	switch name {
	case "fake", "":
		fixtureDir := os.Getenv("HARNESS_FAKE_FIXTURES")
		if fixtureDir == "" {
			fixtureDir = filepath.Join(root, ".fake-fixtures")
		}
		return llm.NewFake(fixtureDir), nil
	case "claude-p":
		return llm.NewClaudeP(""), nil
	}
	return nil, fmt.Errorf("unknown driver %q", name)
}

func configDriverName(root string) string {
	// Read state/config.yaml `models.driver`; fall back to fake.
	b, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		return "fake"
	}
	// Naive parse — avoid pulling in yaml here.
	body := string(b)
	const key = "driver:"
	if idx := indexOfLine(body, key); idx >= 0 {
		rest := body[idx+len(key):]
		end := len(rest)
		for i, r := range rest {
			if r == '\n' {
				end = i
				break
			}
		}
		v := rest[:end]
		v = trimSpaces(v)
		if v != "" {
			return v
		}
	}
	return "fake"
}

func indexOfLine(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func trimSpaces(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}
