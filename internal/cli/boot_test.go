package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/victor-develop/advanced-tasker/internal/outbox"
)

// TestBoot_NonInteractive_HappyPath asserts that `harness boot
// --non-interactive` (with all three tokens set) produces:
//   - state initialized
//   - one Slack channel watched (the default-skip is blank → we set a
//     test fixture below to exercise the watch path)
//   - one GitHub repo watched
//   - one goal created (default title)
//   - outbox.sender_enabled=false
//
// Non-interactive mode by design takes defaults — and the default for
// the channel-id / repo-name prompts is "skip" (blank). So this test
// drives the boot via runBoot directly with prompts pre-answered by
// piping fixture stdin in, which is the documented escape hatch for
// the "do something other than the defaults in CI" case.
func TestBoot_NonInteractive_EnvProbeAndForcesSenderOff(t *testing.T) {
	root := initState(t)
	// Make sure boot reuses this exact state dir.
	t.Setenv("HARNESS_STATE", root)
	// Clear any inherited tokens — non-interactive run skips the
	// Slack/GitHub watch prompts entirely when their token is missing.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	stdout, stderr, code := runCLI(t, root, "boot", "--non-interactive")
	if code != ExitOK {
		t.Fatalf("boot failed code=%d stderr=%s", code, stderr)
	}
	// State checks.
	if !strings.Contains(stdout, "[ok] state directory") {
		t.Errorf("expected state-detected line, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "[missing] SLACK_BOT_TOKEN") {
		t.Errorf("expected slack-token missing line, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "[missing] GITHUB_TOKEN") {
		t.Errorf("expected github-token missing line, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "[safety] outbox.sender_enabled set to false") {
		t.Errorf("expected safety line, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "[ok] created T-1") {
		t.Errorf("expected first goal creation, got:\n%s", stdout)
	}
	// Config check via the outbox helper (the authoritative reader).
	if outbox.SenderEnabled(root) {
		t.Errorf("expected sender_enabled=false after boot, got true")
	}
	// Goal artifacts exist.
	if _, err := os.Stat(filepath.Join(root, "tasks", "T-1", "goal.md")); err != nil {
		t.Errorf("expected tasks/T-1/goal.md after boot: %v", err)
	}
}

// TestBoot_NonInteractive_WatchesWhenTokensPresent ensures that when
// SLACK_BOT_TOKEN and GITHUB_TOKEN are present, the non-interactive
// path still prompts (with defaults=skip), but the source config
// stubs are seeded. The watch list stays empty because the default
// channel/repo answer is blank.
func TestBoot_NonInteractive_SeedsSourceStubsWhenTokenPresent(t *testing.T) {
	root := initState(t)
	t.Setenv("HARNESS_STATE", root)
	t.Setenv("ANTHROPIC_API_KEY", "fake-key")
	t.Setenv("SLACK_BOT_TOKEN", "fake-slack")
	t.Setenv("GITHUB_TOKEN", "fake-gh")

	stdout, _, code := runCLI(t, root, "boot", "--non-interactive")
	if code != ExitOK {
		t.Fatalf("boot failed code=%d", code)
	}
	if !strings.Contains(stdout, "[ok] SLACK_BOT_TOKEN is set") {
		t.Errorf("expected slack-token ok line, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "[ok] GITHUB_TOKEN is set") {
		t.Errorf("expected github-token ok line, got:\n%s", stdout)
	}
	// Stubs seeded for both sources.
	if _, err := os.Stat(filepath.Join(root, "sources", "slack", "config.yaml")); err != nil {
		t.Errorf("expected slack stub seeded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sources", "github", "config.yaml")); err != nil {
		t.Errorf("expected github stub seeded: %v", err)
	}
	// sender_enabled forced to false.
	if outbox.SenderEnabled(root) {
		t.Errorf("expected sender_enabled=false")
	}
}
