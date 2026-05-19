package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestCreateAndReadTask(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	now := time.Now().UTC()
	st := Status{
		ID:        "T-1",
		State:     StateReady,
		Priority:  PriorityNormal,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := CreateTask(root, st, "# T-1\nfoo\n"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStatus(root, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "T-1" || got.State != StateReady {
		t.Errorf("unexpected status: %+v", got)
	}
}

func TestCreateTask_InvalidState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	st := Status{
		ID:        "T-1",
		State:     "bogus",
		Priority:  PriorityNormal,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := CreateTask(root, st, "x"); err == nil {
		t.Error("expected validation error")
	}
}

func TestCreateTask_DuplicateID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	now := time.Now().UTC()
	st := Status{ID: "T-1", State: StateReady, Priority: PriorityNormal, CreatedAt: now, UpdatedAt: now}
	if err := CreateTask(root, st, "x"); err != nil {
		t.Fatal(err)
	}
	if err := CreateTask(root, st, "x"); err == nil {
		t.Error("expected duplicate-task error")
	}
}

func TestBlockedOnTransitions(t *testing.T) {
	s := &Status{ID: "T-1", State: StateReady, BlockedOn: []string{}}
	if !AddBlockedOn(s, "T-2") {
		t.Error("AddBlockedOn should report change")
	}
	if s.State != StateBlocked {
		t.Errorf("state should be blocked, got %s", s.State)
	}
	if AddBlockedOn(s, "T-2") {
		t.Error("AddBlockedOn should be idempotent")
	}
	if !RemoveBlockedOn(s, "T-2") {
		t.Error("RemoveBlockedOn should report change")
	}
	if s.State != StateReady {
		t.Errorf("state should flip back to ready, got %s", s.State)
	}
}

func TestListTasks_Sorting(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	now := time.Now().UTC()
	for _, id := range []string{"T-2", "T-10", "T-1"} {
		st := Status{ID: id, State: StateReady, Priority: PriorityNormal, CreatedAt: now, UpdatedAt: now}
		if err := CreateTask(root, st, ""); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := ListTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"T-1", "T-2", "T-10"}
	if len(ids) != len(want) {
		t.Fatalf("expected %v, got %v", want, ids)
	}
	for i := range ids {
		if ids[i] != want[i] {
			t.Errorf("position %d: want %s, got %s", i, want[i], ids[i])
		}
	}
}
