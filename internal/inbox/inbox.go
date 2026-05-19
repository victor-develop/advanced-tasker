// Package inbox enumerates and reads the inbox queue buckets.
// The buckets are: new, updates, human, agent-reports, anomalies.
// Items are JSON files (per design/02 §inbox).
package inbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Bucket enumerates the inbox subdirectories.
type Bucket string

const (
	BucketNew          Bucket = "new"
	BucketUpdates      Bucket = "updates"
	BucketHuman        Bucket = "human"
	BucketAgentReports Bucket = "agent-reports"
	BucketAnomalies    Bucket = "anomalies"
)

// AllBuckets is the canonical bucket list.
var AllBuckets = []Bucket{BucketNew, BucketUpdates, BucketHuman, BucketAgentReports, BucketAnomalies}

// Item is the canonical inbox JSON shape (design/02 §inbox/<bucket>).
type Item struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	Kind       string    `json:"kind"`
	ReceivedAt time.Time `json:"received_at"`
	Summary    string    `json:"summary"`
	Ref        any       `json:"ref,omitempty"`
	RawPath    string    `json:"raw_path,omitempty"`
}

// Dir returns the on-disk bucket directory.
func Dir(stateRoot string, b Bucket) string {
	return filepath.Join(stateRoot, "inbox", string(b))
}

// PathFor returns the path of an inbox item by id (.json suffix).
func PathFor(stateRoot string, b Bucket, id string) string {
	if !strings.HasSuffix(id, ".json") && !strings.HasSuffix(id, ".md") {
		id = id + ".json"
	}
	return filepath.Join(Dir(stateRoot, b), id)
}

// List returns the basenames in the bucket directory, sorted.
func List(stateRoot string, b Bucket) ([]string, error) {
	entries, err := os.ReadDir(Dir(stateRoot, b))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// Find searches every bucket for the given inbox id (basename without
// extension, or basename with extension).
func Find(stateRoot, id string) (Bucket, string, error) {
	stripped := strings.TrimSuffix(strings.TrimSuffix(id, ".json"), ".md")
	for _, b := range AllBuckets {
		entries, err := os.ReadDir(Dir(stateRoot, b))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if name == id || strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".md") == stripped {
				return b, filepath.Join(Dir(stateRoot, b), name), nil
			}
		}
	}
	return "", "", ErrNotFound
}

// ErrNotFound signals a missing inbox id.
var ErrNotFound = errors.New("inbox item not found")

// ReadJSON parses an inbox JSON file into Item.
func ReadJSON(path string) (*Item, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var it Item
	if err := json.Unmarshal(b, &it); err != nil {
		return nil, fmt.Errorf("parse inbox %s: %w", path, err)
	}
	return &it, nil
}

// WriteJSON writes an Item as JSON.
func WriteJSON(path string, it *Item) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(it, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// AppendAnomaly writes a JSON record into inbox/anomalies/<id>.json
// describing a CLI/validator rejection.
func AppendAnomaly(stateRoot string, ref, message string) (string, error) {
	id := fmt.Sprintf("anomaly-%d", time.Now().UTC().UnixNano())
	it := &Item{
		ID:         id,
		Source:     "harness",
		Kind:       "anomaly",
		ReceivedAt: time.Now().UTC(),
		Summary:    fmt.Sprintf("%s: %s", ref, message),
	}
	p := PathFor(stateRoot, BucketAnomalies, id)
	if err := WriteJSON(p, it); err != nil {
		return "", err
	}
	return p, nil
}
