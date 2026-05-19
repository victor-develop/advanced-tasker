// Package sources owns state/sources/<src>/config.yaml stubs and the
// watch list mutators (`harness watch slack-channel ...`).
//
// Per design/03 + design/10, the source config is opt-in: it is NOT
// created by `harness init`. Pollers must error helpfully if the file
// is absent. `harness config init <source>` writes a documented stub
// idempotently.
package sources

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Source enumerates the supported sources.
type Source string

const (
	SourceSlack  Source = "slack"
	SourceGitHub Source = "github"
)

// ValidSources is the canonical list.
var ValidSources = []Source{SourceSlack, SourceGitHub}

// Dir returns state/sources/<src>/.
func Dir(stateRoot string, src Source) string {
	return filepath.Join(stateRoot, "sources", string(src))
}

// ConfigPath returns state/sources/<src>/config.yaml.
func ConfigPath(stateRoot string, src Source) string {
	return filepath.Join(Dir(stateRoot, src), "config.yaml")
}

// SlackStub is the documented stub written by `harness config init slack`.
// Matches design/08 §config.
const SlackStub = `# state/sources/slack/config.yaml — Slack poller config.
# Seeded by ` + "`harness config init slack`" + `. Edit ` + "`watch.channels`" + `
# directly OR use ` + "`harness watch slack-channel <id>`" + `.
auth:
  token_env: SLACK_BOT_TOKEN
watch:
  channels: []
poll_interval: 30s
`

// GitHubStub is the documented stub written by `harness config init github`.
// Matches design/09 §config.
const GitHubStub = `# state/sources/github/config.yaml — GitHub poller config.
# Seeded by ` + "`harness config init github`" + `. Edit ` + "`watch.repos`" + `
# directly OR use ` + "`harness watch github-repo <owner/repo>`" + `.
auth:
  token_env: GITHUB_TOKEN
watch:
  repos: []
poll_interval: 60s
`

// ParseSource converts a string into a Source enum value.
func ParseSource(s string) (Source, error) {
	for _, v := range ValidSources {
		if string(v) == s {
			return v, nil
		}
	}
	return "", fmt.Errorf("unknown source %q (want one of %v)", s, ValidSources)
}

// StubFor returns the documented stub body for a source.
func StubFor(src Source) string {
	switch src {
	case SourceSlack:
		return SlackStub
	case SourceGitHub:
		return GitHubStub
	}
	return ""
}

// Init writes the source's config stub idempotently. If the file
// already exists, returns (false, nil). On fresh write, returns
// (true, nil).
func Init(stateRoot string, src Source) (bool, error) {
	p := ConfigPath(stateRoot, src)
	if _, err := os.Stat(p); err == nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(p, []byte(StubFor(src)), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// LoadRaw reads state/sources/<src>/config.yaml into a generic tree.
// Missing file → ErrConfigMissing.
func LoadRaw(stateRoot string, src Source) (map[string]any, error) {
	b, err := os.ReadFile(ConfigPath(stateRoot, src))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrConfigMissing
		}
		return nil, err
	}
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse %s config: %w", src, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return raw, nil
}

// SaveRaw atomically writes the raw tree back to the source's
// config.yaml.
func SaveRaw(stateRoot string, src Source, raw map[string]any) error {
	p := ConfigPath(stateRoot, src)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ErrConfigMissing is returned when state/sources/<src>/config.yaml is
// absent. Callers should surface a clear message telling the operator
// to run `harness config init <src>`.
var ErrConfigMissing = errors.New("source config missing — run `harness config init <source>`")

// WatchChannel appends a Slack channel ID to watch.channels.
// Idempotent. The "reason" is preserved as a sibling key on the entry.
func WatchChannel(stateRoot, channelID, reason string) error {
	raw, err := LoadRaw(stateRoot, SourceSlack)
	if err != nil {
		return err
	}
	watch, _ := raw["watch"].(map[string]any)
	if watch == nil {
		watch = map[string]any{}
		raw["watch"] = watch
	}
	channels, _ := watch["channels"].([]any)
	for _, c := range channels {
		if m, ok := c.(map[string]any); ok {
			if id, _ := m["id"].(string); id == channelID {
				return nil
			}
		}
		if s, ok := c.(string); ok && s == channelID {
			return nil
		}
	}
	entry := map[string]any{"id": channelID}
	if reason != "" {
		entry["reason"] = reason
	}
	channels = append(channels, entry)
	watch["channels"] = channels
	return SaveRaw(stateRoot, SourceSlack, raw)
}

// UnwatchChannel removes a Slack channel ID from watch.channels.
func UnwatchChannel(stateRoot, channelID string) error {
	raw, err := LoadRaw(stateRoot, SourceSlack)
	if err != nil {
		return err
	}
	watch, _ := raw["watch"].(map[string]any)
	if watch == nil {
		return nil
	}
	channels, _ := watch["channels"].([]any)
	kept := channels[:0]
	for _, c := range channels {
		if m, ok := c.(map[string]any); ok {
			if id, _ := m["id"].(string); id == channelID {
				continue
			}
		}
		if s, ok := c.(string); ok && s == channelID {
			continue
		}
		kept = append(kept, c)
	}
	watch["channels"] = kept
	return SaveRaw(stateRoot, SourceSlack, raw)
}

// WatchRepo appends a "owner/repo" string to watch.repos.
func WatchRepo(stateRoot, repo string) error {
	if !strings.Contains(repo, "/") {
		return fmt.Errorf("repo %q must be in owner/repo form", repo)
	}
	raw, err := LoadRaw(stateRoot, SourceGitHub)
	if err != nil {
		return err
	}
	watch, _ := raw["watch"].(map[string]any)
	if watch == nil {
		watch = map[string]any{}
		raw["watch"] = watch
	}
	repos, _ := watch["repos"].([]any)
	for _, r := range repos {
		if s, _ := r.(string); s == repo {
			return nil
		}
	}
	repos = append(repos, repo)
	watch["repos"] = repos
	return SaveRaw(stateRoot, SourceGitHub, raw)
}

// UnwatchRepo removes a repo from watch.repos.
func UnwatchRepo(stateRoot, repo string) error {
	raw, err := LoadRaw(stateRoot, SourceGitHub)
	if err != nil {
		return err
	}
	watch, _ := raw["watch"].(map[string]any)
	if watch == nil {
		return nil
	}
	repos, _ := watch["repos"].([]any)
	kept := repos[:0]
	for _, r := range repos {
		if s, _ := r.(string); s == repo {
			continue
		}
		kept = append(kept, r)
	}
	watch["repos"] = kept
	return SaveRaw(stateRoot, SourceGitHub, raw)
}

// Summary returns a sorted summary of watched items across all sources.
func Summary(stateRoot string) ([]string, error) {
	var out []string
	for _, src := range ValidSources {
		raw, err := LoadRaw(stateRoot, src)
		if err != nil {
			if errors.Is(err, ErrConfigMissing) {
				continue
			}
			return nil, err
		}
		watch, _ := raw["watch"].(map[string]any)
		if watch == nil {
			continue
		}
		switch src {
		case SourceSlack:
			channels, _ := watch["channels"].([]any)
			for _, c := range channels {
				if m, ok := c.(map[string]any); ok {
					if id, _ := m["id"].(string); id != "" {
						out = append(out, fmt.Sprintf("slack/%s", id))
					}
				}
				if s, ok := c.(string); ok {
					out = append(out, fmt.Sprintf("slack/%s", s))
				}
			}
		case SourceGitHub:
			repos, _ := watch["repos"].([]any)
			for _, r := range repos {
				if s, _ := r.(string); s != "" {
					out = append(out, fmt.Sprintf("github/%s", s))
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
