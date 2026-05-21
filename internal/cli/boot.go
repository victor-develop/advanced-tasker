package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/victor-develop/advanced-tasker/internal/config"
	"github.com/victor-develop/advanced-tasker/internal/gitops"
	"github.com/victor-develop/advanced-tasker/internal/ids"
	"github.com/victor-develop/advanced-tasker/internal/sources"
	"github.com/victor-develop/advanced-tasker/internal/state"
	"github.com/victor-develop/advanced-tasker/internal/store"
)

// bootEnvVars lists the env-vars `harness boot` reports on. Driver-side
// env-var checks (e.g. ANTHROPIC_API_KEY) live in internal/llm — boot
// only probes presence to inform the operator.
var bootEnvVars = []string{
	"ANTHROPIC_API_KEY",
	"SLACK_BOT_TOKEN",
	"GITHUB_TOKEN",
}

// newBootCmd implements `harness boot`: the interactive first-run that
// detects state + env, walks the operator through opt-in watches, seeds
// one goal, forces outbox.sender_enabled=false, and prints next steps.
//
// Round-3 D4 — pure Go, no LLM call. The `--non-interactive` flag is
// available so tests (and CI) can run boot with sensible defaults.
func newBootCmd(opts *Options) *cobra.Command {
	var nonInteractive bool
	var defaultGoal string
	c := &cobra.Command{
		Use:   "boot",
		Short: "Interactive first-run setup (env probe, watch list, first goal, safety gates)",
		Long: `harness boot walks an operator through the first-run setup:

  1. Detects (and optionally creates) the state directory.
  2. Probes ANTHROPIC_API_KEY / SLACK_BOT_TOKEN / GITHUB_TOKEN presence.
  3. Offers to watch one Slack channel and one GitHub repo (only if the
     matching token is set).
  4. Creates a first goal (default title: "My first goal").
  5. Forces outbox.sender_enabled=false so the first autopilot run is
     safe — no external messages will leave the host until the operator
     opts in via 'harness config set outbox.sender_enabled true'.
  6. Optionally prints the autopilot start command for the operator to
     run themselves; boot will not exec autopilot without explicit
     confirmation.

Pass --non-interactive to take defaults for every prompt (handy for
fixtures, CI, and tests).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBoot(opts, cmd.InOrStdin(), cmd.OutOrStdout(), nonInteractive, defaultGoal)
		},
	}
	c.Flags().BoolVar(&nonInteractive, "non-interactive", false, "take defaults for every prompt; never block on stdin")
	c.Flags().StringVar(&defaultGoal, "goal", "", "override the default first-goal title (default: My first goal)")
	return c
}

// runBoot executes the boot workflow. Kept as a free function so tests
// can call it with controlled stdin/stdout buffers and check the exact
// state-directory side effects.
func runBoot(opts *Options, in io.Reader, out io.Writer, nonInteractive bool, defaultGoal string) error {
	if defaultGoal == "" {
		defaultGoal = "My first goal"
	}
	reader := bufio.NewReader(in)

	root, err := state.Resolve(opts.StateRoot)
	if err != nil {
		return errf(ExitUsage, "resolve state root: %v", err)
	}

	// Step 1: state dir + harness init.
	if !state.IsInitialized(root) {
		fmt.Fprintf(out, "[detect] state directory %s is not initialized.\n", root)
		ans := prompt(reader, out, fmt.Sprintf("Run `harness init` at %s? [Y/n] ", root), "y", nonInteractive)
		if !isYes(ans) {
			return errf(ExitUsage, "aborted: state is not initialized at %s", root)
		}
		if err := state.Init(root); err != nil {
			return errf(ExitGit, "init failed: %v", err)
		}
		fmt.Fprintf(out, "[ok] initialized state at %s\n", root)
	} else {
		fmt.Fprintf(out, "[ok] state directory %s is initialized.\n", root)
	}

	// Step 2: env probe.
	envStatus := map[string]bool{}
	for _, name := range bootEnvVars {
		v := os.Getenv(name)
		envStatus[name] = v != ""
		switch name {
		case "ANTHROPIC_API_KEY":
			if v == "" {
				fmt.Fprintf(out, "[missing] ANTHROPIC_API_KEY is not set — autopilot will need --driver fake until you set it.\n")
			} else {
				fmt.Fprintf(out, "[ok] ANTHROPIC_API_KEY is set\n")
			}
		case "SLACK_BOT_TOKEN":
			if v == "" {
				fmt.Fprintf(out, "[missing] SLACK_BOT_TOKEN is not set — Slack will be skipped\n")
			} else {
				fmt.Fprintf(out, "[ok] SLACK_BOT_TOKEN is set\n")
			}
		case "GITHUB_TOKEN":
			if v == "" {
				fmt.Fprintf(out, "[missing] GITHUB_TOKEN is not set — GitHub will be skipped\n")
			} else {
				fmt.Fprintf(out, "[ok] GITHUB_TOKEN is set\n")
			}
		}
	}

	// Step 3: optional Slack watch (only if token present).
	slackList := []string{}
	if envStatus["SLACK_BOT_TOKEN"] {
		if _, err := sources.Init(root, sources.SourceSlack); err != nil {
			return errf(ExitValidation, "config init slack: %v", err)
		}
		chID := prompt(reader, out, "Watch a Slack channel? Enter channel ID (e.g. C0492) or blank to skip: ", "", nonInteractive)
		chID = strings.TrimSpace(chID)
		if chID != "" {
			reason := prompt(reader, out, "  --reason (optional, blank for none): ", "", nonInteractive)
			reason = strings.TrimSpace(reason)
			if err := sources.WatchChannel(root, chID, reason); err != nil {
				return errf(ExitValidation, "watch slack: %v", err)
			}
			commitConfig(root, "sources/slack/config.yaml", fmt.Sprintf("boot: watch slack-channel %s", chID))
			slackList = append(slackList, chID)
			fmt.Fprintf(out, "[ok] watching slack/%s\n", chID)
		}
	}

	// Step 4: optional GitHub watch (only if token present).
	githubList := []string{}
	if envStatus["GITHUB_TOKEN"] {
		if _, err := sources.Init(root, sources.SourceGitHub); err != nil {
			return errf(ExitValidation, "config init github: %v", err)
		}
		repo := prompt(reader, out, "Watch a GitHub repo? Enter owner/repo (e.g. acme/api) or blank to skip: ", "", nonInteractive)
		repo = strings.TrimSpace(repo)
		if repo != "" {
			if !strings.Contains(repo, "/") {
				fmt.Fprintf(out, "[skip] %q is not in owner/repo form — not watched\n", repo)
			} else {
				if err := sources.WatchRepo(root, repo); err != nil {
					return errf(ExitValidation, "watch github: %v", err)
				}
				commitConfig(root, "sources/github/config.yaml", fmt.Sprintf("boot: watch github-repo %s", repo))
				githubList = append(githubList, repo)
				fmt.Fprintf(out, "[ok] watching github/%s\n", repo)
			}
		}
	}

	// Step 5: first goal.
	title := prompt(reader, out, fmt.Sprintf("First goal title [%s]: ", defaultGoal), defaultGoal, nonInteractive)
	title = strings.TrimSpace(title)
	if title == "" {
		title = defaultGoal
	}
	goalID, err := createGoalForBoot(root, title)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "[ok] created %s — %q\n", goalID, title)

	// Step 6: force outbox.sender_enabled=false. Always — even if
	// previously true — because boot is the round-3 safety net.
	if err := setSenderEnabled(root, false); err != nil {
		return errf(ExitValidation, "force sender_enabled=false: %v", err)
	}
	fmt.Fprintf(out, "[safety] outbox.sender_enabled set to false — no external messages will be sent until you opt in\n")

	// Step 7: autopilot prompt — never exec without explicit yes.
	startCmd := "harness autopilot start --driver claude-p --duration 5m"
	ans := prompt(reader, out, fmt.Sprintf("Run `%s` now? [y/N] ", startCmd), "n", nonInteractive)
	if isYes(ans) {
		fmt.Fprintf(out, "Will run: %s\n", startCmd)
		confirm := prompt(reader, out, "Confirm? [y/N] ", "n", nonInteractive)
		if isYes(confirm) {
			// Even with confirmation, boot only PRINTS the command. The
			// design (D4 step 7) explicitly says "do NOT exec without
			// explicit yes"; boot defers actual execution so the user
			// stays in control of their tty.
			fmt.Fprintf(out, "[note] run the command above from your shell to start the 5-minute autopilot.\n")
		}
	}

	// Step 8: one-paragraph summary.
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "Summary: watch list: slack=%v, github=%v; goal=%s; sender_enabled=false; next: %s\n",
		slackList, githubList, goalID, startCmd)
	return nil
}

// prompt prints msg, reads one line of input, and falls back to def if
// the line is empty. In non-interactive mode the default is always
// taken (no stdin read).
func prompt(r *bufio.Reader, out io.Writer, msg, def string, nonInteractive bool) string {
	if nonInteractive {
		// Echo the default so the transcript is honest about what was
		// chosen.
		fmt.Fprintf(out, "%s%s\n", msg, def)
		return def
	}
	fmt.Fprint(out, msg)
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return def
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return def
	}
	return line
}

func isYes(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	return s == "y" || s == "yes"
}

// createGoalForBoot mirrors runCreateGoal without going through cobra.
func createGoalForBoot(root, title string) (string, error) {
	var goalID string
	err := state.WithStateLock(root, func() error {
		id, err := ids.NextTaskID(idsTasksRoot(root))
		if err != nil {
			return fmt.Errorf("next task id: %w", err)
		}
		now := time.Now().UTC()
		st := store.Status{
			ID:            id,
			State:         store.StateReady,
			Priority:      store.PriorityNormal,
			BlockedOn:     []string{},
			LinkedThreads: []string{},
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		body := fmt.Sprintf("# %s — %s\n\n%s\n", id, title, title)
		if err := store.CreateTask(root, st, body); err != nil {
			return err
		}
		r := gitops.Repo{Dir: root}
		_ = r.Add(filepath.Join("tasks", id))
		if _, err := r.Commit(fmt.Sprintf("boot: create goal %s — %s", id, title)); err != nil && !errors.Is(err, gitops.ErrNothingToCommit) {
			return err
		}
		goalID = id
		return nil
	})
	if err != nil {
		return "", errf(ExitValidation, "create goal: %v", err)
	}
	return goalID, nil
}

// setSenderEnabled writes outbox.sender_enabled=<v> to state/config.yaml
// and commits the change. Boot uses this with v=false to enforce the
// round-3 safety net.
func setSenderEnabled(root string, v bool) error {
	cfgPath := filepath.Join(root, "config.yaml")
	return state.WithStateLock(root, func() error {
		tree, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		if err := config.Set(tree, "outbox.sender_enabled", boolStr(v)); err != nil {
			return err
		}
		if err := config.Save(cfgPath, tree); err != nil {
			return err
		}
		r := gitops.Repo{Dir: root}
		_ = r.Add("config.yaml")
		if _, err := r.Commit(fmt.Sprintf("boot: outbox.sender_enabled=%v", v)); err != nil && !errors.Is(err, gitops.ErrNothingToCommit) {
			return err
		}
		return nil
	})
}

func boolStr(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// commitConfig is a tiny wrapper to keep boot's body readable.
func commitConfig(root, relPath, msg string) {
	r := gitops.Repo{Dir: root}
	_ = r.Add(relPath)
	_, _ = r.Commit(msg)
}
