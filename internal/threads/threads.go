// Package threads reads/writes state/threads/<id>/{meta.json,rollup.md,raw/}.
package threads

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Meta is one thread's meta.json (design/02 §threads).
type Meta struct {
	ID             string    `json:"id"`
	Source         string    `json:"source"`
	URL            string    `json:"url,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
	LastEventAt    time.Time `json:"last_event_at,omitempty"`
	OwnerTask      string    `json:"owner_task,omitempty"`
	Participants   []string  `json:"participants,omitempty"`
	TrackingSince  time.Time `json:"tracking_since,omitempty"`
}

// Dir returns the on-disk directory for a thread.
func Dir(stateRoot, id string) string {
	return filepath.Join(stateRoot, "threads", id)
}

// MetaPath returns the path of meta.json for a thread.
func MetaPath(stateRoot, id string) string {
	return filepath.Join(Dir(stateRoot, id), "meta.json")
}

// RollupPath returns the path of rollup.md for a thread.
func RollupPath(stateRoot, id string) string {
	return filepath.Join(Dir(stateRoot, id), "rollup.md")
}

// DirtyPath returns the .dirty marker path.
func DirtyPath(stateRoot, id string) string {
	return filepath.Join(Dir(stateRoot, id), ".dirty")
}

// RawDir returns the raw events directory for a thread.
func RawDir(stateRoot, id string) string {
	return filepath.Join(Dir(stateRoot, id), "raw")
}

// ReadMeta parses meta.json.
func ReadMeta(stateRoot, id string) (*Meta, error) {
	b, err := os.ReadFile(MetaPath(stateRoot, id))
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse meta %s: %w", id, err)
	}
	return &m, nil
}

// WriteMeta atomically writes meta.json.
func WriteMeta(stateRoot string, m *Meta) error {
	if m.ID == "" {
		return errors.New("meta.id must be set")
	}
	if err := os.MkdirAll(Dir(stateRoot, m.ID), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	p := MetaPath(stateRoot, m.ID)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ReadRollup returns the rollup.md text.
func ReadRollup(stateRoot, id string) (string, error) {
	b, err := os.ReadFile(RollupPath(stateRoot, id))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// WriteRollup atomically writes rollup.md.
func WriteRollup(stateRoot, id, body string) error {
	if err := os.MkdirAll(Dir(stateRoot, id), 0o755); err != nil {
		return err
	}
	p := RollupPath(stateRoot, id)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// MarkDirty touches the .dirty marker.
func MarkDirty(stateRoot, id string) error {
	if err := os.MkdirAll(Dir(stateRoot, id), 0o755); err != nil {
		return err
	}
	f, err := os.Create(DirtyPath(stateRoot, id))
	if err != nil {
		return err
	}
	return f.Close()
}

// ClearDirty removes .dirty if present.
func ClearDirty(stateRoot, id string) error {
	if err := os.Remove(DirtyPath(stateRoot, id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// IsDirty reports whether the marker file exists.
func IsDirty(stateRoot, id string) bool {
	_, err := os.Stat(DirtyPath(stateRoot, id))
	return err == nil
}

// List returns the IDs of every tracked thread (directories under
// state/threads/), sorted.
func List(stateRoot string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(stateRoot, "threads"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// RawEvents returns the basenames in raw/ sorted.
func RawEvents(stateRoot, id string) ([]string, error) {
	entries, err := os.ReadDir(RawDir(stateRoot, id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}
