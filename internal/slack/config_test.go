package slack

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(path, []byte(`
auth:
  token_env: SLACK_BOT_TOKEN
watch:
  channels:
    - id: C123
      reason: test
`), 0o644))

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got, want := cfg.PollInterval.Duration, 30*time.Second; got != want {
		t.Errorf("PollInterval = %v want %v", got, want)
	}
	if got := cfg.MaxConcurrentThreadPolls; got != 4 {
		t.Errorf("MaxConcurrentThreadPolls = %d want 4", got)
	}
	if cfg.Backoff.MaxBackoff.Duration != 5*time.Minute {
		t.Errorf("MaxBackoff = %v want 5m", cfg.Backoff.MaxBackoff.Duration)
	}
	if got, want := cfg.Watch.Channels[0].ID, "C123"; got != want {
		t.Errorf("Watch.Channels[0].ID = %s want %s", got, want)
	}
}

func TestLoadConfigCustomDurations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(path, []byte(`
auth:
  token_env: TOK
watch:
  channels:
    - id: C1
poll_interval: 7s
max_concurrent_thread_polls: 8
backoff:
  on_rate_limit: 10s
  on_error: 2s
  max_backoff: 1m
`), 0o644))

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.PollInterval.Duration != 7*time.Second {
		t.Errorf("PollInterval = %v", cfg.PollInterval.Duration)
	}
	if cfg.MaxConcurrentThreadPolls != 8 {
		t.Errorf("MaxConcurrentThreadPolls = %d", cfg.MaxConcurrentThreadPolls)
	}
}

func TestResolveTokenEnv(t *testing.T) {
	t.Setenv("MY_SLACK_TOKEN", "xoxb-hello")
	c := &Config{Auth: AuthConfig{TokenEnv: "MY_SLACK_TOKEN"}}
	got, err := c.ResolveToken()
	if err != nil {
		t.Fatalf("ResolveToken: %v", err)
	}
	if got != "xoxb-hello" {
		t.Errorf("token = %q", got)
	}
}

func TestResolveTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "tok")
	must(t, os.WriteFile(tokPath, []byte("xoxb-from-file\n"), 0o600))
	c := &Config{Auth: AuthConfig{TokenFile: tokPath}}
	got, err := c.ResolveToken()
	if err != nil {
		t.Fatalf("ResolveToken: %v", err)
	}
	if got != "xoxb-from-file" {
		t.Errorf("token = %q", got)
	}
}

func TestResolveTokenMissing(t *testing.T) {
	c := &Config{Auth: AuthConfig{}}
	if _, err := c.ResolveToken(); err == nil {
		t.Fatal("expected error for missing token")
	}
}

func TestConfigInvalidChannel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	must(t, os.WriteFile(path, []byte(`
auth:
  token_env: T
watch:
  channels:
    - id: ""
      reason: empty
`), 0o644))
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected validate error for empty channel id")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
