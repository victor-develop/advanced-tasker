package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// CursorStore manages per-channel and per-thread cursors atomically.
//
// Layout (per design/08 §Cursors):
//
//	<root>/channels/<channel-id>.json     { "last_ts": "..." }
//	<root>/threads/<thread-id>.json       { "last_reply_ts": "..." }
//
// All writes are crash-safe: write to .tmp, fsync, rename.
type CursorStore struct {
	Root string // typically state/sources/slack/cursors
}

// NewCursorStore constructs a CursorStore rooted at root and ensures the
// subdirectories exist.
func NewCursorStore(root string) (*CursorStore, error) {
	c := &CursorStore{Root: root}
	for _, sub := range []string{"channels", "threads"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir cursors/%s: %w", sub, err)
		}
	}
	return c, nil
}

type channelCursor struct {
	LastTS string `json:"last_ts"`
}

type threadCursor struct {
	LastReplyTS string `json:"last_reply_ts"`
}

// GetChannelCursor returns the last_ts for the given channel, or "" if
// no cursor file exists yet (fresh start).
func (c *CursorStore) GetChannelCursor(channelID string) (string, error) {
	var cur channelCursor
	if err := c.readJSON(filepath.Join(c.Root, "channels", channelID+".json"), &cur); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return cur.LastTS, nil
}

// SetChannelCursor atomically writes the cursor for the given channel.
func (c *CursorStore) SetChannelCursor(channelID, ts string) error {
	return c.writeJSON(filepath.Join(c.Root, "channels", channelID+".json"),
		channelCursor{LastTS: ts})
}

// GetThreadCursor returns the last_reply_ts for the given thread-id, or ""
// if no cursor file exists yet. threadID is the full ID like
// "slack-C0492-1715814123.001200".
func (c *CursorStore) GetThreadCursor(threadID string) (string, error) {
	var cur threadCursor
	if err := c.readJSON(filepath.Join(c.Root, "threads", threadID+".json"), &cur); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return cur.LastReplyTS, nil
}

// SetThreadCursor atomically writes the thread cursor.
func (c *CursorStore) SetThreadCursor(threadID, ts string) error {
	return c.writeJSON(filepath.Join(c.Root, "threads", threadID+".json"),
		threadCursor{LastReplyTS: ts})
}

func (c *CursorStore) readJSON(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

func (c *CursorStore) writeJSON(path string, in any) error {
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'), 0o644)
}

// atomicWrite writes data to path via a sibling .tmp file followed by a
// rename. fsync ensures durability before the rename.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup if we fail before rename.
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}
