package cli

import (
	"strings"

	"github.com/spf13/cobra"

	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
)

// newForcePollCmd implements `slack-poller force-poll [<id>]`.
//
// Behavior (per design/08 §"Tracking lifecycle commands"):
//   - Run one poll cycle immediately for the named channel or thread.
//   - With no arg, poll everything (equivalent to `--once`).
//   - Channel IDs and thread IDs are distinguished by the `slack-` prefix on
//     thread IDs. The `slack-<channel>-<ts>` format also implies a channel.
//
// We share the existing poll machinery via Poller.OnlyTargets, which the
// daemon ignores (it's always nil for the daemon).
func newForcePollCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "force-poll [<channel-id> | <thread-id>]",
		Short: "Run one poll cycle immediately (bypasses schedule)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var targets *slackpkg.PollTargets
			if len(args) == 1 {
				targets = &slackpkg.PollTargets{
					Channels: map[string]struct{}{},
					Threads:  map[string]struct{}{},
				}
				id := args[0]
				if strings.HasPrefix(id, "slack-") {
					if _, _, ok := slackpkg.ParseThreadID(id); !ok {
						return errf(ExitValidation,
							"invalid thread id %q", id)
					}
					targets.Threads[id] = struct{}{}
				} else {
					targets.Channels[id] = struct{}{}
				}
			}
			return runPoll(cmd.Context(), opts, &runPollFlags{
				Once:    true,
				Targets: targets,
			})
		},
	}
}
