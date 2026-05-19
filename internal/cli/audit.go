package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/audit"
	"github.com/victor-develop/advanced-tasker/internal/llm"
)

// newAuditCmd implements `harness audit run|ls|show|checklist edit`.
func newAuditCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "audit", Short: "Audit agent commands"}
	var model string
	run := &cobra.Command{
		Use:   "run",
		Short: "Run one audit (signals + driver narrative)",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			driver := pickDriverFor(root, "auditor")
			_ = model
			path, err := audit.Run(context.Background(), root, driver, true)
			if err != nil {
				return errf(ExitValidation, "%v", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	}
	run.Flags().StringVar(&model, "model", "", "override model (informational)")
	c.AddCommand(run)

	c.AddCommand(
		&cobra.Command{
			Use:   "ls",
			Short: "List audit reports",
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				names, _ := audit.ListReports(root)
				for _, n := range names {
					fmt.Fprintln(cmd.OutOrStdout(), n)
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "show <audit-id>",
			Short: "Print one audit report",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				root, err := requireInitialized(opts)
				if err != nil {
					return err
				}
				p := filepath.Join(root, "audit", "reports", args[0])
				if _, err := os.Stat(p); err != nil {
					p = filepath.Join(root, "audit", "reports", args[0]+".md")
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return errf(ExitValidation, "%v", err)
				}
				fmt.Fprint(cmd.OutOrStdout(), string(b))
				return nil
			},
		},
	)
	cl := &cobra.Command{Use: "checklist", Short: "Manage audit checklist"}
	cl.AddCommand(&cobra.Command{
		Use:   "edit",
		Short: "Open $EDITOR on the checklist (does NOT validate)",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			p := filepath.Join(root, audit.ChecklistFile)
			editor := os.Getenv("EDITOR")
			if editor == "" {
				return errf(ExitUsage, "$EDITOR not set; checklist is at %s", p)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "edit %s with %s\n", p, editor)
			return nil
		},
	})
	c.AddCommand(cl)
	return c
}

// newAuditDaemonCmd implements `harness audit-daemon start|stop|status`.
func newAuditDaemonCmd(opts *Options) *cobra.Command {
	c := &cobra.Command{Use: "audit-daemon", Short: "Audit daemon control"}
	var duration string
	start := &cobra.Command{
		Use:   "start",
		Short: "Run the audit daemon (blocks)",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := requireInitialized(opts)
			if err != nil {
				return err
			}
			d := time.Duration(0)
			if duration != "" {
				dx, err := time.ParseDuration(duration)
				if err != nil {
					return errf(ExitUsage, "%v", err)
				}
				d = dx
			}
			ctx := context.Background()
			if d > 0 {
				ctx2, cancel := context.WithTimeout(ctx, d)
				ctx = ctx2
				defer cancel()
			}
			driver := pickDriverFor(root, "auditor")
			// Cadence: 4h default; configurable later.
			t := time.NewTicker(4 * time.Hour)
			defer t.Stop()
			if _, err := audit.Run(ctx, root, driver, true); err != nil {
				return errf(ExitValidation, "%v", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "audit-daemon: first pass done")
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-t.C:
					_, _ = audit.Run(ctx, root, driver, true)
				}
			}
		},
	}
	start.Flags().StringVar(&duration, "duration", "", "lifetime (Go duration; bounded for tests)")
	c.AddCommand(start)

	c.AddCommand(
		&cobra.Command{Use: "stop", Short: "(no-op; daemons live with autopilot)", RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		}},
		&cobra.Command{Use: "status", Short: "Status of the audit daemon", RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "audit-daemon: integrated with autopilot")
			return nil
		}},
	)
	return c
}

// pickDriverFor reads state/config.yaml `models.driver` and constructs
// the corresponding LLM driver. Defaults to fake when unspecified
// (safer in tests / when CLAUDE binary is absent).
func pickDriverFor(root, role string) llm.Driver {
	// The CLI only uses Fake by default; autopilot/daemons explicitly
	// override via --driver. We don't want CLI verbs (like `harness
	// audit run`) to silently shell out to `claude` unless asked.
	if os.Getenv("HARNESS_DRIVER") == "claude-p" {
		return llm.NewClaudeP("")
	}
	dir := filepath.Join(root, ".harness", "fake-fixtures")
	if _, err := os.Stat(dir); err != nil {
		dir = filepath.Join(root, "telemetry", "fake-fixtures")
	}
	return llm.NewFake(dir)
}
