package render

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/victor-develop/advanced-tasker/internal/state"
	"github.com/victor-develop/advanced-tasker/internal/store"
)

// fixedNow is the timestamp used for golden-file tests so the header
// line is deterministic.
var fixedNow = time.Date(2026, 5, 19, 10, 23, 0, 0, time.UTC)

func TestDashboard_GoldenEmpty(t *testing.T) {
	root := t.TempDir() + "/state"
	if err := state.Init(root); err != nil {
		t.Fatal(err)
	}
	got, err := Dashboard(root, DashboardOptions{Budget: 8000, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	// Section headers and counts under empty state.
	wants := []string{
		"=== HARNESS DASHBOARD — 2026-05-19T10:23Z ===",
		"Token budget: 8000",
		"PINNED",
		"TASKS (0 active)",
		"THREADS (0 tracked)",
		"PENDING REVIEW (0)",
		"AVAILABLE COMMANDS",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in dashboard\n--- got ---\n%s", w, got)
		}
	}
}

func TestDashboard_GoldenWithTasks(t *testing.T) {
	root := t.TempDir() + "/state"
	if err := state.Init(root); err != nil {
		t.Fatal(err)
	}
	// Two top-level goals, one child, one killed.
	now := fixedNow
	for _, st := range []store.Status{
		{ID: "T-1", State: store.StateInProgress, Priority: store.PriorityNormal, CreatedAt: now, UpdatedAt: now},
		{ID: "T-2", State: store.StateReady, Priority: store.PriorityNormal, ParentGoal: "T-1", CreatedAt: now, UpdatedAt: now},
		{ID: "T-3", State: store.StateKilled, Priority: store.PriorityLow, CreatedAt: now, UpdatedAt: now},
	} {
		body := "# " + st.ID + "\n\nGoal body for " + st.ID + "\n"
		if err := store.CreateTask(root, st, body); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Dashboard(root, DashboardOptions{Budget: 8000, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	// 2 active (T-1, T-2); T-3 is killed.
	if !strings.Contains(got, "TASKS (2 active)") {
		t.Errorf("expected 2 active, got:\n%s", got)
	}
	if !strings.Contains(got, "T-1") || !strings.Contains(got, "T-2") {
		t.Errorf("expected T-1 and T-2 in output, got:\n%s", got)
	}
	if strings.Contains(got, "T-3") {
		t.Errorf("killed task T-3 should not appear, got:\n%s", got)
	}
}

func TestBrief_BasicShape(t *testing.T) {
	root := t.TempDir() + "/state"
	if err := state.Init(root); err != nil {
		t.Fatal(err)
	}
	got, err := Brief(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"HARNESS BRIEF", "tasks: 0", "threads tracked: 0", "inbox/new: 0"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in brief\n%s", w, got)
		}
	}
}

func TestPickup_ListsRoles(t *testing.T) {
	root := t.TempDir() + "/state"
	if err := state.Init(root); err != nil {
		t.Fatal(err)
	}
	got, err := Pickup(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"pr-reviewer.md", "planner.md", "researcher.md", "summarizer.md", "auditor.md", "No recommendation"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in pickup\n%s", w, got)
		}
	}
}

func TestDashboard_DeltaCountsInbox(t *testing.T) {
	root := t.TempDir() + "/state"
	if err := state.Init(root); err != nil {
		t.Fatal(err)
	}
	// Write some inbox/new items.
	for i, name := range []string{"a.json", "b.json"} {
		_ = i
		if err := writeFakeInbox(root, "new", name); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Dashboard(root, DashboardOptions{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "inbox/new: 2 item(s)") {
		t.Errorf("expected inbox/new count in delta, got:\n%s", got)
	}
}

func writeFakeInbox(root, bucket, name string) error {
	path := root + "/inbox/" + bucket + "/" + name
	return os.WriteFile(path, []byte("{}\n"), 0o644)
}

// TestDashboard_CommandsCheatSheet verifies the AVAILABLE COMMANDS block
// matches design/04 verbatim and that the round-1 MVP placeholder line
// is no longer rendered (round-3 D1).
// TestDashboard_BiasTowardActionPreamble guards the polish-round-1 fix
// for the round-2 real-data finding that the commander LLM treated real
// inbox items as fictitious and chose --idle. The header must contain
// three explicit reality / non-idle / bias-toward-action directives plus
// concrete Phase 3 / Phase 4 obligations.
func TestDashboard_BiasTowardActionPreamble(t *testing.T) {
	root := t.TempDir() + "/state"
	if err := state.Init(root); err != nil {
		t.Fatal(err)
	}
	got, err := Dashboard(root, DashboardOptions{Budget: 8000, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	wants := []string{
		"REALITY: The signals you see below in inbox/, threads/, and tasks/ are REAL.",
		"hypothetical or as test fixtures. Act on them.",
		"IDLE IS THE EXCEPTION:",
		"If any inbox item is unhandled, you MUST triage",
		"WHEN UNSURE, ACT:",
		"Prefer dispatching a worker (cheap) over deferring",
		"Phase 3 (Reconcile) is concrete: EVERY inbox item below must be passed",
		"Phase 4 (Act) is concrete: for EVERY task in `ready` state",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("dashboard missing preamble line %q\n--- got ---\n%s", w, got)
		}
	}
}

func TestDashboard_CommandsCheatSheet(t *testing.T) {
	root := t.TempDir() + "/state"
	if err := state.Init(root); err != nil {
		t.Fatal(err)
	}
	got, err := Dashboard(root, DashboardOptions{Budget: 8000, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	// The obsolete round-1 disclaimer must be gone.
	if strings.Contains(got, "MVP: dispatch/review/outbox/pin/triage not yet implemented") {
		t.Errorf("obsolete MVP disclaimer still present\n%s", got)
	}
	// Every line from design/04 §AVAILABLE COMMANDS must be present
	// verbatim.
	wants := []string{
		"──── AVAILABLE COMMANDS ────────────────────────────────────",
		"harness triage <id> --action=...",
		"harness dispatch T-<n> --role=...",
		"harness review J-<id> --action=...",
		"harness outbox send --to=... --thread=... --body=... --risk=...",
		"harness task update|kill|defer|merge|split|link|unlink",
		"harness pin renew P-<id>",
		"harness rollup flush <thread-id>",
		`harness tick-log append "..."`,
		`harness tick end --summary "..."`,
		"(use --help on any verb for full args)",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("missing verbatim line %q\n--- got ---\n%s", w, got)
		}
	}
}
