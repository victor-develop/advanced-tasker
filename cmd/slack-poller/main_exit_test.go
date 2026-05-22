package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestBinary_DoctorBadTokenExitsOne forks the real slack-poller binary
// against a stub Slack API server returning invalid_auth and asserts the
// process exit code is 1.
//
// This guards the chain RunE → CommandError → Execute → os.Exit. The unit
// tests in internal/slack/cli/doctor_test.go check the RunE return path;
// this one verifies main wiring isn't accidentally swallowing the code.
func TestBinary_DoctorBadTokenExitsOne(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short")
	}
	if runtime.GOOS == "windows" {
		t.Skip("skip on windows")
	}

	tmp := t.TempDir()
	bin := filepath.Join(tmp, "slack-poller")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}

	stateDir := filepath.Join(tmp, "state")
	if err := os.MkdirAll(filepath.Join(stateDir, "sources", "slack"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := `auth:
  token_env: SLACK_BOT_TOKEN
watch:
  channels:
    - id: CTEST
poll_interval: 30s
`
	if err := os.WriteFile(filepath.Join(stateDir, "sources", "slack", "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(bin, "--state-dir", stateDir, "doctor")
	cmd.Env = append(os.Environ(),
		// Deliberately invalid token; doctor will fail at auth.test against
		// real Slack (the binary uses slack.com by default).
		"SLACK_BOT_TOKEN=xoxb-deliberately-invalid",
	)
	out, err := cmd.CombinedOutput()
	t.Logf("doctor output:\n%s", out)

	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	if got := ee.ExitCode(); got != 1 {
		t.Fatalf("exit code = %d, want 1", got)
	}

	// Sanity: the actionable error message must appear on stderr (mixed
	// into CombinedOutput here).
	if !strings.Contains(string(out), "token invalid") {
		t.Errorf("missing actionable error message; got:\n%s", out)
	}
}
