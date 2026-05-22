//go:build integration

package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockSlack stands up an httptest.Server that mimics the small slice of
// Slack's Web API we hit: conversations.history, conversations.replies,
// and chat.getPermalink. Tests register fixtures keyed by (method,
// channel, optional thread_ts).
type mockSlack struct {
	mu       sync.Mutex
	srv      *httptest.Server
	history  map[string][]mockHistoryPage // key: channel
	replies  map[string][]mockRepliesPage // key: channel+"/"+thread_ts
	calls    []mockCall
}

type mockHistoryPage struct {
	Messages   []map[string]any
	NextCursor string
}

type mockRepliesPage struct {
	Messages   []map[string]any
	NextCursor string
}

type mockCall struct {
	Endpoint  string
	Channel   string
	ThreadTS  string
	Oldest    string
	Cursor    string
}

func newMockSlack(t *testing.T) *mockSlack {
	t.Helper()
	m := &mockSlack{
		history: map[string][]mockHistoryPage{},
		replies: map[string][]mockRepliesPage{},
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockSlack) URL() string {
	// slack-go appends method name to OptionAPIURL. The trailing slash
	// matters.
	return m.srv.URL + "/"
}

func (m *mockSlack) handle(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	endpoint := strings.TrimPrefix(r.URL.Path, "/")
	m.mu.Lock()
	defer m.mu.Unlock()
	switch endpoint {
	case "conversations.history":
		channel := r.Form.Get("channel")
		oldest := r.Form.Get("oldest")
		cursor := r.Form.Get("cursor")
		m.calls = append(m.calls, mockCall{Endpoint: endpoint, Channel: channel, Oldest: oldest, Cursor: cursor})
		pages := m.history[channel]
		// Cursor convention: page N has NextCursor "cN", which on the
		// next request returns page index N (zero-based). So an empty
		// cursor returns page 0; "c1" returns page 1; etc.
		idx := 0
		if cursor != "" {
			for i := range pages {
				if fmt.Sprintf("c%d", i) == cursor {
					idx = i
					break
				}
			}
		}
		var page mockHistoryPage
		if idx < len(pages) {
			page = pages[idx]
		}
		writeJSON(w, map[string]any{
			"ok":       true,
			"messages": page.Messages,
			"has_more": page.NextCursor != "",
			"response_metadata": map[string]any{
				"next_cursor": page.NextCursor,
			},
		})
	case "conversations.replies":
		channel := r.Form.Get("channel")
		thread := r.Form.Get("ts")
		oldest := r.Form.Get("oldest")
		cursor := r.Form.Get("cursor")
		m.calls = append(m.calls, mockCall{Endpoint: endpoint, Channel: channel, ThreadTS: thread, Oldest: oldest, Cursor: cursor})
		key := channel + "/" + thread
		pages := m.replies[key]
		idx := 0
		if cursor != "" {
			for i := range pages {
				if fmt.Sprintf("c%d", i) == cursor {
					idx = i
					break
				}
			}
		}
		var page mockRepliesPage
		if idx < len(pages) {
			page = pages[idx]
		}
		writeJSON(w, map[string]any{
			"ok":       true,
			"messages": page.Messages,
			"has_more": page.NextCursor != "",
			"response_metadata": map[string]any{
				"next_cursor": page.NextCursor,
			},
		})
	case "chat.getPermalink":
		channel := r.Form.Get("channel")
		ts := r.Form.Get("message_ts")
		writeJSON(w, map[string]any{
			"ok":        true,
			"permalink": fmt.Sprintf("https://example.test/archives/%s/p%s", channel, strings.ReplaceAll(ts, ".", "")),
		})
	default:
		writeJSON(w, map[string]any{"ok": false, "error": "unknown_method:" + endpoint})
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (m *mockSlack) addHistoryPages(channel string, pages ...mockHistoryPage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.history[channel] = append(m.history[channel], pages...)
}

func (m *mockSlack) addRepliesPages(channel, threadTS string, pages ...mockRepliesPage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := channel + "/" + threadTS
	m.replies[key] = append(m.replies[key], pages...)
}

func (m *mockSlack) callsFor(endpoint string) []mockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mockCall
	for _, c := range m.calls {
		if c.Endpoint == endpoint {
			out = append(out, c)
		}
	}
	return out
}

// ensure mock is reachable
func TestMockHandlerSmoke(t *testing.T) {
	m := newMockSlack(t)
	resp, err := http.PostForm(m.URL()+"conversations.history",
		url.Values{"channel": {"C1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// helper: build a configured Poller for tests
func buildPoller(t *testing.T, m *mockSlack, stateRoot string, channels []WatchedChannel) *Poller {
	t.Helper()
	cfg := &Config{
		Auth:                     AuthConfig{Token: "xoxb-test"},
		Watch:                    WatchConfig{Channels: channels},
		MaxConcurrentThreadPolls: 2,
	}
	cfg.PollInterval.Duration = 30 * time.Second
	client := NewSlackGoClient("xoxb-test", m.URL())
	cursors, err := NewCursorStore(filepath.Join(stateRoot, "sources", "slack", "cursors"))
	if err != nil {
		t.Fatal(err)
	}
	w := NewWriter(stateRoot)
	w.WriteUpdatePings = true
	return &Poller{Cfg: cfg, Client: client, Cursors: cursors, Writer: w}
}

func TestFreshStart_LandsInInboxNew(t *testing.T) {
	stateRoot := t.TempDir()
	m := newMockSlack(t)
	m.addHistoryPages("C0492", mockHistoryPage{
		Messages: []map[string]any{
			{
				"type": "message",
				"ts":   "1700000100.000100",
				"user": "U_ALICE",
				"text": "hey, what's the status?",
			},
		},
	})

	p := buildPoller(t, m, stateRoot, []WatchedChannel{{ID: "C0492"}})
	res, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if res.InboxNewWritten != 1 {
		t.Errorf("InboxNewWritten = %d, want 1", res.InboxNewWritten)
	}
	if res.RawEventsWritten != 0 {
		t.Errorf("RawEventsWritten = %d, want 0", res.RawEventsWritten)
	}

	// Inbox file present.
	inboxPath := filepath.Join(stateRoot, "inbox", "new", "slack-C0492-1700000100.000100.json")
	b, err := os.ReadFile(inboxPath)
	if err != nil {
		t.Fatalf("inbox file: %v", err)
	}
	var item InboxItem
	if err := json.Unmarshal(b, &item); err != nil {
		t.Fatal(err)
	}
	if item.Ref.Channel != "C0492" || item.Ref.TS != "1700000100.000100" {
		t.Errorf("inbox ref unexpected: %+v", item.Ref)
	}
	if item.Source != "slack" || item.Kind != "new" {
		t.Errorf("inbox source/kind unexpected: %+v", item)
	}
	// No tracked thread dir should exist.
	tdir := filepath.Join(stateRoot, "threads", "slack-C0492-1700000100.000100")
	if _, err := os.Stat(tdir); err == nil {
		t.Errorf("poller must not auto-create thread dir for new message")
	}

	// Cursor advanced.
	cur, err := p.Cursors.GetChannelCursor("C0492")
	if err != nil {
		t.Fatal(err)
	}
	if cur != "1700000100.000100" {
		t.Errorf("channel cursor = %q", cur)
	}
}

func TestOverlapIdempotency(t *testing.T) {
	stateRoot := t.TempDir()
	m := newMockSlack(t)
	msg := map[string]any{
		"type": "message",
		"ts":   "1700000100.000100",
		"user": "U_ALICE",
		"text": "first hello",
	}
	// Same page returned every call.
	m.addHistoryPages("C0492",
		mockHistoryPage{Messages: []map[string]any{msg}},
		mockHistoryPage{Messages: []map[string]any{msg}},
	)
	p := buildPoller(t, m, stateRoot, []WatchedChannel{{ID: "C0492"}})

	res, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.InboxNewWritten != 1 {
		t.Errorf("first poll: InboxNewWritten = %d", res.InboxNewWritten)
	}
	// Reset cursor to simulate overlap.
	must(t, p.Cursors.SetChannelCursor("C0492", ""))
	res2, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res2.InboxNewWritten != 0 {
		t.Errorf("second poll should be no-op, got %d", res2.InboxNewWritten)
	}

	// Only one file under inbox/new.
	entries, err := os.ReadDir(filepath.Join(stateRoot, "inbox", "new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 file in inbox/new, got %d", len(entries))
	}
}

func TestThreadPromotion_UsesThreadLevelPolling(t *testing.T) {
	stateRoot := t.TempDir()
	m := newMockSlack(t)

	// Manually create a tracked thread directory — simulating that the
	// commander has promoted a thread via harness CLI.
	threadID := "slack-C0492-1700000000.000000"
	threadDir := filepath.Join(stateRoot, "threads", threadID)
	must(t, os.MkdirAll(threadDir, 0o755))

	// Channel-level: nothing new at channel level.
	m.addHistoryPages("C0492", mockHistoryPage{Messages: nil})

	// Thread-level: two replies on the tracked thread.
	m.addRepliesPages("C0492", "1700000000.000000", mockRepliesPage{
		Messages: []map[string]any{
			{
				"type":      "message",
				"ts":        "1700000000.000000",
				"thread_ts": "1700000000.000000",
				"user":      "U_ALICE",
				"text":      "thread root",
			},
			{
				"type":      "message",
				"ts":        "1700000050.000000",
				"thread_ts": "1700000000.000000",
				"user":      "U_BOB",
				"text":      "first reply",
			},
		},
	})

	p := buildPoller(t, m, stateRoot, []WatchedChannel{{ID: "C0492"}})
	res, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if res.ThreadsPolled != 1 {
		t.Errorf("ThreadsPolled = %d, want 1", res.ThreadsPolled)
	}
	if res.RawEventsWritten != 2 {
		t.Errorf("RawEventsWritten = %d, want 2", res.RawEventsWritten)
	}

	// conversations.replies must have been called for this thread.
	if calls := m.callsFor("conversations.replies"); len(calls) == 0 {
		t.Errorf("expected at least one conversations.replies call")
	}
	// raw files exist.
	rawDir := filepath.Join(threadDir, "raw")
	for _, ts := range []string{"1700000000.000000", "1700000050.000000"} {
		if _, err := os.Stat(filepath.Join(rawDir, ts+".json")); err != nil {
			t.Errorf("raw event %s missing: %v", ts, err)
		}
	}
	// meta.json created.
	if _, err := os.Stat(filepath.Join(threadDir, "meta.json")); err != nil {
		t.Errorf("meta.json missing: %v", err)
	}
	// .dirty touched.
	if _, err := os.Stat(filepath.Join(threadDir, ".dirty")); err != nil {
		t.Errorf(".dirty missing: %v", err)
	}
	// Thread cursor advanced.
	cur, err := p.Cursors.GetThreadCursor(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if cur != "1700000050.000000" {
		t.Errorf("thread cursor = %q", cur)
	}
}

func TestDedup_PrewrittenRawNotOverwritten(t *testing.T) {
	stateRoot := t.TempDir()
	m := newMockSlack(t)

	threadID := "slack-C0492-1700000000.000000"
	rawDir := filepath.Join(stateRoot, "threads", threadID, "raw")
	must(t, os.MkdirAll(rawDir, 0o755))
	prewritten := filepath.Join(rawDir, "1700000050.000000.json")
	must(t, os.WriteFile(prewritten, []byte(`{"text":"PRE-EXISTING SENTINEL"}`), 0o644))

	m.addHistoryPages("C0492", mockHistoryPage{Messages: nil})
	m.addRepliesPages("C0492", "1700000000.000000", mockRepliesPage{
		Messages: []map[string]any{
			{
				"type":      "message",
				"ts":        "1700000050.000000",
				"thread_ts": "1700000000.000000",
				"user":      "U_BOB",
				"text":      "different text from API",
			},
		},
	})

	p := buildPoller(t, m, stateRoot, []WatchedChannel{{ID: "C0492"}})
	if _, err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(prewritten)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "PRE-EXISTING SENTINEL") {
		t.Errorf("dedup violated; raw file rewritten: %s", b)
	}
}

func TestPagination(t *testing.T) {
	stateRoot := t.TempDir()
	m := newMockSlack(t)
	// Two pages of channel history.
	m.addHistoryPages("C0492",
		mockHistoryPage{
			Messages:   []map[string]any{{"type": "message", "ts": "1700000100.000100", "user": "U1", "text": "p1m1"}},
			NextCursor: "c1",
		},
		mockHistoryPage{
			Messages:   []map[string]any{{"type": "message", "ts": "1700000200.000100", "user": "U2", "text": "p2m1"}},
			NextCursor: "",
		},
	)

	p := buildPoller(t, m, stateRoot, []WatchedChannel{{ID: "C0492"}})
	res, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.InboxNewWritten != 2 {
		t.Errorf("InboxNewWritten = %d, want 2", res.InboxNewWritten)
	}
	// Cursor at newest.
	cur, _ := p.Cursors.GetChannelCursor("C0492")
	if cur != "1700000200.000100" {
		t.Errorf("cursor = %q", cur)
	}
}

func TestCursorCrashSafety_NoAdvanceOnWriteFail(t *testing.T) {
	stateRoot := t.TempDir()
	m := newMockSlack(t)

	threadID := "slack-C0492-1700000000.000000"
	// Make a tracked thread, but make the raw dir unwritable by making
	// it a file instead of a directory. The poller's write call will
	// fail and the cursor must not advance.
	threadDir := filepath.Join(stateRoot, "threads", threadID)
	must(t, os.MkdirAll(threadDir, 0o755))
	rawDirAsFile := filepath.Join(threadDir, "raw")
	must(t, os.WriteFile(rawDirAsFile, []byte("not a directory"), 0o644))

	m.addHistoryPages("C0492", mockHistoryPage{Messages: nil})
	m.addRepliesPages("C0492", "1700000000.000000", mockRepliesPage{
		Messages: []map[string]any{
			{
				"type":      "message",
				"ts":        "1700000050.000000",
				"thread_ts": "1700000000.000000",
				"user":      "U_BOB",
				"text":      "should fail to write",
			},
		},
	})

	p := buildPoller(t, m, stateRoot, []WatchedChannel{{ID: "C0492"}})
	res, _ := p.PollOnce(context.Background())
	// PollOnce does not propagate per-thread errors; check error counter.
	if res.Errors == 0 {
		t.Errorf("expected at least one error from write failure")
	}

	// Thread cursor must NOT have advanced.
	cur, err := p.Cursors.GetThreadCursor(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if cur != "" {
		t.Errorf("thread cursor should remain empty, got %q", cur)
	}
}

func TestThreadCursor_SkipsPreCursorEchoes(t *testing.T) {
	// conversations.replies always returns the thread root. If our
	// cursor already covered the root, we should not re-write it (dedup),
	// and should not write anything when there are no new replies past
	// the cursor.
	stateRoot := t.TempDir()
	m := newMockSlack(t)

	threadID := "slack-C0492-1700000000.000000"
	threadDir := filepath.Join(stateRoot, "threads", threadID)
	must(t, os.MkdirAll(threadDir, 0o755))

	p := buildPoller(t, m, stateRoot, []WatchedChannel{{ID: "C0492"}})
	// Pre-set the cursor past the root.
	must(t, p.Cursors.SetThreadCursor(threadID, "1700000050.000000"))

	m.addHistoryPages("C0492", mockHistoryPage{Messages: nil})
	m.addRepliesPages("C0492", "1700000000.000000", mockRepliesPage{
		Messages: []map[string]any{
			// Root echo
			{"type": "message", "ts": "1700000000.000000", "thread_ts": "1700000000.000000", "user": "U_A", "text": "root"},
		},
	})

	res, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.RawEventsWritten != 0 {
		t.Errorf("RawEventsWritten = %d, want 0 (root echo should be skipped)", res.RawEventsWritten)
	}
}
