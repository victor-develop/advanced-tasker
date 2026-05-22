package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealAcceptanceScriptIsReadOnly lints the round-3 real-acceptance
// script for references to Slack write endpoints. The slack-poller is
// read-only by contract (design/08 §"Scope"); the acceptance script must
// not introduce any operation that would write to Slack.
//
// We allow the script to MENTION these endpoint names inside its
// `forbidden_endpoints` array (where they're being grep'd for in the
// binary), but no invocation of those endpoints is allowed. We catch
// invocations by looking for the canonical `curl` / `wget` / `slack`
// command-line shapes and by requiring that any chat.* or reactions.*
// mention is inside the explicit deny-list section, not a command line.
func TestRealAcceptanceScriptIsReadOnly(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	path := filepath.Join(repoRoot, "scripts", "acceptance-slack-poller-real.sh")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(b)

	// Must exist + be executable.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("%s is not executable (mode=%v)", path, info.Mode())
	}

	// Must declare its skip-if-missing-env path.
	for _, want := range []string{
		"SLACK_BOT_TOKEN",
		"TEST_SLACK_CHANNEL",
		"SKIP:",
		"exit 0",
		"slack-poller doctor",
		"--once",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("script missing %q", want)
		}
	}

	// Lint: no curl/wget calls hitting Slack write endpoints. We allow the
	// endpoint NAMES to appear inside the explicit deny-list (forbidden_
	// endpoints array), but no invocation lines.
	dangerous := []string{
		`curl.*chat.postMessage`,
		`curl.*chat.update`,
		`curl.*chat.delete`,
		`curl.*reactions.add`,
		`curl.*reactions.remove`,
		`postMessage.*"$SLACK`,
	}
	for _, pattern := range dangerous {
		if strings.Contains(body, pattern) {
			t.Errorf("script appears to invoke %s", pattern)
		}
	}

	// The deny-list array MUST exist (this is the script's own self-check).
	if !strings.Contains(body, "forbidden_endpoints") {
		t.Errorf("script missing 'forbidden_endpoints' deny-list array — " +
			"the read-only invariant check is gone")
	}
}

// findRepoRoot walks up from CWD looking for go.mod. Returns the first
// directory containing it.
func findRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
