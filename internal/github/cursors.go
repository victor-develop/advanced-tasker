package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RepoCursor tracks per-repo discovery state.  Stored under
// state/sources/github/cursors/repos/<owner>-<repo>.json.
type RepoCursor struct {
	LastPRDiscoveryAt time.Time `json:"last_pr_discovery_at,omitempty"`
	PullsETag         string    `json:"pulls_etag,omitempty"`
}

// PRCursor tracks per-PR polling state.  Stored under
// state/sources/github/cursors/prs/<owner>-<repo>-<n>.json.
//
// The schema is the one in design/09 §Cursors with one tweak: per-endpoint
// ETags are stored alongside `since` so we can issue conditional requests
// and skip work on 304 responses.
type PRCursor struct {
	LastPolledAt time.Time `json:"last_polled_at"`
	Endpoints    struct {
		IssueCommentsSince  time.Time `json:"issue_comments_since,omitempty"`
		ReviewCommentsSince time.Time `json:"review_comments_since,omitempty"`
		PRUpdatedAt         time.Time `json:"pr_updated_at,omitempty"`
		ReviewsSeenIDs      []int64   `json:"reviews_seen_ids,omitempty"`
		IssueCommentsETag   string    `json:"issue_comments_etag,omitempty"`
		ReviewCommentsETag  string    `json:"review_comments_etag,omitempty"`
		PullETag            string    `json:"pull_etag,omitempty"`
		ReviewsETag         string    `json:"reviews_etag,omitempty"`
	} `json:"endpoints"`
}

// CursorStore is a tiny façade for reading/writing cursor files atomically.
type CursorStore struct {
	Root string // points at state/sources/github/cursors
}

// NewCursorStore returns a store rooted at <stateRoot>/sources/github/cursors.
func NewCursorStore(stateRoot string) *CursorStore {
	return &CursorStore{Root: filepath.Join(stateRoot, "sources", "github", "cursors")}
}

func (s *CursorStore) repoPath(r RepoRef) string {
	return filepath.Join(s.Root, "repos", fmt.Sprintf("%s-%s.json", r.Owner, r.Repo))
}

func (s *CursorStore) prPath(r RepoRef, number int) string {
	return filepath.Join(s.Root, "prs", fmt.Sprintf("%s-%s-%d.json", r.Owner, r.Repo, number))
}

// LoadRepo returns the cursor for a repo.  If absent, an empty cursor is returned.
func (s *CursorStore) LoadRepo(r RepoRef) (*RepoCursor, error) {
	var c RepoCursor
	if err := readJSON(s.repoPath(r), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// SaveRepo writes a repo cursor atomically.
func (s *CursorStore) SaveRepo(r RepoRef, c *RepoCursor) error {
	return writeJSON(s.repoPath(r), c)
}

// LoadPR returns the cursor for a PR.  If absent, an empty cursor is returned.
func (s *CursorStore) LoadPR(r RepoRef, number int) (*PRCursor, error) {
	var c PRCursor
	if err := readJSON(s.prPath(r, number), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// SavePR writes a PR cursor atomically.
func (s *CursorStore) SavePR(r RepoRef, number int, c *PRCursor) error {
	return writeJSON(s.prPath(r, number), c)
}

// DeletePR removes the cursor file for a PR (idempotent — missing file is
// not an error).  Used by `untrack-pr` and by the 404-on-tracked-PR
// archive flow.
func (s *CursorStore) DeletePR(r RepoRef, number int) error {
	path := s.prPath(r, number)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// DeleteRepo removes the discovery cursor for a repo.  Also removes any
// PR cursors that belong to that repo so the slate is fully clean.
// Used by `unwatch`.
func (s *CursorStore) DeleteRepo(r RepoRef) error {
	if err := os.Remove(s.repoPath(r)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", s.repoPath(r), err)
	}
	prsDir := filepath.Join(s.Root, "prs")
	entries, err := os.ReadDir(prsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("readdir %s: %w", prsDir, err)
	}
	prefix := fmt.Sprintf("%s-%s-", r.Owner, r.Repo)
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		if !startsWith(name, prefix) {
			continue
		}
		// Confirm the suffix after the prefix is purely digits + ".json".
		if !looksLikePRCursor(name[len(prefix):]) {
			continue
		}
		if err := os.Remove(filepath.Join(prsDir, name)); err != nil {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
	return nil
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func looksLikePRCursor(tail string) bool {
	if !strings.HasSuffix(tail, ".json") {
		return false
	}
	num := strings.TrimSuffix(tail, ".json")
	if num == "" {
		return false
	}
	for _, c := range num {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// AddReviewSeen records that a review id was processed.  Returns true if added,
// false if it was already present (caller can use this to short-circuit).
func (c *PRCursor) AddReviewSeen(id int64) bool {
	for _, existing := range c.Endpoints.ReviewsSeenIDs {
		if existing == id {
			return false
		}
	}
	c.Endpoints.ReviewsSeenIDs = append(c.Endpoints.ReviewsSeenIDs, id)
	sort.Slice(c.Endpoints.ReviewsSeenIDs, func(i, j int) bool {
		return c.Endpoints.ReviewsSeenIDs[i] < c.Endpoints.ReviewsSeenIDs[j]
	})
	return true
}

// HasReviewSeen reports whether we've already recorded a review id.
func (c *PRCursor) HasReviewSeen(id int64) bool {
	for _, existing := range c.Endpoints.ReviewsSeenIDs {
		if existing == id {
			return true
		}
	}
	return false
}

// readJSON unmarshals a JSON file into v.  Returns nil (leaving v zero) if the
// file does not exist.
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// writeJSON serialises v as pretty JSON and writes it atomically (tmp+rename).
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
