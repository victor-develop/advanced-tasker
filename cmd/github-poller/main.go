// github-poller is the Track C ingestion daemon for advanced-tasker.
//
// It polls GitHub for tracked PRs and discovers new PRs, persisting raw
// events to state/threads/github-*/raw/<event-id>.json and touching the
// .dirty marker after each cycle.  See design/09-github-poller.md for the
// full spec.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	ghp "github.com/victor-develop/advanced-tasker/internal/github"
)

func main() {
	var (
		stateRoot  = flag.String("state-root", "state", "path to state/ directory")
		configPath = flag.String("config", "", "path to sources/github/config.yaml (default: <state-root>/sources/github/config.yaml)")
		once       = flag.Bool("once", false, "run a single poll cycle and exit")
		logLevel   = flag.String("log-level", "info", "log level: debug|info|warn|error")
		baseURL    = flag.String("github-base-url", "", "override GitHub API base URL (for tests / GHE)")
	)
	flag.Parse()

	level := parseLevel(*logLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = ghp.DefaultConfigPath(*stateRoot)
	}
	cfg, err := ghp.LoadConfig(cfgPath)
	if err != nil {
		logger.Error("load config", "path", cfgPath, "error", err)
		os.Exit(2)
	}

	token := cfg.Token()
	if token == "" {
		logger.Warn("GitHub token env is empty; running unauthenticated (rate limits will hurt)",
			"env", cfg.Auth.TokenEnv)
	}
	client, err := ghp.NewClient(token, *baseURL, nil)
	if err != nil {
		logger.Error("build github client", "error", err)
		os.Exit(2)
	}

	poller := &ghp.Poller{
		Config:  cfg,
		Client:  client,
		Cursors: ghp.NewCursorStore(*stateRoot),
		Writer:  ghp.NewWriter(*stateRoot),
		Logger:  logger,
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *once {
		stats, err := poller.RunOnce(ctx)
		if err != nil {
			logger.Error("cycle failed", "error", err)
			os.Exit(1)
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
		return
	}

	if err := runDaemon(ctx, logger, cfg, poller); err != nil {
		logger.Error("daemon exited", "error", err)
		os.Exit(1)
	}
}

func runDaemon(ctx context.Context, logger *slog.Logger, cfg *ghp.Config, poller *ghp.Poller) error {
	tick := time.NewTicker(cfg.PollInterval.Duration)
	defer tick.Stop()

	logger.Info("github-poller started",
		"poll_interval", cfg.PollInterval.Duration.String(),
		"repos", cfg.Watch.Repos,
		"max_concurrent_pr_polls", cfg.MaxConcurrent,
	)

	// Run an initial cycle immediately so startup time isn't wasted.
	if err := runCycleWithBackoff(ctx, logger, cfg, poller); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutdown signal received; exiting cleanly")
			return nil
		case <-tick.C:
			if err := runCycleWithBackoff(ctx, logger, cfg, poller); err != nil {
				return err
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
