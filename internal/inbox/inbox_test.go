package inbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWriteReadJSONRoundtrip is the most basic invariant the rest of
// the harness depends on.
func TestWriteReadJSONRoundtrip(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(Dir(root, BucketNew), 0o755)
	it := &Item{
		ID:         "slack-C0001-1",
		Source:     "slack",
		Kind:       "new",
		ReceivedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Summary:    "first message",
		RawInline: map[string]any{
			"text":      "hello",
			"permalink": "https://example.com/p1",
		},
	}
	p := PathFor(root, BucketNew, it.ID)
	if err := WriteJSON(p, it); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadJSON(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.ID != it.ID || got.Source != it.Source || got.Summary != it.Summary {
		t.Errorf("roundtrip differs: got=%+v want=%+v", got, it)
	}
	if got.RawInline == nil {
		t.Errorf("RawInline missing after roundtrip")
	}
}

// TestAppendAnomaly_LandsInAnomaliesBucket exercises the triage drop
// path's anomaly writer.
func TestAppendAnomaly_LandsInAnomaliesBucket(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(Dir(root, BucketAnomalies), 0o755)
	p, err := AppendAnomaly(root, "rollup:slack-x", "schema parse: oops")
	if err != nil {
		t.Fatalf("AppendAnomaly: %v", err)
	}
	if !strings.HasPrefix(p, filepath.Join(root, "inbox", "anomalies")) {
		t.Errorf("anomaly path under wrong bucket: %s", p)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read anomaly: %v", err)
	}
	var got Item
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse anomaly JSON: %v", err)
	}
	if got.Kind != "anomaly" {
		t.Errorf("anomaly kind wrong: %q", got.Kind)
	}
	if !strings.Contains(got.Summary, "schema parse") {
		t.Errorf("anomaly summary lost the message: %q", got.Summary)
	}
}

// TestList_FilterHonorsBucket.
func TestList_FilterHonorsBucket(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(Dir(root, BucketNew), 0o755)
	os.MkdirAll(Dir(root, BucketHuman), 0o755)
	os.WriteFile(filepath.Join(Dir(root, BucketNew), "a.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(Dir(root, BucketHuman), "b.json"), []byte(`{}`), 0o644)

	news, _ := List(root, BucketNew)
	if len(news) != 1 || news[0] != "a.json" {
		t.Errorf("want one entry in new bucket, got %v", news)
	}
	humans, _ := List(root, BucketHuman)
	if len(humans) != 1 || humans[0] != "b.json" {
		t.Errorf("want one entry in human bucket, got %v", humans)
	}
}
