package engine

import (
	"testing"
)

// TestRecordTick_IdleIncrement asserts the idle/active counter logic
// per design/03 §"engine.json" (idle increments, non-idle resets).
func TestRecordTick_IdleIncrement(t *testing.T) {
	s := &State{Mode: "hybrid"}
	RecordTick(s, true, 0.01, 100)
	if s.ConsecutiveIdle != 1 {
		t.Errorf("after one idle tick, want consecutive_idle=1 got %d", s.ConsecutiveIdle)
	}
	RecordTick(s, true, 0.01, 100)
	if s.ConsecutiveIdle != 2 {
		t.Errorf("after two idle ticks, want consecutive_idle=2 got %d", s.ConsecutiveIdle)
	}
	RecordTick(s, false, 0.05, 200)
	if s.ConsecutiveIdle != 0 {
		t.Errorf("active tick must reset consecutive_idle to 0, got %d", s.ConsecutiveIdle)
	}
	if s.TotalCycles != 3 {
		t.Errorf("want TotalCycles=3, got %d", s.TotalCycles)
	}
	if s.LastCostUSD != 0.05 {
		t.Errorf("want LastCostUSD=0.05, got %f", s.LastCostUSD)
	}
}

// TestLoadSaveRoundtrip persists and reloads State.
func TestLoadSaveRoundtrip(t *testing.T) {
	root := t.TempDir()
	original := &State{Mode: "autopilot", ConsecutiveIdle: 5, TotalCycles: 17}
	if err := Save(root, original); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Mode != "autopilot" || got.ConsecutiveIdle != 5 || got.TotalCycles != 17 {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

// TestLoadMissingReturnsDefault — Load on a fresh dir returns the
// default hybrid state, not an error.
func TestLoadMissingReturnsDefault(t *testing.T) {
	root := t.TempDir()
	got, err := Load(root)
	if err != nil {
		t.Fatalf("load on empty dir should not error: %v", err)
	}
	if got.Mode != "hybrid" {
		t.Errorf("want Mode=hybrid for missing state, got %q", got.Mode)
	}
}
