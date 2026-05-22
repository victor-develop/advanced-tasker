package github

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfig_Defaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  token_env: GITHUB_TOKEN
watch:
  repos:
    - acme/api
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PollInterval.Duration != 60*time.Second {
		t.Errorf("poll_interval default: got %s want 60s", cfg.PollInterval.Duration)
	}
	if cfg.NewPRLookback.Duration != 7*24*time.Hour {
		t.Errorf("new_pr_lookback default: got %s want 168h", cfg.NewPRLookback.Duration)
	}
	if cfg.MaxConcurrent != 4 {
		t.Errorf("max_concurrent default: got %d want 4", cfg.MaxConcurrent)
	}
	if cfg.Auth.Type != "pat" {
		t.Errorf("auth.type default: got %q want pat", cfg.Auth.Type)
	}
}

func TestLoadConfig_RejectsAppAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
auth:
  type: app
watch:
  repos: [acme/api]
`), 0o644)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for unsupported auth.type=app")
	}
}

func TestLoadConfig_RejectsEmptyRepos(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
auth:
  type: pat
watch:
  repos: []
`), 0o644)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for empty repos")
	}
}

func TestLoadConfig_DurationParsing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte(`
auth:
  token_env: GITHUB_TOKEN
watch:
  repos: [acme/api]
poll_interval: 30s
new_pr_lookback: 14d
backoff:
  on_rate_limit: 90s
  on_error: 10s
  max_backoff: 10m
`), 0o644)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval.Duration != 30*time.Second {
		t.Errorf("poll_interval: got %s", cfg.PollInterval.Duration)
	}
	if cfg.NewPRLookback.Duration != 14*24*time.Hour {
		t.Errorf("new_pr_lookback: got %s", cfg.NewPRLookback.Duration)
	}
	if cfg.Backoff.MaxBackoff.Duration != 10*time.Minute {
		t.Errorf("max_backoff: got %s", cfg.Backoff.MaxBackoff.Duration)
	}
}

func TestParseRepo(t *testing.T) {
	r, err := ParseRepo("acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if r.Owner != "acme" || r.Repo != "api" {
		t.Errorf("got %+v", r)
	}
	if _, err := ParseRepo("invalid"); err == nil {
		t.Error("expected error for missing slash")
	}
	if _, err := ParseRepo("/foo"); err == nil {
		t.Error("expected error for empty owner")
	}
	if _, err := ParseRepo("foo/"); err == nil {
		t.Error("expected error for empty repo")
	}
}

func TestThreadID_HandlesDashedNames(t *testing.T) {
	id := ThreadID(RepoRef{Owner: "acme-co", Repo: "foo-bar"}, 42)
	want := "github-acme-co-foo-bar-pr-42"
	if id != want {
		t.Errorf("ThreadID: got %q want %q", id, want)
	}
}
