package gitops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInitAndCommit(t *testing.T) {
	dir := t.TempDir()
	r := Repo{Dir: dir}
	if err := r.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Initial commit should fail with ErrNothingToCommit (no staged changes).
	if _, err := r.Commit("empty"); !errors.Is(err, ErrNothingToCommit) {
		t.Fatalf("expected ErrNothingToCommit, got %v", err)
	}

	// Add a file, commit, expect a SHA.
	fp := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(fp, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Add("hello.txt"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	sha, err := r.Commit("test: add hello")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(sha) < 7 {
		t.Errorf("expected sha, got %q", sha)
	}

	// A second commit with no staged changes should again error.
	if _, err := r.Commit("no-op"); !errors.Is(err, ErrNothingToCommit) {
		t.Fatalf("expected ErrNothingToCommit, got %v", err)
	}
}

func TestIndexClean_NoHead(t *testing.T) {
	dir := t.TempDir()
	r := Repo{Dir: dir}
	if err := r.Init(); err != nil {
		t.Fatal(err)
	}
	clean, err := r.IndexClean()
	if err != nil {
		t.Fatal(err)
	}
	if !clean {
		t.Errorf("expected clean before any add")
	}
	// Stage something, then expect not clean.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Add("a.txt"); err != nil {
		t.Fatal(err)
	}
	clean, err = r.IndexClean()
	if err != nil {
		t.Fatal(err)
	}
	if clean {
		t.Errorf("expected not clean after add")
	}
}
