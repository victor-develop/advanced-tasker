package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI invokes the cobra command tree with args and returns stdout,
// stderr, and exit-code-equivalent error. State root is forced to
// stateRoot via the HARNESS_STATE env var (not --state, because
// `harness task update --state ...` legitimately wants its own local
// --state flag per design/03).
func runCLI(t *testing.T, stateRoot string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	t.Setenv("HARNESS_STATE", stateRoot)
	cmd := New()
	var sout, serr bytes.Buffer
	cmd.SetOut(&sout)
	cmd.SetErr(&serr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err == nil {
		return sout.String(), serr.String(), ExitOK
	}
	if ce, ok := err.(cliError); ok {
		return sout.String(), serr.String() + ce.msg + "\n", ce.code
	}
	return sout.String(), serr.String() + err.Error() + "\n", ExitUsage
}

// initState invokes `harness init` for a fresh tempdir and returns the
// state path.
func initState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "state")
	if _, _, code := runCLI(t, root, "init"); code != ExitOK {
		t.Fatalf("init failed: code=%d", code)
	}
	return root
}

func TestInit_FreshDir(t *testing.T) {
	root := initState(t)
	if _, err := os.Stat(filepath.Join(root, "config.yaml")); err != nil {
		t.Errorf("missing config.yaml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Errorf("missing .git: %v", err)
	}
	// Must also have the inbox/new dir (poller contract).
	if _, err := os.Stat(filepath.Join(root, "inbox", "new")); err != nil {
		t.Errorf("missing inbox/new: %v", err)
	}
	// And the threads/ dir.
	if _, err := os.Stat(filepath.Join(root, "threads")); err != nil {
		t.Errorf("missing threads/: %v", err)
	}
}

func TestInit_RejectsExisting(t *testing.T) {
	root := initState(t)
	if _, _, code := runCLI(t, root, "init"); code == ExitOK {
		t.Errorf("expected non-zero exit on re-init")
	}
}

func TestConfigGetSet(t *testing.T) {
	root := initState(t)
	out, _, code := runCLI(t, root, "config", "get", "models.commander")
	if code != ExitOK {
		t.Fatalf("config get failed: %d", code)
	}
	if !strings.Contains(out, "claude-opus") {
		t.Errorf("expected default model in output, got %q", out)
	}
	if _, _, code := runCLI(t, root, "config", "set", "models.commander", "claude-sonnet"); code != ExitOK {
		t.Fatalf("config set failed: %d", code)
	}
	out2, _, _ := runCLI(t, root, "config", "get", "models.commander")
	if !strings.Contains(out2, "claude-sonnet") {
		t.Errorf("expected updated value, got %q", out2)
	}
	// Set a brand-new dotted path.
	if _, _, code := runCLI(t, root, "config", "set", "schedule.active_window.interval", "3m"); code != ExitOK {
		t.Fatalf("nested config set failed: %d", code)
	}
	out3, _, _ := runCLI(t, root, "config", "get", "schedule.active_window.interval")
	if !strings.Contains(out3, "3m") {
		t.Errorf("expected 3m, got %q", out3)
	}
}

func TestGoalCreate_ReturnsID(t *testing.T) {
	root := initState(t)
	out, _, code := runCLI(t, root, "goal", "create", "Improve ingest")
	if code != ExitOK {
		t.Fatalf("goal create failed: %d", code)
	}
	id := strings.TrimSpace(out)
	if id != "T-1" {
		t.Errorf("expected T-1, got %q", id)
	}
	if _, err := os.Stat(filepath.Join(root, "tasks", "T-1", "goal.md")); err != nil {
		t.Errorf("goal.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tasks", "T-1", "status.json")); err != nil {
		t.Errorf("status.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tasks", "T-1", "log.md")); err != nil {
		t.Errorf("log.md missing: %v", err)
	}
}

func TestTaskCreate_WithParent(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "Root")
	out, _, code := runCLI(t, root, "task", "create", "Child", "--parent", "T-1")
	if code != ExitOK {
		t.Fatalf("task create failed: %d", code)
	}
	id := strings.TrimSpace(out)
	if id != "T-2" {
		t.Errorf("expected T-2, got %q", id)
	}
}

func TestTaskUpdate(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "G")
	if _, _, code := runCLI(t, root, "task", "update", "T-1", "--state", "in-progress"); code != ExitOK {
		t.Fatalf("task update failed: %d", code)
	}
	out, _, _ := runCLI(t, root, "task", "show", "T-1")
	if !strings.Contains(out, "in-progress") {
		t.Errorf("show output missing new state: %q", out)
	}
}

func TestTaskUpdate_InvalidState(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "G")
	if _, _, code := runCLI(t, root, "task", "update", "T-1", "--state", "bogus"); code != ExitValidation {
		t.Errorf("expected validation error for bogus state, got %d", code)
	}
}

func TestTaskKill(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "G")
	if _, _, code := runCLI(t, root, "task", "kill", "T-1", "--reason", "scope cut"); code != ExitOK {
		t.Fatalf("kill failed: %d", code)
	}
	out, _, _ := runCLI(t, root, "task", "show", "T-1")
	if !strings.Contains(out, "killed") {
		t.Errorf("expected killed state, got %q", out)
	}
}

func TestTaskDefer(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "G")
	if _, _, code := runCLI(t, root, "task", "defer", "T-1", "--reason", "no capacity"); code != ExitOK {
		t.Fatalf("defer failed: %d", code)
	}
	out, _, _ := runCLI(t, root, "task", "show", "T-1")
	if !strings.Contains(out, "deferred") {
		t.Errorf("expected deferred state, got %q", out)
	}
}

func TestTaskSplit(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "Parent")
	out, _, code := runCLI(t, root, "task", "split", "T-1", "Sub A", "Sub B")
	if code != ExitOK {
		t.Fatalf("split failed: %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] != "T-2" || lines[1] != "T-3" {
		t.Errorf("unexpected split output: %q", out)
	}
}

func TestTaskMerge(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "Keep")
	runCLI(t, root, "goal", "create", "Absorb")
	runCLI(t, root, "task", "create", "Blocked", "--parent", "T-1")
	runCLI(t, root, "link", "T-3", "blocked-on", "T-2")
	if _, _, code := runCLI(t, root, "task", "merge", "T-1", "T-2"); code != ExitOK {
		t.Fatalf("merge failed: %d", code)
	}
	out, _, _ := runCLI(t, root, "task", "show", "T-2")
	if !strings.Contains(out, "killed") {
		t.Errorf("absorbed task should be killed: %q", out)
	}
	out3, _, _ := runCLI(t, root, "task", "show", "T-3")
	if !strings.Contains(out3, "blocked_on: T-1") {
		t.Errorf("T-3's blocker should be rewritten to T-1: %q", out3)
	}
}

func TestLinkCycleRejected(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "A")
	runCLI(t, root, "goal", "create", "B")
	runCLI(t, root, "goal", "create", "C")
	if _, _, code := runCLI(t, root, "link", "T-1", "blocked-on", "T-2"); code != ExitOK {
		t.Fatalf("first link failed: %d", code)
	}
	if _, _, code := runCLI(t, root, "link", "T-2", "blocked-on", "T-3"); code != ExitOK {
		t.Fatalf("second link failed: %d", code)
	}
	// T-3 → T-1 would close a cycle.
	_, stderr, code := runCLI(t, root, "link", "T-3", "blocked-on", "T-1")
	if code != ExitValidation {
		t.Errorf("expected ExitValidation for cycle, got %d (stderr=%s)", code, stderr)
	}
	if !strings.Contains(stderr, "cycle") {
		t.Errorf("stderr should mention cycle: %q", stderr)
	}
}

func TestLinkSelfRejected(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "G")
	_, _, code := runCLI(t, root, "link", "T-1", "blocked-on", "T-1")
	if code != ExitValidation {
		t.Errorf("expected ExitValidation for self-link, got %d", code)
	}
}

func TestUnlink_StateTransition(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "A")
	runCLI(t, root, "goal", "create", "B")
	runCLI(t, root, "link", "T-1", "blocked-on", "T-2")
	out, _, _ := runCLI(t, root, "task", "show", "T-1")
	if !strings.Contains(out, "blocked") {
		t.Errorf("T-1 should be blocked, got %q", out)
	}
	runCLI(t, root, "unlink", "T-1", "T-2")
	out2, _, _ := runCLI(t, root, "task", "show", "T-1")
	if !strings.Contains(out2, "ready") {
		t.Errorf("T-1 should be ready after unlink, got %q", out2)
	}
}

func TestUnlink_MissingEdge(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "A")
	runCLI(t, root, "goal", "create", "B")
	if _, _, code := runCLI(t, root, "unlink", "T-1", "T-2"); code != ExitValidation {
		t.Errorf("expected validation error unlinking nonexistent edge, got %d", code)
	}
}

func TestDepsShow(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "A")
	runCLI(t, root, "goal", "create", "B")
	runCLI(t, root, "goal", "create", "C")
	runCLI(t, root, "link", "T-1", "blocked-on", "T-2")
	runCLI(t, root, "link", "T-2", "blocked-on", "T-3")
	out, _, code := runCLI(t, root, "deps", "show", "T-1")
	if code != ExitOK {
		t.Fatalf("deps show failed: %d", code)
	}
	if !strings.Contains(out, "T-2") || !strings.Contains(out, "T-3") {
		t.Errorf("expected upstream chain, got %q", out)
	}
}

func TestDepsCycles_None(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "A")
	runCLI(t, root, "goal", "create", "B")
	runCLI(t, root, "link", "T-1", "blocked-on", "T-2")
	out, _, code := runCLI(t, root, "deps", "cycles")
	if code != ExitOK {
		t.Fatalf("deps cycles failed: %d", code)
	}
	if !strings.Contains(out, "no cycles") {
		t.Errorf("expected 'no cycles', got %q", out)
	}
}

func TestTaskLs_Filter(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "A")
	runCLI(t, root, "goal", "create", "B")
	runCLI(t, root, "task", "kill", "T-2", "--reason", "x")
	out, _, _ := runCLI(t, root, "task", "ls")
	if !strings.Contains(out, "T-1") || !strings.Contains(out, "T-2") {
		t.Errorf("ls should show all tasks: %q", out)
	}
	out2, _, _ := runCLI(t, root, "task", "ls", "--state", "killed")
	if !strings.Contains(out2, "T-2") || strings.Contains(out2, "T-1") {
		t.Errorf("filtered ls wrong: %q", out2)
	}
}

func TestRestateGoal(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "Original")
	if _, _, code := runCLI(t, root, "task", "restate-goal", "T-1", "Revised goal"); code != ExitOK {
		t.Fatalf("restate-goal failed: %d", code)
	}
	out, _, _ := runCLI(t, root, "task", "show", "T-1")
	if !strings.Contains(out, "Revised goal") {
		t.Errorf("expected restated goal, got %q", out)
	}
}

func TestRenderDashboard(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "Ingest reliability")
	runCLI(t, root, "task", "create", "Retry", "--parent", "T-1")
	out, _, code := runCLI(t, root, "render", "dashboard")
	if code != ExitOK {
		t.Fatalf("render dashboard failed: %d", code)
	}
	for _, want := range []string{"HARNESS DASHBOARD", "TASKS (2 active)", "T-1", "T-2", "THREADS", "PENDING REVIEW", "AVAILABLE COMMANDS"} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard missing %q\n%s", want, out)
		}
	}
}

func TestRenderDashboard_Empty(t *testing.T) {
	root := initState(t)
	out, _, code := runCLI(t, root, "render", "dashboard")
	if code != ExitOK {
		t.Fatalf("render dashboard failed: %d", code)
	}
	if !strings.Contains(out, "TASKS (0 active)") {
		t.Errorf("expected 0 active tasks, got %q", out)
	}
}

func TestRenderBrief(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "G")
	out, _, code := runCLI(t, root, "render", "brief")
	if code != ExitOK {
		t.Fatalf("render brief failed: %d", code)
	}
	if !strings.Contains(out, "HARNESS BRIEF") || !strings.Contains(out, "tasks: 1") {
		t.Errorf("brief output unexpected: %q", out)
	}
}

func TestPickup(t *testing.T) {
	root := initState(t)
	out, _, code := runCLI(t, root, "pickup")
	if code != ExitOK {
		t.Fatalf("pickup failed: %d", code)
	}
	// Confirm role names appear (seeded by `harness init`).
	for _, want := range []string{"pr-reviewer.md", "planner.md", "researcher.md", "summarizer.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("pickup missing %q: %s", want, out)
		}
	}
	if !strings.Contains(out, "No recommendation") {
		t.Errorf("pickup must not recommend: %s", out)
	}
}

func TestRequireInitialized(t *testing.T) {
	// Running a verb without init should error out with usage exit code.
	dir := t.TempDir()
	root := filepath.Join(dir, "state")
	if _, _, code := runCLI(t, root, "task", "ls"); code == ExitOK {
		t.Errorf("expected error running verb before init")
	}
}

func TestVersion(t *testing.T) {
	cmd := New()
	var sout bytes.Buffer
	cmd.SetOut(&sout)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(sout.String()) == "" {
		t.Errorf("version output empty")
	}
}

func TestMutationsCommit(t *testing.T) {
	root := initState(t)
	// Count commits before and after a verb to confirm we're committing.
	before := commitCount(t, root)
	runCLI(t, root, "goal", "create", "G")
	after := commitCount(t, root)
	if after <= before {
		t.Errorf("expected commit count to grow, before=%d after=%d", before, after)
	}
}

func commitCount(t *testing.T, root string) int {
	t.Helper()
	out, err := runGit(root, "rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
	return n
}

// runGit invokes git inside root for inspection (test helper only).
func runGit(root string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	return string(out), err
}
