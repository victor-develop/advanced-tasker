// Package engine tracks per-state engine metadata (consecutive_idle,
// total_cycles, last_activated, mode) persisted at state/engine.json.
package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// State is the persisted engine snapshot.
type State struct {
	Mode             string    `json:"mode"`
	ConsecutiveIdle  int       `json:"consecutive_idle"`
	TotalCycles      int       `json:"total_cycles"`
	LastActivated    time.Time `json:"last_activated,omitempty"`
	LastTick         time.Time `json:"last_tick,omitempty"`
	LastCostUSD      float64   `json:"last_cost_usd,omitempty"`
	LastDurationMS   int64     `json:"last_duration_ms,omitempty"`
}

// Path returns the on-disk path of engine.json.
func Path(stateRoot string) string {
	return filepath.Join(stateRoot, "engine.json")
}

// Load reads engine.json. Missing → returns a zero-value State with
// Mode="hybrid".
func Load(stateRoot string) (*State, error) {
	b, err := os.ReadFile(Path(stateRoot))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &State{Mode: "hybrid"}, nil
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Mode == "" {
		s.Mode = "hybrid"
	}
	return &s, nil
}

// Save atomically writes engine.json.
func Save(stateRoot string, s *State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	p := Path(stateRoot)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// RecordTick updates State with one tick outcome. If idle, increments
// ConsecutiveIdle; otherwise resets it.
func RecordTick(s *State, idle bool, cost float64, durMS int64) {
	s.TotalCycles++
	s.LastTick = time.Now().UTC()
	s.LastCostUSD = cost
	s.LastDurationMS = durMS
	if idle {
		s.ConsecutiveIdle++
	} else {
		s.ConsecutiveIdle = 0
	}
}
