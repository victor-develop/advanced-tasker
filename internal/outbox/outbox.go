// Package outbox owns the on-disk outbox queue. Items move:
//   outbox/awaiting-human/<id>.yaml  (risk normal|high)
//   outbox/pending/<id>.yaml         (risk low OR approved)
//   outbox/sent/<id>.yaml            (sender daemon succeeded)
//   outbox/failed/<id>.yaml          (provider error after retries)
//
// Only outbox/sent/ is git-tracked; the rest are gitignored queues.
// See design/07.
package outbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// State enumerates the queue directories.
type State string

const (
	StatePending        State = "pending"
	StateAwaitingHuman  State = "awaiting-human"
	StateSent           State = "sent"
	StateFailed         State = "failed"
)

// AllStates lists each possible outbox state.
var AllStates = []State{StatePending, StateAwaitingHuman, StateSent, StateFailed}

// Risk classification per design/07.
type Risk string

const (
	RiskLow    Risk = "low"
	RiskNormal Risk = "normal"
	RiskHigh   Risk = "high"
)

// ValidRisks for input validation.
var ValidRisks = []Risk{RiskLow, RiskNormal, RiskHigh}

// Item is one outbox YAML file.
type Item struct {
	ID            string    `yaml:"id"`
	CreatedAt     time.Time `yaml:"created_at"`
	CreatedBy     string    `yaml:"created_by"`
	To            string    `yaml:"to"`
	Ref           Ref       `yaml:"ref"`
	BodyFile      string    `yaml:"body_file"`
	Body          string    `yaml:"body,omitempty"`
	Risk          Risk      `yaml:"risk"`
	RevokeWindow  string    `yaml:"revoke_window,omitempty"`

	// Sender-populated:
	SentAt          time.Time      `yaml:"sent_at,omitempty"`
	SenderResponse  map[string]any `yaml:"sender_response,omitempty"`
	RetryCount      int            `yaml:"retry_count,omitempty"`
	LastError       string         `yaml:"last_error,omitempty"`
}

// Ref locates the message in the upstream provider.
type Ref struct {
	Thread    string `yaml:"thread"`
	InReplyTo string `yaml:"in_reply_to,omitempty"`
}

// PathFor returns the on-disk YAML path for an outbox item.
func PathFor(stateRoot string, st State, id string) string {
	return filepath.Join(stateRoot, "outbox", string(st), id+".yaml")
}

// Dir returns the queue dir for the given state.
func Dir(stateRoot string, st State) string {
	return filepath.Join(stateRoot, "outbox", string(st))
}

// Write atomically writes the YAML.
func Write(dst string, it *Item) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(it)
	if err != nil {
		return fmt.Errorf("marshal outbox: %w", err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Read parses an outbox YAML.
func Read(src string) (*Item, error) {
	b, err := os.ReadFile(src)
	if err != nil {
		return nil, err
	}
	var it Item
	if err := yaml.Unmarshal(b, &it); err != nil {
		return nil, fmt.Errorf("parse outbox %s: %w", src, err)
	}
	return &it, nil
}

// Find searches every state dir for the given id.
func Find(stateRoot, id string) (State, string, error) {
	for _, st := range AllStates {
		p := PathFor(stateRoot, st, id)
		if _, err := os.Stat(p); err == nil {
			return st, p, nil
		}
	}
	return "", "", ErrNotFound
}

// ErrNotFound is returned by Find when no queue has the id.
var ErrNotFound = errors.New("outbox item not found")

// Move atomically moves an outbox item from one state to another.
func Move(stateRoot string, it *Item, from, to State) (string, error) {
	src := PathFor(stateRoot, from, it.ID)
	dst := PathFor(stateRoot, to, it.ID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := Write(dst, it); err != nil {
		return "", err
	}
	if src != dst {
		if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("remove old outbox file: %w", err)
		}
	}
	return dst, nil
}

// ListByState returns the IDs in a state dir.
func ListByState(stateRoot string, st State) ([]string, error) {
	entries, err := os.ReadDir(Dir(stateRoot, st))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(ids)
	return ids, nil
}

// IsValidRisk reports whether the string maps to a known risk.
func IsValidRisk(r string) bool {
	for _, v := range ValidRisks {
		if string(v) == r {
			return true
		}
	}
	return false
}

// RankRisk returns 0/1/2 for low/normal/high so callers can compare
// risks numerically (used by the "commander cannot downgrade" guard).
func RankRisk(r Risk) int {
	switch r {
	case RiskLow:
		return 0
	case RiskNormal:
		return 1
	case RiskHigh:
		return 2
	}
	return -1
}
