// Package daemon contains the autopilot daemons: rollup-updater,
// worker-runner, outbox-sender, commander-scheduler, audit-daemon.
//
// Each daemon's Run function blocks until ctx is cancelled and is
// designed to be safe to call as a goroutine. They share a common
// pattern: poll-or-tick, do bounded work, sleep, repeat.
package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/victor-develop/advanced-tasker/internal/llm"
)

// Bus is a tiny coordination context shared by daemons (state root,
// driver, sink for stderr-style messages).
type Bus struct {
	StateRoot string
	Driver    llm.Driver
	DryRunOutbox bool

	mu  sync.Mutex
	out []string
}

// NewBus constructs a Bus.
func NewBus(stateRoot string, driver llm.Driver) *Bus {
	return &Bus{StateRoot: stateRoot, Driver: driver}
}

// Log records a one-liner for later inspection.
func (b *Bus) Log(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.out = append(b.out, time.Now().UTC().Format(time.RFC3339)+" "+line)
}

// Lines returns a snapshot of buffered log lines.
func (b *Bus) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.out))
	copy(out, b.out)
	return out
}

// sleepCtx waits for d or ctx cancellation, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// runHarness executes `harness <args>` against the same binary that is
// currently running and feeds optional stdin. Daemons use this to
// re-enter the CLI for state mutations so the validation+commit code
// has exactly one home.
func runHarness(bus *Bus, stdin string, args ...string) (string, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	// Resolve symlinks (Go's t.TempDir-spawned binary path is fine
	// without this, but production binaries may be linked).
	full, err := filepath.EvalSymlinks(exe)
	if err == nil {
		exe = full
	}
	all := append([]string{"--state-dir", bus.StateRoot}, args...)
	return runProgram(exe, stdin, all)
}

// runProgram is a thin wrapper around exec for testability.
var runProgram = func(exe, stdin string, args []string) (string, string, error) {
	// Implemented in daemon_exec.go to keep this file driver-free.
	return execProgram(exe, stdin, args)
}

// concatLines joins lines for log output.
func concatLines(lines []string) string {
	return strings.Join(lines, "\n")
}
