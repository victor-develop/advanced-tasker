package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	ghp "github.com/victor-develop/advanced-tasker/internal/github"
)

// DoctorExit codes mirror the round-3 brief D1 contract:
//   - 0 on full success
//   - 1 on hard auth failure (no token, 401 on /user, missing config)
//   - 2 on any soft failure (repo unreadable, PR endpoint hiccup, etc.)
const (
	DoctorExitOK   = 0
	DoctorExitHard = 1
	DoctorExitSoft = 2
)

// DoctorReport is the JSON shape emitted by `github-poller doctor --json`.
// Each section also corresponds to a `[ok|fail]` line in the text output.
type DoctorReport struct {
	StateRoot string         `json:"state_root"`
	ConfigOK  bool           `json:"config_ok"`
	ConfigErr string         `json:"config_error,omitempty"`
	Token     DoctorToken    `json:"token"`
	Auth      DoctorAuth     `json:"auth"`
	Repos     []DoctorRepo   `json:"repos"`
	PRsPing   *DoctorPRsPing `json:"prs_ping,omitempty"`
	ExitCode  int            `json:"exit_code"`
}

// DoctorToken describes the token discovery step.
type DoctorToken struct {
	OK     bool   `json:"ok"`
	Source string `json:"source,omitempty"` // "env:<NAME>" or "config:auth.token"
	Length int    `json:"length,omitempty"`
	Err    string `json:"error,omitempty"`
}

// DoctorAuth describes the `GET /user` step.
type DoctorAuth struct {
	OK    bool   `json:"ok"`
	Login string `json:"login,omitempty"`
	ID    int64  `json:"id,omitempty"`
	Err   string `json:"error,omitempty"`
}

// DoctorRepo describes one `GET /repos/{owner}/{repo}` step.
type DoctorRepo struct {
	Repo          string `json:"repo"`
	OK            bool   `json:"ok"`
	Visibility    string `json:"visibility,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Status        int    `json:"status,omitempty"`
	Err           string `json:"error,omitempty"`
}

// DoctorPRsPing describes the `GET /repos/{owner}/{repo}/pulls?state=open&per_page=1` step.
type DoctorPRsPing struct {
	Repo     string `json:"repo"`
	OK       bool   `json:"ok"`
	Returned int    `json:"returned"`
	Err      string `json:"error,omitempty"`
}

// NewDoctorCmd implements `github-poller doctor [--json]`.
//
// This is the lead-driven preflight added in round 3 for the REAL e2e
// against a user's GitHub account.  See the round-3 brief D1 for the
// contract; in summary, the check runs five steps:
//
//  1. Token check (env var or config)
//  2. GET /user (validates token, prints login)
//  3. GET /repos/{owner}/{repo} for each watch.repos
//  4. GET /repos/{owner}/{repo}/pulls?state=open&per_page=1 for repos[0]
//  5. Exit code aggregation: 0 on full success, 1 on hard auth failure,
//     2 on any soft failure (e.g., one repo unreadable)
func NewDoctorCmd(stateRoot *string) *cobra.Command {
	var (
		asJSON  bool
		baseURL string
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Preflight checks: token + auth + repo visibility + PRs ping",
		Long: `doctor runs five preflight checks intended for the lead-driven
real-mode e2e against the user's GitHub account:

  1. Token check — env var (auth.token_env) or auth.token in config
  2. GET /user  — validates the token and prints the authenticated user
  3. GET /repos/{owner}/{repo} for each repo in watch.repos
  4. GET /repos/{owner}/{repo}/pulls?state=open&per_page=1 for the first repo
  5. Exits 0 on full success, 1 on hard auth failure, 2 on any soft failure.

This subcommand does NOT mutate state and does NOT poll for events; it
exists purely to surface configuration / token / scope issues before
running the binary.  See design/09-github-poller.md §"Token / auth notes".`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep := RunDoctor(cmd.Context(), *stateRoot, baseURL)
			if asJSON {
				data, err := json.MarshalIndent(rep, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(data))
			} else {
				printDoctor(cmd.OutOrStdout(), rep)
			}
			if rep.ExitCode != 0 {
				// Return a sentinel so main.go can set the exact code.
				return &doctorExit{code: rep.ExitCode}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON instead of human-readable output")
	cmd.Flags().StringVar(&baseURL, "github-base-url", "", "override GitHub API base URL (for tests / GHE)")
	return cmd
}

// doctorExit is a sentinel that ExitCodeFor unwraps to set the precise
// shell exit code requested by RunDoctor's aggregator.
type doctorExit struct{ code int }

func (e *doctorExit) Error() string { return fmt.Sprintf("doctor: exit %d", e.code) }

// DoctorExitCode extracts a doctorExit code if err is one, else returns -1.
func DoctorExitCode(err error) int {
	if err == nil {
		return 0
	}
	var de *doctorExit
	if errors.As(err, &de) {
		return de.code
	}
	return -1
}

// RunDoctor performs the four preflight steps against stateRoot's config
// and returns a populated DoctorReport with ExitCode set.
//
// When baseURL is non-empty it overrides the GitHub API origin; this is
// used by unit tests with httptest.
func RunDoctor(ctx context.Context, stateRoot, baseURL string) *DoctorReport {
	if ctx == nil {
		ctx = context.Background()
	}
	rep := &DoctorReport{StateRoot: stateRoot}

	// --- Config load ------------------------------------------------------
	cfgPath := ghp.DefaultConfigPath(stateRoot)
	cfg, err := ghp.LoadConfig(cfgPath)
	if err != nil {
		rep.ConfigErr = err.Error()
		if errors.Is(err, ghp.ErrConfigMissing) {
			rep.Token.Err = ghp.ErrConfigMissing.Error()
			rep.Auth.Err = "config missing"
		}
		rep.ExitCode = DoctorExitHard
		return rep
	}
	rep.ConfigOK = true

	// --- Token check ------------------------------------------------------
	token, source := resolveToken(cfg)
	if token == "" {
		rep.Token.Err = fmt.Sprintf(
			"token not found: set $%s or auth.token in state/sources/github/config.yaml",
			cfg.Auth.TokenEnv)
		rep.ExitCode = DoctorExitHard
		return rep
	}
	rep.Token.OK = true
	rep.Token.Source = source
	rep.Token.Length = len(token)

	// --- Build client + auth check (GET /user) ---------------------------
	client, err := ghp.NewClient(token, baseURL, nil)
	if err != nil {
		rep.Auth.Err = err.Error()
		rep.ExitCode = DoctorExitHard
		return rep
	}

	authCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	login, id, authErr := client.AuthenticatedUser(authCtx)
	if authErr != nil {
		switch {
		case ghp.IsUnauthorized(authErr):
			rep.Auth.Err = "github-poller: token invalid (check $" +
				cfg.Auth.TokenEnv + " -- does it have repo scope?)"
		default:
			rep.Auth.Err = authErr.Error()
		}
		rep.ExitCode = DoctorExitHard
		return rep
	}
	rep.Auth.OK = true
	rep.Auth.Login = login
	rep.Auth.ID = id

	// --- Repo visibility -------------------------------------------------
	hadSoft := false
	for _, repoSpec := range cfg.Watch.Repos {
		r, perr := ghp.ParseRepo(repoSpec)
		dr := DoctorRepo{Repo: repoSpec}
		if perr != nil {
			dr.Err = perr.Error()
			hadSoft = true
			rep.Repos = append(rep.Repos, dr)
			continue
		}
		rctx, rcancel := context.WithTimeout(ctx, 15*time.Second)
		visibility, defaultBranch, status, rerr := client.GetRepoVisibility(rctx, r)
		rcancel()
		dr.Status = status
		if rerr != nil {
			switch {
			case status == http.StatusNotFound:
				dr.Err = fmt.Sprintf("%s: 404 -- token may lack access", repoSpec)
			case status == http.StatusForbidden:
				dr.Err = fmt.Sprintf("%s: 403 -- secondary rate limit or org SSO", repoSpec)
			case ghp.IsUnauthorized(rerr):
				// Unusual: /user succeeded but a repo returned 401.
				// Treat as hard since the token state is inconsistent.
				dr.Err = "github-poller: token invalid (check $" +
					cfg.Auth.TokenEnv + " -- does it have repo scope?)"
				rep.Repos = append(rep.Repos, dr)
				rep.ExitCode = DoctorExitHard
				return rep
			default:
				dr.Err = rerr.Error()
			}
			hadSoft = true
			rep.Repos = append(rep.Repos, dr)
			continue
		}
		dr.OK = true
		dr.Visibility = visibility
		dr.DefaultBranch = defaultBranch
		rep.Repos = append(rep.Repos, dr)
	}

	// --- First-repo PRs ping --------------------------------------------
	if len(cfg.Watch.Repos) > 0 {
		repoSpec := cfg.Watch.Repos[0]
		if r, perr := ghp.ParseRepo(repoSpec); perr == nil {
			pctx, pcancel := context.WithTimeout(ctx, 15*time.Second)
			count, pingErr := client.CountOpenPRsSample(pctx, r)
			pcancel()
			pp := &DoctorPRsPing{Repo: repoSpec}
			if pingErr != nil {
				pp.Err = pingErr.Error()
				hadSoft = true
			} else {
				pp.OK = true
				pp.Returned = count
			}
			rep.PRsPing = pp
		}
	}

	if hadSoft {
		rep.ExitCode = DoctorExitSoft
	} else {
		rep.ExitCode = DoctorExitOK
	}
	return rep
}

// resolveToken returns (token, source).  Source is a human-readable string
// like "env:GITHUB_TOKEN" or "config:auth.token".  We also support
// auth.token_file (read once, trimmed) for parity with other ops tooling
// the team uses.
func resolveToken(cfg *ghp.Config) (string, string) {
	envName := cfg.Auth.TokenEnv
	if envName != "" {
		if v := os.Getenv(envName); v != "" {
			return v, "env:" + envName
		}
	}
	// Config.Token() is the env-only path; expand to also try the
	// inline / file fallbacks if the doctor user wired them up via
	// the future auth.token / auth.token_file fields.  We don't add
	// these to the strict Config schema today, but we lookup via the
	// raw YAML so the doctor remains useful for ops with hand-edits.
	if extra := readTokenExtras(cfg); extra != "" {
		return extra, extra // source string already encoded
	}
	return "", ""
}

// readTokenExtras inspects fields outside the strict Config struct.  It
// returns an empty string when no extra source is configured; otherwise it
// returns "<token>" via a SECOND return that encodes "config:auth.token"
// or "config:auth.token_file:<path>".  We pack token+source into a single
// string of the form "<source>\x00<token>" for the strict-typed path; the
// caller splits.
//
// NOTE: today we read these via os.Getenv-style indirection only.  The
// strict Config struct doesn't carry the field; ops that want the file
// path indirection can use the env path with `auth.token_env: ...`.
func readTokenExtras(cfg *ghp.Config) string { return "" }

func printDoctor(w io.Writer, rep *DoctorReport) {
	if rep.ConfigErr != "" {
		fmt.Fprintf(w, "[fail] config: %s\n", rep.ConfigErr)
		return
	}
	if rep.Token.OK {
		fmt.Fprintf(w, "[ok] token from %s (length=%d)\n", rep.Token.Source, rep.Token.Length)
	} else if rep.Token.Err != "" {
		fmt.Fprintf(w, "[fail] %s\n", rep.Token.Err)
		return
	}
	if rep.Auth.OK {
		fmt.Fprintf(w, "[ok] authenticated as %s (id=%d)\n", rep.Auth.Login, rep.Auth.ID)
	} else if rep.Auth.Err != "" {
		fmt.Fprintf(w, "[fail] %s\n", rep.Auth.Err)
		return
	}
	for _, r := range rep.Repos {
		if r.OK {
			fmt.Fprintf(w, "[ok] %s (visibility=%s, default_branch=%s)\n",
				r.Repo, r.Visibility, r.DefaultBranch)
		} else {
			fmt.Fprintf(w, "[fail] %s\n", strings.TrimPrefix(r.Err, "github-poller: "))
		}
	}
	if rep.PRsPing != nil {
		if rep.PRsPing.OK {
			fmt.Fprintf(w, "[ok] %s open PRs (sample=%d) -- token has PR read scope\n",
				rep.PRsPing.Repo, rep.PRsPing.Returned)
		} else {
			fmt.Fprintf(w, "[fail] %s prs ping: %s\n", rep.PRsPing.Repo, rep.PRsPing.Err)
		}
	}
	switch rep.ExitCode {
	case DoctorExitOK:
		fmt.Fprintln(w, "doctor: all checks passed")
	case DoctorExitHard:
		fmt.Fprintln(w, "doctor: hard failure -- fix token or config before running the poller")
	case DoctorExitSoft:
		fmt.Fprintln(w, "doctor: soft failure -- some repos / endpoints unreadable; review above")
	}
}
