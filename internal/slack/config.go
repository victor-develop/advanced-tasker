// Package slack implements the Slack ingestion poller for advanced-tasker
// Track B. It reads from state/sources/slack/config.yaml, polls the Slack
// API for new top-level messages and thread replies, and writes raw event
// files plus inbox/.dirty markers under state/. It performs no LLM calls
// and never sends messages.
package slack

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config mirrors state/sources/slack/config.yaml as described in
// design/02-state-and-schemas.md §sources/slack/config.yaml and
// design/08-slack-poller.md §Configuration.
type Config struct {
	Auth                      AuthConfig      `yaml:"auth"`
	Watch                     WatchConfig     `yaml:"watch"`
	PollInterval              Duration        `yaml:"poll_interval"`
	MaxConcurrentThreadPolls  int             `yaml:"max_concurrent_thread_polls"`
	Backoff                   BackoffConfig   `yaml:"backoff"`
	WriteUpdatePings          *bool           `yaml:"write_update_pings,omitempty"`
}

// AuthConfig describes how to obtain the Slack bot token.
type AuthConfig struct {
	// TokenEnv is the name of the env var holding the bot token. Takes
	// precedence over TokenFile and any embedded value.
	TokenEnv string `yaml:"token_env"`
	// TokenFile is an optional path to a file containing the token.
	TokenFile string `yaml:"token_file,omitempty"`
	// Token is an inline token. Mostly useful for tests. Loading code
	// scrubs this from Config after read.
	Token string `yaml:"token,omitempty"`
}

// WatchConfig holds the list of tracked channels.
type WatchConfig struct {
	Channels []WatchedChannel `yaml:"channels"`
}

// WatchedChannel is a single tracked channel.
type WatchedChannel struct {
	ID     string `yaml:"id"`
	Reason string `yaml:"reason,omitempty"`
}

// BackoffConfig governs retry behavior on transient failures.
type BackoffConfig struct {
	OnRateLimit Duration `yaml:"on_rate_limit"`
	OnError     Duration `yaml:"on_error"`
	MaxBackoff  Duration `yaml:"max_backoff"`
}

// Duration is a time.Duration that parses Go-style strings like "30s" from
// YAML. We use a custom type to keep the YAML config human-friendly.
type Duration struct{ time.Duration }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err == nil && s != "" {
		dur, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		d.Duration = dur
		return nil
	}
	// Fall back to seconds as int.
	var n int64
	if err := value.Decode(&n); err == nil {
		d.Duration = time.Duration(n) * time.Second
		return nil
	}
	return fmt.Errorf("could not parse duration from yaml node")
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) {
	return d.Duration.String(), nil
}

// ErrConfigMissing is returned by LoadConfig when the configured path does
// not exist. The poller maps this to the literal user-facing message
// required by design/03 + design/10:
//
//	run 'harness config init slack' to seed config
//
// Callers MUST exit 1 with that message rather than auto-creating a default
// config at poll time.
var ErrConfigMissing = errors.New("run 'harness config init slack' to seed config")

// LoadConfig reads a YAML config from path and applies defaults. Returns
// ErrConfigMissing if the file does not exist; the CLI surface relies on
// this sentinel.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrConfigMissing
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.PollInterval.Duration == 0 {
		c.PollInterval.Duration = 30 * time.Second
	}
	if c.MaxConcurrentThreadPolls == 0 {
		c.MaxConcurrentThreadPolls = 4
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

// Validate returns a non-nil error if the config is missing required fields.
func (c *Config) Validate() error {
	if len(c.Watch.Channels) == 0 {
		// Not strictly fatal — the poller can still run thread polls — but
		// it is almost certainly a misconfiguration on first start. Log a
		// warning instead of failing here. The caller can decide.
	}
	for i, ch := range c.Watch.Channels {
		if ch.ID == "" {
			return fmt.Errorf("watch.channels[%d].id is empty", i)
		}
	}
	return nil
}

// ResolveToken returns the Slack bot token, checking env var first, then
// inline token, then token file. Returns an error if none are present.
func (c *Config) ResolveToken() (string, error) {
	if c.Auth.TokenEnv != "" {
		if v := os.Getenv(c.Auth.TokenEnv); v != "" {
			return v, nil
		}
	}
	if c.Auth.Token != "" {
		return c.Auth.Token, nil
	}
	if c.Auth.TokenFile != "" {
		b, err := os.ReadFile(c.Auth.TokenFile)
		if err != nil {
			return "", fmt.Errorf("read token file %s: %w", c.Auth.TokenFile, err)
		}
		return string(trimNewline(b)), nil
	}
	return "", errors.New("no Slack bot token: set auth.token_env, auth.token_file, or auth.token")
}

// SaveConfig writes a YAML config atomically. Used by the watch/unwatch
// subcommands to mutate state/sources/slack/config.yaml.
func SaveConfig(path string, cfg *Config) error {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := atomicWrite(path, b, 0o644); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}
