//go:build integration

package slack

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBinarySmokeOnce verifies the slack-poller binary, run with --once
// against a mock Slack server, writes a raw event file and touches .dirty
// for a pre-tracked thread.
func TestBinarySmokeOnce(t *testing.T) {
	if os.Getenv("SKIP_BINARY_SMOKE") == "1" {
		t.Skip("SKIP_BINARY_SMOKE=1")
	}

	// Locate repo root via go env GOMOD then dir.
	cmd := exec.CommandContext(context.Background(), "go", "env", "GOMOD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	repoRoot := filepath.Dir(strings.TrimSpace(string(out)))
	stateRoot := t.TempDir()

	// Spin up mock Slack.
	m := newMockSlack(t)

	// Pre-track a thread so the poller takes the tracked-thread path.
	threadID := "slack-C0492-1700000000.000000"
	must(t, os.MkdirAll(filepath.Join(stateRoot, "threads", threadID), 0o755))
	m.addHistoryPages("C0492", mockHistoryPage{Messages: nil})
	m.addRepliesPages("C0492", "1700000000.000000", mockRepliesPage{
		Messages: []map[string]any{
			{
				"type":      "message",
				"ts":        "1700000050.000000",
				"thread_ts": "1700000000.000000",
				"user":      "U_ALICE",
				"text":      "smoke test reply",
			},
		},
	})

	// Write config.
	cfgDir := filepath.Join(stateRoot, "sources", "slack")
	must(t, os.MkdirAll(cfgDir, 0o755))
	must(t, os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(`
auth:
  token_env: TEST_SLACK_TOKEN
watch:
  channels:
    - id: C0492
poll_interval: 5s
`), 0o644))

	// Build and run the binary.
	binPath := filepath.Join(t.TempDir(), "slack-poller")
	build := exec.CommandContext(context.Background(), "go", "build",
		"-o", binPath, "./cmd/slack-poller")
	build.Dir = repoRoot
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}

	run := exec.CommandContext(context.Background(), binPath,
		"--state-dir", stateRoot, "--once", "--api-url", m.URL(),
		"--log-level", "debug")
	run.Env = append(os.Environ(), "TEST_SLACK_TOKEN=xoxb-fake-token")
	out2, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("slack-poller --once: %v\noutput:\n%s", err, out2)
	}
	t.Logf("binary output:\n%s", out2)

	// Verify raw event file.
	rawPath := filepath.Join(stateRoot, "threads", threadID, "raw", "1700000050.000000.json")
	if _, err := os.Stat(rawPath); err != nil {
		t.Fatalf("raw event missing: %v", err)
	}
	// Verify .dirty marker.
	dirtyPath := filepath.Join(stateRoot, "threads", threadID, ".dirty")
	if _, err := os.Stat(dirtyPath); err != nil {
		t.Fatalf(".dirty missing: %v", err)
	}
	// Verify cursor advanced.
	curPath := filepath.Join(stateRoot, "sources", "slack", "cursors",
		"threads", threadID+".json")
	if _, err := os.Stat(curPath); err != nil {
		t.Fatalf("thread cursor missing: %v", err)
	}
}
