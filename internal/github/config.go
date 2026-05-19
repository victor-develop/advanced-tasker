// Package github implements the Track C GitHub PR poller.
//
// See design/09-github-poller.md for the spec.  The package is split into
// a config loader, cursor IO, a thin client wrapper, a filesystem writer,
// and an orchestrator that ties everything together.
package github

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrConfigMissing is returned by LoadConfig when the config file does not
// exist on disk.  The cmd/github-poller main and the C6 lifecycle CLIs
// surface this as the literal operator-facing message:
//
//	run 'harness config init github' to seed config
//
// See design/03 §"harness config init <source>" and design/10
// §"What `harness init` does".
var ErrConfigMissing = errors.New("run 'harness config init github' to seed config")

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
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrConfigMissing
		}
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

// LoadConfigRaw returns the raw YAML document and Config; used by the C6
// lifecycle CLIs (watch/unwatch) which need to round-trip the file while
// preserving any operator-added fields.  Unlike LoadConfig it does NOT
// apply defaults or validate `watch.repos` non-empty — those are
// invariants for the poller, not for the lifecycle CLIs.
func LoadConfigRaw(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrConfigMissing
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return &root, nil
}

// SaveConfigRaw atomically writes a yaml.Node tree back to disk.
func SaveConfigRaw(path string, root *yaml.Node) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	data, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// WatchRepos returns the watch.repos list from the YAML tree, or nil if
// absent.  Mutating the returned slice does not mutate the tree.
func WatchRepos(root *yaml.Node) []string {
	seq := findRepoSeq(root)
	if seq == nil {
		return nil
	}
	out := make([]string, 0, len(seq.Content))
	for _, n := range seq.Content {
		if n.Kind == yaml.ScalarNode {
			out = append(out, n.Value)
		}
	}
	return out
}

// SetWatchRepos replaces the watch.repos list in-place, creating the
// `watch` mapping and `repos` sequence node if missing.
func SetWatchRepos(root *yaml.Node, repos []string) {
	if root == nil || len(root.Content) == 0 {
		// Empty doc: build the minimal scaffold.
		root.Kind = yaml.DocumentNode
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	doc := root.Content[0]
	watch := findOrCreateMapEntry(doc, "watch")
	if watch.Kind == 0 {
		watch.Kind = yaml.MappingNode
	}
	repoNode := findOrCreateMapEntry(watch, "repos")
	repoNode.Kind = yaml.SequenceNode
	repoNode.Tag = "!!seq"
	repoNode.Content = repoNode.Content[:0]
	for _, r := range repos {
		repoNode.Content = append(repoNode.Content, &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   "!!str",
			Value: r,
		})
	}
}

func findRepoSeq(root *yaml.Node) *yaml.Node {
	if root == nil || len(root.Content) == 0 {
		return nil
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "watch" {
			watch := doc.Content[i+1]
			if watch.Kind != yaml.MappingNode {
				return nil
			}
			for j := 0; j+1 < len(watch.Content); j += 2 {
				if watch.Content[j].Value == "repos" && watch.Content[j+1].Kind == yaml.SequenceNode {
					return watch.Content[j+1]
				}
			}
		}
	}
	return nil
}

// findOrCreateMapEntry locates the value node for `key` inside the given
// mapping node, creating an empty mapping value if absent.
func findOrCreateMapEntry(parent *yaml.Node, key string) *yaml.Node {
	if parent.Kind != yaml.MappingNode {
		parent.Kind = yaml.MappingNode
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			return parent.Content[i+1]
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	v := &yaml.Node{Kind: yaml.MappingNode}
	parent.Content = append(parent.Content, k, v)
	return v
}
