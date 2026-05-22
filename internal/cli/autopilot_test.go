package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAutopilotFakeDriverShortRun is a fast smoke that the full daemon
// loop boots and exits cleanly with --driver fake --duration 2s. We do
// NOT assert any specific events fired (timing-dependent); see
// scripts/acceptance-autopilot.sh for the assertion-rich version.
func TestAutopilotFakeDriverShortRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping autopilot smoke under -short")
	}
	root := initState(t)
	// Seed a goal so there's at least one task in state.
	runCLI(t, root, "goal", "create", "G")

	// Need to seed at least an empty fixture dir so Fake driver has a
	// home. Missing fixtures fall back to "[fake-driver: no fixture..."
	// strings — that's fine for this smoke test.
	fix := filepath.Join(t.TempDir(), "fake")
	for _, sub := range []string{"commander", "worker", "updater", "auditor"} {
		os.MkdirAll(filepath.Join(fix, sub), 0o755)
	}
	t.Setenv("HARNESS_FAKE_FIXTURES", fix)

	_, _, code := runCLI(t, root, "autopilot", "start", "--driver", "fake", "--duration", "2s")
	if code != ExitOK {
		t.Fatalf("autopilot smoke failed: %d", code)
	}
	// Verify a tick-log file appeared.
	entries, _ := os.ReadDir(filepath.Join(root, "tick-log"))
	if len(entries) == 0 {
		t.Errorf("expected at least one tick-log entry; got none")
	}
	// audit reports
	a, _ := os.ReadDir(filepath.Join(root, "audit", "reports"))
	if len(a) == 0 {
		t.Errorf("expected at least one audit report")
	}
}

// TestSummaryLog_OneLinePerTick is the regression test for the round-2
// e2e finding that telemetry/summary.log received two lines per tick
// (once from the scheduler, once from `harness tick end`). After the
// polish fix, `harness tick end` is the single writer and the scheduler
// passes session_id / is_error through as flags.
func TestSummaryLog_OneLinePerTick(t *testing.T) {
	root := initState(t)

	summary := filepath.Join(root, "telemetry", "summary.log")
	if _, err := os.Stat(summary); !os.IsNotExist(err) {
		// init may have created an empty file or not — measure delta.
		_ = os.Remove(summary)
	}

	// One scheduler-like end-to-end: start a tick, end it with the
	// flags the scheduler passes in production.
	_, _, code := runCLI(t, root, "tick", "start", "--as", "test-agent", "--ttl", "1m")
	if code != ExitOK {
		t.Fatalf("tick start: %d", code)
	}
	_, _, code = runCLI(t, root, "tick", "end", "--idle",
		"--cost-usd=0.0123",
		"--duration-ms=4567",
		"--session-id=test-session-abc",
	)
	if code != ExitOK {
		t.Fatalf("tick end: %d", code)
	}

	b, err := os.ReadFile(summary)
	if err != nil {
		t.Fatalf("read summary.log: %v", err)
	}
	// Count non-empty lines.
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	n := 0
	for _, ln := range lines {
		if len(bytes.TrimSpace(ln)) > 0 {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("summary.log lines = %d, want 1\n--full--\n%s", n, b)
	}

	// And the line carries the session_id we passed (not empty).
	if !strings.Contains(string(b), "session=test-session-abc") {
		t.Errorf("summary.log missing session=test-session-abc\n%s", b)
	}
}
