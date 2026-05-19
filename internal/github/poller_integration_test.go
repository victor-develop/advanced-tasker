//go:build integration

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Integration tests exercise the orchestrator against a stub GitHub API
// served by httptest.  Each test composes a small "fake API" that the
// poller talks to so we can verify the C1+C2+C3+C4+C5 scenarios from the
// agent brief:
//
//   1. Fresh start (7d lookback) → inbox/new entries
//   2. Cursor overlap → no duplicate raw/ files
//   3. PR state change between polls → pr-state-<iso>.json snapshot
//   4. PR deleted (404) → anomaly, no crash
//   5. Rate limit (403 with rate-limit headers) → backoff respected
//   6. ETag 304 → no file written
//   7. Dedup via event ID under boundary jitter

// --- helpers --------------------------------------------------------------

// fakeAPI is a httptest.Server that records request paths/queries and serves
// scripted responses keyed by handler.
type fakeAPI struct {
	t        *testing.T
	srv      *httptest.Server
	mux      *http.ServeMux
	calls    map[string]int
	handlers map[string]http.HandlerFunc
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{
		t:        t,
		mux:      http.NewServeMux(),
		calls:    map[string]int{},
		handlers: map[string]http.HandlerFunc{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		f.calls[key]++
		h, ok := f.handlers[key]
		if !ok {
			t.Logf("fake API: no handler for %s (query=%q)", key, r.URL.RawQuery)
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) on(method, path string, h http.HandlerFunc) {
	f.handlers[method+" "+path] = h
}

// baseURL returns the API base URL with trailing slash, suitable for the
// go-github WithEnterpriseURLs override.
func (f *fakeAPI) baseURL() string { return f.srv.URL + "/" }

// newTestPoller wires everything together for one test.
func newTestPoller(t *testing.T, dir string, api *fakeAPI, repos []string, now time.Time) *Poller {
	t.Helper()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
auth:
  token_env: TEST_TOKEN_ENV_DOES_NOT_MATTER
watch:
  repos: [%s]
poll_interval: 60s
new_pr_lookback: 7d
max_concurrent_pr_polls: 2
`, strings.Join(repos, ", "))), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient("test-token", api.baseURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Poller{
		Config:  cfg,
		Client:  client,
		Cursors: NewCursorStore(dir),
		Writer:  NewWriter(dir),
		Logger:  logger,
		Now:     func() time.Time { return now },
	}
}

// stubPRJSON returns a minimal PR payload as a JSON byte slice.
func stubPRJSON(number int, owner, repo, title, author, state string, updatedAt time.Time) []byte {
	pr := map[string]any{
		"id":         int64(1000 + number),
		"number":     number,
		"state":      state,
		"title":      title,
		"draft":      false,
		"html_url":   fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, number),
		"created_at": updatedAt.Add(-time.Hour).Format(time.RFC3339),
		"updated_at": updatedAt.Format(time.RFC3339),
		"user": map[string]any{
			"id":    int64(1),
			"login": author,
		},
	}
	data, err := json.Marshal(pr)
	if err != nil {
		panic(err)
	}
	return data
}

// --- C1: fresh start, 7d lookback discovers PRs --------------------------

func TestIntegration_FreshStartDiscoversPRs(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)

	// /repos/acme/api/pulls
	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[`))
		_, _ = w.Write(stubPRJSON(1284, "acme", "api", "Refactor retry",
			"alice", "open", now.Add(-2*time.Hour)))
		_, _ = w.Write([]byte(`,`))
		_, _ = w.Write(stubPRJSON(1285, "acme", "api", "Add tests",
			"bob", "open", now.Add(-1*time.Hour)))
		_, _ = w.Write([]byte(`]`))
	})

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.PRsDiscovered != 2 {
		t.Errorf("expected 2 PRs discovered; got %d", stats.PRsDiscovered)
	}
	for _, n := range []int{1284, 1285} {
		path := filepath.Join(dir, "inbox", "new", fmt.Sprintf("github-acme-api-pr-%d.json", n))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("inbox new missing for %d: %v", n, err)
		}
	}
	// Cursor written.
	cursor, err := p.Cursors.LoadRepo(RepoRef{Owner: "acme", Repo: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if cursor.PullsETag != `"abc"` {
		t.Errorf("ETag not stored: %q", cursor.PullsETag)
	}
}

// --- C2 + C3: tracked PR fetches comments + state snapshot ---------------

func setupTrackedPR(t *testing.T, dir string, number int) {
	t.Helper()
	threadDir := filepath.Join(dir, "threads", fmt.Sprintf("github-acme-api-pr-%d", number))
	if err := os.MkdirAll(filepath.Join(threadDir, "raw"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_TrackedPR_PollsAllEndpoints(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	setupTrackedPR(t, dir, 1284)

	// No new PRs in discovery scan.
	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"d-1"`)
		_, _ = w.Write([]byte(`[]`))
	})
	// PR metadata.
	api.on("GET", "/repos/acme/api/pulls/1284", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"pr-v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(stubPRJSON(1284, "acme", "api", "Refactor", "alice",
			"open", now.Add(-30*time.Minute)))
	})
	// Issue comments.
	api.on("GET", "/repos/acme/api/issues/1284/comments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"ic-v1"`)
		_, _ = w.Write([]byte(fmt.Sprintf(`[
  {"id": 99001, "user": {"id": 1, "login": "alice"}, "body": "looks good", "created_at": %q, "updated_at": %q, "html_url": "https://github.com/acme/api/pull/1284#issuecomment-99001"}
]`, now.Add(-10*time.Minute).Format(time.RFC3339), now.Add(-10*time.Minute).Format(time.RFC3339))))
	})
	// Review comments.
	api.on("GET", "/repos/acme/api/pulls/1284/comments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"rc-v1"`)
		_, _ = w.Write([]byte(fmt.Sprintf(`[
  {"id": 88001, "user": {"id": 2, "login": "bob"}, "body": "nit", "created_at": %q, "updated_at": %q, "html_url": "https://github.com/acme/api/pull/1284#discussion_r88001"}
]`, now.Add(-5*time.Minute).Format(time.RFC3339), now.Add(-5*time.Minute).Format(time.RFC3339))))
	})
	// Reviews.
	api.on("GET", "/repos/acme/api/pulls/1284/reviews", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"rv-v1"`)
		_, _ = w.Write([]byte(fmt.Sprintf(`[
  {"id": 77001, "user": {"id": 1, "login": "alice"}, "state": "APPROVED", "body": "ship it", "submitted_at": %q, "html_url": "https://github.com/acme/api/pull/1284#pullrequestreview-77001"}
]`, now.Add(-3*time.Minute).Format(time.RFC3339))))
	})

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.RawEventsWritten < 4 {
		t.Errorf("expected at least 4 raw events (pr-state + 3 comments/reviews); got %d", stats.RawEventsWritten)
	}

	threadDir := filepath.Join(dir, "threads", "github-acme-api-pr-1284")
	check := func(name string) {
		t.Helper()
		matches, _ := filepath.Glob(filepath.Join(threadDir, "raw", name))
		if len(matches) == 0 {
			t.Errorf("missing raw event: %s", name)
		}
	}
	check("issue-comment-99001.json")
	check("review-comment-88001.json")
	check("review-77001.json")
	check("pr-state-*.json")

	// .dirty exists.
	if _, err := os.Stat(filepath.Join(threadDir, ".dirty")); err != nil {
		t.Errorf(".dirty missing: %v", err)
	}
	// meta.json exists.
	if _, err := os.Stat(filepath.Join(threadDir, "meta.json")); err != nil {
		t.Errorf("meta.json missing: %v", err)
	}
}

// --- C5: cursor overlap, second poll must NOT duplicate raw/ files -------

func TestIntegration_CursorOverlap_NoDuplicates(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	setupTrackedPR(t, dir, 1284)

	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	api.on("GET", "/repos/acme/api/pulls/1284", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(stubPRJSON(1284, "acme", "api", "Refactor", "alice",
			"open", now.Add(-30*time.Minute)))
	})
	api.on("GET", "/repos/acme/api/issues/1284/comments", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fmt.Sprintf(`[
  {"id": 99001, "user": {"id": 1, "login": "alice"}, "body": "x", "created_at": %q, "updated_at": %q}
]`, now.Add(-10*time.Minute).Format(time.RFC3339), now.Add(-10*time.Minute).Format(time.RFC3339))))
	})
	api.on("GET", "/repos/acme/api/pulls/1284/comments", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	api.on("GET", "/repos/acme/api/pulls/1284/reviews", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Second cycle at a later time but server returns the SAME comment
	// (boundary jitter — `since=last-60s` would re-fetch it).  Our
	// EventExists dedup must skip the write.
	p.Now = func() time.Time { return now.Add(time.Minute) }
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.RawEventsWritten != 0 {
		t.Errorf("second cycle should not write duplicates; wrote %d", stats.RawEventsWritten)
	}
	// Exactly one issue-comment file should exist.
	threadDir := filepath.Join(dir, "threads", "github-acme-api-pr-1284")
	matches, _ := filepath.Glob(filepath.Join(threadDir, "raw", "issue-comment-*.json"))
	if len(matches) != 1 {
		t.Errorf("expected exactly 1 issue-comment file; got %v", matches)
	}
}

// --- C2 (state change): closing a PR between polls writes a snapshot -----

func TestIntegration_PRStateChange_SnapshotWritten(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	setupTrackedPR(t, dir, 1284)

	state := "open"
	var updatedAt time.Time
	updatedAt = now.Add(-30 * time.Minute)

	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	api.on("GET", "/repos/acme/api/pulls/1284", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(stubPRJSON(1284, "acme", "api", "Refactor", "alice", state, updatedAt))
	})
	api.on("GET", "/repos/acme/api/issues/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/reviews", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	threadDir := filepath.Join(dir, "threads", "github-acme-api-pr-1284")
	matches, _ := filepath.Glob(filepath.Join(threadDir, "raw", "pr-state-*.json"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 pr-state file after first poll; got %v", matches)
	}

	// Second poll: state changes to closed, updated_at moves forward.
	state = "closed"
	updatedAt = now.Add(10 * time.Minute)
	p.Now = func() time.Time { return now.Add(15 * time.Minute) }
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	matches2, _ := filepath.Glob(filepath.Join(threadDir, "raw", "pr-state-*.json"))
	if len(matches2) != 2 {
		t.Fatalf("expected 2 pr-state files after state change; got %v", matches2)
	}
}

// --- C2 (404 handling): deleted PR is logged as anomaly, no crash --------

func TestIntegration_PRDeleted_Anomaly(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	setupTrackedPR(t, dir, 1284)

	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	api.on("GET", "/repos/acme/api/pulls/1284", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	// other endpoints should still be reachable in principle
	api.on("GET", "/repos/acme/api/issues/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/reviews", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce should not error on 404: %v", err)
	}
	if stats.AnomaliesRecorded != 1 {
		t.Errorf("expected 1 anomaly; got %d", stats.AnomaliesRecorded)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "inbox", "anomalies", "*"))
	if len(matches) == 0 {
		t.Error("anomaly file missing")
	}
}

// --- C5 (rate limit): 403 with rate-limit headers is handled --------------

func TestIntegration_RateLimit_NoCrash(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)

	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", now.Add(time.Minute).Unix()))
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	})

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce should swallow rate-limit: %v", err)
	}
	if stats.PRsDiscovered != 0 {
		t.Errorf("rate-limited cycle should discover 0 PRs; got %d", stats.PRsDiscovered)
	}
}

// --- Performance: 304 ETag short-circuits a fetch -------------------------

func TestIntegration_ETag304_NoFileWritten(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	setupTrackedPR(t, dir, 1284)

	// Seed PR cursor with an ETag for issue comments.
	pr := RepoRef{Owner: "acme", Repo: "api"}
	cursors := NewCursorStore(dir)
	c, _ := cursors.LoadPR(pr, 1284)
	c.Endpoints.IssueCommentsETag = `"ic-old"`
	_ = cursors.SavePR(pr, 1284, c)

	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(stubPRJSON(1284, "acme", "api", "Refactor", "alice", "open", now.Add(-30*time.Minute)))
	})
	api.on("GET", "/repos/acme/api/issues/1284/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"ic-old"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	api.on("GET", "/repos/acme/api/pulls/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/reviews", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.NotModifiedCount == 0 {
		t.Errorf("expected at least one 304 response; got 0")
	}
	// No issue-comment files should exist.
	matches, _ := filepath.Glob(filepath.Join(dir, "threads", "github-acme-api-pr-1284", "raw", "issue-comment-*.json"))
	if len(matches) != 0 {
		t.Errorf("expected no issue-comment files written; got %v", matches)
	}
}

// --- C5 (boundary jitter): same comment served on two cycles, no dup ----

func TestIntegration_BoundaryJitter_DedupViaEventID(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	setupTrackedPR(t, dir, 1284)

	commentTS := now.Add(-30 * time.Second) // boundary-adjacent
	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(stubPRJSON(1284, "acme", "api", "x", "alice", "open", now.Add(-1*time.Hour)))
	})
	api.on("GET", "/repos/acme/api/issues/1284/comments", func(w http.ResponseWriter, r *http.Request) {
		// Always return the same comment; emulates GitHub's jitter at
		// since= boundaries.
		_, _ = w.Write([]byte(fmt.Sprintf(`[
  {"id": 99001, "user": {"id": 1, "login": "alice"}, "body": "x", "created_at": %q, "updated_at": %q}
]`, commentTS.Format(time.RFC3339), commentTS.Format(time.RFC3339))))
	})
	api.on("GET", "/repos/acme/api/pulls/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/reviews", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	// Run three cycles back to back.
	for i := 0; i < 3; i++ {
		p.Now = func() time.Time { return now.Add(time.Duration(i) * time.Minute) }
		if _, err := p.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "threads", "github-acme-api-pr-1284", "raw", "issue-comment-*.json"))
	if len(matches) != 1 {
		t.Errorf("expected exactly 1 issue-comment file across 3 cycles; got %v", matches)
	}
}
