package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	ghp "github.com/victor-develop/advanced-tasker/internal/github"
)

// NewWatchCmd implements `github-poller watch <owner/repo>`.
// Per design/09 §"Tracking lifecycle": add the repo to watch.repos,
// no-op (exit 0) if already watched.
func NewWatchCmd(stateRoot *string) *cobra.Command {
	return &cobra.Command{
		Use:   "watch <owner/repo>",
		Short: "Add a repo to the watch list (state/sources/github/config.yaml)",
		Long: `watch appends <owner/repo> to watch.repos in
state/sources/github/config.yaml.  Idempotent: re-running on an already-
watched repo exits 0 with a notice and does not duplicate.

The config file must already exist; run 'harness config init github' first
if it doesn't.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return DoWatch(*stateRoot, args[0])
		},
	}
}

// DoWatch is the verb body, exposed for unit tests.
func DoWatch(stateRoot, repoArg string) error {
	r, err := ghp.ParseRepo(repoArg)
	if err != nil {
		return newUsageError("%v", err)
	}
	cfgPath, err := requireConfig(stateRoot)
	if err != nil {
		return err
	}
	root, err := ghp.LoadConfigRaw(cfgPath)
	if err != nil {
		return err
	}
	existing := ghp.WatchRepos(root)
	for _, e := range existing {
		if strings.EqualFold(e, r.String()) {
			fmt.Printf("already watching %s; no changes\n", r)
			return nil
		}
	}
	merged := append([]string{}, existing...)
	merged = append(merged, r.String())
	sort.Strings(merged)
	ghp.SetWatchRepos(root, merged)
	if err := ghp.SaveConfigRaw(cfgPath, root); err != nil {
		return err
	}
	if err := mkdirsForCursors(stateRoot); err != nil {
		return err
	}
	fmt.Printf("watching %s (%d total)\n", r, len(merged))
	return nil
}

// NewUnwatchCmd implements `github-poller unwatch <owner/repo>`.
// Per design/09: remove from watch.repos, clear repo + PR cursors for
// that repo.
func NewUnwatchCmd(stateRoot *string) *cobra.Command {
	return &cobra.Command{
		Use:   "unwatch <owner/repo>",
		Short: "Remove a repo from the watch list and clear its cursors",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return DoUnwatch(*stateRoot, args[0])
		},
	}
}

// DoUnwatch is the verb body, exposed for unit tests.
func DoUnwatch(stateRoot, repoArg string) error {
	r, err := ghp.ParseRepo(repoArg)
	if err != nil {
		return newUsageError("%v", err)
	}
	cfgPath, err := requireConfig(stateRoot)
	if err != nil {
		return err
	}
	root, err := ghp.LoadConfigRaw(cfgPath)
	if err != nil {
		return err
	}
	existing := ghp.WatchRepos(root)
	found := false
	out := make([]string, 0, len(existing))
	for _, e := range existing {
		if strings.EqualFold(e, r.String()) {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		fmt.Printf("not watching %s; no changes\n", r)
		return nil
	}
	ghp.SetWatchRepos(root, out)
	if err := ghp.SaveConfigRaw(cfgPath, root); err != nil {
		return err
	}
	// Clear cursors so a future re-watch starts clean.
	cursors := ghp.NewCursorStore(stateRoot)
	if err := cursors.DeleteRepo(r); err != nil {
		return err
	}
	fmt.Printf("unwatched %s; cursors cleared\n", r)
	return nil
}
