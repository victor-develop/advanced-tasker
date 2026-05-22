package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
)

// newWatchCmd implements `slack-poller watch <channel-id> [--reason ...]`.
//
// Behavior (per design/08 §"Tracking lifecycle commands"):
//   - Append a WatchedChannel to state/sources/slack/config.yaml.
//   - If the channel is already watched, exit 0 with a notice (idempotent).
//   - Reload happens on the next daemon poll cycle (i.e., the next restart
//     in v1 — there is no SIGHUP hot-reload).
func newWatchCmd(opts *Options) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "watch <channel-id>",
		Short: "Add a channel to the watch list",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			channel := args[0]
			if channel == "" {
				return errf(ExitValidation, "channel id is empty")
			}
			stateDir := resolveStateDir(opts.StateDir)
			cfg, path, err := loadConfigOrExit(stateDir)
			if err != nil {
				return err
			}
			for _, ch := range cfg.Watch.Channels {
				if ch.ID == channel {
					fmt.Fprintf(cmd.OutOrStdout(),
						"channel %s already watched (no change)\n", channel)
					return nil
				}
			}
			cfg.Watch.Channels = append(cfg.Watch.Channels,
				slackpkg.WatchedChannel{ID: channel, Reason: reason})
			if err := slackpkg.SaveConfig(path, cfg); err != nil {
				return errf(ExitUsage, "save config: %v", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"watching channel %s (reason=%q)\n", channel, reason)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "",
		"free-form reason recorded in config.yaml")
	return cmd
}

// newUnwatchCmd implements `slack-poller unwatch <channel-id>`.
//
// Behavior:
//   - Remove the channel from watch.channels.
//   - Delete the channel cursor file so a future re-add starts fresh.
//   - Idempotent: removing an already-absent channel exits 0.
func newUnwatchCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "unwatch <channel-id>",
		Short: "Remove a channel from the watch list (clears its cursor)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			channel := args[0]
			stateDir := resolveStateDir(opts.StateDir)
			cfg, path, err := loadConfigOrExit(stateDir)
			if err != nil {
				return err
			}
			filtered := cfg.Watch.Channels[:0]
			removed := false
			for _, ch := range cfg.Watch.Channels {
				if ch.ID == channel {
					removed = true
					continue
				}
				filtered = append(filtered, ch)
			}
			cfg.Watch.Channels = filtered
			if !removed {
				fmt.Fprintf(cmd.OutOrStdout(),
					"channel %s not watched (no change)\n", channel)
				return nil
			}
			if err := slackpkg.SaveConfig(path, cfg); err != nil {
				return errf(ExitUsage, "save config: %v", err)
			}
			// Clear the cursor file. Errors are surfaced but non-fatal —
			// the config has already been updated.
			curPath := filepath.Join(stateDir, "sources", "slack",
				"cursors", "channels", channel+".json")
			if rmErr := removeIfExists(curPath); rmErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: could not remove cursor %s: %v\n", curPath, rmErr)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"unwatched channel %s; cursor cleared\n", channel)
			return nil
		},
	}
}

// removeIfExists deletes path if it exists; returns nil if the file is
// already absent.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
