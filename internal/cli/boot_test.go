package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/victor-develop/advanced-tasker/internal/outbox"
)

// withLookPath swaps the package-level lookPath stub for the duration
// of a test. Restores on cleanup.
func withLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	saved := lookPath
	lookPath = fn
	t.Cleanup(func() { lookPath = saved })
}

// TestBoot_NonInteractive_NoEnvProbeForAnthropicKey asserts the round-3
// pre-merge fix: boot must NOT mention ANTHROPIC_API_KEY in any form,
// because the production driver `claude-p` authenticates via Claude
// Code's OAuth (no API key env var).
func TestBoot_NonInteractive_NoEnvProbeForAnthropicKey(t *testing.T) {
	root := initState(t)
	t.Setenv("HARNESS_STATE", root)
	// Even when the env var IS set, boot must not call it out.
	t.Setenv("ANTHROPIC_API_KEY", "should-be-ignored")
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	// Default config has models.driver=claude-p; stub PATH so the
	// test doesn't depend on the developer's machine.
	withLookPath(t, func(name string) (string, error) {
		if name == "claude" {
			return "/usr/local/bin/claude", nil
		}
		return "", os.ErrNotExist
	})
	stdout, stderr, code := runCLI(t, root, "boot", "--non-interactive")
	if code != ExitOK {
		t.Fatalf("boot failed code=%d stderr=%s", code, stderr)
	}
	if strings.Contains(stdout, "ANTHROPIC_API_KEY") {
		t.Errorf("boot must NOT mention ANTHROPIC_API_KEY; claude-p uses OAuth.\n--- stdout ---\n%s", stdout)
	}
}

// TestBoot_NonInteractive_EnvProbeAndForcesSenderOff confirms the
// per-source token probe lines (Slack/GitHub) and the safety-net
// behavior remain intact after the round-3 pre-merge driver-probe
// refactor.
func TestBoot_NonInteractive_EnvProbeAndForcesSenderOff(t *testing.T) {
	root := initState(t)
	t.Setenv("HARNESS_STATE", root)
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	withLookPath(t, func(name string) (string, error) {
		if name == "claude" {
			return "/usr/local/bin/claude", nil
		}
		return "", os.ErrNotExist
	})
	stdout, stderr, code := runCLI(t, root, "boot", "--non-interactive")
	if code != ExitOK {
		t.Fatalf("boot failed code=%d stderr=%s", code, stderr)
	}
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
	if outbox.SenderEnabled(root) {
		t.Errorf("expected sender_enabled=false after boot, got true")
	}
	if _, err := os.Stat(filepath.Join(root, "tasks", "T-1", "goal.md")); err != nil {
		t.Errorf("expected tasks/T-1/goal.md after boot: %v", err)
	}
}

// TestBoot_NonInteractive_SeedsSourceStubsWhenTokenPresent.
func TestBoot_NonInteractive_SeedsSourceStubsWhenTokenPresent(t *testing.T) {
	root := initState(t)
	t.Setenv("HARNESS_STATE", root)
	t.Setenv("SLACK_BOT_TOKEN", "fake-slack")
	t.Setenv("GITHUB_TOKEN", "fake-gh")
	withLookPath(t, func(name string) (string, error) {
		if name == "claude" {
			return "/usr/local/bin/claude", nil
		}
		return "", os.ErrNotExist
	})
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
	if _, err := os.Stat(filepath.Join(root, "sources", "slack", "config.yaml")); err != nil {
		t.Errorf("expected slack stub seeded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sources", "github", "config.yaml")); err != nil {
		t.Errorf("expected github stub seeded: %v", err)
	}
	if outbox.SenderEnabled(root) {
		t.Errorf("expected sender_enabled=false")
	}
}

// TestBoot_ClaudePDriverProbe_FindsBinary exercises the round-3
// pre-merge fix: when models.driver=claude-p AND `claude` is on PATH,
// boot prints the "claude CLI found at <path>" line and does NOT
// mention ANTHROPIC_API_KEY.
func TestBoot_ClaudePDriverProbe_FindsBinary(t *testing.T) {
	root := initState(t)
	t.Setenv("HARNESS_STATE", root)
	// Default DefaultConfigYAML already sets models.driver=claude-p.
	withLookPath(t, func(name string) (string, error) {
		if name == "claude" {
			return "/opt/homebrew/bin/claude", nil
		}
		return "", os.ErrNotExist
	})
	stdout, _, code := runCLI(t, root, "boot", "--non-interactive")
	if code != ExitOK {
		t.Fatalf("boot failed code=%d", code)
	}
	want := "[ok] claude CLI found at /opt/homebrew/bin/claude (used by claude-p driver; auth managed via 'claude /login')"
	if !strings.Contains(stdout, want) {
		t.Errorf("expected claude-p probe line %q in:\n%s", want, stdout)
	}
	if strings.Contains(stdout, "ANTHROPIC_API_KEY") {
		t.Errorf("claude-p probe must not mention ANTHROPIC_API_KEY:\n%s", stdout)
	}
}

// TestBoot_ClaudePDriverProbe_BinaryMissing — `claude` not on PATH
// emits the actionable missing-line that points at Claude Code install
// or models.driver=fake.
func TestBoot_ClaudePDriverProbe_BinaryMissing(t *testing.T) {
	root := initState(t)
	t.Setenv("HARNESS_STATE", root)
	withLookPath(t, func(name string) (string, error) {
		return "", os.ErrNotExist
	})
	stdout, _, code := runCLI(t, root, "boot", "--non-interactive")
	if code != ExitOK {
		t.Fatalf("boot failed code=%d", code)
	}
	want := "[missing] 'claude' CLI not found on PATH — install Claude Code or set models.driver=fake to test offline"
	if !strings.Contains(stdout, want) {
		t.Errorf("expected missing-claude line %q in:\n%s", want, stdout)
	}
	if strings.Contains(stdout, "ANTHROPIC_API_KEY") {
		t.Errorf("missing-claude probe must not mention ANTHROPIC_API_KEY:\n%s", stdout)
	}
}

// TestBoot_FakeDriverProbe — models.driver=fake emits the
// no-credentials-needed line and skips the PATH lookup.
func TestBoot_FakeDriverProbe(t *testing.T) {
	root := initState(t)
	t.Setenv("HARNESS_STATE", root)
	// Flip the driver to fake.
	if _, _, code := runCLI(t, root, "config", "set", "models.driver", "fake"); code != ExitOK {
		t.Fatalf("config set models.driver=fake: %d", code)
	}
	// lookPath should not be invoked, but stub anyway so a regression
	// would surface clearly.
	called := false
	withLookPath(t, func(name string) (string, error) {
		called = true
		return "/should/not/be/used", nil
	})
	stdout, _, code := runCLI(t, root, "boot", "--non-interactive")
	if code != ExitOK {
		t.Fatalf("boot failed code=%d", code)
	}
	want := "[ok] fake driver selected — no LLM credentials needed"
	if !strings.Contains(stdout, want) {
		t.Errorf("expected fake-driver line %q in:\n%s", want, stdout)
	}
	if called {
		t.Errorf("fake driver path must not invoke lookPath")
	}
	if strings.Contains(stdout, "ANTHROPIC_API_KEY") {
		t.Errorf("fake-driver probe must not mention ANTHROPIC_API_KEY:\n%s", stdout)
	}
}
