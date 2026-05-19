package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFakeReadsFixture(t *testing.T) {
	dir := t.TempDir()
	roleDir := filepath.Join(dir, "commander")
	if err := os.MkdirAll(roleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roleDir, "0.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roleDir, "1.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := NewFake(dir)
	r1, err := f.Invoke(context.Background(), "prompt", InvokeOptions{Role: RoleCommander})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Output != "hello" {
		t.Errorf("first: %q", r1.Output)
	}
	r2, _ := f.Invoke(context.Background(), "prompt", InvokeOptions{Role: RoleCommander})
	if r2.Output != "world" {
		t.Errorf("second: %q", r2.Output)
	}
	if len(f.Calls()) != 2 {
		t.Errorf("calls=%d", len(f.Calls()))
	}
}

func TestFakeMissingFixture(t *testing.T) {
	f := NewFake(t.TempDir())
	r, err := f.Invoke(context.Background(), "p", InvokeOptions{Role: RoleCommander})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(r.Output, "[fake-driver:") {
		t.Errorf("expected fallback marker, got %q", r.Output)
	}
}

func TestFakeStreamCapture(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "updater"), 0o755)
	os.WriteFile(filepath.Join(dir, "updater", "0.txt"), []byte("body"), 0o644)
	jsonl := filepath.Join(t.TempDir(), "x.jsonl")
	f := NewFake(dir)
	r, err := f.Invoke(context.Background(), "p", InvokeOptions{Role: RoleUpdater, StreamJSONPath: jsonl})
	if err != nil {
		t.Fatal(err)
	}
	if r.RawArtifact != jsonl {
		t.Errorf("artifact path: %q want %q", r.RawArtifact, jsonl)
	}
	got, _ := os.ReadFile(jsonl)
	if !strings.Contains(string(got), "result") {
		t.Errorf("expected result event line, got %q", got)
	}
}

func TestClaudePParseStreamJSON(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"system"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello "}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"world"}]}}`,
		`{"type":"result","total_cost_usd":0.42,"duration_ms":1234,"is_error":false,"session_id":"abc"}`,
	}, "\n")
	var r InvokeResult
	parseStreamJSON(jsonl, &r)
	if r.Output != "hello world" {
		t.Errorf("output: %q", r.Output)
	}
	if r.CostUSD != 0.42 {
		t.Errorf("cost: %v", r.CostUSD)
	}
	if r.DurationMS != 1234 {
		t.Errorf("duration: %v", r.DurationMS)
	}
	if r.SessionID != "abc" {
		t.Errorf("session: %q", r.SessionID)
	}
}
