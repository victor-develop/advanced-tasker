package ids

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNextTaskID_Empty(t *testing.T) {
	dir := t.TempDir()
	id, err := NextTaskID(filepath.Join(dir, "tasks"))
	if err != nil {
		t.Fatalf("NextTaskID on missing dir: %v", err)
	}
	if id != "T-1" {
		t.Errorf("expected T-1, got %q", id)
	}
}

func TestNextTaskID_Monotonic(t *testing.T) {
	dir := t.TempDir()
	tasks := filepath.Join(dir, "tasks")
	for _, n := range []string{"T-1", "T-2", "T-5", "not-a-task"} {
		if err := os.MkdirAll(filepath.Join(tasks, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	id, err := NextTaskID(tasks)
	if err != nil {
		t.Fatalf("NextTaskID: %v", err)
	}
	if id != "T-6" {
		t.Errorf("expected T-6, got %q", id)
	}
}

func TestNormalizeTaskID(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"T-1", "T-1", false},
		{"t-12", "T-12", false},
		{"12", "T-12", false},
		{"", "", true},
		{"foo", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeTaskID(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeTaskID(%q) wanted error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeTaskID(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeTaskID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidIDs(t *testing.T) {
	if !ValidTaskID("T-1") {
		t.Error("T-1 should be valid")
	}
	if ValidTaskID("X-1") {
		t.Error("X-1 should not be valid")
	}
	if !ValidJobID("J-abc123") {
		t.Error("J-abc123 should be valid")
	}
	if ValidJobID("J-ABC") {
		t.Error("upper-case J id should not be valid")
	}
}
