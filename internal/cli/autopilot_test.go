package cli

import (
	"os"
	"path/filepath"
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
