package slack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestThreadIDAndParse(t *testing.T) {
	id := ThreadID("C0492", "1715814123.001200")
	if id != "slack-C0492-1715814123.001200" {
		t.Errorf("ThreadID = %s", id)
	}
	ch, ts, ok := ParseThreadID(id)
	if !ok || ch != "C0492" || ts != "1715814123.001200" {
		t.Errorf("ParseThreadID failed: %s %s %v", ch, ts, ok)
	}
}

func TestParseThreadIDInvalid(t *testing.T) {
	cases := []string{"", "github-foo-1", "slack-only", "slack-"}
	for _, c := range cases {
		if _, _, ok := ParseThreadID(c); ok {
			t.Errorf("expected ParseThreadID(%q) to fail", c)
		}
	}
}

func TestWriteRawEventCreatesAndDedups(t *testing.T) {
	stateRoot := t.TempDir()
	w := NewWriter(stateRoot)
	w.Clock = func() time.Time { return time.Date(2026, 5, 19, 10, 15, 0, 0, time.UTC) }

	threadID := "slack-C0492-1715814123.001200"
	ev := &Event{
		Channel:            "C0492",
		TS:                 "1715814200.000100",
		ThreadTS:           "1715814123.001200",
		User:               "U07ABCDEF",
		Text:               "hello",
		Permalink:          "https://acme.slack.com/p1",
		IsTopLevelInThread: false,
	}

	wrote, err := w.WriteRawEvent(threadID, ev)
	if err != nil {
		t.Fatalf("WriteRawEvent: %v", err)
	}
	if !wrote {
		t.Errorf("expected wrote=true on fresh write")
	}

	path := filepath.Join(stateRoot, "threads", threadID, "raw", ev.TS+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	var roundtrip Event
	if err := json.Unmarshal(b, &roundtrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundtrip.Text != "hello" {
		t.Errorf("text = %q", roundtrip.Text)
	}
	if roundtrip.Source != "slack" {
		t.Errorf("source = %q", roundtrip.Source)
	}
	if roundtrip.CapturedAt == "" {
		t.Errorf("captured_at empty")
	}

	// Second write must be a no-op (dedup).
	wrote2, err := w.WriteRawEvent(threadID, ev)
	if err != nil {
		t.Fatalf("WriteRawEvent (2nd): %v", err)
	}
	if wrote2 {
		t.Errorf("expected wrote=false on dedup")
	}

	// File contents must not have changed.
	b2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(b2) {
		t.Errorf("file mutated on dedup write")
	}
}

func TestRawEventExists(t *testing.T) {
	w := NewWriter(t.TempDir())
	threadID := "slack-C1-100.000"
	if w.RawEventExists(threadID, "100.000") {
		t.Fatal("should not exist yet")
	}
	if _, err := w.WriteRawEvent(threadID, &Event{Channel: "C1", TS: "100.000", ThreadTS: "100.000"}); err != nil {
		t.Fatal(err)
	}
	if !w.RawEventExists(threadID, "100.000") {
		t.Fatal("should exist now")
	}
}

func TestEnsureMetaFreshAndUpdate(t *testing.T) {
	stateRoot := t.TempDir()
	w := NewWriter(stateRoot)
	w.Clock = func() time.Time { return time.Date(2026, 5, 19, 10, 15, 0, 0, time.UTC) }

	threadID := "slack-C1-1700000000.000000"
	if err := w.EnsureMeta(threadID, "C1", "1700000000.000000",
		"https://link", "1700000000.000000", "1700000100.000000",
		[]string{"U_A"}); err != nil {
		t.Fatalf("EnsureMeta fresh: %v", err)
	}

	metaPath := filepath.Join(stateRoot, "threads", threadID, "meta.json")
	b, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != threadID {
		t.Errorf("id = %s", m.ID)
	}
	if m.URL != "https://link" {
		t.Errorf("url = %s", m.URL)
	}
	if len(m.Participants) != 1 || m.Participants[0] != "U_A" {
		t.Errorf("participants = %v", m.Participants)
	}
	firstLastEvent := m.LastEventAt

	// Update with a newer event and a new participant.
	w.Clock = func() time.Time { return time.Date(2026, 5, 19, 11, 0, 0, 0, time.UTC) }
	if err := w.EnsureMeta(threadID, "C1", "1700000000.000000",
		"https://link2", "1700000050.000000", "1700000200.000000",
		[]string{"U_B"}); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.LastEventAt == firstLastEvent {
		t.Errorf("expected LastEventAt to advance")
	}
	if len(m.Participants) != 2 {
		t.Errorf("expected union of participants, got %v", m.Participants)
	}
}

func TestTouchDirty(t *testing.T) {
	stateRoot := t.TempDir()
	w := NewWriter(stateRoot)
	threadID := "slack-C1-9000.000"
	if err := w.TouchDirty(threadID); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(stateRoot, "threads", threadID, ".dirty")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("dirty not created: %v", err)
	}
	// Calling again should still succeed.
	if err := w.TouchDirty(threadID); err != nil {
		t.Fatalf("second touch: %v", err)
	}
}

func TestInboxNewIdempotent(t *testing.T) {
	stateRoot := t.TempDir()
	w := NewWriter(stateRoot)
	item := &InboxItem{
		ID:      "slack-C1-1234567890.000100",
		Summary: "hello",
		Ref:     InboxRef{Channel: "C1", TS: "1234567890.000100", User: "U_A"},
	}
	wrote, err := w.WriteInboxNew(item)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("expected wrote=true")
	}
	wrote2, err := w.WriteInboxNew(item)
	if err != nil {
		t.Fatal(err)
	}
	if wrote2 {
		t.Fatal("expected wrote=false on overlap")
	}
}

func TestUpdatePingGatedByFlag(t *testing.T) {
	stateRoot := t.TempDir()
	w := NewWriter(stateRoot)
	// Default: disabled.
	if err := w.WriteUpdatePing("slack-C1-1.0", "2.0", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "inbox", "updates")); err == nil {
		t.Fatal("expected no updates dir when WriteUpdatePings=false")
	}

	w.WriteUpdatePings = true
	if err := w.WriteUpdatePing("slack-C1-1.0", "2.0", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "inbox", "updates", "slack-C1-1.0-2.0.json")); err != nil {
		t.Fatalf("update ping not written: %v", err)
	}
}

func TestListTrackedThreads(t *testing.T) {
	stateRoot := t.TempDir()
	w := NewWriter(stateRoot)
	threadsDir := w.Layout.ThreadsDir()
	for _, name := range []string{"slack-C1-1.0", "slack-C2-2.0", "github-foo", "not-a-thread"} {
		must(t, os.MkdirAll(filepath.Join(threadsDir, name), 0o755))
	}
	ids, err := w.ListTrackedThreads()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "slack-C1-1.0" || ids[1] != "slack-C2-2.0" {
		t.Errorf("ListTrackedThreads = %v", ids)
	}
}

func TestSlackTSToISO(t *testing.T) {
	got, err := SlackTSToISO("1715814123.001200")
	if err != nil {
		t.Fatal(err)
	}
	// 1715814123 = 2024-05-15T23:02:03Z (UTC)
	if got != "2024-05-15T23:02:03Z" {
		t.Errorf("got %s", got)
	}
}
