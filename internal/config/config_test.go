package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(fp, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestGet_Nested(t *testing.T) {
	fp := writeCfg(t, "models:\n  commander: opus\n  updater: haiku\n")
	tree, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	v, err := Get(tree, "models.commander")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v != "opus" {
		t.Errorf("expected opus, got %v", v)
	}
}

func TestGet_Missing(t *testing.T) {
	fp := writeCfg(t, "models:\n  commander: opus\n")
	tree, _ := Load(fp)
	_, err := Get(tree, "models.worker")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected ErrNotExist, got %v", err)
	}
}

func TestSet_CreatesPath(t *testing.T) {
	fp := writeCfg(t, "mode: hybrid\n")
	tree, _ := Load(fp)
	if err := Set(tree, "schedule.active_window.interval", "3m"); err != nil {
		t.Fatal(err)
	}
	v, err := Get(tree, "schedule.active_window.interval")
	if err != nil {
		t.Fatal(err)
	}
	if v != "3m" {
		t.Errorf("expected 3m, got %v", v)
	}
}

func TestSet_ScalarParse(t *testing.T) {
	fp := writeCfg(t, "mode: hybrid\n")
	tree, _ := Load(fp)
	if err := Set(tree, "limits.dashboard_token_budget", "8000"); err != nil {
		t.Fatal(err)
	}
	v, _ := Get(tree, "limits.dashboard_token_budget")
	if i, ok := v.(int); !ok || i != 8000 {
		t.Errorf("expected int 8000, got %T %v", v, v)
	}
	if err := Set(tree, "git.auto_commit", "false"); err != nil {
		t.Fatal(err)
	}
	v, _ = Get(tree, "git.auto_commit")
	if b, ok := v.(bool); !ok || b {
		t.Errorf("expected bool false, got %T %v", v, v)
	}
}

func TestRoundTrip(t *testing.T) {
	fp := writeCfg(t, "models:\n  commander: opus\n")
	tree, _ := Load(fp)
	if err := Set(tree, "models.commander", "sonnet"); err != nil {
		t.Fatal(err)
	}
	if err := Save(fp, tree); err != nil {
		t.Fatal(err)
	}
	tree2, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := Get(tree2, "models.commander")
	if v != "sonnet" {
		t.Errorf("expected sonnet, got %v", v)
	}
}
