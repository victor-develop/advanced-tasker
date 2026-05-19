// github-poller is the Track C ingestion daemon for advanced-tasker.
//
// It polls GitHub for tracked PRs and discovers new PRs, persisting raw
// events to state/threads/github-*/raw/<event-id>.json and touching the
// .dirty marker after each cycle.  See design/09-github-poller.md for the
// full spec, including the C6 tracking lifecycle subcommands implemented
// here (`watch`, `unwatch`, `track-pr`, `untrack-pr`, `status`,
// `force-poll`).
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	ghp "github.com/victor-develop/advanced-tasker/internal/github"
	cli "github.com/victor-develop/advanced-tasker/internal/github/cli"
)

func main() {
	root := &cobra.Command{
		Use:   "github-poller",
		Short: "Track C: GitHub PR ingestion daemon and lifecycle CLI",
		Long: `github-poller polls GitHub for tracked PRs and writes raw events
under state/threads/github-*/.  See design/09-github-poller.md.

Lifecycle subcommands (C6):
  watch       Add a repo to state/sources/github/config.yaml watch.repos
  unwatch     Remove a repo and clear its cursors
  track-pr    Promote an inbox/new PR to a tracked PR
  untrack-pr  Stop polling a PR (optionally archive the thread)
  status      Show tracked repos + PRs + cursors
  force-poll  Run one cycle immediately for the named scope
`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	var stateRoot string
	root.PersistentFlags().StringVar(&stateRoot, "state-root", "state",
		"path to state/ directory (alias: --state-dir for harness CLI consistency)")
	// Accept --state-dir as an alias.  See design/03 §"Global flags".
	root.PersistentFlags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		if strings.EqualFold(name, "state-dir") {
			return pflag.NormalizedName("state-root")
		}
		return pflag.NormalizedName(name)
	})

	runCmd := cli.NewRunCmd(&stateRoot)
	root.AddCommand(runCmd)
	root.AddCommand(cli.NewWatchCmd(&stateRoot))
	root.AddCommand(cli.NewUnwatchCmd(&stateRoot))
	root.AddCommand(cli.NewTrackPRCmd(&stateRoot))
	root.AddCommand(cli.NewUntrackPRCmd(&stateRoot))
	root.AddCommand(cli.NewStatusCmd(&stateRoot))
	root.AddCommand(cli.NewForcePollCmd(&stateRoot))

	// Round-1 binary supported flags directly without a subcommand
	// (e.g. `github-poller --once`).  Preserve that by inheriting the
	// `run` subcommand's flags on the root, and dispatching to its
	// RunE when called with no positional subcommand.  See scripts/smoke.sh.
	root.Flags().AddFlagSet(runCmd.Flags())
	root.RunE = runCmd.RunE

	if err := root.Execute(); err != nil {
		// ErrConfigMissing was already printed verbatim by the run /
		// force-poll handlers; don't double-print.  See the round-2
		// agent brief: "exit 1 with the literal message".
		if !errors.Is(err, ghp.ErrConfigMissing) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(cli.ExitCodeFor(err))
	}
}
