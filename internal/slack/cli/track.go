package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
)

// newTrackThreadCmd implements `slack-poller track-thread <channel> <ts>`.
//
// Behavior (per design/08 §"Tracking lifecycle commands"):
//  1. Read state/inbox/new/slack-<C>-<ts>.json
//  2. mkdir -p state/threads/slack-<C>-<ts>/raw
//  3. Move the raw_inline payload into raw/<ts>.json (in the Event schema
//     so the rollup updater sees a consistent shape)
//  4. Write meta.json (per design/08 §"Meta initialization")
//  5. Touch .dirty
//  6. Delete the inbox/new entry
//
// Idempotency:
//   - If raw/<ts>.json already exists, the command is a no-op (exit 0).
//   - If the inbox/new entry is missing AND the thread does not exist yet,
//     exit 2 with a clear error (caller asked to promote something we
//     don't have).
//   - If the inbox/new entry is missing but the thread already exists, exit
//     0 (already promoted).
func newTrackThreadCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "track-thread <channel-id> <thread_ts>",
		Short: "Promote an inbox/new item to a tracked thread",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			channel, ts := args[0], args[1]
			if channel == "" || ts == "" {
				return errf(ExitValidation, "channel-id and thread_ts required")
			}
			stateDir := resolveStateDir(opts.StateDir)
			// Loading config also validates "harness config init slack has
			// run" — but the spec doesn't require it for thread tracking;
			// we still call it so that running the verb on an uninitialized
			// state directory surfaces the same exit-1 message.
			if _, _, err := loadConfigOrExit(stateDir); err != nil {
				return err
			}
			threadID := slackpkg.ThreadID(channel, ts)
			threadDir := filepath.Join(stateDir, "threads", threadID)
			rawPath := filepath.Join(threadDir, "raw", ts+".json")
			inboxPath := filepath.Join(stateDir, "inbox", "new",
				threadID+".json")

			rawExists := fileExists(rawPath)
			threadExists := dirExists(threadDir)
			inboxExists := fileExists(inboxPath)

			// Already promoted? Idempotent no-op.
			if rawExists {
				fmt.Fprintf(cmd.OutOrStdout(),
					"thread %s already tracked (raw/%s.json exists)\n",
					threadID, ts)
				return nil
			}
			if threadExists && !inboxExists {
				// Thread dir exists but no inbox entry to consume and no
				// raw root either. Treat as already-promoted-by-another-
				// path — touch .dirty and EnsureMeta so the rollup runs.
				if err := ensureMetaAndDirty(stateDir, threadID, channel, ts, "", "U"); err != nil {
					return errf(ExitValidation, "%v", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"thread %s already exists; refreshed meta + .dirty\n",
					threadID)
				return nil
			}
			if !inboxExists {
				return errf(ExitValidation,
					"inbox/new entry %s missing; cannot promote thread %s",
					filepath.Base(inboxPath), threadID)
			}

			// Read & parse the inbox/new entry.
			item, err := readInboxItem(inboxPath)
			if err != nil {
				return errf(ExitValidation,
					"read inbox/new entry %s: %v", inboxPath, err)
			}

			// Build the Event from raw_inline.
			ev := inboxItemToEvent(item, channel, ts)
			w := slackpkg.NewWriter(stateDir)
			if _, err := w.WriteRawEvent(threadID, ev); err != nil {
				return errf(ExitUsage, "write raw event: %v", err)
			}
			// EnsureMeta: derive participants from the inbox entry.
			user := item.Ref.User
			permalink := ""
			if raw, ok := item.RawInline["permalink"].(string); ok {
				permalink = raw
			}
			if err := w.EnsureMeta(threadID, channel, ts, permalink,
				ts, ts, []string{user}); err != nil {
				return errf(ExitUsage, "ensure meta: %v", err)
			}
			if err := w.TouchDirty(threadID); err != nil {
				return errf(ExitUsage, "touch dirty: %v", err)
			}
			// Remove the inbox entry last — only if everything above
			// succeeded. The poller will not re-add it unless it sees the
			// channel-history feed include the message AND the thread dir
			// has been removed; thread dir now exists, so future polls
			// dedup via raw/<ts>.json.
			if err := os.Remove(inboxPath); err != nil &&
				!errors.Is(err, os.ErrNotExist) {
				return errf(ExitUsage,
					"remove inbox/new entry: %v", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"promoted %s -> %s\n", filepath.Base(inboxPath), threadID)
			return nil
		},
	}
}

// newUntrackThreadCmd implements `slack-poller untrack-thread <id> [--archive]`.
//
// Behavior:
//   - Without --archive: remove the thread's cursor file (so a future
//     re-track starts fresh). The thread directory is left intact for the
//     commander to handle (e.g., `harness thread untrack` archives it).
//   - With --archive: rename state/threads/<id> to
//     state/threads/_archive/<id>. The cursor is also removed.
//
// Idempotent: re-running on an already-untracked id exits 0.
func newUntrackThreadCmd(opts *Options) *cobra.Command {
	var archive bool
	cmd := &cobra.Command{
		Use:   "untrack-thread <thread-id>",
		Short: "Stop polling a tracked thread",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			threadID := args[0]
			if _, _, ok := slackpkg.ParseThreadID(threadID); !ok {
				return errf(ExitValidation,
					"invalid thread id %q (expected slack-<channel>-<ts>)",
					threadID)
			}
			stateDir := resolveStateDir(opts.StateDir)
			if _, _, err := loadConfigOrExit(stateDir); err != nil {
				return err
			}

			// Clear cursor.
			curPath := filepath.Join(stateDir, "sources", "slack",
				"cursors", "threads", threadID+".json")
			if err := removeIfExists(curPath); err != nil {
				return errf(ExitUsage,
					"remove thread cursor: %v", err)
			}

			threadDir := filepath.Join(stateDir, "threads", threadID)
			if !dirExists(threadDir) {
				fmt.Fprintf(cmd.OutOrStdout(),
					"thread %s not tracked (no change)\n", threadID)
				return nil
			}

			if archive {
				archiveBase := filepath.Join(stateDir, "threads", "_archive")
				if err := os.MkdirAll(archiveBase, 0o755); err != nil {
					return errf(ExitUsage,
						"mkdir _archive: %v", err)
				}
				dst := filepath.Join(archiveBase, threadID)
				// If a previous archive exists, suffix with a timestamp.
				if dirExists(dst) {
					dst = dst + "-" + time.Now().UTC().Format("20060102T150405Z")
				}
				if err := os.Rename(threadDir, dst); err != nil {
					return errf(ExitUsage,
						"archive thread dir: %v", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"untracked %s; archived to %s\n", threadID, dst)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"untracked %s; cursor cleared (thread dir preserved)\n",
				threadID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&archive, "archive", false,
		"rename the thread directory to threads/_archive/<id>/ as well")
	return cmd
}

// readInboxItem reads + unmarshals a state/inbox/new/<id>.json file.
func readInboxItem(path string) (*slackpkg.InboxItem, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var item slackpkg.InboxItem
	if err := json.Unmarshal(b, &item); err != nil {
		return nil, fmt.Errorf("parse inbox item: %w", err)
	}
	return &item, nil
}

// inboxItemToEvent maps an inbox/new item to an Event suitable for raw/.
// Falls back to the ref + raw_inline payload.
func inboxItemToEvent(item *slackpkg.InboxItem, channel, ts string) *slackpkg.Event {
	ev := &slackpkg.Event{
		ID:                 item.ID,
		Source:             "slack",
		Channel:            channel,
		TS:                 ts,
		ThreadTS:           ts,
		User:               item.Ref.User,
		Text:               "",
		Permalink:          "",
		IsTopLevelInThread: true,
	}
	if item.Ref.ThreadTS != nil && *item.Ref.ThreadTS != "" {
		ev.ThreadTS = *item.Ref.ThreadTS
		ev.IsTopLevelInThread = (ev.TS == ev.ThreadTS)
	}
	if v, ok := item.RawInline["text"].(string); ok {
		ev.Text = v
	}
	if v, ok := item.RawInline["permalink"].(string); ok {
		ev.Permalink = v
	}
	if v, ok := item.RawInline["blocks"]; ok {
		if b, err := json.Marshal(v); err == nil {
			ev.Blocks = b
		}
	}
	if v, ok := item.RawInline["reactions"]; ok {
		if b, err := json.Marshal(v); err == nil {
			ev.Reactions = b
		}
	}
	return ev
}

// ensureMetaAndDirty is a small helper used when a thread dir was created
// outside this command.
func ensureMetaAndDirty(stateDir, threadID, channel, ts, permalink, user string) error {
	w := slackpkg.NewWriter(stateDir)
	if err := w.EnsureMeta(threadID, channel, ts, permalink,
		ts, ts, []string{user}); err != nil {
		return fmt.Errorf("ensure meta: %w", err)
	}
	return w.TouchDirty(threadID)
}

// fileExists reports whether path exists and is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

