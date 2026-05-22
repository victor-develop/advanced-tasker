package slack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRawInlineAlignmentWithDesign verifies that a poller-produced
// inbox/new/slack-*.json file contains a `raw_inline` object with `text`
// and `permalink` keys, and NOT a `raw_path` field. This is the schema
// fixed by design PR #5 (round-3 clarifications) — see
// design/02-state-and-schemas.md §"Raw event location" and
// design/08-slack-poller.md §"New (untracked) thread -> inbox/new".
//
// Round-2 PR #2 body flagged a `raw_path` vs `raw_inline` design
// inconsistency; design PR #5 unified on `raw_inline`. This test guards
// against any future code drift back to `raw_path`.
func TestRawInlineAlignmentWithDesign(t *testing.T) {
	stateRoot := t.TempDir()
	w := NewWriter(stateRoot)

	item := &InboxItem{
		ID:      "slack-C0492-1700000999.000400",
		Source:  "slack",
		Kind:    "new",
		Summary: "hey, what's the status of the ingest pipeline?",
		Ref: InboxRef{
			Channel: "C0492",
			TS:      "1700000999.000400",
			User:    "U_ALICE",
		},
		RawInline: map[string]any{
			"text":      "hey, what's the status of the ingest pipeline?",
			"permalink": "https://acme.slack.com/archives/C0492/p1700000999000400",
		},
	}
	wrote, err := w.WriteInboxNew(item)
	if err != nil {
		t.Fatalf("WriteInboxNew: %v", err)
	}
	if !wrote {
		t.Fatal("expected wrote=true on first write")
	}

	path := filepath.Join(stateRoot, "inbox", "new", item.ID+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read inbox/new: %v", err)
	}

	// Decode generically so we can assert the JSON keys directly, not the
	// Go struct shape. This is the contract the rollup updater / commander
	// reads against.
	var blob map[string]any
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, present := blob["raw_path"]; present {
		t.Errorf("inbox/new entry contains forbidden `raw_path` key — "+
			"design PR #5 unified on `raw_inline`. Full JSON:\n%s", string(raw))
	}

	inlineAny, ok := blob["raw_inline"]
	if !ok {
		t.Fatalf("inbox/new entry missing `raw_inline` key. Full JSON:\n%s", string(raw))
	}
	inline, ok := inlineAny.(map[string]any)
	if !ok {
		t.Fatalf("raw_inline is not a JSON object: %v", inlineAny)
	}
	if text, ok := inline["text"].(string); !ok || text == "" {
		t.Errorf("raw_inline.text missing or empty; got %v", inline["text"])
	}
	if pl, ok := inline["permalink"].(string); !ok || pl == "" {
		t.Errorf("raw_inline.permalink missing or empty; got %v", inline["permalink"])
	}
}

// TestInboxItemMarshalUsesRawInlineFieldName guards the struct tag — the
// JSON field MUST be `raw_inline`, never `raw_path`. A previous draft of
// design/02 used `raw_path` (see design PR #5 §"The `raw_path` field name
// was used in earlier drafts.").
func TestInboxItemMarshalUsesRawInlineFieldName(t *testing.T) {
	item := &InboxItem{
		ID:      "slack-X-1",
		Source:  "slack",
		Kind:    "new",
		Summary: "x",
		Ref:     InboxRef{Channel: "X", TS: "1", User: "U"},
		RawInline: map[string]any{
			"text":      "x",
			"permalink": "https://x.test",
		},
	}
	b, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !contains(s, `"raw_inline"`) {
		t.Errorf("marshaled JSON missing `raw_inline`:\n%s", s)
	}
	if contains(s, `"raw_path"`) {
		t.Errorf("marshaled JSON contains forbidden `raw_path`:\n%s", s)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) &&
		(len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
