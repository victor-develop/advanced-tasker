// Package github implements the Track C GitHub PR poller.
//
// See design/09-github-poller.md for the spec.  The package is split into
// a config loader, cursor IO, a thin client wrapper, a filesystem writer,
// and an orchestrator that ties everything together.
package github

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config mirrors state/sources/github/config.yaml from design/09 §Configuration.
type Config struct {
	Auth struct {
		TokenEnv string `yaml:"token_env"`
		Type     string `yaml:"type"` // pat | app (only pat supported in MVP)
	} `yaml:"auth"`
	Watch struct {
		Repos []string `yaml:"repos"`
	} `yaml:"watch"`
	PollInterval   Duration `yaml:"poll_interval"`
	NewPRLookback  Duration `yaml:"new_pr_lookback"`
	MaxConcurrent  int      `yaml:"max_concurrent_pr_polls"`
	Backoff        struct {
		OnRateLimit Duration `yaml:"on_rate_limit"`
		OnError     Duration `yaml:"on_error"`
		MaxBackoff  Duration `yaml:"max_backoff"`
	} `yaml:"backoff"`
}

// Duration is a YAML-friendly wrapper for time.Duration that accepts the
// human strings used in the design docs (e.g. "60s", "7d", "5m").
type Duration struct{ time.Duration }

// UnmarshalYAML parses durations including the "d" (days) suffix that
// time.ParseDuration does not handle natively.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	s := strings.TrimSpace(value.Value)
	if s == "" {
		return nil
	}
	if strings.HasSuffix(s, "d") {
		days, err := time.ParseDuration(strings.TrimSuffix(s, "d") + "h")
		if err != nil {
			return fmt.Errorf("invalid days duration %q: %w", s, err)
		}
		d.Duration = days * 24
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// RepoRef splits an "owner/repo" string.
type RepoRef struct {
	Owner string
	Repo  string
}

func (r RepoRef) String() string { return r.Owner + "/" + r.Repo }

// ParseRepo splits "owner/repo".
func ParseRepo(s string) (RepoRef, error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return RepoRef{}, fmt.Errorf("invalid repo %q: expected owner/repo", s)
	}
	return RepoRef{Owner: parts[0], Repo: parts[1]}, nil
}

// LoadConfig reads a YAML config file and applies defaults.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Auth.Type == "" {
		c.Auth.Type = "pat"
	}
	if c.Auth.TokenEnv == "" {
		c.Auth.TokenEnv = "GITHUB_TOKEN"
	}
	if c.PollInterval.Duration == 0 {
		c.PollInterval.Duration = 60 * time.Second
	}
	if c.NewPRLookback.Duration == 0 {
		c.NewPRLookback.Duration = 7 * 24 * time.Hour
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 4
	}
	if c.Backoff.OnRateLimit.Duration == 0 {
		c.Backoff.OnRateLimit.Duration = 60 * time.Second
	}
	if c.Backoff.OnError.Duration == 0 {
		c.Backoff.OnError.Duration = 5 * time.Second
	}
	if c.Backoff.MaxBackoff.Duration == 0 {
		c.Backoff.MaxBackoff.Duration = 5 * time.Minute
	}
}

func (c *Config) validate() error {
	if c.Auth.Type != "pat" {
		return fmt.Errorf("auth.type %q unsupported (only %q in MVP)", c.Auth.Type, "pat")
	}
	if len(c.Watch.Repos) == 0 {
		return fmt.Errorf("watch.repos is empty")
	}
	for _, r := range c.Watch.Repos {
		if _, err := ParseRepo(r); err != nil {
			return err
		}
	}
	return nil
}

// Repos returns parsed RepoRefs.
func (c *Config) Repos() []RepoRef {
	out := make([]RepoRef, 0, len(c.Watch.Repos))
	for _, r := range c.Watch.Repos {
		parsed, _ := ParseRepo(r)
		out = append(out, parsed)
	}
	return out
}

// Token reads the auth token from the configured env var.
func (c *Config) Token() string { return os.Getenv(c.Auth.TokenEnv) }

// DefaultConfigPath returns the path to the config file under a state root.
func DefaultConfigPath(stateRoot string) string {
	return filepath.Join(stateRoot, "sources", "github", "config.yaml")
}
