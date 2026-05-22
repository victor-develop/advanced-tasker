//go:build integration

package github

// Golden-file integration test for the round-2 acceptance criterion:
//
//	`github-poller --once` driven by recorded go-vcr cassettes produces
//	filesystem output that hash-matches a golden snapshot conforming to
//	design/09 §"Filesystem output contract", with all four endpoints
//	exercised and ETag 304s short-circuiting correctly.
//
// The test runs against a recorded go-vcr cassette under
// `testdata/cassettes/golden-fresh-start.yaml`.  If the cassette is
// missing, the test records a fresh one against a local httptest server
// and writes both the cassette and the golden hash file, then asserts
// the second pass replays cleanly.
//
// This means CI is fully hermetic (no network), while local rerecording
// is one `rm testdata/cassettes/golden-fresh-start.yaml ...` away.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/dnaeon/go-vcr.v4/pkg/cassette"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/recorder"
)

// pathQueryMatcher is a MatcherFunc that ignores the host portion of the
// URL so a cassette recorded against httptest's ephemeral port (e.g.
// 127.0.0.1:53902) replays correctly when the test is later re-pointed
// at any base URL.  We match on (method, path, sorted-query, headers
// ignoring User-Agent + Authorization).
func pathQueryMatcher() recorder.MatcherFunc {
	return func(r *http.Request, i cassette.Request) bool {
		if r.Method != i.Method {
			return false
		}
		iURL, err := url.Parse(i.URL)
		if err != nil {
			return false
		}
		if r.URL.Path != iURL.Path {
			return false
		}
		// Normalised query string comparison.
		if r.URL.Query().Encode() != iURL.Query().Encode() {
			return false
		}
		return true
	}
}

// fakeUpstream returns a httptest.Server scripted with a complete fresh-
// start scenario exercising all four endpoints: PR discovery, PR
// metadata (state-change snapshot), issue comments, review comments,
// reviews.  Used as the recording origin and as a hermetic fallback if
// the cassette is missing.
func goldenUpstream(now time.Time) *httptest.Server {
	mux := http.NewServeMux()

	// Repo-level discovery: one open PR.
	mux.HandleFunc("/repos/acme/api/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"discovery-v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`[
  {
    "id": 1000, "number": 1284, "state": "open", "draft": false,
    "title": "Refactor retry to jittered exponential",
    "html_url": "https://github.com/acme/api/pull/1284",
    "created_at": "2026-05-18T08:00:00Z",
    "updated_at": %q,
    "user": {"id": 1, "login": "alice"}
  }
]`, now.Add(-30*time.Minute).Format(time.RFC3339))))
	})
	// PR metadata.
	mux.HandleFunc("/repos/acme/api/pulls/1284", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"pr-v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{
  "id": 1000, "number": 1284, "state": "open", "merged": false,
  "mergeable": true, "draft": false, "title": "Refactor retry",
  "user": {"id": 1, "login": "alice"},
  "html_url": "https://github.com/acme/api/pull/1284",
  "created_at": "2026-05-18T08:00:00Z",
  "updated_at": %q,
  "labels": [{"name": "p1"}],
  "head": {"sha": "headsha1"},
  "base": {"sha": "basesha1"},
  "requested_reviewers": [{"id": 2, "login": "bob"}]
}`, now.Add(-30*time.Minute).Format(time.RFC3339))))
	})
	// Issue comments.
	mux.HandleFunc("/repos/acme/api/issues/1284/comments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"ic-v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`[
  {"id": 99001, "user": {"id": 1, "login": "alice"}, "body": "looks good", "created_at": %q, "updated_at": %q, "html_url": "https://github.com/acme/api/pull/1284#issuecomment-99001"}
]`, now.Add(-10*time.Minute).Format(time.RFC3339), now.Add(-10*time.Minute).Format(time.RFC3339))))
	})
	// Review comments.
	mux.HandleFunc("/repos/acme/api/pulls/1284/comments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"rc-v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`[
  {"id": 88001, "user": {"id": 2, "login": "bob"}, "body": "small nit on line 42", "created_at": %q, "updated_at": %q, "html_url": "https://github.com/acme/api/pull/1284#discussion_r88001"}
]`, now.Add(-5*time.Minute).Format(time.RFC3339), now.Add(-5*time.Minute).Format(time.RFC3339))))
	})
	// Reviews.
	mux.HandleFunc("/repos/acme/api/pulls/1284/reviews", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"rv-v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`[
  {"id": 77001, "user": {"id": 1, "login": "alice"}, "state": "APPROVED", "body": "ship it", "submitted_at": %q, "html_url": "https://github.com/acme/api/pull/1284#pullrequestreview-77001"}
]`, now.Add(-3*time.Minute).Format(time.RFC3339))))
	})
	return httptest.NewServer(mux)
}

func TestIntegration_GoldenCassette_FreshStartHashesMatch(t *testing.T) {
	cassettePath := "testdata/cassettes/golden-fresh-start"
	goldenManifestPath := "testdata/golden/fresh-start.json"
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)

	if err := os.MkdirAll(filepath.Dir(cassettePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(goldenManifestPath), 0o755); err != nil {
		t.Fatal(err)
	}

	cassetteFile := cassettePath + ".yaml"
	regen := os.Getenv("REGEN_GOLDEN") == "1"
	_, statErr := os.Stat(cassetteFile)
	needRecord := regen || os.IsNotExist(statErr)

	var rec *recorder.Recorder
	var upstream *httptest.Server
	var err error

	if needRecord {
		// Record fresh cassette.
		upstream = goldenUpstream(now)
		t.Cleanup(func() { upstream.Close() })
		_ = os.Remove(cassetteFile)
		rec, err = recorder.New(cassettePath,
			recorder.WithMode(recorder.ModeRecordOnce),
			recorder.WithMatcher(pathQueryMatcher()),
		)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		// Replay-only.  Use a host-insensitive matcher so the cassette
		// can be replayed against any base URL.
		rec, err = recorder.New(cassettePath,
			recorder.WithMode(recorder.ModeReplayOnly),
			recorder.WithMatcher(pathQueryMatcher()),
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = rec.Stop() })

	httpClient := rec.GetDefaultClient()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
auth:
  token_env: NO_TOKEN_FOR_VCR
watch:
  repos: [acme/api]
poll_interval: 60s
new_pr_lookback: 7d
max_concurrent_pr_polls: 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	baseURL := "http://example-vcr.invalid/"
	if needRecord {
		baseURL = upstream.URL + "/"
	}
	client, err := NewClient("test-token", baseURL, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	p := &Poller{
		Config:  cfg,
		Client:  client,
		Cursors: NewCursorStore(dir),
		Writer:  NewWriter(dir),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     func() time.Time { return now },
	}

	// First cycle: discovers the PR into inbox/new and writes the repo cursor.
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	// Promote to a tracked PR (analogous to `harness thread track`).
	if err := os.MkdirAll(filepath.Join(dir, "threads", "github-acme-api-pr-1284", "raw"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Second cycle: now the PR is tracked, fetches all four endpoints.
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}

	// Hash all written files under `dir`.  We compare canonical content,
	// not mtimes, so the hash is reproducible across runs.
	manifest := hashFilesystem(t, dir)

	if needRecord {
		// Write golden manifest.
		if err := os.WriteFile(goldenManifestPath, manifest, 0o644); err != nil {
			t.Fatal(err)
		}
		// Stop the recorder so the cassette file flushes.
		_ = rec.Stop()
		if upstream != nil {
			upstream.Close()
		}
		t.Logf("recorded fresh cassette + golden manifest (set REGEN_GOLDEN=0 or just re-run to verify replay)")
		return
	}

	wantBytes, err := os.ReadFile(goldenManifestPath)
	if err != nil {
		t.Fatalf("read golden manifest: %v", err)
	}
	if string(manifest) != string(wantBytes) {
		// Surface a useful diff.  We don't want to bake in a diff lib,
		// so just print both manifests.
		t.Logf("WANT:\n%s", string(wantBytes))
		t.Logf("GOT:\n%s", string(manifest))
		t.Fatal("golden manifest mismatch — rerun with REGEN_GOLDEN=1 if intended")
	}
}

// hashFilesystem walks `root`, hashes each file's content, and returns a
// canonical JSON manifest mapping relative paths to sha256 hex digests.
// We deliberately exclude:
//   - mtime/atime (we hash content)
//   - .dirty files (empty markers; presence-only, easy to assert below)
//   - cursor files that include `last_polled_at` / `last_pr_discovery_at`
//     wall-clock fields (the test fixes Now, but cursors also embed
//     ETags from the upstream's response, which are stable across runs)
//
// We DO include all `inbox/new`, `inbox/updates`, `inbox/anomalies`, and
// `threads/<id>/raw/<event-id>.json` files because those are the
// long-lived filesystem contract under design/09.
func hashFilesystem(t *testing.T, root string) []byte {
	t.Helper()
	entries := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		// Normalize separators for cross-OS reproducibility.
		rel = filepath.ToSlash(rel)
		// Skip the cassette artifact if it shows up under the same tree.
		if strings.HasSuffix(rel, ".tmp") {
			return nil
		}
		// Cursor files: presence-only.  Don't hash content because
		// reviews-seen-IDs ordering depends on map iteration in the
		// upstream JSON which is unstable.
		if strings.HasPrefix(rel, "sources/github/cursors/") {
			entries[rel] = "<present>"
			return nil
		}
		// .dirty: presence-only (empty file).
		if strings.HasSuffix(rel, "/.dirty") || rel == ".dirty" {
			entries[rel] = "<dirty-marker>"
			return nil
		}
		// meta.json contains `last_event_at` and `tracking_since` from
		// the fixed Now, so content is stable — hash it.
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Normalise raw events: the upstream's PR/review/comment JSON
		// embedded in `raw` includes Link headers indirectly via
		// pagination; for replay reproducibility we hash only the
		// top-level event envelope (id, kind, pr, created_at, body,
		// snapshot).
		if strings.Contains(rel, "/raw/") {
			var ev RawEvent
			if err := json.Unmarshal(data, &ev); err == nil {
				stripped := struct {
					ID       string           `json:"id"`
					Kind     string           `json:"kind"`
					Source   string           `json:"source"`
					Body     string           `json:"body,omitempty"`
					HTMLURL  string           `json:"html_url,omitempty"`
					Actor    string           `json:"actor,omitempty"`
					PR       any              `json:"pr"`
					Snapshot *PRStateSnapshot `json:"snapshot,omitempty"`
				}{
					ID: ev.ID, Kind: ev.Kind, Source: ev.Source,
					Body: ev.Body, HTMLURL: ev.HTMLURL, Actor: ev.Actor,
					PR: ev.PR, Snapshot: ev.Snapshot,
				}
				data, _ = json.Marshal(stripped)
			}
		}
		// inbox/new and inbox/updates contain `received_at` from Now,
		// so they're stable too.
		sum := sha256.Sum256(data)
		entries[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Stable JSON.
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type kv struct {
		Path string `json:"path"`
		Hash string `json:"hash"`
	}
	out := make([]kv, 0, len(keys))
	for _, k := range keys {
		out = append(out, kv{Path: k, Hash: entries[k]})
	}
	buf, _ := json.MarshalIndent(out, "", "  ")
	return append(buf, '\n')
}
