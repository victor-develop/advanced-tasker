// Package ids implements monotonic ID generation and parsing for
// harness object IDs (T-<n>, J-<short-uuid>, O-<short-uuid>, etc.).
//
// Task IDs are allocated by scanning the existing state/tasks/ directory
// for the highest T-<n> currently present and returning n+1. This avoids
// a separate counter file and survives manual edits.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	taskIDPattern = regexp.MustCompile(`^T-(\d+)$`)
	jobIDPattern  = regexp.MustCompile(`^J-[a-z0-9]+$`)
)

// NextTaskID scans tasksDir for existing T-<n> directories and returns
// the next monotonic ID as "T-<n+1>". If tasksDir does not exist or is
// empty, returns "T-1".
func NextTaskID(tasksDir string) (string, error) {
	max := 0
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "T-1", nil
		}
		return "", fmt.Errorf("read tasks dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m := taskIDPattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return fmt.Sprintf("T-%d", max+1), nil
}

// ValidTaskID returns true iff id matches the T-<n> form.
func ValidTaskID(id string) bool {
	return taskIDPattern.MatchString(id)
}

// ValidJobID returns true iff id matches the J-<short-uuid> form.
func ValidJobID(id string) bool {
	return jobIDPattern.MatchString(id)
}

// NewJobID returns a fresh J-<short-uuid>.
func NewJobID() (string, error) {
	s, err := ShortUUID()
	if err != nil {
		return "", err
	}
	return "J-" + s, nil
}

// NewOutboxID returns a fresh O-<short-uuid>.
func NewOutboxID() (string, error) {
	s, err := ShortUUID()
	if err != nil {
		return "", err
	}
	return "O-" + s, nil
}

// ValidOutboxID returns true iff id matches the O-<short-uuid> form.
func ValidOutboxID(id string) bool {
	return strings.HasPrefix(id, "O-") && len(id) > 2
}

// ShortUUID returns a short lowercase hex random identifier.
func ShortUUID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// TaskDir returns the absolute on-disk path for the given task ID.
func TaskDir(stateRoot, taskID string) string {
	return filepath.Join(stateRoot, "tasks", taskID)
}

// TasksRoot returns the absolute on-disk path of state/tasks/.
func TasksRoot(stateRoot string) string {
	return filepath.Join(stateRoot, "tasks")
}

// NormalizeTaskID accepts T-1, t-1, or 1 and returns canonical "T-1".
func NormalizeTaskID(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("empty task id")
	}
	upper := strings.ToUpper(s)
	if ValidTaskID(upper) {
		return upper, nil
	}
	// Accept bare integer like "12".
	if _, err := strconv.Atoi(s); err == nil {
		return "T-" + s, nil
	}
	return "", fmt.Errorf("not a valid task id: %q", s)
}
