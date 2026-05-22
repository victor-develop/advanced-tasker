package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	gh "github.com/google/go-github/v75/github"
)

// Writer owns all filesystem mutations under state/ for the GitHub poller.
//
// Hard contract (see design/02 §File mutation rules and the agent brief):
//
//	Writes ONLY under:
//	  state/threads/github-*/
//	  state/inbox/new/github-*
//	  state/inbox/updates/github-*
//	  state/sources/github/
type Writer struct {
	StateRoot string
}

// NewWriter returns a Writer rooted at <stateRoot> (typically `state/`).
func NewWriter(stateRoot string) *Writer { return &Writer{StateRoot: stateRoot} }

// ThreadID returns the canonical thread id for a PR.
func ThreadID(r RepoRef, number int) string {
	return fmt.Sprintf("github-%s-%s-pr-%d", r.Owner, r.Repo, number)
}

func (w *Writer) threadDir(id string) string {
	return filepath.Join(w.StateRoot, "threads", id)
}

func (w *Writer) rawPath(id, eventID string) string {
	return filepath.Join(w.threadDir(id), "raw", eventID+".json")
}

// RawEvent is the persisted shape per design/09 §Raw event files.  The full
// upstream payload goes in `Raw`.  Top-level fields are convenience aliases
// for the rollup updater (which is in Track A).
type RawEvent struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	CapturedAt time.Time `json:"captured_at"`
	Kind       string    `json:"kind"`
	PR         struct {
		Owner  string `json:"owner"`
		Repo   string `json:"repo"`
		Number int    `json:"number"`
	} `json:"pr"`
	Actor     string    `json:"actor,omitempty"`
	ActorID   int64     `json:"actor_id,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	Body      string    `json:"body,omitempty"`
	HTMLURL   string    `json:"html_url,omitempty"`
	// Snapshot is populated for kind=pr-state events per design/09
	// §"State change detection".  Captures the explicit subset of fields
	// the design requires so the rollup updater doesn't have to introspect
	// the full PullRequest payload.
	Snapshot *PRStateSnapshot `json:"snapshot,omitempty"`
	Raw      any              `json:"raw"`
}

// PRStateSnapshot is the explicit "delta of interest" written into
// pr-state-<updated_at>.json events.  Field set per design/09 §"State
// change detection".
type PRStateSnapshot struct {
	State              string   `json:"state"`
	Merged             bool     `json:"merged"`
	Mergeable          *bool    `json:"mergeable"`
	Labels             []string `json:"labels"`
	HeadSHA            string   `json:"head_sha"`
	BaseSHA            string   `json:"base_sha"`
	RequestedReviewers []string `json:"requested_reviewers"`
	Draft              bool     `json:"draft"`
}

// Meta is the schema in design/02 §threads/<id>/meta.json restricted to the
// fields the poller is allowed to populate.  owner_task stays nil; the
// commander sets it later.
type Meta struct {
	ID            string    `json:"id"`
	Source        string    `json:"source"`
	URL           string    `json:"url"`
	CreatedAt     time.Time `json:"created_at"`
	LastEventAt   time.Time `json:"last_event_at"`
	OwnerTask     *string   `json:"owner_task"`
	Participants  []string  `json:"participants"`
	TrackingSince time.Time `json:"tracking_since"`
}

// EventExists reports whether a raw event file is already on disk.  This is
// the secondary dedup gate after `since` overlap (design/09 §Dedup).
func (w *Writer) EventExists(id, eventID string) (bool, error) {
	_, err := os.Stat(w.rawPath(id, eventID))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// ThreadExists reports whether a tracked-PR directory exists on disk.
func (w *Writer) ThreadExists(id string) (bool, error) {
	_, err := os.Stat(w.threadDir(id))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// WriteRawEvent persists an event file (idempotent).  Returns the path written
// and a `wrote` flag indicating whether the file was newly created.  If the
// file already exists we leave it alone so concurrent re-polls don't churn it.
func (w *Writer) WriteRawEvent(id string, ev RawEvent) (path string, wrote bool, err error) {
	path = w.rawPath(id, ev.ID)
	if _, statErr := os.Stat(path); statErr == nil {
		return path, false, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", false, statErr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", false, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return "", false, fmt.Errorf("marshal raw event: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", false, fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", false, fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return path, true, nil
}

// TouchDirty creates (or refreshes mtime of) state/threads/<id>/.dirty.
func (w *Writer) TouchDirty(id string) error {
	dir := w.threadDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, ".dirty")
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("touch %s: %w", path, err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	return f.Close()
}

// LoadMeta returns the meta.json for a thread; returns (nil, nil) if missing.
func (w *Writer) LoadMeta(id string) (*Meta, error) {
	path := filepath.Join(w.threadDir(id), "meta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &m, nil
}

// SaveMeta writes meta.json atomically.
func (w *Writer) SaveMeta(id string, m *Meta) error {
	path := filepath.Join(w.threadDir(id), "meta.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	return os.Rename(tmp, path)
}

// EnsureMeta loads existing meta or initialises one from the PR object.
func (w *Writer) EnsureMeta(id string, pr *gh.PullRequest, now time.Time) (*Meta, error) {
	if pr == nil {
		return nil, fmt.Errorf("EnsureMeta requires non-nil PR")
	}
	existing, err := w.LoadMeta(id)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	m := &Meta{
		ID:            id,
		Source:        "github",
		URL:           pr.GetHTMLURL(),
		CreatedAt:     pr.GetCreatedAt().Time,
		LastEventAt:   pr.GetUpdatedAt().Time,
		Participants:  []string{pr.GetUser().GetLogin()},
		TrackingSince: now,
	}
	if err := w.SaveMeta(id, m); err != nil {
		return nil, err
	}
	return m, nil
}

// UpdateMeta loads, mutates via fn, and saves atomically.  Safe to call after
// EnsureMeta; if meta is missing the function returns an error.
func (w *Writer) UpdateMeta(id string, fn func(*Meta)) error {
	m, err := w.LoadMeta(id)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("meta.json missing for thread %s", id)
	}
	fn(m)
	return w.SaveMeta(id, m)
}

// MergeParticipants returns a sorted, de-duplicated union of two lists.
func MergeParticipants(existing []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(existing)+len(additions))
	out := make([]string, 0, len(existing)+len(additions))
	for _, list := range [][]string{existing, additions} {
		for _, s := range list {
			if s == "" {
				continue
			}
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// --- inbox writers --------------------------------------------------------

// InboxNew is the shape written to state/inbox/new/github-<owner>-<repo>-pr-<n>.json
// per design/09 §"New PRs → inbox/new".
type InboxNew struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	Kind       string    `json:"kind"`
	ReceivedAt time.Time `json:"received_at"`
	Summary    string    `json:"summary"`
	Ref        struct {
		Owner  string `json:"owner"`
		Repo   string `json:"repo"`
		Number int    `json:"number"`
		Title  string `json:"title"`
		Author string `json:"author"`
		URL    string `json:"url"`
		State  string `json:"state"`
		Draft  bool   `json:"draft"`
	} `json:"ref"`
}

// WriteInboxNew creates the inbox/new entry for a newly discovered PR.  If the
// file is already present we leave it untouched (the commander or harness owns
// promotion to tracking).
func (w *Writer) WriteInboxNew(pr *gh.PullRequest, r RepoRef, now time.Time) (path string, wrote bool, err error) {
	id := ThreadID(r, pr.GetNumber())
	path = filepath.Join(w.StateRoot, "inbox", "new", id+".json")
	if _, statErr := os.Stat(path); statErr == nil {
		return path, false, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", false, statErr
	}
	item := InboxNew{
		ID:         id,
		Source:     "github",
		Kind:       "new",
		ReceivedAt: now,
		Summary: fmt.Sprintf("%s opened PR #%d in %s/%s: %q",
			pr.GetUser().GetLogin(), pr.GetNumber(), r.Owner, r.Repo, pr.GetTitle()),
	}
	item.Ref.Owner = r.Owner
	item.Ref.Repo = r.Repo
	item.Ref.Number = pr.GetNumber()
	item.Ref.Title = pr.GetTitle()
	item.Ref.Author = pr.GetUser().GetLogin()
	item.Ref.URL = pr.GetHTMLURL()
	item.Ref.State = pr.GetState()
	item.Ref.Draft = pr.GetDraft()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", false, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return "", false, fmt.Errorf("marshal inbox new: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", false, fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", false, err
	}
	return path, true, nil
}

// InboxUpdate is the schema for one collapsed-per-cycle update ping.
type InboxUpdate struct {
	ID            string    `json:"id"`
	Source        string    `json:"source"`
	Kind          string    `json:"kind"`
	ReceivedAt    time.Time `json:"received_at"`
	Summary       string    `json:"summary"`
	LatestEventID string    `json:"latest_event_id"`
	RawPath       string    `json:"raw_path"`
}

// WriteInboxUpdate writes one update ping per PR per cycle (design/09 §Updates
// ping).  The filename embeds the latest event id, so multiple events in the
// same cycle collapse into the most recent one.
func (w *Writer) WriteInboxUpdate(id string, latestEventID, summary, rawRelPath string, now time.Time) (string, error) {
	fileName := fmt.Sprintf("%s-%s.json", id, latestEventID)
	path := filepath.Join(w.StateRoot, "inbox", "updates", fileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	item := InboxUpdate{
		ID:            id,
		Source:        "github",
		Kind:          "update",
		ReceivedAt:    now,
		Summary:       summary,
		LatestEventID: latestEventID,
		RawPath:       rawRelPath,
	}
	data, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// AnomaliesDir returns where anomaly notes can be parked.  We do not write
// there in MVP unless we hit a 404 on a tracked PR (handled in the orchestrator).
func (w *Writer) AnomaliesDir() string {
	return filepath.Join(w.StateRoot, "inbox", "anomalies")
}

// ArchiveThread moves state/threads/<id>/ to state/threads/_archive/<id>-<ts>/.
// Used by the 404 handler and by `github-poller untrack-pr --archive`.
// Idempotent: if the source thread is missing, returns ("", nil).
func (w *Writer) ArchiveThread(id string) (string, error) {
	src := w.threadDir(id)
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	dstDir := filepath.Join(w.StateRoot, "threads", "_archive")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dstDir, err)
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	dst := filepath.Join(dstDir, fmt.Sprintf("%s-%s", id, stamp))
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("rename %s -> %s: %w", src, dst, err)
	}
	return dst, nil
}

// AnomalyName returns a stable filename for an anomaly keyed on `id`.
// Unlike WriteAnomaly's nanosecond suffix this allows callers to write
// idempotent anomalies (one per logical event).
func (w *Writer) WriteAnomalyStable(id, kind string, payload any) (string, error) {
	if err := os.MkdirAll(w.AnomaliesDir(), 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(w.AnomaliesDir(), fmt.Sprintf("github-%s-%s.json", id, kind))
	if _, err := os.Stat(path); err == nil {
		// Already recorded; don't churn.
		return path, nil
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// WriteAnomaly writes a freeform JSON anomaly note for human review.
func (w *Writer) WriteAnomaly(id string, payload any) (string, error) {
	if err := os.MkdirAll(w.AnomaliesDir(), 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("github-%s-%d.json", id, time.Now().UnixNano())
	path := filepath.Join(w.AnomaliesDir(), name)
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}
