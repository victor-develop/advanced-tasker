package tick

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestClaim_ContendedRejected — once a lease is held, a second Claim
// must return ErrContended (until the lease expires).
func TestClaim_ContendedRejected(t *testing.T) {
	root := t.TempDir()
	if _, err := Claim(root, "alice", os.Getpid(), 10*time.Second); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	_, err := Claim(root, "bob", os.Getpid()+1, 10*time.Second)
	if err == nil {
		t.Errorf("expected contention on second claim")
	}
	// Release lets us claim again.
	if err := Release(root); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := Claim(root, "carol", os.Getpid(), 10*time.Second); err != nil {
		t.Fatalf("post-release claim: %v", err)
	}
}

// TestLogFrontmatterAndFinalize exercises the start → log → end cycle
// that `harness tick start` / `tick end --idle` calls through.
func TestLogFrontmatterAndFinalize(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "tick-log"), 0o755)
	path, when := LogFileForNow(root)
	if err := WriteFrontmatter(path, when.Format("2006-01-02T15-04-05Z"), "tester"); err != nil {
		t.Fatal(err)
	}
	if err := AppendLog(path, "saw nothing actionable"); err != nil {
		t.Fatal(err)
	}
	if err := FinalizeLog(path, 1500, 0.01, true, 1, "no-op tick"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"tick_id:",
		"agent: tester",
		"saw nothing actionable",
		"duration_ms: 1500",
		"idle: true",
		"consecutive_idle: 1",
		"no-op tick",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("tick log missing %q\n--- got ---\n%s", want, s)
		}
	}
}

// TestCurrentLogPath_ReturnsNewestEntry mimics the dashboard renderer
// pulling the most recent tick.
func TestCurrentLogPath_ReturnsNewestEntry(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tick-log")
	os.MkdirAll(dir, 0o755)
	for _, n := range []string{"2026-01-01.md", "2026-01-02.md", "2026-01-03.md"} {
		os.WriteFile(filepath.Join(dir, n), []byte("---\n"), 0o644)
	}
	p, ok := CurrentLogPath(root)
	if !ok || !strings.HasSuffix(p, "2026-01-03.md") {
		t.Errorf("expected newest entry, got %q ok=%v", p, ok)
	}
}
