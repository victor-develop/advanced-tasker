package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	ghp "github.com/victor-develop/advanced-tasker/internal/github"
)

// NewRunCmd returns the daemon/one-shot poller as a cobra subcommand.
// The previous flag-based binary lived in cmd/github-poller/main.go; we
// now route through cobra so the lifecycle verbs (C6) share the same
// state-root resolution.
func NewRunCmd(stateRoot *string) *cobra.Command {
	var (
		once     bool
		logLevel string
		baseURL  string
		cfgPath  string
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the poller daemon (or --once for one cycle)",
		Long: `run is the default poller mode.  Without flags it loops on
poll_interval; with --once it runs exactly one cycle and exits.  See
design/09 §"Daemon process model".

This is also the default action when the binary is invoked with no
subcommand, for backwards-compatibility with the round-1 binary.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return doRun(*stateRoot, cfgPath, once, logLevel, baseURL)
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "run a single poll cycle and exit")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug|info|warn|error")
	cmd.Flags().StringVar(&baseURL, "github-base-url", "", "override GitHub API base URL (for tests / GHE)")
	cmd.Flags().StringVar(&cfgPath, "config", "", "path to sources/github/config.yaml (default: <state-root>/sources/github/config.yaml)")
	return cmd
}

func doRun(stateRoot, cfgPath string, once bool, logLevel, baseURL string) error {
	level := parseLevel(logLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if cfgPath == "" {
		cfgPath = ghp.DefaultConfigPath(stateRoot)
	}
	cfg, err := ghp.LoadConfig(cfgPath)
	if err != nil {
		if errors.Is(err, ghp.ErrConfigMissing) {
			fmt.Fprintln(os.Stderr, ghp.ErrConfigMissing.Error())
			return err
		}
		return err
	}

	token := cfg.Token()
	if token == "" {
		logger.Warn("GitHub token env is empty; running unauthenticated (rate limits will hurt)",
			"env", cfg.Auth.TokenEnv)
	}
	client, err := ghp.NewClient(token, baseURL, nil)
	if err != nil {
		return fmt.Errorf("build github client: %w", err)
	}

	poller := &ghp.Poller{
		Config:  cfg,
		Client:  client,
		Cursors: ghp.NewCursorStore(stateRoot),
		Writer:  ghp.NewWriter(stateRoot),
		Logger:  logger,
	}

	// See design/09 §"Daemon process model" and the round-2 graceful
	// SIGTERM requirement.
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

	shutdownCh := make(chan os.Signal, 2)
	signal.Notify(shutdownCh, syscall.SIGTERM, syscall.SIGINT)
	shutdown := &shutdownFlag{}
	go func() {
		sig := <-shutdownCh
		shutdown.signal(sig.String())
		if sig == syscall.SIGINT {
			runCancel()
			return
		}
		go func() {
			select {
			case <-time.After(30 * time.Second):
				logger.Warn("graceful shutdown exceeded 30s; force-cancelling")
				runCancel()
			case <-runCtx.Done():
			}
		}()
	}()

	if once {
		stats, err := poller.RunOnce(runCtx)
		if err != nil {
			return fmt.Errorf("cycle failed: %w", err)
		}
		logger.Info("cycle complete",
			"repos", stats.ReposPolled,
			"new_prs", stats.PRsDiscovered,
			"prs_polled", stats.PRsPolled,
			"raw_events", stats.RawEventsWritten,
			"not_modified", stats.NotModifiedCount,
			"anomalies", stats.AnomaliesRecorded,
			"errors", stats.Errors,
		)
		return nil
	}

	return runDaemon(runCtx, logger, cfg, poller, shutdown)
}

// shutdownFlag is a tiny goroutine-safe latch flipped on SIGTERM.
type shutdownFlag struct {
	mu     sync.Mutex
	armed  bool
	reason string
}

func (s *shutdownFlag) signal(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armed {
		return
	}
	s.armed = true
	s.reason = reason
}

func (s *shutdownFlag) firing() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.armed, s.reason
}

func runDaemon(ctx context.Context, logger *slog.Logger, cfg *ghp.Config, poller *ghp.Poller, shutdown *shutdownFlag) error {
	tick := time.NewTicker(cfg.PollInterval.Duration)
	defer tick.Stop()

	logger.Info("github-poller started",
		"poll_interval", cfg.PollInterval.Duration.String(),
		"repos", cfg.Watch.Repos,
		"max_concurrent_pr_polls", cfg.MaxConcurrent,
	)

	if err := runCycleWithBackoff(ctx, logger, cfg, poller); err != nil {
		return err
	}
	if armed, reason := shutdown.firing(); armed {
		logger.Info("shutdown signal received; exiting cleanly after first cycle",
			"signal", reason)
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutdown ctx cancelled; exiting")
			return nil
		case <-tick.C:
			if armed, reason := shutdown.firing(); armed {
				logger.Info("shutdown signal received; skipping next cycle",
					"signal", reason)
				return nil
			}
			if err := runCycleWithBackoff(ctx, logger, cfg, poller); err != nil {
				return err
			}
			if armed, reason := shutdown.firing(); armed {
				logger.Info("shutdown signal received; exiting cleanly after cycle",
					"signal", reason)
				return nil
			}
		}
	}
}

func runCycleWithBackoff(ctx context.Context, logger *slog.Logger, cfg *ghp.Config, poller *ghp.Poller) error {
	stats, err := poller.RunOnce(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		logger.Error("cycle errored; sleeping before next attempt",
			"backoff", cfg.Backoff.OnError.Duration.String(),
			"error", err)
		select {
		case <-time.After(cfg.Backoff.OnError.Duration):
		case <-ctx.Done():
			return nil
		}
		return nil
	}
	logger.Info("cycle complete",
		"repos", stats.ReposPolled,
		"new_prs", stats.PRsDiscovered,
		"prs_polled", stats.PRsPolled,
		"raw_events", stats.RawEventsWritten,
		"not_modified", stats.NotModifiedCount,
		"anomalies", stats.AnomaliesRecorded,
		"errors", stats.Errors,
	)
	return nil
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	fmt.Fprintf(os.Stderr, "unknown log level %q; defaulting to info\n", s)
	return slog.LevelInfo
}
