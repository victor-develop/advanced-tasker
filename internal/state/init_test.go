package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInit_CreatesLayout(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "state")
	if err := Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}

	mustDir := func(p string) {
		t.Helper()
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("missing: %s: %v", p, err)
			return
		}
		if !info.IsDir() {
			t.Errorf("not a dir: %s", p)
		}
	}
	mustFile := func(p string) {
		t.Helper()
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("missing: %s: %v", p, err)
			return
		}
		if info.IsDir() {
			t.Errorf("not a file: %s", p)
		}
	}

	for _, sub := range Layout {
		mustDir(filepath.Join(root, sub))
	}
	mustDir(filepath.Join(root, ".git"))
	mustFile(filepath.Join(root, "config.yaml"))
	mustFile(filepath.Join(root, ".gitignore"))
	mustFile(filepath.Join(root, "roles", "pr-reviewer.md"))
	mustFile(filepath.Join(root, ".git", "hooks", "post-commit"))

	if !IsInitialized(root) {
		t.Errorf("IsInitialized returned false on freshly-initted root")
	}
}

func TestInit_RefusesExisting(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "state")
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	if err := Init(root); err == nil {
		t.Errorf("expected Init to reject existing state")
	}
}

func TestInit_GitignoreExcludesQueues(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "state")
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"inbox/", "jobs/", "outbox/pending/", "outbox/awaiting-human/", "outbox/failed/", "telemetry/"} {
		if !contains(body, want) {
			t.Errorf("gitignore missing entry %q", want)
		}
	}
	// outbox/sent/ is NOT ignored — it is git-tracked per design/07.
	if contains(body, "outbox/sent/") {
		t.Errorf("gitignore must NOT exclude outbox/sent/")
	}
}

func contains(haystack []byte, needle string) bool {
	return string(haystack) != "" && indexOf(string(haystack), needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
