package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPostCommitHookCatchesLedgerViolation simulates a smuggled-in
// commit that mutates a ledger line; the post-commit hook MUST reset
// HEAD and write an anomaly file.
//
// This test requires `harness` on PATH (we point HARNESS_BIN to the
// freshly-built binary in t.TempDir()).
func TestPostCommitHookCatchesLedgerViolation(t *testing.T) {
	root := initState(t)
	// Build a binary the hook can call.
	bin := filepath.Join(t.TempDir(), "harness")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/victor-develop/advanced-tasker/cmd/harness").CombinedOutput(); err != nil {
		t.Fatalf("build harness: %v %s", err, out)
	}
	// Seed an initial rollup via the CLI so the validator is happy.
	runCLI(t, root, "thread", "track", "slack-T1-1")
	initial := `---
id: slack-T1-1
source: slack
state: in-progress
---
## Goal
g
## Current ask
- a
## Open questions
- [ ] q
## Decisions ledger
- 2026-01-01: A
- 2026-01-02: B
## Verbatim pins
> "x" — y (— pinned by human)
`
	f := filepath.Join(t.TempDir(), "v1.md")
	os.WriteFile(f, []byte(initial), 0o644)
	if _, _, code := runCLI(t, root, "rollup", "update", "slack-T1-1", "--file", f); code != ExitOK {
		t.Fatalf("seed rollup failed: %d", code)
	}
	headBefore := gitHead(t, root)

	// Now smuggle a violation: write directly + git commit, bypassing CLI.
	rollPath := filepath.Join(root, "threads", "slack-T1-1", "rollup.md")
	bad := strings.Replace(initial, "- 2026-01-02: B\n", "", 1) // remove second ledger line
	if err := os.WriteFile(rollPath, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "threads/slack-T1-1/rollup.md")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	cmd = exec.Command("git", "commit", "-m", "smuggled violation")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HARNESS_BIN="+bin)
	out, err := cmd.CombinedOutput()
	// The hook should reset HEAD and exit non-zero. git commit then
	// returns non-zero — that's the signal we want.
	_ = err
	t.Logf("git commit output: %s", out)

	headAfter := gitHead(t, root)
	if headAfter != headBefore {
		t.Errorf("expected HEAD to remain at %s after hook reset, got %s\n%s", headBefore, headAfter, out)
	}
	// Anomaly file should exist.
	entries, _ := os.ReadDir(filepath.Join(root, "inbox", "anomalies"))
	if len(entries) == 0 {
		t.Errorf("expected anomaly file written by post-commit hook")
	}
}

func gitHead(t *testing.T, root string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestRollupUpdate_FrontmatterIDMismatchRejected exercises design/05
// §"Step 0.5" via the CLI: a rollup whose frontmatter.id does not match
// the thread directory MUST exit 2, write an inbox anomaly, and leave
// HEAD untouched. (Round-3 D3.)
func TestRollupUpdate_FrontmatterIDMismatchRejected(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "thread", "track", "slack-AAA-1")
	headBefore := gitHead(t, root)

	bad := `---
id: slack-WRONG-id
source: slack
state: in-progress
---
## Goal
g
## Current ask
- a
## Open questions
- [ ] q
## Decisions ledger
## Verbatim pins
`
	f := filepath.Join(t.TempDir(), "bad.md")
	if err := os.WriteFile(f, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runCLI(t, root, "rollup", "update", "slack-AAA-1", "--file", f)
	if code != ExitValidation {
		t.Fatalf("expected exit %d on id mismatch, got %d (stderr=%s)", ExitValidation, code, stderr)
	}
	if !strings.Contains(stderr, "frontmatter.id") || !strings.Contains(stderr, "slack-WRONG-id") || !strings.Contains(stderr, "slack-AAA-1") {
		t.Errorf("expected stderr to mention both ids; got %q", stderr)
	}
	// HEAD unchanged.
	if got := gitHead(t, root); got != headBefore {
		t.Errorf("expected HEAD unchanged after rejection, got %s want %s", got, headBefore)
	}
	// Anomaly written.
	entries, _ := os.ReadDir(filepath.Join(root, "inbox", "anomalies"))
	if len(entries) == 0 {
		t.Errorf("expected anomaly file written on id mismatch")
	}
}

// TestPostCommitHookCatchesFrontmatterIDMismatch smuggles a wrong-id
// commit past the CLI (direct git plumbing) and asserts the post-commit
// hook reverts via `git reset --hard HEAD~1` and drops an anomaly.
// (Round-3 D3.)
func TestPostCommitHookCatchesFrontmatterIDMismatch(t *testing.T) {
	root := initState(t)
	bin := filepath.Join(t.TempDir(), "harness")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/victor-develop/advanced-tasker/cmd/harness").CombinedOutput(); err != nil {
		t.Fatalf("build harness: %v %s", err, out)
	}
	// Seed a valid rollup via the CLI so we have a HEAD~1 to compare to.
	runCLI(t, root, "thread", "track", "slack-T2-1")
	good := `---
id: slack-T2-1
source: slack
state: in-progress
---
## Goal
g
## Current ask
- a
## Open questions
- [ ] q
## Decisions ledger
- 2026-01-01: A
## Verbatim pins
`
	f := filepath.Join(t.TempDir(), "good.md")
	os.WriteFile(f, []byte(good), 0o644)
	if _, _, code := runCLI(t, root, "rollup", "update", "slack-T2-1", "--file", f); code != ExitOK {
		t.Fatalf("seed rollup failed: %d", code)
	}
	headBefore := gitHead(t, root)

	// Smuggle: rewrite the file with a mismatched id; commit via raw git.
	bad := strings.Replace(good, "id: slack-T2-1", "id: slack-SOMEONE-ELSE-99", 1)
	rollPath := filepath.Join(root, "threads", "slack-T2-1", "rollup.md")
	if err := os.WriteFile(rollPath, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	gitAdd := exec.Command("git", "add", "threads/slack-T2-1/rollup.md")
	gitAdd.Dir = root
	if out, err := gitAdd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	gitCommit := exec.Command("git", "commit", "-m", "smuggled id mismatch")
	gitCommit.Dir = root
	gitCommit.Env = append(os.Environ(), "HARNESS_BIN="+bin)
	out, _ := gitCommit.CombinedOutput()
	t.Logf("git commit output: %s", out)

	if got := gitHead(t, root); got != headBefore {
		t.Errorf("expected HEAD to remain at %s after hook revert, got %s\n%s", headBefore, got, out)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "inbox", "anomalies"))
	if len(entries) == 0 {
		t.Errorf("expected anomaly file written by post-commit hook")
	}
}
