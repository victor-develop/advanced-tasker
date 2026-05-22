package threads

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/victor-develop/advanced-tasker/internal/rollup"
)

// TestMetaRoundtrip writes and re-reads meta.json.
func TestMetaRoundtrip(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	m := &Meta{
		ID:            "slack-C0001-12345",
		Source:        "slack",
		URL:           "https://example.com/p1",
		CreatedAt:     now,
		LastEventAt:   now,
		OwnerTask:     "T-7",
		Participants:  []string{"alice", "bob"},
		TrackingSince: now,
	}
	if err := WriteMeta(root, m); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadMeta(root, m.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.ID != m.ID || got.OwnerTask != m.OwnerTask || got.Source != m.Source {
		t.Errorf("meta differs after roundtrip: %+v", got)
	}
	if len(got.Participants) != 2 {
		t.Errorf("participants lost: %v", got.Participants)
	}
}

// TestDirtyMarkerLifecycle.
func TestDirtyMarkerLifecycle(t *testing.T) {
	root := t.TempDir()
	id := "slack-C0002-1"
	if IsDirty(root, id) {
		t.Errorf("expected not dirty before MarkDirty")
	}
	if err := MarkDirty(root, id); err != nil {
		t.Fatal(err)
	}
	if !IsDirty(root, id) {
		t.Errorf("expected dirty after MarkDirty")
	}
	if err := ClearDirty(root, id); err != nil {
		t.Fatal(err)
	}
	if IsDirty(root, id) {
		t.Errorf("expected clean after ClearDirty")
	}
}

// TestRollupFrontmatterParser verifies the shared rollup parser sees
// frontmatter correctly. This catches drift between the threads
// package's on-disk format and the rollup validator.
func TestRollupFrontmatterParser(t *testing.T) {
	root := t.TempDir()
	id := "github-acme-api-pr-99"
	body := `---
id: github-acme-api-pr-99
source: github
state: in-progress
---

## Goal
Test.

## Current ask
- one

## Open questions
- [ ] q

## Decisions ledger

## Verbatim pins
`
	if err := WriteRollup(root, id, body); err != nil {
		t.Fatalf("write rollup: %v", err)
	}
	got, err := ReadRollup(root, id)
	if err != nil {
		t.Fatalf("read rollup: %v", err)
	}
	r, err := rollup.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Front.ID != id {
		t.Errorf("frontmatter.id=%q want %q", r.Front.ID, id)
	}
	if r.Front.State != "in-progress" {
		t.Errorf("frontmatter.state=%q want in-progress", r.Front.State)
	}
}

// TestList_OrdersThreadIDs ensures the harness can iterate stable order.
func TestList_OrdersThreadIDs(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "threads", "c"), 0o755)
	os.MkdirAll(filepath.Join(root, "threads", "a"), 0o755)
	os.MkdirAll(filepath.Join(root, "threads", "b"), 0o755)
	got, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("List not sorted: %v", got)
	}
}
