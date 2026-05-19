// Command slack-poller is the Track B Slack ingestion daemon for
// advanced-tasker.
//
// It polls Slack for new top-level messages and thread replies, writes raw
// event files under state/threads/slack-*/raw/, and drops new-thread items
// into state/inbox/new/. It performs NO LLM calls and NEVER posts
// messages — it is strictly read-only against Slack.
//
// Subcommands: watch, unwatch, track-thread, untrack-thread, status,
// force-poll (see design/08-slack-poller.md §"Tracking lifecycle
// commands"). With no subcommand the binary runs the polling daemon;
// `--once` exits after one cycle.
//
// See design/08-slack-poller.md for the full spec.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/victor-develop/advanced-tasker/internal/slack/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	os.Exit(cli.Execute(ctx, os.Args[1:]))
}
