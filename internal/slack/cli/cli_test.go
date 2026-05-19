package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
)

// runCmd executes the cobra root with args using stateDir, returning
// (stdout, stderr, exit-code-derived-from-error).
func runCmd(t *testing.T, stateDir string, args ...string) (string, string, int) {
	t.Helper()
	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	full := append([]string{"--state-dir", stateDir}, args...)
	root.SetArgs(full)
	err := root.ExecuteContext(context.Background())
	code := 0
	if err != nil {
		code = ExtractExitCode(err)
		// cobra is configured with SilenceErrors=true at the root; the
		// command implementations don't print the error message via cobra's
		// own machinery either, so we don't see it in stderr unless we add
		// it ourselves. Append for test visibility.
		stderr.WriteString(err.Error())
		stderr.WriteString("\n")
	}
	return stdout.String(), stderr.String(), code
}

// seedConfig writes a minimal valid config to state/sources/slack/config.yaml.
func seedConfig(t *testing.T, stateDir string, channels ...string) {
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
	b.WriteString("poll_interval: 30s\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigMissingExitsWithDocumentedMessage(t *testing.T) {
	stateDir := t.TempDir()
	_, stderr, code := runCmd(t, stateDir, "status")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	wantMsg := "run 'harness config init slack' to seed config"
	if !strings.Contains(stderr, wantMsg) {
		t.Errorf("stderr does not contain %q:\n%s", wantMsg, stderr)
	}
}

func TestWatchAddsChannelIdempotent(t *testing.T) {
	stateDir := t.TempDir()
	seedConfig(t, stateDir, "C_OLD")

	t.Run("add new", func(t *testing.T) {
		stdout, _, code := runCmd(t, stateDir, "watch", "C_NEW",
			"--reason", "PR alerts")
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if !strings.Contains(stdout, "watching channel C_NEW") {
			t.Errorf("stdout = %q", stdout)
		}
		cfg, err := slackpkg.LoadConfig(filepath.Join(stateDir,
			"sources", "slack", "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range cfg.Watch.Channels {
			if c.ID == "C_NEW" {
				found = true
				if c.Reason != "PR alerts" {
					t.Errorf("reason = %q", c.Reason)
				}
			}
		}
		if !found {
			t.Errorf("C_NEW not in config after watch")
		}
	})

	t.Run("re-add is idempotent", func(t *testing.T) {
		stdout, _, code := runCmd(t, stateDir, "watch", "C_OLD")
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if !strings.Contains(stdout, "already watched") {
			t.Errorf("stdout = %q", stdout)
		}
	})
}

func TestUnwatchRemovesChannelAndCursor(t *testing.T) {
	stateDir := t.TempDir()
	seedConfig(t, stateDir, "C0492", "C1234")

	// Pre-seed a cursor so we can verify deletion.
	cursors, err := slackpkg.NewCursorStore(filepath.Join(stateDir,
		"sources", "slack", "cursors"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cursors.SetChannelCursor("C0492", "100.000"); err != nil {
		t.Fatal(err)
	}
	curPath := filepath.Join(stateDir, "sources", "slack", "cursors",
		"channels", "C0492.json")

	t.Run("unwatch existing", func(t *testing.T) {
		stdout, _, code := runCmd(t, stateDir, "unwatch", "C0492")
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if !strings.Contains(stdout, "unwatched channel C0492") {
			t.Errorf("stdout = %q", stdout)
		}
		// Cursor must be gone.
		if _, err := os.Stat(curPath); err == nil {
			t.Errorf("cursor file should have been deleted")
		}
		// Channel must be gone from config.
		cfg, err := slackpkg.LoadConfig(filepath.Join(stateDir,
			"sources", "slack", "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cfg.Watch.Channels {
			if c.ID == "C0492" {
				t.Errorf("C0492 still in config after unwatch")
			}
		}
	})

	t.Run("unwatch absent is idempotent", func(t *testing.T) {
		stdout, _, code := runCmd(t, stateDir, "unwatch", "C_ABSENT")
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if !strings.Contains(stdout, "not watched") {
			t.Errorf("stdout = %q", stdout)
		}
	})
}

// TestTrackThreadHappyPath promotes an inbox/new entry to a tracked thread.
func TestTrackThreadHappyPath(t *testing.T) {
	stateDir := t.TempDir()
	seedConfig(t, stateDir, "C0492")

	// Pre-seed inbox/new entry.
	w := slackpkg.NewWriter(stateDir)
	threadTS := "1700000100.000100"
	item := &slackpkg.InboxItem{
		ID:      slackpkg.ThreadID("C0492", threadTS),
		Summary: "hey",
		Ref: slackpkg.InboxRef{
			Channel: "C0492",
			TS:      threadTS,
			User:    "U_ALICE",
		},
		RawInline: map[string]any{
			"text":      "hey, what's the status?",
			"permalink": "https://example.test/p1",
		},
	}
	if _, err := w.WriteInboxNew(item); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runCmd(t, stateDir, "track-thread", "C0492",
		threadTS)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "promoted") {
		t.Errorf("stdout = %q", stdout)
	}

	// Files we expect.
	threadID := slackpkg.ThreadID("C0492", threadTS)
	rawPath := filepath.Join(stateDir, "threads", threadID, "raw",
		threadTS+".json")
	if _, err := os.Stat(rawPath); err != nil {
		t.Errorf("raw event missing: %v", err)
	}
	metaPath := filepath.Join(stateDir, "threads", threadID, "meta.json")
	b, err := os.ReadFile(metaPath)
	if err != nil {
		t.Errorf("meta missing: %v", err)
	}
	var m slackpkg.Meta
	if err := json.Unmarshal(b, &m); err != nil {
		t.Errorf("parse meta: %v", err)
	}
	if m.URL == "" {
		t.Errorf("meta.url empty")
	}
	dirtyPath := filepath.Join(stateDir, "threads", threadID, ".dirty")
	if _, err := os.Stat(dirtyPath); err != nil {
		t.Errorf(".dirty missing: %v", err)
	}
	inboxPath := filepath.Join(stateDir, "inbox", "new", threadID+".json")
	if _, err := os.Stat(inboxPath); err == nil {
		t.Errorf("inbox/new entry should be deleted")
	}
}

// TestTrackThreadMissingInboxRejected verifies exit 2 + clear error.
func TestTrackThreadMissingInboxRejected(t *testing.T) {
	stateDir := t.TempDir()
	seedConfig(t, stateDir, "C0492")

	_, stderr, code := runCmd(t, stateDir, "track-thread", "C0492",
		"1700000200.000200")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "missing") {
		t.Errorf("stderr should mention missing inbox entry: %q", stderr)
	}
}

// TestTrackThreadIdempotent ensures re-running on an already-tracked thread
// exits 0 silently.
func TestTrackThreadIdempotent(t *testing.T) {
	stateDir := t.TempDir()
	seedConfig(t, stateDir, "C0492")

	threadTS := "1700000100.000100"
	threadID := slackpkg.ThreadID("C0492", threadTS)
	rawDir := filepath.Join(stateDir, "threads", threadID, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rawDir, threadTS+".json"),
		[]byte(`{"id":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := runCmd(t, stateDir, "track-thread", "C0492",
		threadTS)
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "already tracked") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestUntrackThreadDeletesCursor(t *testing.T) {
	stateDir := t.TempDir()
	seedConfig(t, stateDir, "C0492")

	threadID := "slack-C0492-1700000000.000000"
	threadDir := filepath.Join(stateDir, "threads", threadID)
	if err := os.MkdirAll(threadDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cursors, _ := slackpkg.NewCursorStore(filepath.Join(stateDir,
		"sources", "slack", "cursors"))
	if err := cursors.SetThreadCursor(threadID, "100.000"); err != nil {
		t.Fatal(err)
	}

	t.Run("no archive", func(t *testing.T) {
		stdout, _, code := runCmd(t, stateDir, "untrack-thread", threadID)
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if !strings.Contains(stdout, "cursor cleared") {
			t.Errorf("stdout = %q", stdout)
		}
		curPath := filepath.Join(stateDir, "sources", "slack",
			"cursors", "threads", threadID+".json")
		if _, err := os.Stat(curPath); err == nil {
			t.Errorf("cursor should be deleted")
		}
		// Thread dir preserved.
		if _, err := os.Stat(threadDir); err != nil {
			t.Errorf("thread dir should remain: %v", err)
		}
	})

	t.Run("archive moves dir", func(t *testing.T) {
		// Re-create the cursor for this subtest.
		if err := cursors.SetThreadCursor(threadID, "100.000"); err != nil {
			t.Fatal(err)
		}
		stdout, _, code := runCmd(t, stateDir, "untrack-thread", threadID,
			"--archive")
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if !strings.Contains(stdout, "archived to") {
			t.Errorf("stdout = %q", stdout)
		}
		if _, err := os.Stat(threadDir); err == nil {
			t.Errorf("thread dir should be moved out of threads/")
		}
		archivePath := filepath.Join(stateDir, "threads", "_archive",
			threadID)
		if _, err := os.Stat(archivePath); err != nil {
			t.Errorf("archive missing: %v", err)
		}
	})
}

func TestStatusJSON(t *testing.T) {
	stateDir := t.TempDir()
	seedConfig(t, stateDir, "C0492", "C1234")

	threadID := "slack-C0492-1700000000.000000"
	if err := os.MkdirAll(filepath.Join(stateDir, "threads", threadID),
		0o755); err != nil {
		t.Fatal(err)
	}
	cursors, _ := slackpkg.NewCursorStore(filepath.Join(stateDir,
		"sources", "slack", "cursors"))
	if err := cursors.SetChannelCursor("C0492", "100.000"); err != nil {
		t.Fatal(err)
	}
	if err := cursors.SetThreadCursor(threadID, "200.000"); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := runCmd(t, stateDir, "status", "--json")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var report StatusReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("parse JSON: %v\n%s", err, stdout)
	}
	if len(report.TrackedChannels) != 2 {
		t.Errorf("TrackedChannels = %d", len(report.TrackedChannels))
	}
	if len(report.TrackedThreads) != 1 {
		t.Errorf("TrackedThreads = %d", len(report.TrackedThreads))
	}
	if report.TrackedThreads[0].LastReplyTS != "200.000" {
		t.Errorf("thread cursor = %q", report.TrackedThreads[0].LastReplyTS)
	}
	gotChannel := report.TrackedChannels[0]
	if gotChannel.ID == "C0492" && gotChannel.LastTS != "100.000" {
		t.Errorf("C0492 last_ts = %q", gotChannel.LastTS)
	}
}

func TestStatusHumanReadable(t *testing.T) {
	stateDir := t.TempDir()
	seedConfig(t, stateDir, "C_X")
	stdout, _, code := runCmd(t, stateDir, "status")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"config path:", "tracked channels (1):",
		"C_X", "tracked threads (0):"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status output missing %q\n%s", want, stdout)
		}
	}
}
