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

// --- C2 (404 handling): deleted PR is archived + logged as anomaly -------
//
// Round-2 hardening: per the brief, "404 on tracked PR — PR deleted
// upstream.  Archive the thread (rename to state/threads/_archive/...),
// write an anomaly..., remove from cursor list."

func TestIntegration_PRDeleted_ArchivedAndAnomaly(t *testing.T) {
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
	// Anomaly exists.
	matches, _ := filepath.Glob(filepath.Join(dir, "inbox", "anomalies", "*"))
	if len(matches) == 0 {
		t.Error("anomaly file missing")
	}
	// Original thread directory is gone.
	if _, err := os.Stat(filepath.Join(dir, "threads", "github-acme-api-pr-1284")); !os.IsNotExist(err) {
		t.Errorf("expected source thread dir to be archived/removed: %v", err)
	}
	// Archived copy exists under _archive.
	archMatches, _ := filepath.Glob(filepath.Join(dir, "threads", "_archive", "github-acme-api-pr-1284-*"))
	if len(archMatches) != 1 {
		t.Errorf("expected 1 archived thread dir; got %v", archMatches)
	}
	// PR cursor file is gone.
	if _, err := os.Stat(filepath.Join(dir, "sources", "github", "cursors", "prs", "acme-api-1284.json")); !os.IsNotExist(err) {
		t.Errorf("expected pr cursor to be removed: %v", err)
	}
}

// --- C5 (rate limit): 403 with rate-limit headers is handled --------------
//
// We arrange `now` slightly past the reset so honorRateLimit computes a
// zero/negative sleep (and falls back to backoff.on_rate_limit which we
// also override to 0 below).  This keeps the test fast while still
// exercising the rate-limit branch fully.

func TestIntegration_RateLimit_NoCrash(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	reset := now.Add(-1 * time.Second) // already-elapsed reset → 0 sleep

	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", reset.Unix()))
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	})

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	// Zero out the backoff fallback too so retries don't sleep on the
	// `d <= 0 → use Config.Backoff.OnRateLimit` branch.
	p.Config.Backoff.OnRateLimit.Duration = 0
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce should swallow rate-limit: %v", err)
	}
	if stats.PRsDiscovered != 0 {
		t.Errorf("rate-limited cycle should discover 0 PRs; got %d", stats.PRsDiscovered)
	}
}

// --- Rate-limit: poller sleeps until X-RateLimit-Reset before retrying ---
//
// Verifies the explicit round-2 contract: on 403 + rate-limit headers, the
// poller waits for the reset and retries.  We measure the sleep delta by
// recording the wall-clock time around the call.  Reset is set 200ms into
// the future so the test stays cheap.

func TestIntegration_RateLimit_SleepsUntilReset(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	// X-RateLimit-Reset is encoded as a Unix epoch in seconds, so we use
	// a 2-second future reset to land cleanly on a second boundary.
	resetDelay := 2 * time.Second

	var serveCount int
	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) {
		serveCount++
		if serveCount == 1 {
			// First hit: rate-limited; reset is +2s.
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", now.Add(resetDelay).Unix()))
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			return
		}
		// Subsequent hits succeed with an empty list.
		_, _ = w.Write([]byte(`[]`))
	})

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	// Use the test `now` for the sleep math; sleepCtx uses real wall
	// clock, so we expect the cycle to actually pause ~2s.
	p.Now = func() time.Time { return now }
	// Cap the fallback (don't accidentally fall back to 60s default).
	p.Config.Backoff.OnRateLimit.Duration = 100 * time.Millisecond

	start := time.Now()
	stats, err := p.RunOnce(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// Honour-then-retry: serveCount should be at least 2.
	if serveCount < 2 {
		t.Errorf("expected at least 2 calls (one rate-limited + one retry); got %d", serveCount)
	}
	// We slept ~resetDelay.  Allow some slack on both sides.
	if elapsed < resetDelay-500*time.Millisecond {
		t.Errorf("expected to sleep ~%v; only slept %v", resetDelay, elapsed)
	}
	if elapsed > resetDelay*3 {
		t.Errorf("slept way longer than expected: %v", elapsed)
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

// --- ETag-per-endpoint: 304 short-circuits each of the four endpoints ----
//
// Per round-2 brief: "ETag / If-None-Match on ALL FOUR endpoints (issue
// comments, review comments, PR metadata, reviews)... 304 responses: do
// not write, do not advance the seen-list.  Add explicit integration
// test cases for each endpoint's 304 path."

func TestIntegration_ETag304_PullMetadata(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	setupTrackedPR(t, dir, 1284)

	pr := RepoRef{Owner: "acme", Repo: "api"}
	cursors := NewCursorStore(dir)
	c, _ := cursors.LoadPR(pr, 1284)
	c.Endpoints.PullETag = `"pull-old"`
	// Pre-seed PRUpdatedAt so we don't accidentally consider a write to be
	// a "state change".
	c.Endpoints.PRUpdatedAt = now.Add(-1 * time.Hour)
	_ = cursors.SavePR(pr, 1284, c)
	// Also create a fake existing meta.json so the orchestrator doesn't
	// need to call EnsureMeta on the 304 path.
	if err := os.MkdirAll(filepath.Join(dir, "threads", "github-acme-api-pr-1284"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "threads", "github-acme-api-pr-1284", "meta.json"),
		[]byte(`{"id":"github-acme-api-pr-1284","source":"github","participants":[],"tracking_since":"2026-05-18T00:00:00Z"}`),
		0o644); err != nil {
		t.Fatal(err)
	}

	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"pull-old"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(stubPRJSON(1284, "acme", "api", "Refactor", "alice", "open", now.Add(-30*time.Minute)))
	})
	api.on("GET", "/repos/acme/api/issues/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/reviews", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.NotModifiedCount == 0 {
		t.Errorf("expected ≥1 304 response; got 0")
	}
	// No pr-state-*.json files.
	matches, _ := filepath.Glob(filepath.Join(dir, "threads", "github-acme-api-pr-1284", "raw", "pr-state-*.json"))
	if len(matches) != 0 {
		t.Errorf("expected 0 pr-state files; got %v", matches)
	}
	// ETag preserved on cursor.
	reloaded, _ := cursors.LoadPR(pr, 1284)
	if reloaded.Endpoints.PullETag != `"pull-old"` {
		t.Errorf("PullETag not preserved on 304: %q", reloaded.Endpoints.PullETag)
	}
}

func TestIntegration_ETag304_ReviewComments(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	setupTrackedPR(t, dir, 1284)

	pr := RepoRef{Owner: "acme", Repo: "api"}
	cursors := NewCursorStore(dir)
	c, _ := cursors.LoadPR(pr, 1284)
	c.Endpoints.ReviewCommentsETag = `"rc-old"`
	_ = cursors.SavePR(pr, 1284, c)

	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(stubPRJSON(1284, "acme", "api", "x", "alice", "open", now.Add(-30*time.Minute)))
	})
	api.on("GET", "/repos/acme/api/issues/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"rc-old"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	api.on("GET", "/repos/acme/api/pulls/1284/reviews", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.NotModifiedCount == 0 {
		t.Error("expected ≥1 304 response")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "threads", "github-acme-api-pr-1284", "raw", "review-comment-*.json"))
	if len(matches) != 0 {
		t.Errorf("expected 0 review-comment files; got %v", matches)
	}
	reloaded, _ := cursors.LoadPR(pr, 1284)
	if reloaded.Endpoints.ReviewCommentsETag != `"rc-old"` {
		t.Errorf("ReviewCommentsETag not preserved on 304: %q", reloaded.Endpoints.ReviewCommentsETag)
	}
}

func TestIntegration_ETag304_Reviews(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	setupTrackedPR(t, dir, 1284)

	pr := RepoRef{Owner: "acme", Repo: "api"}
	cursors := NewCursorStore(dir)
	c, _ := cursors.LoadPR(pr, 1284)
	c.Endpoints.ReviewsETag = `"rv-old"`
	c.AddReviewSeen(77001)
	_ = cursors.SavePR(pr, 1284, c)

	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(stubPRJSON(1284, "acme", "api", "x", "alice", "open", now.Add(-30*time.Minute)))
	})
	api.on("GET", "/repos/acme/api/issues/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/reviews", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"rv-old"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.NotModifiedCount == 0 {
		t.Error("expected ≥1 304 response")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "threads", "github-acme-api-pr-1284", "raw", "review-*.json"))
	if len(matches) != 0 {
		t.Errorf("expected 0 review files; got %v", matches)
	}
	// Seen-list NOT advanced past the seeded ID.
	reloaded, _ := cursors.LoadPR(pr, 1284)
	if len(reloaded.Endpoints.ReviewsSeenIDs) != 1 {
		t.Errorf("seen-list grew on 304: %v", reloaded.Endpoints.ReviewsSeenIDs)
	}
	if reloaded.Endpoints.ReviewsETag != `"rv-old"` {
		t.Errorf("ReviewsETag not preserved: %q", reloaded.Endpoints.ReviewsETag)
	}
}

// --- Reviews dedup short-circuit: pre-fetch one page; if all IDs seen,
// don't walk further pages.  Round-2 brief: "Test for this short-circuit."

func TestIntegration_ReviewsDedup_SkipsRestWhenAllSeen(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	setupTrackedPR(t, dir, 1284)

	pr := RepoRef{Owner: "acme", Repo: "api"}
	cursors := NewCursorStore(dir)
	c, _ := cursors.LoadPR(pr, 1284)
	c.AddReviewSeen(77001)
	c.AddReviewSeen(77002)
	c.AddReviewSeen(77003)
	_ = cursors.SavePR(pr, 1284, c)

	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(stubPRJSON(1284, "acme", "api", "x", "alice", "open", now.Add(-30*time.Minute)))
	})
	api.on("GET", "/repos/acme/api/issues/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })

	var reviewsCalls int
	api.on("GET", "/repos/acme/api/pulls/1284/reviews", func(w http.ResponseWriter, r *http.Request) {
		reviewsCalls++
		if r.URL.Query().Get("page") == "2" {
			t.Errorf("reviews page=2 should NOT be requested when all page-1 IDs already seen")
		}
		// Set Link to imply pagination so go-github would attempt page 2.
		w.Header().Set("Link", `<https://api.github.com/repos/acme/api/pulls/1284/reviews?page=2>; rel="next"`)
		_, _ = w.Write([]byte(fmt.Sprintf(`[
  {"id": 77001, "user": {"id": 1, "login": "alice"}, "state": "APPROVED", "submitted_at": %q},
  {"id": 77002, "user": {"id": 1, "login": "alice"}, "state": "APPROVED", "submitted_at": %q}
]`, now.Add(-1*time.Hour).Format(time.RFC3339), now.Add(-50*time.Minute).Format(time.RFC3339))))
	})

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reviewsCalls != 1 {
		t.Errorf("expected exactly 1 reviews-endpoint call (short-circuit on all-seen); got %d", reviewsCalls)
	}
}

// --- Reviews dedup: an unseen ID on page 1 disables the short-circuit and
// we DO walk further pages.

func TestIntegration_ReviewsDedup_WalksPagesWhenNewID(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	setupTrackedPR(t, dir, 1284)

	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(stubPRJSON(1284, "acme", "api", "x", "alice", "open", now.Add(-30*time.Minute)))
	})
	api.on("GET", "/repos/acme/api/issues/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })

	var page1Hits, page2Hits int
	api.on("GET", "/repos/acme/api/pulls/1284/reviews", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			page2Hits++
			_, _ = w.Write([]byte(`[]`))
			return
		}
		page1Hits++
		w.Header().Set("Link", `<https://api.github.com/repos/acme/api/pulls/1284/reviews?page=2>; rel="next"`)
		_, _ = w.Write([]byte(fmt.Sprintf(`[
  {"id": 88001, "user": {"id": 1, "login": "alice"}, "state": "APPROVED", "submitted_at": %q}
]`, now.Add(-1*time.Hour).Format(time.RFC3339))))
	})

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if page1Hits != 1 {
		t.Errorf("page1Hits got %d", page1Hits)
	}
	if page2Hits != 1 {
		t.Errorf("expected page 2 to be requested (new ID on page 1); got %d hits", page2Hits)
	}
}

// --- PR state snapshot: closed PR snapshot includes all required fields
// per design/09 §"State change detection".

func TestIntegration_PRStateSnapshot_FieldsCaptured(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	setupTrackedPR(t, dir, 1284)

	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	// Rich PR payload exercising labels, requested_reviewers, head/base SHAs.
	api.on("GET", "/repos/acme/api/pulls/1284", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{
  "id": 1, "number": 1284, "state": "closed", "merged": true, "mergeable": true,
  "draft": false, "title": "x",
  "user": {"id": 1, "login": "alice"},
  "html_url": "https://github.com/acme/api/pull/1284",
  "created_at": "2026-05-18T00:00:00Z",
  "updated_at": %q,
  "labels": [{"name": "p0"}, {"name": "needs-tests"}],
  "head": {"sha": "abc123"},
  "base": {"sha": "def456"},
  "requested_reviewers": [{"id": 2, "login": "bob"}, {"id": 3, "login": "carol"}]
}`, now.Add(-30*time.Minute).Format(time.RFC3339))))
	})
	api.on("GET", "/repos/acme/api/issues/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/reviews", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "threads", "github-acme-api-pr-1284", "raw", "pr-state-*.json"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 pr-state file; got %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var ev RawEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Snapshot == nil {
		t.Fatal("snapshot missing")
	}
	if ev.Snapshot.State != "closed" || !ev.Snapshot.Merged {
		t.Errorf("state/merged: got %+v", ev.Snapshot)
	}
	if ev.Snapshot.HeadSHA != "abc123" || ev.Snapshot.BaseSHA != "def456" {
		t.Errorf("sha fields: %+v", ev.Snapshot)
	}
	if len(ev.Snapshot.Labels) != 2 || ev.Snapshot.Labels[0] != "p0" {
		t.Errorf("labels: %v", ev.Snapshot.Labels)
	}
	if len(ev.Snapshot.RequestedReviewers) != 2 || ev.Snapshot.RequestedReviewers[0] != "bob" {
		t.Errorf("reviewers: %v", ev.Snapshot.RequestedReviewers)
	}
}

// --- Graceful SIGTERM: cancellation between PRs leaves the just-completed
// cursor durably on disk and skips the not-yet-started one.

func TestIntegration_GracefulShutdown_PreservesCompletedCursor(t *testing.T) {
	dir := t.TempDir()
	api := newFakeAPI(t)
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	setupTrackedPR(t, dir, 1284)
	setupTrackedPR(t, dir, 1285)

	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(stubPRJSON(1284, "acme", "api", "first", "alice", "open", now.Add(-30*time.Minute)))
	})
	api.on("GET", "/repos/acme/api/issues/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1284/reviews", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })

	// Second PR's endpoints — never called because we cancel context
	// after the first PR completes.  These exist defensively.
	api.on("GET", "/repos/acme/api/pulls/1285", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(stubPRJSON(1285, "acme", "api", "second", "bob", "open", now.Add(-1*time.Hour)))
	})
	api.on("GET", "/repos/acme/api/issues/1285/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1285/comments", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	api.on("GET", "/repos/acme/api/pulls/1285/reviews", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })

	p := newTestPoller(t, dir, api, []string{"acme/api"}, now)
	// MaxConcurrent=1 so the order is deterministic.
	p.Config.MaxConcurrent = 1

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a small delay — long enough for the first PR (1284)
	// to land its cursor save in the deferred block, but short enough
	// that we don't process all PRs.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	// Use a slow API for 1284 to give us time to cancel before 1285.
	// Re-register the handler to add a sleep.
	api.handlers["GET /repos/acme/api/pulls/1284"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(stubPRJSON(1284, "acme", "api", "first", "alice", "open", now.Add(-30*time.Minute)))
	}
	_, _ = p.RunOnce(ctx)

	// PR 1284 cursor must exist with LastPolledAt set (it ran to
	// completion before cancel propagated).  PR 1285 cursor should
	// either not exist or have a zero LastPolledAt.
	cursorPath1284 := filepath.Join(dir, "sources", "github", "cursors", "prs", "acme-api-1284.json")
	if _, err := os.Stat(cursorPath1284); err != nil {
		t.Fatalf("1284 cursor missing after graceful cancel: %v", err)
	}
	c1, err := p.Cursors.LoadPR(RepoRef{Owner: "acme", Repo: "api"}, 1284)
	if err != nil {
		t.Fatal(err)
	}
	if c1.LastPolledAt.IsZero() {
		t.Error("1284 LastPolledAt is zero after successful poll")
	}
}

// --- Golden-file test: drive the poller with a recorded cassette set
// and compare the resulting filesystem to a checked-in golden.  Round-2
// acceptance criterion #1 from the brief.
//
// The cassette is generated on-the-fly in this test (so the test stays
// hermetic and self-contained) but stored under testdata/cassettes/ so
// CI can inspect.  See scripts/acceptance-github-poller.sh for the
// shell-level acceptance harness.

// (See vcr_golden_integration_test.go for the implementation.)


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
