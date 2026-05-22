package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// StateLayout encapsulates the directory roots the writer touches under
// state/. design/02 and design/08 define the contract.
type StateLayout struct {
	// StateRoot is the absolute path to state/.
	StateRoot string
}

// ThreadsDir returns state/threads.
func (s StateLayout) ThreadsDir() string { return filepath.Join(s.StateRoot, "threads") }

// ThreadDir returns state/threads/<id>.
func (s StateLayout) ThreadDir(id string) string {
	return filepath.Join(s.ThreadsDir(), id)
}

// RawDir returns state/threads/<id>/raw.
func (s StateLayout) RawDir(id string) string {
	return filepath.Join(s.ThreadDir(id), "raw")
}

// MetaPath returns state/threads/<id>/meta.json.
func (s StateLayout) MetaPath(id string) string {
	return filepath.Join(s.ThreadDir(id), "meta.json")
}

// DirtyPath returns state/threads/<id>/.dirty.
func (s StateLayout) DirtyPath(id string) string {
	return filepath.Join(s.ThreadDir(id), ".dirty")
}

// InboxNewDir returns state/inbox/new.
func (s StateLayout) InboxNewDir() string {
	return filepath.Join(s.StateRoot, "inbox", "new")
}

// InboxUpdatesDir returns state/inbox/updates.
func (s StateLayout) InboxUpdatesDir() string {
	return filepath.Join(s.StateRoot, "inbox", "updates")
}

// SourcesSlackDir returns state/sources/slack.
func (s StateLayout) SourcesSlackDir() string {
	return filepath.Join(s.StateRoot, "sources", "slack")
}

// CursorsDir returns state/sources/slack/cursors.
func (s StateLayout) CursorsDir() string {
	return filepath.Join(s.SourcesSlackDir(), "cursors")
}

// ThreadID builds a slack thread id per design/02 §ID conventions and
// design/08 — slack-<channel-id>-<thread_ts>. For a top-level message with
// no thread, thread_ts equals ts.
func ThreadID(channelID, threadTS string) string {
	return "slack-" + channelID + "-" + threadTS
}

// Event is the normalized form persisted under raw/<ts>.json. It captures
// everything we care to retain — we keep blocks/reactions/subtype as raw
// JSON to avoid lossy parsing.
type Event struct {
	ID                 string          `json:"id"`
	Source             string          `json:"source"`
	CapturedAt         string          `json:"captured_at"`
	Channel            string          `json:"channel"`
	TS                 string          `json:"ts"`
	ThreadTS           string          `json:"thread_ts,omitempty"`
	User               string          `json:"user,omitempty"`
	UserName           string          `json:"user_name,omitempty"`
	Text               string          `json:"text"`
	Blocks             json.RawMessage `json:"blocks,omitempty"`
	Reactions          json.RawMessage `json:"reactions,omitempty"`
	Subtype            string          `json:"subtype,omitempty"`
	IsTopLevelInThread bool            `json:"is_top_level_in_thread"`
	Permalink          string          `json:"permalink,omitempty"`
}

// Meta is the threads/<id>/meta.json schema per design/02.
type Meta struct {
	ID             string   `json:"id"`
	Source         string   `json:"source"`
	URL            string   `json:"url,omitempty"`
	CreatedAt      string   `json:"created_at"`
	LastEventAt    string   `json:"last_event_at"`
	OwnerTask      *string  `json:"owner_task"`
	Participants   []string `json:"participants"`
	TrackingSince  string   `json:"tracking_since"`
}

// InboxItem is the inbox/new/<id>.json schema for new (untracked) threads.
type InboxItem struct {
	ID         string         `json:"id"`
	Source     string         `json:"source"`
	Kind       string         `json:"kind"`
	ReceivedAt string         `json:"received_at"`
	Summary    string         `json:"summary"`
	Ref        InboxRef       `json:"ref"`
	RawInline  map[string]any `json:"raw_inline,omitempty"`
}

// InboxRef is the lightweight reference block embedded in InboxItem.
type InboxRef struct {
	Channel  string  `json:"channel"`
	TS       string  `json:"ts"`
	ThreadTS *string `json:"thread_ts"`
	User     string  `json:"user,omitempty"`
}

// UpdatePing is the inbox/updates/<thread-id>-<latest-ts>.json schema —
// a lightweight ping that a tracked thread has new replies.
type UpdatePing struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	Kind       string `json:"kind"`
	ReceivedAt string `json:"received_at"`
	ThreadID   string `json:"thread_id"`
	LatestTS   string `json:"latest_ts"`
	NewCount   int    `json:"new_count"`
}

// Writer encapsulates the filesystem write side of the poller. All methods
// are safe to call from multiple goroutines as long as they target
// distinct thread IDs.
type Writer struct {
	Layout StateLayout
	// WriteUpdatePings controls whether inbox/updates pings are emitted.
	// Default false (the spec marks this optional).
	WriteUpdatePings bool
	// Clock allows tests to inject a deterministic time. If nil, time.Now
	// is used.
	Clock func() time.Time
}

// NewWriter constructs a Writer rooted at stateRoot.
func NewWriter(stateRoot string) *Writer {
	return &Writer{Layout: StateLayout{StateRoot: stateRoot}}
}

func (w *Writer) now() time.Time {
	if w.Clock != nil {
		return w.Clock()
	}
	return time.Now().UTC()
}

// ThreadIsTracked reports whether state/threads/<id>/ exists. Pollers use
// this to decide between writing into a tracked-thread raw dir vs. dropping
// a top-level message into inbox/new.
func (w *Writer) ThreadIsTracked(threadID string) bool {
	info, err := os.Stat(w.Layout.ThreadDir(threadID))
	if err != nil {
		return false
	}
	return info.IsDir()
}

// RawEventExists reports whether state/threads/<id>/raw/<ts>.json already
// exists. Pollers MUST check this before writing (design/08 §Dedup).
func (w *Writer) RawEventExists(threadID, ts string) bool {
	_, err := os.Stat(filepath.Join(w.Layout.RawDir(threadID), ts+".json"))
	return err == nil
}

// WriteRawEvent writes an event to state/threads/<id>/raw/<ts>.json.
// If the file already exists, it is left untouched (idempotent overlap).
// Returns wrote=true if a new file was created.
func (w *Writer) WriteRawEvent(threadID string, ev *Event) (wrote bool, err error) {
	rawDir := w.Layout.RawDir(threadID)
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir raw dir: %w", err)
	}
	path := filepath.Join(rawDir, ev.TS+".json")
	if _, err := os.Stat(path); err == nil {
		// Dedup: do not overwrite. design/08 §Dedup.
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if ev.CapturedAt == "" {
		ev.CapturedAt = w.now().Format(time.RFC3339)
	}
	if ev.Source == "" {
		ev.Source = "slack"
	}
	b, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return false, err
	}
	if err := atomicWrite(path, append(b, '\n'), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// TouchDirty creates an empty .dirty marker in state/threads/<id>/. It is
// safe to call repeatedly — we just bump mtime if the file exists, which
// is what the rollup updater watches.
func (w *Writer) TouchDirty(threadID string) error {
	dir := w.Layout.ThreadDir(threadID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := w.Layout.DirtyPath(threadID)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	now := w.now()
	if err := os.Chtimes(path, now, now); err != nil {
		return err
	}
	return nil
}

// EnsureMeta writes meta.json for a tracked thread if it does not already
// exist. If it does, it updates last_event_at and unions participants.
//
// earliestTS, latestTS are Slack ts strings used to derive timestamps.
// participants is the union of usernames or user IDs observed.
func (w *Writer) EnsureMeta(threadID, channelID, threadTS, permalink string,
	earliestTS, latestTS string, participants []string) error {

	path := w.Layout.MetaPath(threadID)
	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		earliestISO, _ := SlackTSToISO(earliestTS)
		latestISO, _ := SlackTSToISO(latestTS)
		nowISO := w.now().Format(time.RFC3339)
		m := Meta{
			ID:            threadID,
			Source:        "slack",
			URL:           permalink,
			CreatedAt:     earliestISO,
			LastEventAt:   latestISO,
			OwnerTask:     nil,
			Participants:  dedupSorted(participants),
			TrackingSince: nowISO,
		}
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		return atomicWrite(path, append(b, '\n'), 0o644)
	case err != nil:
		return err
	}
	var m Meta
	if err := json.Unmarshal(existing, &m); err != nil {
		return fmt.Errorf("parse existing meta.json for %s: %w", threadID, err)
	}
	if latestTS != "" {
		latestISO, _ := SlackTSToISO(latestTS)
		if latestISO > m.LastEventAt {
			m.LastEventAt = latestISO
		}
	}
	if m.URL == "" && permalink != "" {
		m.URL = permalink
	}
	merged := unionStrings(m.Participants, participants)
	m.Participants = merged
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'), 0o644)
}

// WriteInboxNew writes a new-thread item to state/inbox/new/<id>.json.
// If the file already exists, it is left untouched (idempotent overlap).
func (w *Writer) WriteInboxNew(item *InboxItem) (wrote bool, err error) {
	if err := os.MkdirAll(w.Layout.InboxNewDir(), 0o755); err != nil {
		return false, err
	}
	path := filepath.Join(w.Layout.InboxNewDir(), item.ID+".json")
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if item.ReceivedAt == "" {
		item.ReceivedAt = w.now().Format(time.RFC3339)
	}
	if item.Source == "" {
		item.Source = "slack"
	}
	if item.Kind == "" {
		item.Kind = "new"
	}
	b, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return false, err
	}
	if err := atomicWrite(path, append(b, '\n'), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// WriteUpdatePing writes one inbox/updates ping per (thread, poll cycle).
// The filename collapses to <thread-id>-<latest-ts>.json; if two pings have
// the same latest_ts they no-op (idempotent).
func (w *Writer) WriteUpdatePing(threadID, latestTS string, newCount int) error {
	if !w.WriteUpdatePings {
		return nil
	}
	if err := os.MkdirAll(w.Layout.InboxUpdatesDir(), 0o755); err != nil {
		return err
	}
	name := threadID + "-" + latestTS + ".json"
	path := filepath.Join(w.Layout.InboxUpdatesDir(), name)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	p := UpdatePing{
		ID:         threadID + "-" + latestTS,
		Source:     "slack",
		Kind:       "update",
		ReceivedAt: w.now().Format(time.RFC3339),
		ThreadID:   threadID,
		LatestTS:   latestTS,
		NewCount:   newCount,
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'), 0o644)
}

// ListTrackedThreads enumerates the slack-* directories under
// state/threads/. Returned IDs are basenames (no path).
func (w *Writer) ListTrackedThreads() ([]string, error) {
	entries, err := os.ReadDir(w.Layout.ThreadsDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "slack-") {
			continue
		}
		ids = append(ids, name)
	}
	sort.Strings(ids)
	return ids, nil
}

// ParseThreadID splits "slack-<channel>-<thread_ts>" into (channel, ts).
// Channel IDs in Slack do not contain '-' (they are letters+digits), so we
// split on the LAST '-' to be safe in case channel ID conventions change.
func ParseThreadID(id string) (channel, ts string, ok bool) {
	if !strings.HasPrefix(id, "slack-") {
		return "", "", false
	}
	rest := strings.TrimPrefix(id, "slack-")
	idx := strings.LastIndex(rest, "-")
	if idx < 0 {
		return "", "", false
	}
	return rest[:idx], rest[idx+1:], true
}

// SlackTSToISO converts a Slack ts string ("1715814123.001200") to RFC3339
// UTC. Returns the input unchanged if parsing fails.
func SlackTSToISO(ts string) (string, error) {
	if ts == "" {
		return "", nil
	}
	f, err := strconv.ParseFloat(ts, 64)
	if err != nil {
		return ts, err
	}
	sec := int64(math.Floor(f))
	nsec := int64((f - math.Floor(f)) * 1e9)
	return time.Unix(sec, nsec).UTC().Format(time.RFC3339), nil
}

func dedupSorted(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	m := make(map[string]struct{}, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		m[s] = struct{}{}
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func unionStrings(a, b []string) []string {
	combined := make([]string, 0, len(a)+len(b))
	combined = append(combined, a...)
	combined = append(combined, b...)
	return dedupSorted(combined)
}
