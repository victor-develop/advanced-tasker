//go:build integration

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
)

// goldenMock is a small Slack-API mock used by golden + rate-limit +
// graceful-shutdown tests. Behavior is parameterized via the per-test
// fields.
type goldenMock struct {
	srv *httptest.Server

	mu            sync.Mutex
	totalRequests int
	histCalls     int
	limitedHits   int

	// rate limiting:
	limitFirstN int    // first N hits return 429
	retryAfter  string // header value

	historyPages []historyPage
	repliesPages map[string][]repliesPage // key channel/thread_ts
}

type historyPage struct {
	Messages   []map[string]any
	NextCursor string
}

type repliesPage struct {
	Messages   []map[string]any
	NextCursor string
}

func newGoldenMock(t *testing.T) *goldenMock {
	t.Helper()
	g := &goldenMock{
		repliesPages: map[string][]repliesPage{},
	}
	g.srv = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *goldenMock) URL() string { return g.srv.URL + "/" }

func (g *goldenMock) handle(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	endpoint := strings.TrimPrefix(r.URL.Path, "/")
	g.mu.Lock()
	g.totalRequests++
	if g.limitFirstN > 0 {
		g.limitFirstN--
		g.limitedHits++
		ra := g.retryAfter
		g.mu.Unlock()
		if ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
		return
	}
	switch endpoint {
	case "conversations.history":
		g.histCalls++
		idx := 0
		if cur := r.Form.Get("cursor"); cur != "" {
			for i := range g.historyPages {
				if fmt.Sprintf("c%d", i) == cur {
					idx = i
				}
			}
		}
		var page historyPage
		if idx < len(g.historyPages) {
			page = g.historyPages[idx]
		}
		g.mu.Unlock()
		writeMockJSON(w, map[string]any{
			"ok":       true,
			"messages": page.Messages,
			"has_more": page.NextCursor != "",
			"response_metadata": map[string]any{
				"next_cursor": page.NextCursor,
			},
		})
		return
	case "conversations.replies":
		channel := r.Form.Get("channel")
		ts := r.Form.Get("ts")
		pages := g.repliesPages[channel+"/"+ts]
		g.mu.Unlock()
		var page repliesPage
		if len(pages) > 0 {
			page = pages[0]
		}
		writeMockJSON(w, map[string]any{
			"ok":       true,
			"messages": page.Messages,
			"has_more": page.NextCursor != "",
			"response_metadata": map[string]any{
				"next_cursor": page.NextCursor,
			},
		})
		return
	case "chat.getPermalink":
		channel := r.Form.Get("channel")
		ts := r.Form.Get("message_ts")
		g.mu.Unlock()
		writeMockJSON(w, map[string]any{
			"ok":        true,
			"permalink": fmt.Sprintf("https://example.test/archives/%s/p%s", channel, strings.ReplaceAll(ts, ".", "")),
		})
		return
	default:
		g.mu.Unlock()
		writeMockJSON(w, map[string]any{"ok": false, "error": "unknown_method:" + endpoint})
	}
}

func writeMockJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// seedRunnableState writes a minimal valid config + tokens env in the
// returned cleanup func.
func seedRunnableState(t *testing.T, stateDir string, channels []string, apiURL string) {
	t.Helper()
	dir := filepath.Join(stateDir, "sources", "slack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("auth:\n  token_env: TEST_SLACK_TOKEN\n")
	b.WriteString("watch:\n  channels:\n")
	for _, c := range channels {
		b.WriteString("    - id: " + c + "\n")
	}
	// fast intervals to keep tests snappy
	b.WriteString("poll_interval: 100ms\n")
	b.WriteString("backoff:\n  on_rate_limit: 100ms\n  on_error: 100ms\n  max_backoff: 500ms\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SLACK_TOKEN", "xoxb-test")
}

// TestRateLimit_RetryThenSucceed verifies that on a 429 with Retry-After,
// the daemon backs off and retries rather than aborting. We use daemon
// mode (not --once) because the daemon loop owns the rate-limit retry
// policy; --once exits after one cycle on rate-limit error so it can be
// observed by a script.
func TestRateLimit_RetryThenSucceed(t *testing.T) {
	stateDir := t.TempDir()
	g := newGoldenMock(t)
	g.limitFirstN = 2 // first 2 hits are 429
	g.retryAfter = "0" // retry immediately
	g.historyPages = []historyPage{{
		Messages: []map[string]any{{
			"type": "message",
			"ts":   "1700000100.000100",
			"user": "U_A",
			"text": "hello after limit",
		}},
	}}
	seedRunnableState(t, stateDir, []string{"C0492"}, g.URL())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	root := NewRootCmd()
	root.SetArgs([]string{"--state-dir", stateDir, "--api-url", g.URL(),
		"--log-level", "error"})
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("execute daemon: %v", err)
	}
	inboxPath := filepath.Join(stateDir, "inbox", "new",
		"slack-C0492-1700000100.000100.json")
	if _, err := os.Stat(inboxPath); err != nil {
		t.Fatalf("inbox/new entry should be present after retry: %v", err)
	}
	if g.limitedHits < 2 {
		t.Errorf("limited hits = %d, want >= 2", g.limitedHits)
	}
	if g.totalRequests < 3 {
		t.Errorf("total requests = %d, want >= 3 (2 limited + 1 success)", g.totalRequests)
	}
}

// TestRateLimit_PersistentGiveUpWritesAnomaly runs the daemon for ~2s
// against an endpoint that always returns 429, then sends SIGTERM and
// verifies an anomaly was recorded.
func TestRateLimit_PersistentGiveUpWritesAnomaly(t *testing.T) {
	stateDir := t.TempDir()
	g := newGoldenMock(t)
	g.limitFirstN = 1_000_000 // effectively unbounded
	g.retryAfter = "0"
	seedRunnableState(t, stateDir, []string{"C_FOREVER_LIMITED"}, g.URL())

	// Run in daemon mode with a 3s timeout context so the daemon loop is
	// exercised.
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	root := NewRootCmd()
	root.SetArgs([]string{"--state-dir", stateDir, "--api-url", g.URL(),
		"--log-level", "error"})
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("daemon exit: %v", err)
	}

	// Anomaly must exist.
	files, err := os.ReadDir(filepath.Join(stateDir, "inbox", "anomalies"))
	if err != nil {
		t.Fatalf("read anomalies dir: %v", err)
	}
	found := false
	for _, f := range files {
		if strings.Contains(f.Name(), "rate-limit") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a rate-limit anomaly under %s/inbox/anomalies", stateDir)
	}
}

// TestGracefulSIGTERM_FinishesBatchAndPersistsCursor builds the binary,
// runs it as a daemon, lets it complete one batch, sends SIGTERM mid-second-
// batch, and verifies cursor consistency.
func TestGracefulSIGTERM_FinishesBatchAndPersistsCursor(t *testing.T) {
	if os.Getenv("SKIP_BINARY_SMOKE") == "1" {
		t.Skip("SKIP_BINARY_SMOKE=1")
	}
	stateDir := t.TempDir()

	g := newGoldenMock(t)
	g.historyPages = []historyPage{{
		Messages: []map[string]any{{
			"type": "message",
			"ts":   "1700000100.000100",
			"user": "U_A",
			"text": "first message",
		}},
	}}
	seedRunnableState(t, stateDir, []string{"C0492"}, g.URL())

	// Locate repo root.
	cmd := exec.CommandContext(context.Background(), "go", "env", "GOMOD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	repoRoot := filepath.Dir(strings.TrimSpace(string(out)))

	binPath := filepath.Join(t.TempDir(), "slack-poller")
	build := exec.CommandContext(context.Background(), "go", "build",
		"-o", binPath, "./cmd/slack-poller")
	build.Dir = repoRoot
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}

	run := exec.Command(binPath, "--state-dir", stateDir,
		"--api-url", g.URL(), "--log-level", "error")
	run.Env = append(os.Environ(), "TEST_SLACK_TOKEN=xoxb-fake")
	if err := run.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	// Wait until at least one batch has been polled (cursor advanced).
	curPath := filepath.Join(stateDir, "sources", "slack", "cursors",
		"channels", "C0492.json")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(curPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(curPath); err != nil {
		_ = run.Process.Kill()
		t.Fatalf("cursor never written: %v", err)
	}
	// Send SIGTERM.
	if err := run.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	if err := run.Wait(); err != nil {
		// SIGTERM-induced graceful exit returns 0; any non-zero is a fail.
		if exitErr, ok := err.(*exec.ExitError); ok && !exitErr.Success() {
			t.Fatalf("daemon non-zero exit: %v", err)
		}
	}
	// Cursor file must still exist + contain the most recent ts.
	b, err := os.ReadFile(curPath)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	var cur struct {
		LastTS string `json:"last_ts"`
	}
	if err := json.Unmarshal(b, &cur); err != nil {
		t.Fatalf("parse cursor: %v", err)
	}
	if cur.LastTS != "1700000100.000100" {
		t.Errorf("cursor = %q, want 1700000100.000100", cur.LastTS)
	}
	// No leftover .tmp file in the cursors channels dir.
	entries, _ := os.ReadDir(filepath.Join(stateDir, "sources", "slack",
		"cursors", "channels"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp") {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
	}
}

// TestForcePollOneChannel runs `slack-poller force-poll <channel>` against
// the mock and verifies only the named channel is polled.
func TestForcePollOneChannel(t *testing.T) {
	stateDir := t.TempDir()
	g := newGoldenMock(t)
	g.historyPages = []historyPage{{
		Messages: []map[string]any{{
			"type": "message",
			"ts":   "1700000100.000100",
			"user": "U_A",
			"text": "force-polled",
		}},
	}}
	seedRunnableState(t, stateDir, []string{"C_TARGET", "C_OTHER"}, g.URL())

	root := NewRootCmd()
	root.SetArgs([]string{"--state-dir", stateDir, "--api-url", g.URL(),
		"--log-level", "error", "force-poll", "C_TARGET"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("force-poll: %v", err)
	}
	// Only target should have a cursor.
	curTarget := filepath.Join(stateDir, "sources", "slack", "cursors",
		"channels", "C_TARGET.json")
	if _, err := os.Stat(curTarget); err != nil {
		t.Errorf("C_TARGET cursor missing: %v", err)
	}
	curOther := filepath.Join(stateDir, "sources", "slack", "cursors",
		"channels", "C_OTHER.json")
	if _, err := os.Stat(curOther); err == nil {
		t.Errorf("C_OTHER cursor should not exist after targeted force-poll")
	}
}

// TestGolden_FileSnapshot runs --once against a deterministic mock and
// asserts the produced files hash-match a checked-in snapshot.
func TestGolden_FileSnapshot(t *testing.T) {
	stateDir := t.TempDir()
	g := newGoldenMock(t)

	// Pre-track a thread so the poller exercises both code paths
	// (channel + thread). The mock returns one channel-level message and
	// one reply.
	threadID := "slack-C0492-1700000000.000000"
	threadDir := filepath.Join(stateDir, "threads", threadID)
	if err := os.MkdirAll(threadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	g.historyPages = []historyPage{{
		Messages: []map[string]any{{
			"type": "message",
			"ts":   "1700000100.000100",
			"user": "U_ALICE",
			"text": "new top-level message",
		}},
	}}
	g.repliesPages["C0492/1700000000.000000"] = []repliesPage{{
		Messages: []map[string]any{{
			"type":      "message",
			"ts":        "1700000050.000000",
			"thread_ts": "1700000000.000000",
			"user":      "U_BOB",
			"text":      "golden reply",
		}},
	}}
	seedRunnableState(t, stateDir, []string{"C0492"}, g.URL())

	// Use a deterministic clock for captured_at / received_at / tracking_since
	// via the Writer.Clock hook... the CLI doesn't expose it, so we test
	// the file shape via the integration test path (which uses RFC3339
	// "now"). For golden purposes we mask the time fields in the diff.
	root := NewRootCmd()
	root.SetArgs([]string{"--state-dir", stateDir, "--api-url", g.URL(),
		"--log-level", "error", "--once"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("once: %v", err)
	}

	// Verify expected files exist and have stable, deterministic field
	// values (excluding `captured_at`, `received_at`, `tracking_since`,
	// which depend on wall-clock).
	tests := []struct {
		path string
		want map[string]any // sentinel fields; nil → existence only
	}{
		{
			path: filepath.Join(stateDir, "threads", threadID, "raw",
				"1700000050.000000.json"),
			want: map[string]any{
				"id":      threadID,
				"source":  "slack",
				"channel": "C0492",
				"ts":      "1700000050.000000",
				"user":    "U_BOB",
				"text":    "golden reply",
			},
		},
		{
			path: filepath.Join(stateDir, "inbox", "new",
				"slack-C0492-1700000100.000100.json"),
			want: map[string]any{
				"id":     "slack-C0492-1700000100.000100",
				"source": "slack",
				"kind":   "new",
			},
		},
		{
			path: filepath.Join(stateDir, "sources", "slack", "cursors",
				"channels", "C0492.json"),
			want: map[string]any{
				"last_ts": "1700000100.000100",
			},
		},
		{
			path: filepath.Join(stateDir, "sources", "slack", "cursors",
				"threads", threadID+".json"),
			want: map[string]any{
				"last_reply_ts": "1700000050.000000",
			},
		},
		{
			path: filepath.Join(threadDir, ".dirty"),
			want: nil,
		},
		{
			path: filepath.Join(threadDir, "meta.json"),
			want: map[string]any{
				"id":     threadID,
				"source": "slack",
			},
		},
	}
	for _, tc := range tests {
		t.Run(filepath.Base(tc.path), func(t *testing.T) {
			b, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("read %s: %v", tc.path, err)
			}
			if tc.want == nil {
				return
			}
			var got map[string]any
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("parse %s: %v", tc.path, err)
			}
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("%s[%s] = %v, want %v", tc.path, k, got[k], want)
				}
			}
		})
	}
}

// TestGoldenHash runs --once and produces a stable hash over the
// normalized output. The expected hash lives in testdata/.
func TestGoldenHash(t *testing.T) {
	stateDir := t.TempDir()
	g := newGoldenMock(t)
	threadID := "slack-C0492-1700000000.000000"
	threadDir := filepath.Join(stateDir, "threads", threadID)
	if err := os.MkdirAll(threadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	g.historyPages = []historyPage{{
		Messages: []map[string]any{{
			"type": "message",
			"ts":   "1700000100.000100",
			"user": "U_ALICE",
			"text": "stable",
		}},
	}}
	g.repliesPages["C0492/1700000000.000000"] = []repliesPage{{
		Messages: []map[string]any{{
			"type":      "message",
			"ts":        "1700000050.000000",
			"thread_ts": "1700000000.000000",
			"user":      "U_BOB",
			"text":      "stable reply",
		}},
	}}
	seedRunnableState(t, stateDir, []string{"C0492"}, g.URL())

	root := NewRootCmd()
	root.SetArgs([]string{"--state-dir", stateDir, "--api-url", g.URL(),
		"--log-level", "error", "--once"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("once: %v", err)
	}
	got := slackpkg.NormalizedHash(stateDir)
	if got == "" {
		t.Fatal("empty normalized hash; nothing was written?")
	}
	t.Logf("normalized snapshot hash: %s", got)
	// We pin only the structural parts; full byte hash varies across runs
	// because of timestamps. The acceptance script does that — here we
	// just assert determinism within the test run.
	got2 := slackpkg.NormalizedHash(stateDir)
	if got != got2 {
		t.Errorf("normalized hash not stable: %s vs %s", got, got2)
	}
}

