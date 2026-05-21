// Package cli implements the slack-poller cobra command tree.
//
// The default (root) action runs the polling daemon. Subcommands implement
// the lifecycle verbs (`watch`, `unwatch`, `track-thread`, `untrack-thread`,
// `status`, `force-poll`). All subcommands share the same global flags
// — `--state-dir`, `--api-url`, `--log-level`.
//
// Exit codes (per design/03 §"Output and exit codes" — slack-poller maps
// onto a subset):
//
//	0  success / idempotent no-op
//	1  usage / config-missing / IO error
//	2  validation error (e.g. invalid args, inbox/new entry missing)
//	3  contention / fatal auth (token invalid)
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
)

// Options are the shared flags + derived paths every command needs.
type Options struct {
	StateDir string
	APIURL   string
	LogLevel string
	// Stdout/Stderr allow tests to capture output; default to os.Stdout/Stderr.
	Stdout *os.File
	Stderr *os.File
}

// ExitCode lets command implementations request a non-zero exit code with
// a particular semantic.
type ExitCode int

const (
	ExitOK          ExitCode = 0
	ExitUsage       ExitCode = 1
	ExitValidation  ExitCode = 2
	ExitAuthFatal   ExitCode = 3
)

// CommandError carries a desired exit code.
type CommandError struct {
	Code ExitCode
	Err  error
}

func (e *CommandError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *CommandError) Unwrap() error { return e.Err }

func errf(code ExitCode, format string, args ...any) *CommandError {
	return &CommandError{Code: code, Err: fmt.Errorf(format, args...)}
}

// ExtractExitCode pulls the exit code from err. Defaults to 1.
func ExtractExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ce *CommandError
	if errors.As(err, &ce) {
		return int(ce.Code)
	}
	return 1
}

// NewRootCmd builds the cobra tree.
func NewRootCmd() *cobra.Command {
	opts := &Options{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	root := &cobra.Command{
		Use:   "slack-poller",
		Short: "Slack ingestion daemon for advanced-tasker (Track B)",
		Long: `slack-poller polls Slack channels and threads, writing raw event
files under state/threads/slack-*/raw/ and new-thread items under
state/inbox/new/. With no subcommand it runs as a daemon; --once exits
after one poll cycle.

See design/08-slack-poller.md for the full spec.`,
		// Default behavior with no subcommand: run the polling daemon.
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPoll(cmd.Context(), opts, &runPollFlags{
				Once: rootFlags.Once,
			})
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&opts.StateDir, "state-dir", defaultStateDir(),
		"path to state/ directory (env: HARNESS_STATE)")
	root.PersistentFlags().StringVar(&opts.APIURL, "api-url", "",
		"override Slack API URL (testing only)")
	root.PersistentFlags().StringVar(&opts.LogLevel, "log-level", "info",
		"log level (debug|info|warn|error)")

	root.Flags().BoolVar(&rootFlags.Once, "once", false,
		"run a single poll cycle then exit")

	root.AddCommand(
		newWatchCmd(opts),
		newUnwatchCmd(opts),
		newTrackThreadCmd(opts),
		newUntrackThreadCmd(opts),
		newStatusCmd(opts),
		newForcePollCmd(opts),
		newDoctorCmd(opts),
	)
	return root
}

// rootFlags holds default-action-only flags so subcommands don't surface
// them in their own --help output.
var rootFlags struct {
	Once bool
}

// Execute parses argv and runs the appropriate command, returning an
// integer exit code. Callers (cmd/slack-poller/main.go) should
// `os.Exit(cli.Execute(ctx, os.Args[1:]))`.
func Execute(ctx context.Context, args []string) int {
	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, FormatAuthError(err))
		return ExtractExitCode(err)
	}
	return 0
}

// FormatAuthError converts an error from a command into the stderr line
// printed on exit. Auth failures and missing-scope errors get a fixed
// operator-actionable phrasing so playbooks can grep on them; everything
// else flows through unchanged with a `slack-poller:` prefix.
func FormatAuthError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, slackpkg.ErrFatalAuth) || slackpkg.IsFatalAuthError(err) {
		if scope := slackpkg.MissingScope(err); scope != "" {
			return "slack-poller: missing scope: " + scope +
				" (grant via Slack app config and reinstall)"
		}
		return "slack-poller: token invalid (check SLACK_BOT_TOKEN, bot must be in channel)"
	}
	return "slack-poller: " + err.Error()
}

// resolveStateDir returns the configured state directory, defaulting to
// $HARNESS_STATE then "./state".
func resolveStateDir(stateDir string) string {
	if stateDir != "" {
		return stateDir
	}
	if env := os.Getenv("HARNESS_STATE"); env != "" {
		return env
	}
	return "state"
}

// defaultStateDir is the cobra flag default; falls back to "state" if the
// env var is unset.
func defaultStateDir() string {
	if env := os.Getenv("HARNESS_STATE"); env != "" {
		return env
	}
	return "state"
}

// configPath returns the absolute path of state/sources/slack/config.yaml.
func configPath(stateDir string) string {
	return filepath.Join(stateDir, "sources", "slack", "config.yaml")
}

// loadConfigOrExit loads the config and maps the missing-file sentinel to
// the documented exit-1 message. Any other load error is a usage error.
func loadConfigOrExit(stateDir string) (*slackpkg.Config, string, error) {
	path := configPath(stateDir)
	cfg, err := slackpkg.LoadConfig(path)
	if err != nil {
		if errors.Is(err, slackpkg.ErrConfigMissing) {
			// The literal message required by design/03 + design/10 + brief.
			return nil, path, &CommandError{
				Code: ExitUsage,
				Err:  errors.New("run 'harness config init slack' to seed config"),
			}
		}
		return nil, path, &CommandError{Code: ExitUsage, Err: err}
	}
	return cfg, path, nil
}

// newLogger builds a structured stderr JSON logger at the configured level.
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
	return slog.New(slog.NewJSONHandler(os.Stderr,
		&slog.HandlerOptions{Level: lvl}))
}

// newCursorStore builds a CursorStore rooted under stateDir.
func newCursorStore(stateDir string) (*slackpkg.CursorStore, error) {
	return slackpkg.NewCursorStore(filepath.Join(stateDir, "sources", "slack", "cursors"))
}

// newWriter builds a slack Writer for stateDir.
func newWriter(stateDir string, writeUpdatePings bool) *slackpkg.Writer {
	w := slackpkg.NewWriter(stateDir)
	w.WriteUpdatePings = writeUpdatePings
	return w
}

// buildClient constructs the Slack APIClient for a poll/force-poll command.
func buildClient(cfg *slackpkg.Config, opts *Options) (slackpkg.APIClient, error) {
	token, err := cfg.ResolveToken()
	if err != nil {
		return nil, &CommandError{Code: ExitUsage, Err: err}
	}
	// Disable in-client retries; the daemon loop handles 429 backoff
	// (per design/08 §"Error handling" — "Sleep `Retry-After` then backoff
	// per config" is the daemon's job, not the HTTP client's).
	return slackpkg.NewSlackGoClientWithConfig(token, slackpkg.SlackGoClientConfig{
		APIURL:            opts.APIURL,
		MaxRetries429:     0,
		RateLimitFallback: cfg.Backoff.OnRateLimit.Duration,
		MaxBackoff:        cfg.Backoff.MaxBackoff.Duration,
	}), nil
}
