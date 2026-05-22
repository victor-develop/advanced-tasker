// Package tick implements the commander tick lifecycle: claim
// commander lock, write tick-log, release. See design/03 §"Tick and
// dashboard" and design/04.
package tick

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// LockPath returns the on-disk path for the commander lease file.
func LockPath(stateRoot string) string {
	return filepath.Join(stateRoot, "commander.lock")
}

// Lease records who holds the commander lock and until when.
type Lease struct {
	HeldBy     string    `json:"held_by"`
	PID        int       `json:"pid,omitempty"`
	ClaimedAt  time.Time `json:"claimed_at"`
	LeaseUntil time.Time `json:"lease_until"`
}

// Claim atomically writes a Lease iff no current lock is held (or the
// existing lock is stale). Returns ErrContended on conflict.
func Claim(stateRoot, agent string, pid int, ttl time.Duration) (*Lease, error) {
	p := LockPath(stateRoot)
	now := time.Now().UTC()
	existing, err := readLease(p)
	if err == nil && existing != nil {
		if existing.LeaseUntil.After(now) {
			if existing.PID != 0 && !pidAlive(existing.PID) {
				// Stale lock — proceed to reclaim.
			} else {
				return nil, ErrContended
			}
		}
	}
	lease := &Lease{
		HeldBy:     agent,
		PID:        pid,
		ClaimedAt:  now,
		LeaseUntil: now.Add(ttl),
	}
	if err := writeLease(p, lease); err != nil {
		return nil, err
	}
	return lease, nil
}

// Release deletes the commander lease.
func Release(stateRoot string) error {
	if err := os.Remove(LockPath(stateRoot)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// CurrentLease returns the currently held lease (if any).
func CurrentLease(stateRoot string) (*Lease, error) {
	return readLease(LockPath(stateRoot))
}

// ErrContended is returned by Claim when a live lease already exists.
var ErrContended = errors.New("commander already claimed")

func readLease(p string) (*Lease, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var l Lease
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

func writeLease(p string, l *Lease) error {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// pidAlive returns true iff a process with that PID still exists (used
// for PID-aware stale-lock detection per design/03).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

// LogFileForNow returns the tick-log path for the current UTC instant
// (one file per tick).
func LogFileForNow(stateRoot string) (string, time.Time) {
	now := time.Now().UTC()
	id := now.Format("2006-01-02T15-04-05Z")
	return filepath.Join(stateRoot, "tick-log", id+".md"), now
}

// CurrentLogPath returns the most recent file under tick-log/ if any.
func CurrentLogPath(stateRoot string) (string, bool) {
	entries, err := os.ReadDir(filepath.Join(stateRoot, "tick-log"))
	if err != nil {
		return "", false
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Strings(names)
	return filepath.Join(stateRoot, "tick-log", names[len(names)-1]), true
}

// WriteFrontmatter starts a new tick-log file with frontmatter.
func WriteFrontmatter(path string, tickID string, agent string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf("---\ntick_id: %s\nagent: %s\n---\n\n", tickID, agent)
	return os.WriteFile(path, []byte(body), 0o644)
}

// AppendLog appends a paragraph to a tick-log file.
func AppendLog(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strings.TrimRight(text, "\n") + "\n")
	return err
}

// FinalizeLog appends summary frontmatter footer (duration/cost/idle).
func FinalizeLog(path string, durationMS int64, costUSD float64, idle bool, consecutiveIdle int, summary string) error {
	footer := fmt.Sprintf(
		"\n---\nduration_ms: %d\ncost_usd: %g\nidle: %v\nconsecutive_idle: %d\n---\n\n%s\n",
		durationMS, costUSD, idle, consecutiveIdle, strings.TrimSpace(summary),
	)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(footer)
	return err
}
