package audit

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/victor-develop/advanced-tasker/internal/store"
)

// TestComputeSignals_StuckInProgress exercises the Go-side signal
// extraction over a synthesized state directory.
//
// We create a task in state=in-progress whose UpdatedAt is older than
// the check's threshold; ComputeSignals must surface it as a "watch"
// finding for the stuck_in_progress check.
func TestComputeSignals_StuckInProgress(t *testing.T) {
	root := t.TempDir()
	// Layout the audit signals reads from.
	for _, sub := range []string{"tasks", "jobs/done", "jobs/failed", "jobs/in-flight", "jobs/pending", "inbox/anomalies"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	if err := store.CreateTask(root, store.Status{
		ID:        "T-7",
		State:     store.StateInProgress,
		Priority:  store.PriorityNormal,
		CreatedAt: old,
		UpdatedAt: old,
	}, "# T-7\n"); err != nil {
		t.Fatal(err)
	}

	cl := &Checklist{Checks: []Check{
		{
			Name:          "stuck_in_progress",
			SeverityOnHit: "watch",
			Query:         "tasks_in_progress_unchanged_for_days",
			Args:          map[string]any{"days": 7},
			Hint:          "consider splitting/killing",
		},
	}}
	got := ComputeSignals(root, cl)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Severity != "watch" {
		t.Errorf("want severity=watch, got %s", got[0].Severity)
	}
	if got[0].Detail == "" || got[0].Hint == "" {
		t.Errorf("want non-empty detail/hint, got %+v", got[0])
	}
}

// TestComputeSignals_HealthyWhenNothingStuck is the complementary
// happy-path case.
func TestComputeSignals_HealthyWhenNothingStuck(t *testing.T) {
	root := t.TempDir()
	for _, sub := range []string{"tasks", "jobs/done", "jobs/failed", "jobs/in-flight", "jobs/pending", "inbox/anomalies"} {
		os.MkdirAll(filepath.Join(root, sub), 0o755)
	}
	// A fresh in-progress task is not stuck.
	store.CreateTask(root, store.Status{
		ID:        "T-1",
		State:     store.StateInProgress,
		Priority:  store.PriorityNormal,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}, "# T-1\n")

	cl := &Checklist{Checks: []Check{{
		Name:          "stuck_in_progress",
		SeverityOnHit: "watch",
		Query:         "tasks_in_progress_unchanged_for_days",
		Args:          map[string]any{"days": 7},
	}}}
	got := ComputeSignals(root, cl)
	if got[0].Severity != "healthy" {
		t.Errorf("expected healthy, got %s — detail=%s", got[0].Severity, got[0].Detail)
	}
}
