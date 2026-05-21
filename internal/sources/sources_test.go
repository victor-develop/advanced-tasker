package sources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInit_IdempotentSlack covers the `harness config init slack`
// happy path AND the no-op re-run case.
func TestInit_IdempotentSlack(t *testing.T) {
	root := t.TempDir()
	created, err := Init(root, SourceSlack)
	if err != nil {
		t.Fatalf("init slack: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on first init")
	}
	// Re-run is a no-op (created=false, no error, no overwrite).
	body1, _ := os.ReadFile(ConfigPath(root, SourceSlack))
	created2, err := Init(root, SourceSlack)
	if err != nil {
		t.Fatalf("second init: %v", err)
	}
	if created2 {
		t.Errorf("expected created=false on second init")
	}
	body2, _ := os.ReadFile(ConfigPath(root, SourceSlack))
	if string(body1) != string(body2) {
		t.Errorf("config was overwritten on second init")
	}
}

// TestWatchChannel_AppendsAndIdempotent verifies `harness watch
// slack-channel C0492 --reason "..."` semantics.
func TestWatchChannel_AppendsAndIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := Init(root, SourceSlack); err != nil {
		t.Fatal(err)
	}
	if err := WatchChannel(root, "C0492", "data team"); err != nil {
		t.Fatalf("watch: %v", err)
	}
	body, _ := os.ReadFile(ConfigPath(root, SourceSlack))
	if !strings.Contains(string(body), "C0492") || !strings.Contains(string(body), "data team") {
		t.Errorf("config missing channel/reason:\n%s", body)
	}
	// Re-run is a no-op (no duplicate entry).
	if err := WatchChannel(root, "C0492", "data team"); err != nil {
		t.Fatal(err)
	}
	body2, _ := os.ReadFile(ConfigPath(root, SourceSlack))
	if strings.Count(string(body2), "C0492") != 1 {
		t.Errorf("expected single entry, got:\n%s", body2)
	}
}

// TestWatchRepo_OwnerRepoFormatEnforced + idempotency.
func TestWatchRepo_FormatAndIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, err := Init(root, SourceGitHub); err != nil {
		t.Fatal(err)
	}
	if err := WatchRepo(root, "no-slash"); err == nil {
		t.Errorf("expected error on bare repo name")
	}
	if err := WatchRepo(root, "acme/api"); err != nil {
		t.Fatalf("watch: %v", err)
	}
	body, _ := os.ReadFile(ConfigPath(root, SourceGitHub))
	if !strings.Contains(string(body), "acme/api") {
		t.Errorf("config missing repo:\n%s", body)
	}
	// Idempotent.
	if err := WatchRepo(root, "acme/api"); err != nil {
		t.Fatal(err)
	}
	body2, _ := os.ReadFile(ConfigPath(root, SourceGitHub))
	if strings.Count(string(body2), "acme/api") != 1 {
		t.Errorf("expected single entry, got:\n%s", body2)
	}
}

// TestLoadRaw_MissingFileReturnsTypedError exercises the operator-friendly
// error path pollers depend on.
func TestLoadRaw_MissingFileReturnsTypedError(t *testing.T) {
	root := t.TempDir()
	_, err := LoadRaw(root, SourceSlack)
	if err == nil || err.Error() == "" {
		t.Fatalf("expected non-nil error for missing config")
	}
	// Pollers match on the sentinel.
	if err != ErrConfigMissing {
		t.Errorf("expected ErrConfigMissing sentinel, got %T %v", err, err)
	}
}

// TestSummary_AggregatesBothSources.
func TestSummary_AggregatesBothSources(t *testing.T) {
	root := t.TempDir()
	Init(root, SourceSlack)
	Init(root, SourceGitHub)
	WatchChannel(root, "Cabc", "")
	WatchRepo(root, "acme/api")
	out, err := Summary(root)
	if err != nil {
		t.Fatal(err)
	}
	gotSlack, gotGH := false, false
	for _, line := range out {
		if line == "slack/Cabc" {
			gotSlack = true
		}
		if line == "github/acme/api" {
			gotGH = true
		}
	}
	if !gotSlack || !gotGH {
		t.Errorf("summary missing entries, got %v", out)
	}
	// Confirm the data lives where pollers expect.
	if _, err := os.Stat(filepath.Join(root, "sources", "slack", "config.yaml")); err != nil {
		t.Errorf("slack config not on expected path: %v", err)
	}
}
