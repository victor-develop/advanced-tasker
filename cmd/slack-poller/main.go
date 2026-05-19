// Command slack-poller is the Track B Slack ingestion daemon for
// advanced-tasker.
//
// It polls Slack for new top-level messages and thread replies, writes raw
// event files under state/threads/slack-*/raw/, and drops new-thread items
// into state/inbox/new/. It performs NO LLM calls and NEVER posts
// messages — it is strictly read-only against Slack.
//
// See design/08-slack-poller.md for the full spec.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "slack-poller: %v\n", err)
		os.Exit(exitCode(err))
	}
}

type flags struct {
	stateRoot string
	once      bool
	apiURL    string
	logLevel  string
}

func parseFlags(args []string) (*flags, error) {
	fs := flag.NewFlagSet("slack-poller", flag.ContinueOnError)
	f := &flags{}
	fs.StringVar(&f.stateRoot, "state", "state", "path to state/ directory")
	fs.BoolVar(&f.once, "once", false, "run a single poll cycle then exit")
	fs.StringVar(&f.apiURL, "api-url", "", "override Slack API URL (testing only)")
	fs.StringVar(&f.logLevel, "log-level", "info", "log level (debug|info|warn|error)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return f, nil
}

func run(args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	logger := newLogger(f.logLevel)
	slog.SetDefault(logger)

	cfgPath := filepath.Join(f.stateRoot, "sources", "slack", "config.yaml")
	cfg, err := slackpkg.LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	token, err := cfg.ResolveToken()
	if err != nil {
		return err
	}

	client := slackpkg.NewSlackGoClient(token, f.apiURL)
	cursorsRoot := filepath.Join(f.stateRoot, "sources", "slack", "cursors")
	cursors, err := slackpkg.NewCursorStore(cursorsRoot)
	if err != nil {
		return fmt.Errorf("init cursors: %w", err)
	}

	writer := slackpkg.NewWriter(f.stateRoot)
	if cfg.WriteUpdatePings != nil {
		writer.WriteUpdatePings = *cfg.WriteUpdatePings
	}

	poller := &slackpkg.Poller{
		Cfg:     cfg,
		Client:  client,
		Cursors: cursors,
		Writer:  writer,
		Logger:  logger,
	}

	ctx, cancel := signalContext()
	defer cancel()

	if f.once {
		res, err := poller.PollOnce(ctx)
		logResult(logger, res)
		return err
	}

	return daemonLoop(ctx, logger, poller, cfg.PollInterval.Duration, cfg.Backoff)
}

// daemonLoop runs PollOnce on a tick, backing off on transient errors.
func daemonLoop(ctx context.Context, logger *slog.Logger, p *slackpkg.Poller,
	interval time.Duration, backoff slackpkg.BackoffConfig) error {

	logger.Info("slack-poller starting",
		slog.Duration("poll_interval", interval))

	currentBackoff := backoff.OnError.Duration
	for {
		start := time.Now()
		res, err := p.PollOnce(ctx)
		logResult(logger, res)

		if err != nil {
			if errors.Is(err, context.Canceled) {
				logger.Info("shutdown requested")
				return nil
			}
			logger.Error("poll cycle error", slog.String("err", err.Error()))
			// Exponential backoff capped at MaxBackoff.
			currentBackoff *= 2
			if currentBackoff > backoff.MaxBackoff.Duration {
				currentBackoff = backoff.MaxBackoff.Duration
			}
			if err := sleep(ctx, currentBackoff); err != nil {
				return nil
			}
			continue
		}

		// Reset backoff on success.
		currentBackoff = backoff.OnError.Duration

		elapsed := time.Since(start)
		next := interval - elapsed
		if next < time.Second {
			next = time.Second
		}
		if err := sleep(ctx, next); err != nil {
			return nil
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func logResult(logger *slog.Logger, r slackpkg.PollResult) {
	logger.Info("poll cycle complete",
		slog.Int("channels_polled", r.ChannelsPolled),
		slog.Int("threads_polled", r.ThreadsPolled),
		slog.Int("raw_events_written", r.RawEventsWritten),
		slog.Int("inbox_new_written", r.InboxNewWritten),
		slog.Int("update_pings_written", r.UpdatePingsWritten),
		slog.Int("errors", r.Errors))
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func exitCode(err error) int {
	if errors.Is(err, slackpkg.ErrFatalAuth) {
		return 3
	}
	return 1
}
