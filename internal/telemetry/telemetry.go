// Package telemetry records per-tick/worker cost+duration into
// state/telemetry/summary.log and exposes ls/show/cost queries used by
// `harness telemetry ...`.
package telemetry

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SummaryPath returns the state/telemetry/summary.log path.
func SummaryPath(stateRoot string) string {
	return filepath.Join(stateRoot, "telemetry", "summary.log")
}

// AppendSummary appends one line per the design/10 format:
//
//	<iso> <kind>  cost=$<f>  dur=<ms>ms  err=<bool>  session=<id>
func AppendSummary(stateRoot, kind string, cost float64, durMS int64, isErr bool, session string) error {
	p := SummaryPath(stateRoot)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line := fmt.Sprintf(
		"%s %s  cost=$%.4f  dur=%dms  err=%v  session=%s\n",
		time.Now().UTC().Format(time.RFC3339), kind, cost, durMS, isErr, session,
	)
	_, err = f.WriteString(line)
	return err
}

// List returns the basenames under state/telemetry/{ticks,workers,audits}/.
func List(stateRoot string) ([]string, error) {
	var out []string
	for _, sub := range []string{"ticks", "workers", "audits"} {
		entries, err := os.ReadDir(filepath.Join(stateRoot, "telemetry", sub))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			out = append(out, sub+"/"+e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Show returns the body of one telemetry file (path is the value from
// List, e.g. "ticks/2026-05-19T....jsonl").
func Show(stateRoot, rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(stateRoot, "telemetry", rel))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CostSince returns the total cost recorded in summary.log on or after
// since. Pass zero time to mean "all time".
func CostSince(stateRoot string, since time.Time) (float64, error) {
	f, err := os.Open(SummaryPath(stateRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var total float64
	for sc.Scan() {
		line := sc.Text()
		// Expect: "<iso> <kind>  cost=$<f>  dur=..."
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		ts, err := time.Parse(time.RFC3339, parts[0])
		if err != nil {
			continue
		}
		if !since.IsZero() && ts.Before(since) {
			continue
		}
		for _, p := range parts {
			if !strings.HasPrefix(p, "cost=$") {
				continue
			}
			var v float64
			if _, err := fmt.Sscanf(strings.TrimPrefix(p, "cost=$"), "%f", &v); err == nil {
				total += v
			}
		}
	}
	return total, nil
}

// Rotate moves files older than olderThan out of telemetry/* into
// telemetry/archive/. Empty Archive policy: just delete (simpler v1).
func Rotate(stateRoot string, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	moved := 0
	for _, sub := range []string{"ticks", "workers", "audits"} {
		dir := filepath.Join(stateRoot, "telemetry", sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
			_ = os.Remove(filepath.Join(dir, e.Name()))
			moved++
		}
	}
	return moved, nil
}
