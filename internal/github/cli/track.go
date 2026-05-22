package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	ghp "github.com/victor-develop/advanced-tasker/internal/github"
)

// NewTrackPRCmd implements `github-poller track-pr <owner/repo> <pr-number>`.
// Per design/09 §"Tracking lifecycle":
//
//	Promote an inbox/new PR to a tracked PR:
//	  - Read state/inbox/new/github-<owner>-<repo>-pr-<n>.json
//	  - mkdir state/threads/github-<owner>-<repo>-pr-<n>/raw
//	  - Write meta.json (per design/09 §"Meta initialization")
//	  - Touch .dirty
//	  - Delete the inbox/new entry
//	Idempotent.
func NewTrackPRCmd(stateRoot *string) *cobra.Command {
	return &cobra.Command{
		Use:   "track-pr <owner/repo> <pr-number>",
		Short: "Promote an inbox/new PR to a tracked PR",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			n, err := strconv.Atoi(args[1])
			if err != nil || n <= 0 {
				return newUsageError("invalid pr-number %q", args[1])
			}
			return DoTrackPR(*stateRoot, args[0], n)
		},
	}
}

// DoTrackPR is the verb body.
func DoTrackPR(stateRoot, repoArg string, number int) error {
	r, err := ghp.ParseRepo(repoArg)
	if err != nil {
		return newUsageError("%v", err)
	}
	id := ghp.ThreadID(r, number)
	writer := ghp.NewWriter(stateRoot)

	threadDir := filepath.Join(stateRoot, "threads", id)
	rawDir := filepath.Join(threadDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", rawDir, err)
	}

	// If meta.json already exists we're in idempotent re-run territory:
	// just make sure .dirty exists and the inbox/new entry is gone.
	existing, err := writer.LoadMeta(id)
	if err != nil {
		return err
	}
	if existing == nil {
		// Look for the inbox/new entry to harvest meta.  If it's
		// missing we still seed a minimal meta — the operator may
		// have run `harness thread track` directly.
		inboxPath := filepath.Join(stateRoot, "inbox", "new", id+".json")
		now := time.Now().UTC()
		meta := &ghp.Meta{
			ID:            id,
			Source:        "github",
			URL:           fmt.Sprintf("https://github.com/%s/%s/pull/%d", r.Owner, r.Repo, number),
			CreatedAt:     now,
			LastEventAt:   now,
			Participants:  []string{},
			TrackingSince: now,
		}
		if data, rerr := os.ReadFile(inboxPath); rerr == nil {
			var in ghp.InboxNew
			if err := json.Unmarshal(data, &in); err == nil {
				meta.URL = in.Ref.URL
				if in.Ref.Author != "" {
					meta.Participants = []string{in.Ref.Author}
				}
				meta.CreatedAt = in.ReceivedAt
			}
		} else if !errors.Is(rerr, os.ErrNotExist) {
			return fmt.Errorf("read inbox/new entry: %w", rerr)
		}
		if err := writer.SaveMeta(id, meta); err != nil {
			return err
		}
	}

	if err := writer.TouchDirty(id); err != nil {
		return err
	}

	// Remove the inbox/new entry (idempotent).
	inboxPath := filepath.Join(stateRoot, "inbox", "new", id+".json")
	if err := os.Remove(inboxPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove inbox/new entry: %w", err)
	}

	if err := mkdirsForCursors(stateRoot); err != nil {
		return err
	}

	fmt.Printf("tracked %s\n", id)
	return nil
}

// NewUntrackPRCmd implements `github-poller untrack-pr <owner/repo> <pr-number> [--archive]`.
func NewUntrackPRCmd(stateRoot *string) *cobra.Command {
	var archive bool
	cmd := &cobra.Command{
		Use:   "untrack-pr <owner/repo> <pr-number>",
		Short: "Stop polling a PR (optionally archive its thread dir)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			n, err := strconv.Atoi(args[1])
			if err != nil || n <= 0 {
				return newUsageError("invalid pr-number %q", args[1])
			}
			return DoUntrackPR(*stateRoot, args[0], n, archive)
		},
	}
	cmd.Flags().BoolVar(&archive, "archive", false,
		"also rename thread dir to state/threads/_archive/")
	return cmd
}

// DoUntrackPR is the verb body.
func DoUntrackPR(stateRoot, repoArg string, number int, archive bool) error {
	r, err := ghp.ParseRepo(repoArg)
	if err != nil {
		return newUsageError("%v", err)
	}
	id := ghp.ThreadID(r, number)
	cursors := ghp.NewCursorStore(stateRoot)
	if err := cursors.DeletePR(r, number); err != nil {
		return err
	}
	if archive {
		dst, aerr := ghp.NewWriter(stateRoot).ArchiveThread(id)
		if aerr != nil {
			return aerr
		}
		if dst == "" {
			fmt.Printf("untracked %s (no thread dir to archive)\n", id)
			return nil
		}
		fmt.Printf("untracked %s; archived to %s\n", id, dst)
		return nil
	}
	fmt.Printf("untracked %s; thread dir left in place\n", id)
	return nil
}
