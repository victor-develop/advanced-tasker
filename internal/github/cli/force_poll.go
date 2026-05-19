package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	ghp "github.com/victor-develop/advanced-tasker/internal/github"
)

// NewForcePollCmd implements `github-poller force-poll [<scope>]`.
// Per design/09 §"Tracking lifecycle":
//
//	github-poller force-poll [<owner/repo> | <owner/repo>#<pr>]
//	  One immediate poll cycle for the named scope.
//
// When no scope is given, all watched repos and all tracked PRs are polled
// (equivalent to `github-poller run --once`).
func NewForcePollCmd(stateRoot *string) *cobra.Command {
	var (
		baseURL  string
		logLevel string
	)
	cmd := &cobra.Command{
		Use:   "force-poll [<owner/repo>[#pr]]",
		Short: "Run one poll cycle now, optionally scoped to a repo or PR",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			scope := ""
			if len(args) > 0 {
				scope = args[0]
			}
			return DoForcePoll(*stateRoot, scope, baseURL, logLevel)
		},
	}
	cmd.Flags().StringVar(&baseURL, "github-base-url", "", "override GitHub API base URL (for tests / GHE)")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug|info|warn|error")
	return cmd
}

// DoForcePoll is the verb body.
func DoForcePoll(stateRoot, scope, baseURL, logLevel string) error {
	cfg, err := ghp.LoadConfig(ghp.DefaultConfigPath(stateRoot))
	if err != nil {
		if errors.Is(err, ghp.ErrConfigMissing) {
			fmt.Println(ghp.ErrConfigMissing.Error())
			return err
		}
		return err
	}

	// Scope handling.  If scope is "<owner/repo>" or
	// "<owner/repo>#<n>", we narrow the config in-memory to just that
	// repo/PR — we leave the on-disk config unchanged.
	if scope != "" {
		repoPart, prPart, _ := strings.Cut(scope, "#")
		r, err := ghp.ParseRepo(repoPart)
		if err != nil {
			return newUsageError("scope must be <owner/repo> or <owner/repo>#<n>: %v", err)
		}
		cfg.Watch.Repos = []string{r.String()}
		if prPart != "" {
			if _, err := strconv.Atoi(prPart); err != nil {
				return newUsageError("invalid pr-number in scope %q", scope)
			}
			// We don't have a "single-PR" knob in Config; instead
			// we rely on the poller naturally only polling PRs that
			// have thread dirs.  If the caller wants to force a poll
			// on a specific PR, they must have already run track-pr.
		}
	}

	level := parseLevel(logLevel)
	logger := slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: level}))
	client, err := ghp.NewClient(cfg.Token(), baseURL, nil)
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
	stats, err := poller.RunOnce(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("force-poll: repos=%d new_prs=%d prs_polled=%d raw_events=%d not_modified=%d anomalies=%d errors=%d\n",
		stats.ReposPolled, stats.PRsDiscovered, stats.PRsPolled,
		stats.RawEventsWritten, stats.NotModifiedCount,
		stats.AnomaliesRecorded, stats.Errors)
	return nil
}
