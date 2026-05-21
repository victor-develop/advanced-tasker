package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
)

// DoctorReport is the structured output of `slack-poller doctor --json`. It
// is the same shape printed in the human-readable form, just emitted as
// indented JSON.
type DoctorReport struct {
	ConfigPath string        `json:"config_path"`
	Token      TokenCheck    `json:"token"`
	Auth       AuthCheck     `json:"auth"`
	Channels   []ChannelInfo `json:"channels"`
	Summary    string        `json:"summary"`
	// ExitCode is the exit code the doctor returns: 0 ok, 1 hard fail (token
	// invalid / no channel access), 2 soft signals (e.g. empty history).
	ExitCode int `json:"exit_code"`
}

// TokenCheck reports the result of step 1.
type TokenCheck struct {
	OK     bool   `json:"ok"`
	Source string `json:"source"`           // "env:$NAME" | "config:auth.token" | "file:<path>" | ""
	Length int    `json:"length,omitempty"` // bytes; non-zero on OK
	Reason string `json:"reason,omitempty"` // present on failure
}

// AuthCheck reports the result of step 2.
type AuthCheck struct {
	OK        bool   `json:"ok"`
	BotName   string `json:"bot_name,omitempty"`
	TeamName  string `json:"team_name,omitempty"`
	BotID     string `json:"bot_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
	SlackCode string `json:"slack_code,omitempty"`
}

// ChannelInfo reports the result of step 3+4 for one channel.
type ChannelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	OK            bool   `json:"ok"`
	BotHasAccess  bool   `json:"bot_has_access"`
	HistoryOK     bool   `json:"history_ok"`
	HistoryEmpty  bool   `json:"history_empty,omitempty"`
	Reason        string `json:"reason,omitempty"`
	SlackCode     string `json:"slack_code,omitempty"`
	// Hint is an actionable next step for the operator.
	Hint string `json:"hint,omitempty"`
}

// DoctorOptions holds the dependency injection seams used by tests. Production
// callers leave both nil: BuildClient is filled in by newDoctorCmd at runtime.
type DoctorOptions struct {
	// BuildClient overrides the default Slack client constructor. Tests pass
	// a mock APIClient via this hook.
	BuildClient func(cfg *slackpkg.Config, apiURL string) (slackpkg.APIClient, error)
}

// testDoctorBuildClient lets tests inject a custom client builder into the
// production `slack-poller doctor` subcommand. Production code never sets
// this. The unit-test layer sets it before constructing the cobra tree and
// resets it after each test via t.Cleanup.
var testDoctorBuildClient func(cfg *slackpkg.Config, apiURL string) (slackpkg.APIClient, error)

// newDoctorCmd implements `slack-poller doctor [--json]`.
//
// Behavior (per round-3 brief §D2):
//  1. Token check  - read token from configured source.
//  2. Auth check   - call auth.test.
//  3. Channel ping - call conversations.info for the first watched channel.
//  4. History ping - call conversations.history limit=1 for that channel.
//  5. Exit code    - 0 ok / 1 hard fail / 2 soft fail.
//
// The output (human-readable) prints one `[ok]` / `[fail]` line per check
// and an actionable hint on failure. With `--json`, the same data is
// emitted as a single DoctorReport object on stdout.
func newDoctorCmd(opts *Options) *cobra.Command {
	return newDoctorCmdWithOpts(opts, DoctorOptions{})
}

// newDoctorCmdWithOpts allows tests to inject a custom client builder.
func newDoctorCmdWithOpts(opts *Options, dopts DoctorOptions) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run first-boot sanity checks (token, auth, channel access)",
		Long: `doctor performs a series of sanity checks against Slack to validate
the slack-poller is configured correctly:

  1. token check  -- token resolves from auth.token_env / auth.token / auth.token_file
  2. auth check   -- auth.test succeeds against Slack
  3. channel ping -- conversations.info shows the bot can see watch.channels[0]
  4. history ping -- conversations.history limit=1 succeeds

Exit code is 0 on all-pass, 1 on hard failure (token invalid or no channel
access), 2 on soft signal (e.g. empty history).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			stateDir := resolveStateDir(opts.StateDir)
			cfgPath := configPath(stateDir)
			cfg, _, err := loadConfigOrExit(stateDir)
			if err != nil {
				return err
			}
			builder := dopts.BuildClient
			if builder == nil {
				builder = testDoctorBuildClient
			}
			if builder == nil {
				builder = defaultDoctorBuildClient
			}
			report := runDoctor(cmd.Context(), cfgPath, cfg, opts.APIURL, builder)
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return errf(ExitUsage, "encode JSON: %v", err)
				}
			} else {
				printDoctorReport(cmd.OutOrStdout(), report)
			}
			if report.ExitCode == 0 {
				return nil
			}
			return &CommandError{
				Code: ExitCode(report.ExitCode),
				Err:  errors.New(report.Summary),
			}
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"emit JSON DoctorReport instead of human-readable output")
	return cmd
}

// defaultDoctorBuildClient is the production APIClient builder used when no
// override is injected.
func defaultDoctorBuildClient(cfg *slackpkg.Config, apiURL string) (slackpkg.APIClient, error) {
	token, err := cfg.ResolveToken()
	if err != nil {
		return nil, err
	}
	return slackpkg.NewSlackGoClientWithConfig(token, slackpkg.SlackGoClientConfig{
		APIURL:            apiURL,
		MaxRetries429:     0,
		RateLimitFallback: cfg.Backoff.OnRateLimit.Duration,
		MaxBackoff:        cfg.Backoff.MaxBackoff.Duration,
	}), nil
}

// runDoctor performs the four checks and builds a DoctorReport. It never
// panics. The returned report.ExitCode is the recommended process exit
// code.
func runDoctor(ctx context.Context, cfgPath string, cfg *slackpkg.Config,
	apiURL string, build func(*slackpkg.Config, string) (slackpkg.APIClient, error)) *DoctorReport {

	if ctx == nil {
		ctx = context.Background()
	}
	rep := &DoctorReport{ConfigPath: cfgPath}

	// Step 1: token check.
	rep.Token = checkToken(cfg)
	if !rep.Token.OK {
		rep.ExitCode = 1
		rep.Summary = "token not found: set $" + cfg.Auth.TokenEnv +
			" or auth.token in state/sources/slack/config.yaml"
		if cfg.Auth.TokenEnv == "" {
			rep.Summary = "token not found: set $SLACK_BOT_TOKEN or auth.token in state/sources/slack/config.yaml"
		}
		return rep
	}

	// Build the client. If we have a token but the builder still errors,
	// surface that as a token-stage failure (config-level issue).
	client, err := build(cfg, apiURL)
	if err != nil {
		rep.ExitCode = 1
		rep.Token.Reason = err.Error()
		rep.Token.OK = false
		rep.Summary = "could not construct Slack client: " + err.Error()
		return rep
	}

	// Step 2: auth check.
	authCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rep.Auth = checkAuth(authCtx, client)
	if !rep.Auth.OK {
		rep.ExitCode = 1
		rep.Summary = "token invalid (check SLACK_BOT_TOKEN, bot must be in channel)"
		if rep.Auth.SlackCode == "missing_scope" {
			scope := rep.Auth.Reason
			rep.Summary = "missing scope: " + scope
		}
		return rep
	}

	// Steps 3+4: per-channel check. Only the first watched channel is
	// probed (the brief says "for the first channel in watch.channels");
	// the loop below would naturally support all channels, but to keep the
	// CLI fast on cold boot we stop after the first probe.
	if len(cfg.Watch.Channels) == 0 {
		rep.ExitCode = 2
		rep.Summary = "no channels configured: add one via `slack-poller watch <channel-id>`"
		return rep
	}

	ch := cfg.Watch.Channels[0]
	probeCtx, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel2()
	ci := checkChannel(probeCtx, client, ch.ID, rep.Auth.BotName)
	rep.Channels = append(rep.Channels, ci)
	if !ci.OK {
		rep.ExitCode = 1
		rep.Summary = "channel " + ch.ID + " inaccessible: " + ci.Reason
		return rep
	}

	if ci.HistoryEmpty {
		rep.ExitCode = 2
		rep.Summary = "all checks passed; channel " + ch.ID +
			" has no recent messages (soft signal, not an error)"
		return rep
	}

	rep.ExitCode = 0
	rep.Summary = "all checks passed"
	return rep
}

// checkToken implements step 1.
func checkToken(cfg *slackpkg.Config) TokenCheck {
	if cfg.Auth.TokenEnv != "" {
		v := os.Getenv(cfg.Auth.TokenEnv)
		if v != "" {
			return TokenCheck{OK: true, Source: "env $" + cfg.Auth.TokenEnv, Length: len(v)}
		}
		// Fall through to other sources but remember what we tried.
	}
	if cfg.Auth.Token != "" {
		return TokenCheck{OK: true, Source: "config auth.token", Length: len(cfg.Auth.Token)}
	}
	if cfg.Auth.TokenFile != "" {
		_, err := os.Stat(cfg.Auth.TokenFile)
		if err == nil {
			return TokenCheck{OK: true, Source: "file " + cfg.Auth.TokenFile,
				Length: -1} // unknown without reading; safe to omit
		}
		return TokenCheck{OK: false, Source: "file " + cfg.Auth.TokenFile,
			Reason: "token file unreadable: " + err.Error()}
	}
	source := ""
	if cfg.Auth.TokenEnv != "" {
		source = "env $" + cfg.Auth.TokenEnv
	}
	return TokenCheck{
		OK:     false,
		Source: source,
		Reason: "no token: tried auth.token_env, auth.token, auth.token_file",
	}
}

// checkAuth implements step 2.
func checkAuth(ctx context.Context, client slackpkg.APIClient) AuthCheck {
	info, err := client.AuthTest(ctx)
	if err != nil {
		ac := AuthCheck{OK: false, Reason: err.Error()}
		if code := slackpkg.AuthFailCode(err); code != "" {
			ac.SlackCode = code
		}
		if scope := slackpkg.MissingScope(err); scope != "" {
			ac.SlackCode = "missing_scope"
			ac.Reason = scope
		}
		return ac
	}
	return AuthCheck{
		OK:       true,
		BotName:  info.User,
		TeamName: info.Team,
		BotID:    info.BotID,
		UserID:   info.UserID,
	}
}

// checkChannel implements steps 3+4. botName is the bot username from
// auth.test, used to format an actionable `/invite @bot` hint.
func checkChannel(ctx context.Context, client slackpkg.APIClient,
	channelID, botName string) ChannelInfo {

	info, err := client.ConversationInfo(ctx, channelID)
	out := ChannelInfo{ID: channelID}
	if err != nil {
		out.Reason = err.Error()
		// Try to extract a Slack code for a more actionable hint.
		msg := err.Error()
		switch {
		case strings.Contains(msg, "channel_not_found"):
			out.SlackCode = "channel_not_found"
			out.Hint = "channel id does not exist or the bot cannot see it; check the ID"
		case strings.Contains(msg, "not_in_channel"):
			out.SlackCode = "not_in_channel"
			out.Hint = "invite the bot via `/invite @" + safeName(botName) +
				"` in #" + channelID
		case strings.Contains(msg, "missing_scope"):
			out.SlackCode = "missing_scope"
			if scope := slackpkg.MissingScope(err); scope != "" {
				out.Hint = "grant the bot the `" + scope +
					"` scope and reinstall the app"
			}
		case strings.Contains(msg, "is_archived"):
			out.SlackCode = "is_archived"
			out.Hint = "channel is archived; unarchive it or remove from watch list"
		}
		return out
	}
	out.Name = info.Name
	out.BotHasAccess = info.IsMember
	if info.IsArchived {
		out.Reason = "channel is archived"
		out.SlackCode = "is_archived"
		out.Hint = "unarchive the channel or remove it from watch.channels"
		return out
	}
	if !info.IsMember {
		out.Reason = "bot is not a member of " + channelID
		out.SlackCode = "not_in_channel"
		out.Hint = "invite the bot via `/invite @" + safeName(botName) +
			"` in #" + safeName(info.Name)
		return out
	}

	// History probe.
	page, err := client.History(ctx, slackpkg.HistoryParams{
		ChannelID: channelID,
		Limit:     1,
	})
	if err != nil {
		// Channel info passed but history failed — usually missing scope.
		out.Reason = "history probe failed: " + err.Error()
		if scope := slackpkg.MissingScope(err); scope != "" {
			out.SlackCode = "missing_scope"
			out.Hint = "grant the bot the `" + scope +
				"` scope and reinstall the app"
		}
		return out
	}
	out.HistoryOK = true
	out.HistoryEmpty = len(page.Messages) == 0
	out.OK = true
	return out
}

// safeName returns a fallback when name is empty so the hint string still
// reads sensibly.
func safeName(name string) string {
	if name == "" {
		return "<bot>"
	}
	return name
}

// printDoctorReport renders the human-readable form. The contract: one
// `[ok]` or `[fail]` line per check, followed by an optional hint, ending
// with a one-line summary.
func printDoctorReport(w io.Writer, r *DoctorReport) {
	fmt.Fprintf(w, "config: %s\n", r.ConfigPath)

	// Token.
	if r.Token.OK {
		src := r.Token.Source
		if r.Token.Length > 0 {
			fmt.Fprintf(w, "[ok]   token from %s (length=%d)\n", src, r.Token.Length)
		} else {
			fmt.Fprintf(w, "[ok]   token from %s\n", src)
		}
	} else {
		fmt.Fprintf(w, "[fail] %s\n", r.Token.Reason)
	}

	// Auth.
	switch {
	case !r.Token.OK:
		// nothing else to print; the token failure dominates.
	case r.Auth.OK:
		fmt.Fprintf(w, "[ok]   authenticated as %s in workspace %s\n",
			r.Auth.BotName, r.Auth.TeamName)
	default:
		if r.Auth.SlackCode == "missing_scope" {
			fmt.Fprintf(w, "[fail] auth.test missing scope: %s\n", r.Auth.Reason)
		} else {
			fmt.Fprintf(w, "[fail] auth.test failed (%s): %s\n",
				orDash(r.Auth.SlackCode), r.Auth.Reason)
		}
	}

	// Channels.
	for _, c := range r.Channels {
		name := c.Name
		if name == "" {
			name = "?"
		}
		switch {
		case c.OK && c.HistoryEmpty:
			fmt.Fprintf(w, "[ok]   channel %s (%s) -- bot has access; history is empty\n",
				c.ID, name)
		case c.OK:
			fmt.Fprintf(w, "[ok]   channel %s (%s) -- bot has access\n",
				c.ID, name)
		default:
			fmt.Fprintf(w, "[fail] channel %s: %s -- %s\n", c.ID,
				orDash(c.SlackCode), c.Reason)
			if c.Hint != "" {
				fmt.Fprintf(w, "       hint: %s\n", c.Hint)
			}
		}
	}

	fmt.Fprintf(w, "summary: %s (exit %d)\n", r.Summary, r.ExitCode)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
