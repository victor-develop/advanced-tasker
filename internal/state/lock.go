package state

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockFile is the path to the harness-wide mutation lock relative to the
// state root.
const LockFile = ".harness-lock"

// WithStateLock acquires an exclusive flock on state/<LockFile> for the
// duration of fn. Mutating CLI verbs should wrap themselves in this so
// concurrent harness processes serialize cleanly.
//
// The lock file is created (and left in place) on first use; only the
// flock is taken/released per call.
func WithStateLock(root string, fn func() error) error {
	lockPath := filepath.Join(root, LockFile)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("ensure lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	defer func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}()
	return fn()
}
