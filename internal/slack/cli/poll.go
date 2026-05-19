package cli

import (
	"context"
	"errors"
	"log/slog"
	"time"

	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
)

// runPollFlags collects the flags specific to the default poll action.
type runPollFlags struct {
	Once    bool
	Targets *slackpkg.PollTargets
}

// runPoll runs the default action: either a single poll cycle (--once) or
// the long-running daemon loop. Implements the graceful-shutdown contract:
// when the parent context is canceled, an in-flight PollOnce finishes,
// cursors are persisted (the writer's atomic-rename semantics make that
// automatic), and the function returns nil.
func runPoll(ctx context.Context, opts *Options, flags *runPollFlags) error {
	logger := newLogger(opts.LogLevel)
	slog.SetDefault(logger)

	stateDir := resolveStateDir(opts.StateDir)
	cfg, _, err := loadConfigOrExit(stateDir)
	if err != nil {
		return err
	}
	client, err := buildClient(cfg, opts)
	if err != nil {
		return err
	}
	cursors, err := newCursorStore(stateDir)
	if err != nil {
		return errf(ExitUsage, "init cursors: %v", err)
	}
	writer := newWriter(stateDir, cfg.WriteUpdatePings != nil && *cfg.WriteUpdatePings)

	poller := &slackpkg.Poller{
		Cfg:         cfg,
		Client:      client,
		Cursors:     cursors,
		Writer:      writer,
		Logger:      logger,
		OnlyTargets: flags.Targets,
	}

	if flags.Once {
		res, err := poller.PollOnce(ctx)
		logResult(logger, res)
		if err != nil {
			if slackpkg.IsContextErr(err) {
				logger.Info("shutdown requested mid-once",
					slog.String("err", err.Error()))
				return nil
			}
			if errors.Is(err, slackpkg.ErrFatalAuth) ||
				slackpkg.IsFatalAuthError(err) {
				return &CommandError{Code: ExitAuthFatal, Err: err}
			}
			return errf(ExitUsage, "%v", err)
		}
		return nil
	}

	return daemonLoop(ctx, logger, poller, cfg.PollInterval.Duration,
		cfg.Backoff)
}

// daemonLoop runs PollOnce on a tick, backing off on transient errors.
// Returns nil on graceful shutdown (context canceled). Exits with an error
// when slack reports an unrecoverable auth failure.
func daemonLoop(ctx context.Context, logger *slog.Logger, p *slackpkg.Poller,
	interval time.Duration, backoff slackpkg.BackoffConfig) error {

	logger.Info("slack-poller starting",
		slog.Duration("poll_interval", interval))

	currentBackoff := backoff.OnError.Duration
	for {
		if err := ctx.Err(); err != nil {
			logger.Info("shutdown complete")
			return nil
		}
		start := time.Now()
		res, err := p.PollOnce(ctx)
		logResult(logger, res)

		if err != nil {
			if slackpkg.IsContextErr(err) {
				logger.Info("shutdown requested")
				return nil
			}
			if errors.Is(err, slackpkg.ErrFatalAuth) ||
				slackpkg.IsFatalAuthError(err) {
				return &CommandError{Code: ExitAuthFatal, Err: err}
			}

			// Rate-limit specific path. We do not abort the daemon — we
			// sleep per Retry-After (capped at MaxBackoff) and try again
			// next tick. Persistent 429 → write anomaly and keep going.
			if retryAfter, ok := slackpkg.RateLimitWait(err); ok {
				wait := retryAfter
				if wait <= 0 {
					wait = backoff.OnRateLimit.Duration
				}
				if wait > backoff.MaxBackoff.Duration {
					wait = backoff.MaxBackoff.Duration
				}
				if _, anomalyErr := p.WriteRateLimitAnomaly("daemon", retryAfter); anomalyErr != nil {
					logger.Error("write rate-limit anomaly",
						slog.String("err", anomalyErr.Error()))
				}
				logger.Warn("rate limited",
					slog.Duration("retry_after", retryAfter),
					slog.Duration("sleep", wait))
				if err := sleep(ctx, wait); err != nil {
					return nil
				}
				continue
			}

			logger.Error("poll cycle error", slog.String("err", err.Error()))
			currentBackoff *= 2
			if currentBackoff > backoff.MaxBackoff.Duration {
				currentBackoff = backoff.MaxBackoff.Duration
			}
			if err := sleep(ctx, currentBackoff); err != nil {
				return nil
			}
			continue
		}

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

// sleep waits for d or ctx, whichever comes first.
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

// logResult logs a structured summary of a poll cycle.
func logResult(logger *slog.Logger, r slackpkg.PollResult) {
	logger.Info("poll cycle complete",
		slog.Int("channels_polled", r.ChannelsPolled),
		slog.Int("threads_polled", r.ThreadsPolled),
		slog.Int("raw_events_written", r.RawEventsWritten),
		slog.Int("inbox_new_written", r.InboxNewWritten),
		slog.Int("update_pings_written", r.UpdatePingsWritten),
		slog.Int("anomalies_written", r.AnomaliesWritten),
		slog.Int("errors", r.Errors))
}

