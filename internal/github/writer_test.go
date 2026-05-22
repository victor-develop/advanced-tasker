package github

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	gh "github.com/google/go-github/v75/github"
)

func TestWriter_EventLifecycle(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir)
	r := RepoRef{Owner: "acme", Repo: "api"}
	id := ThreadID(r, 1284)

	ev := RawEvent{
		ID:         "issue-comment-100",
		Source:     "github",
		Kind:       "issue-comment",
		CapturedAt: time.Now(),
		Body:       "hello",
	}
	ev.PR.Owner = "acme"
	ev.PR.Repo = "api"
	ev.PR.Number = 1284

	// First write should land on disk.
	path, wrote, err := w.WriteRawEvent(id, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("first write should report wrote=true")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file missing: %v", err)
	}

	// Second write should be a no-op (idempotent).
	_, wrote2, err := w.WriteRawEvent(id, ev)
	if err != nil {
		t.Fatal(err)
	}
	if wrote2 {
		t.Fatal("second write should report wrote=false (dedup)")
	}

	// EventExists confirms presence.
	exists, err := w.EventExists(id, "issue-comment-100")
	if err != nil || !exists {
		t.Fatalf("EventExists: %v %v", exists, err)
	}

	// .dirty appears after TouchDirty.
	if err := w.TouchDirty(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "threads", id, ".dirty")); err != nil {
		t.Fatalf(".dirty missing: %v", err)
	}
}

func TestWriter_MetaInitAndUpdate(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir)
	r := RepoRef{Owner: "acme", Repo: "api"}
	id := ThreadID(r, 1284)

	now := time.Date(2026, 5, 19, 10, 15, 0, 0, time.UTC)
	pr := &gh.PullRequest{
		Number:    gh.Ptr(1284),
		HTMLURL:   gh.Ptr("https://github.com/acme/api/pull/1284"),
		CreatedAt: &gh.Timestamp{Time: now.Add(-24 * time.Hour)},
		UpdatedAt: &gh.Timestamp{Time: now},
		User:      &gh.User{Login: gh.Ptr("alice"), ID: gh.Ptr(int64(1))},
	}
	meta, err := w.EnsureMeta(id, pr, now)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != id || meta.Source != "github" || meta.URL != pr.GetHTMLURL() {
		t.Errorf("meta init mismatch: %+v", meta)
	}
	if len(meta.Participants) != 1 || meta.Participants[0] != "alice" {
		t.Errorf("participants: %v", meta.Participants)
	}

	// Second EnsureMeta should return the existing meta unchanged.
	again, err := w.EnsureMeta(id, pr, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !again.TrackingSince.Equal(meta.TrackingSince) {
		t.Errorf("EnsureMeta clobbered tracking_since")
	}

	if err := w.UpdateMeta(id, func(m *Meta) {
		m.Participants = MergeParticipants(m.Participants, "bob")
		m.LastEventAt = now.Add(2 * time.Hour)
	}); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := w.LoadMeta(id)
	if len(reloaded.Participants) != 2 {
		t.Errorf("expected 2 participants: %v", reloaded.Participants)
	}
}

func TestWriter_InboxNewIdempotent(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir)
	r := RepoRef{Owner: "acme", Repo: "api"}

	pr := &gh.PullRequest{
		Number:    gh.Ptr(1284),
		Title:     gh.Ptr("Refactor"),
		HTMLURL:   gh.Ptr("https://github.com/acme/api/pull/1284"),
		User:      &gh.User{Login: gh.Ptr("alice")},
		State:     gh.Ptr("open"),
		Draft:     gh.Ptr(false),
		CreatedAt: &gh.Timestamp{Time: time.Now()},
		UpdatedAt: &gh.Timestamp{Time: time.Now()},
	}
	path, wrote, err := w.WriteInboxNew(pr, r, time.Now())
	if err != nil || !wrote {
		t.Fatalf("first write: %v wrote=%v", err, wrote)
	}
	var got InboxNew
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Ref.Number != 1284 || got.Ref.Author != "alice" {
		t.Errorf("inbox new content mismatch: %+v", got)
	}

	_, wrote2, err := w.WriteInboxNew(pr, r, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if wrote2 {
		t.Fatal("expected idempotent skip")
	}
}

func TestMergeParticipants(t *testing.T) {
	got := MergeParticipants([]string{"alice", "bob"}, "bob", "carol", "")
	want := []string{"alice", "bob", "carol"}
	if len(got) != len(want) {
		t.Fatalf("len: got %v want %v", got, want)
	}
	for i, s := range want {
		if got[i] != s {
			t.Errorf("idx %d: got %q want %q", i, got[i], s)
		}
	}
}

func TestWriter_AnomalyAndUpdate(t *testing.T) {
	dir := t.TempDir()
	w := NewWriter(dir)

	path, err := w.WriteAnomaly("github-acme-api-pr-1284", map[string]any{
		"kind": "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("anomaly missing: %v", err)
	}

	upath, err := w.WriteInboxUpdate(
		"github-acme-api-pr-1284", "issue-comment-99",
		"1 new event(s)", "threads/github-acme-api-pr-1284/raw/issue-comment-99.json",
		time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(upath); err != nil {
		t.Fatalf("update file missing: %v", err)
	}
}
