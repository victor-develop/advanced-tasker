package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/victor-develop/advanced-tasker/internal/daemon"
	"github.com/victor-develop/advanced-tasker/internal/llm"
)

// newSenderForTest builds an outbox sender wired to a fake LLM driver
// and pointed at the given state root. ProvideProviderSend is left
// nil — the test exercising sender_enabled=false never reaches the
// provider call, and the gating happens before that branch.
func newSenderForTest(t *testing.T, root string) *daemon.OutboxSender {
	t.Helper()
	bus := daemon.NewBus(root, llm.NewFake(t.TempDir()))
	return daemon.NewOutboxSender(bus)
}

// TestDispatchRoundtrip exercises the full dispatch → render →
// submit-report → review pipeline.
func TestDispatchRoundtrip(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "Ship feature")

	// Dispatch a job.
	out, _, code := runCLI(t, root, "dispatch", "T-1", "--role", "pr-reviewer")
	if code != ExitOK {
		t.Fatalf("dispatch failed: %d", code)
	}
	jobID := strings.TrimSpace(out)
	if !strings.HasPrefix(jobID, "J-") {
		t.Fatalf("expected J-..., got %q", out)
	}

	// Render worker input.
	rendered, _, code := runCLI(t, root, "render", "worker-input", jobID)
	if code != ExitOK {
		t.Fatalf("render worker-input failed: %d", code)
	}
	for _, want := range []string{"pr-reviewer", jobID, "REQUIRED OUTCOME"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("worker-input missing %q\n%s", want, rendered)
		}
	}

	// Worker writes a report.
	report := `job_id: ` + jobID + `
outcome: approve
confidence: med
tldr: looks good
next:
  - action: task.update
    args:
      id: T-1
      state: in-progress
artifacts: []
`
	rf := filepath.Join(t.TempDir(), "report.yaml")
	if err := os.WriteFile(rf, []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	// Submit fails until the job is claimed by some worker / autopilot,
	// but submit-report is the same verb anyway — design supports
	// claim → submit. For ergonomics we accept submit-report from
	// pending too. Confirm.
	if _, _, code := runCLI(t, root, "submit-report", jobID, "--file", rf); code != ExitOK {
		t.Fatalf("submit-report failed: %d", code)
	}
	// Job should now be in done/.
	if _, err := os.Stat(filepath.Join(root, "jobs", "done", jobID+".yaml")); err != nil {
		t.Errorf("job not in done/: %v", err)
	}
	// Review and accept.
	if _, _, code := runCLI(t, root, "review", jobID, "--action", "accept"); code != ExitOK {
		t.Fatalf("review failed: %d", code)
	}
	// T-1 should be in-progress now.
	showOut, _, _ := runCLI(t, root, "task", "show", "T-1")
	if !strings.Contains(showOut, "in-progress") {
		t.Errorf("review did not execute task.update: %s", showOut)
	}
}

// TestSubmitReportTLDRTooLong rejects with exit 2.
func TestSubmitReportTLDRTooLong(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "G")
	out, _, _ := runCLI(t, root, "dispatch", "T-1", "--role", "researcher")
	jobID := strings.TrimSpace(out)
	report := "job_id: " + jobID + "\noutcome: found\nconfidence: med\ntldr: " + strings.Repeat("a", 250) + "\n"
	rf := filepath.Join(t.TempDir(), "r.yaml")
	os.WriteFile(rf, []byte(report), 0o644)
	if _, _, code := runCLI(t, root, "submit-report", jobID, "--file", rf); code != ExitValidation {
		t.Errorf("expected ExitValidation for long tldr, got %d", code)
	}
}

// TestOutboxRiskDowngradeRejected — the commander review tries to
// lower the worker's stated risk; must exit 2.
func TestOutboxRiskDowngradeRejected(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "G")
	out, _, _ := runCLI(t, root, "dispatch", "T-1", "--role", "slack-drafter")
	jobID := strings.TrimSpace(out)
	report := `job_id: ` + jobID + `
outcome: draft
confidence: med
tldr: please send
next:
  - action: outbox.send
    risk: high
    args:
      thread: slack-C0001-1111
      body_file: msg.md
`
	rf := filepath.Join(t.TempDir(), "r.yaml")
	os.WriteFile(rf, []byte(report), 0o644)
	if _, _, code := runCLI(t, root, "submit-report", jobID, "--file", rf); code != ExitOK {
		t.Fatalf("submit failed: %d", code)
	}
	// Now try to downgrade via review --risk=low → must reject.
	_, _, code := runCLI(t, root, "review", jobID, "--action", "accept", "--risk", "low")
	if code != ExitValidation {
		t.Errorf("expected ExitValidation when downgrading risk, got %d", code)
	}
}

// TestRollupUpdateAppendOnly: try to remove a ledger line — exit 2 +
// anomaly file.
func TestRollupUpdateAppendOnly(t *testing.T) {
	root := initState(t)
	// Track a thread first.
	if _, _, code := runCLI(t, root, "thread", "track", "slack-C1-100"); code != ExitOK {
		t.Fatalf("track failed: %d", code)
	}
	initial := `---
id: slack-C1-100
source: slack
state: in-progress
---
## Goal
Test.
## Current ask
- one
## Open questions
- [ ] q1
## Decisions ledger
- 2026-01-01: A
- 2026-01-02: B
## Verbatim pins
> "stay" — alice (— pinned by human)
`
	f1 := filepath.Join(t.TempDir(), "v1.md")
	os.WriteFile(f1, []byte(initial), 0o644)
	if _, _, code := runCLI(t, root, "rollup", "update", "slack-C1-100", "--file", f1); code != ExitOK {
		t.Fatalf("first update failed: %d", code)
	}
	// Now propose a rollup that REMOVES a ledger line.
	bad := `---
id: slack-C1-100
source: slack
state: in-progress
---
## Goal
Test.
## Current ask
- one
## Open questions
- [ ] q1
## Decisions ledger
- 2026-01-01: A
## Verbatim pins
> "stay" — alice (— pinned by human)
`
	f2 := filepath.Join(t.TempDir(), "v2.md")
	os.WriteFile(f2, []byte(bad), 0o644)
	_, _, code := runCLI(t, root, "rollup", "update", "slack-C1-100", "--file", f2)
	if code != ExitValidation {
		t.Errorf("expected ExitValidation for ledger removal, got %d", code)
	}
	// An anomaly file must exist.
	anomalies, _ := os.ReadDir(filepath.Join(root, "inbox", "anomalies"))
	if len(anomalies) == 0 {
		t.Errorf("expected anomaly file written")
	}
}

// TestRollupHumanPinRemovalRejected — try to drop a human pin.
func TestRollupHumanPinRemovalRejected(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "thread", "track", "slack-C1-200")
	initial := `---
id: slack-C1-200
source: slack
state: new
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
> "must keep" — bob (— pinned by human)
`
	f1 := filepath.Join(t.TempDir(), "v1.md")
	os.WriteFile(f1, []byte(initial), 0o644)
	runCLI(t, root, "rollup", "update", "slack-C1-200", "--file", f1)
	// Try a version without the human pin.
	bad := strings.Replace(initial, `> "must keep" — bob (— pinned by human)`+"\n", "", 1)
	f2 := filepath.Join(t.TempDir(), "v2.md")
	os.WriteFile(f2, []byte(bad), 0o644)
	_, _, code := runCLI(t, root, "rollup", "update", "slack-C1-200", "--file", f2)
	if code != ExitValidation {
		t.Errorf("expected ExitValidation for human-pin removal, got %d", code)
	}
}

// TestConfigInitIdempotent — running `harness config init slack` twice
// does not overwrite.
func TestConfigInitIdempotent(t *testing.T) {
	root := initState(t)
	if _, _, code := runCLI(t, root, "config", "init", "slack"); code != ExitOK {
		t.Fatalf("config init slack failed: %d", code)
	}
	cfgPath := filepath.Join(root, "sources", "slack", "config.yaml")
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("config not seeded: %v", err)
	}
	want := string(body)
	// Run again — must not overwrite.
	if _, _, code := runCLI(t, root, "config", "init", "slack"); code != ExitOK {
		t.Errorf("second config init should be idempotent ok, got %d", code)
	}
	body2, _ := os.ReadFile(cfgPath)
	if string(body2) != want {
		t.Errorf("config was overwritten on second init")
	}
}

// TestHarnessInitDoesNotSeedSourceConfig — design says init must NOT
// seed sources/<src>/config.yaml.
func TestHarnessInitDoesNotSeedSourceConfig(t *testing.T) {
	root := initState(t)
	for _, src := range []string{"slack", "github"} {
		p := filepath.Join(root, "sources", src, "config.yaml")
		if _, err := os.Stat(p); err == nil {
			t.Errorf("init should NOT seed %s — design/10", p)
		}
	}
}

// TestWatchChannelMutatesSourceConfig — sanity check the mutator.
func TestWatchChannelMutatesSourceConfig(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "config", "init", "slack")
	if _, _, code := runCLI(t, root, "watch", "slack-channel", "C-test"); code != ExitOK {
		t.Fatalf("watch failed: %d", code)
	}
	body, _ := os.ReadFile(filepath.Join(root, "sources", "slack", "config.yaml"))
	if !strings.Contains(string(body), "C-test") {
		t.Errorf("config.yaml does not contain new channel:\n%s", body)
	}
}

// TestOutboxSendLowRiskQueuesToPending — risk=low → outbox/pending.
func TestOutboxSendLowRiskQueuesToPending(t *testing.T) {
	root := initState(t)
	bodyFile := filepath.Join(t.TempDir(), "msg.md")
	os.WriteFile(bodyFile, []byte("hi"), 0o644)
	out, _, code := runCLI(t, root, "outbox", "send",
		"--to", "slack", "--thread", "slack-C1-1", "--risk", "low",
		"--body", bodyFile)
	if code != ExitOK {
		t.Fatalf("outbox send failed: %d", code)
	}
	id := strings.TrimSpace(out)
	if _, err := os.Stat(filepath.Join(root, "outbox", "pending", id+".yaml")); err != nil {
		t.Errorf("low-risk should land in pending/, got %v", err)
	}
}

// TestOutboxSendNormalQueuesToAwaitingHuman.
func TestOutboxSendNormalQueuesToAwaitingHuman(t *testing.T) {
	root := initState(t)
	bodyFile := filepath.Join(t.TempDir(), "msg.md")
	os.WriteFile(bodyFile, []byte("hi"), 0o644)
	out, _, code := runCLI(t, root, "outbox", "send",
		"--to", "slack", "--thread", "slack-C1-1", "--risk", "normal",
		"--body", bodyFile)
	if code != ExitOK {
		t.Fatalf("outbox send failed: %d", code)
	}
	id := strings.TrimSpace(out)
	if _, err := os.Stat(filepath.Join(root, "outbox", "awaiting-human", id+".yaml")); err != nil {
		t.Errorf("normal-risk should land in awaiting-human/, got %v", err)
	}
}

// TestOutboxRevokeFresh — revoke within window deletes the pending file.
func TestOutboxRevokeFreshPending(t *testing.T) {
	root := initState(t)
	bodyFile := filepath.Join(t.TempDir(), "msg.md")
	os.WriteFile(bodyFile, []byte("hi"), 0o644)
	out, _, _ := runCLI(t, root, "outbox", "send",
		"--to", "slack", "--thread", "slack-C1-1", "--risk", "low",
		"--body", bodyFile)
	id := strings.TrimSpace(out)
	if _, _, code := runCLI(t, root, "outbox", "revoke", id); code != ExitOK {
		t.Errorf("revoke failed: %d", code)
	}
	if _, err := os.Stat(filepath.Join(root, "outbox", "pending", id+".yaml")); err == nil {
		t.Errorf("expected file deleted after revoke")
	}
}

// TestOutboxFlushDryRun — `harness outbox flush --dry-run` prints intent
// without sending.
func TestOutboxFlushDryRun(t *testing.T) {
	root := initState(t)
	bodyFile := filepath.Join(t.TempDir(), "msg.md")
	os.WriteFile(bodyFile, []byte("hi"), 0o644)
	runCLI(t, root, "outbox", "send", "--to", "slack",
		"--thread", "slack-C1-1", "--risk", "low", "--body", bodyFile)
	out, _, code := runCLI(t, root, "outbox", "flush", "--dry-run")
	if code != ExitOK {
		t.Fatalf("flush --dry-run failed: %d", code)
	}
	if !strings.Contains(out, "[dry-run]") {
		t.Errorf("expected dry-run marker, got %s", out)
	}
}

// TestTriageActionTask creates a task from inbox.
func TestTriageActionTask(t *testing.T) {
	root := initState(t)
	// Create an inbox/new item by hand.
	inboxDir := filepath.Join(root, "inbox", "new")
	os.MkdirAll(inboxDir, 0o755)
	os.WriteFile(filepath.Join(inboxDir, "slack-X-1.json"), []byte(`{"id":"slack-X-1","summary":"investigate"}`), 0o644)
	if _, _, code := runCLI(t, root, "triage", "slack-X-1", "--action", "task", "--title", "look into thing"); code != ExitOK {
		t.Fatalf("triage failed: %d", code)
	}
	if _, err := os.Stat(filepath.Join(root, "tasks", "T-1", "status.json")); err != nil {
		t.Errorf("expected T-1 to be created: %v", err)
	}
}

// TestTriageActionDrop deletes the inbox file.
func TestTriageActionDrop(t *testing.T) {
	root := initState(t)
	inboxDir := filepath.Join(root, "inbox", "new")
	os.MkdirAll(inboxDir, 0o755)
	path := filepath.Join(inboxDir, "x.json")
	os.WriteFile(path, []byte(`{"id":"x"}`), 0o644)
	if _, _, code := runCLI(t, root, "triage", "x", "--action", "drop"); code != ExitOK {
		t.Fatalf("triage drop failed: %d", code)
	}
	if _, err := os.Stat(path); err == nil {
		t.Errorf("inbox file should be removed")
	}
}

// TestDoctorReportsClean — `harness doctor` exits 0 on a fresh state.
func TestDoctorReportsClean(t *testing.T) {
	root := initState(t)
	out, _, code := runCLI(t, root, "doctor")
	if code != ExitOK {
		t.Fatalf("doctor failed: %d", code)
	}
	for _, want := range []string{"state:", "post-commit hook: installed", "doctor: OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor missing %q\n%s", want, out)
		}
	}
}

// TestClaimAndReleaseJob — a worker daemon's claim/release cycle.
func TestClaimAndReleaseJob(t *testing.T) {
	root := initState(t)
	runCLI(t, root, "goal", "create", "G")
	out, _, _ := runCLI(t, root, "dispatch", "T-1", "--role", "researcher")
	jobID := strings.TrimSpace(out)
	if _, _, code := runCLI(t, root, "claim", "job", jobID, "--as", "tester"); code != ExitOK {
		t.Fatalf("claim job failed: %d", code)
	}
	if _, err := os.Stat(filepath.Join(root, "jobs", "in-flight", jobID+".yaml")); err != nil {
		t.Errorf("job not moved to in-flight/: %v", err)
	}
	if _, _, code := runCLI(t, root, "release", "job", jobID); code != ExitOK {
		t.Fatalf("release job failed: %d", code)
	}
	if _, err := os.Stat(filepath.Join(root, "jobs", "pending", jobID+".yaml")); err != nil {
		t.Errorf("job not restored to pending/: %v", err)
	}
}

// TestTickStartAndEnd — claim lock and finalize.
func TestTickStartAndEnd(t *testing.T) {
	root := initState(t)
	if _, _, code := runCLI(t, root, "tick", "start", "--as", "tester"); code != ExitOK {
		t.Fatalf("tick start failed: %d", code)
	}
	if _, _, code := runCLI(t, root, "tick-log", "append", "saw nothing"); code != ExitOK {
		t.Fatalf("tick-log append failed: %d", code)
	}
	if _, _, code := runCLI(t, root, "tick", "end", "--idle"); code != ExitOK {
		t.Fatalf("tick end failed: %d", code)
	}
	// engine.json must show consecutive_idle=1
	out, _, _ := runCLI(t, root, "engine", "status")
	if !strings.Contains(out, "consecutive_idle=1") {
		t.Errorf("engine status missing consecutive_idle=1: %s", out)
	}
}

// TestPostCommitHookExists.
func TestPostCommitHookExists(t *testing.T) {
	root := initState(t)
	info, err := os.Stat(filepath.Join(root, ".git", "hooks", "post-commit"))
	if err != nil {
		t.Fatalf("hook missing: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("post-commit hook not executable")
	}
}

// TestAuditChecklistSeeded.
func TestAuditChecklistSeeded(t *testing.T) {
	root := initState(t)
	if _, err := os.Stat(filepath.Join(root, "audit", "checklist.yaml")); err != nil {
		t.Errorf("audit/checklist.yaml not seeded: %v", err)
	}
}

// TestAuditRunProducesReport runs audit with the fake driver.
func TestAuditRunProducesReport(t *testing.T) {
	root := initState(t)
	if _, _, code := runCLI(t, root, "audit", "run"); code != ExitOK {
		t.Fatalf("audit run failed: %d", code)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "audit", "reports"))
	if len(entries) == 0 {
		t.Errorf("audit report not written")
	}
}

// TestOutboxSenderEnabledFalseDefersToAwaitingHuman: when config has
// outbox.sender_enabled=false, the sender daemon moves the pending
// item to awaiting-human/ without invoking any provider call. Round-3
// D5 acceptance.
func TestOutboxSenderEnabledFalseDefersToAwaitingHuman(t *testing.T) {
	root := initState(t)
	// Force the explicit-false case (fresh `harness init` already
	// seeds false in the round-3 default, but be defensive — older
	// state directories the test framework reuses might differ).
	if _, _, code := runCLI(t, root, "config", "set", "outbox.sender_enabled", "false"); code != ExitOK {
		t.Fatalf("set sender_enabled=false: %d", code)
	}
	// Drop a low-risk pending item directly (avoid go-imports
	// dependency on the outbox/risk paths in this test).
	bodyFile := filepath.Join(t.TempDir(), "msg.md")
	os.WriteFile(bodyFile, []byte("hi"), 0o644)
	out, _, code := runCLI(t, root, "outbox", "send",
		"--to", "slack", "--thread", "slack-C1-1", "--risk", "low",
		"--body", bodyFile)
	if code != ExitOK {
		t.Fatalf("outbox send failed: %d", code)
	}
	id := strings.TrimSpace(out)
	pendingPath := filepath.Join(root, "outbox", "pending", id+".yaml")
	awaitPath := filepath.Join(root, "outbox", "awaiting-human", id+".yaml")
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("expected item in pending/ before daemon runs: %v", err)
	}

	// Run the sender daemon body inline. We invoke ProcessOnce so we
	// don't have to manage goroutine lifecycle in the test.
	if err := newSenderForTest(t, root).ProcessOnce(id); err != nil {
		t.Fatalf("sender.ProcessOnce: %v", err)
	}

	if _, err := os.Stat(pendingPath); err == nil {
		t.Errorf("expected item moved out of pending/ after deferral")
	}
	if _, err := os.Stat(awaitPath); err != nil {
		t.Errorf("expected item in awaiting-human/, got %v", err)
	}
}

// TestStateDirFlagRename — the global flag is --state-dir, not --state.
func TestStateDirFlagRename(t *testing.T) {
	root := initState(t)
	// --state-dir works:
	cmd := New()
	cmd.SetArgs([]string{"--state-dir", root, "task", "ls"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("--state-dir should be a global flag: %v", err)
	}
	// `--state ready` is still legal as a local arg of task update:
	runCLI(t, root, "goal", "create", "G")
	if _, _, code := runCLI(t, root, "task", "update", "T-1", "--state", "in-progress"); code != ExitOK {
		t.Errorf("task update --state should still work locally, got %d", code)
	}
}
