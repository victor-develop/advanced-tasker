// Package store reads and writes the durable per-task files
// (goal.md, status.json, log.md) under state/tasks/<id>/, plus utilities
// for iterating all known tasks.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/victor-develop/advanced-tasker/internal/ids"
)

// TaskState is the enum from design/02 §status.json.
type TaskState string

const (
	StateReady      TaskState = "ready"
	StateInProgress TaskState = "in-progress"
	StateBlocked    TaskState = "blocked"
	StateDeferred   TaskState = "deferred"
	StateDone       TaskState = "done"
	StateKilled     TaskState = "killed"
)

// Priority is the priority enum.
type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
)

// ValidTaskStates is the canonical accepted set.
var ValidTaskStates = []TaskState{StateReady, StateInProgress, StateBlocked, StateDeferred, StateDone, StateKilled}

// ValidPriorities is the canonical accepted set.
var ValidPriorities = []Priority{PriorityLow, PriorityNormal, PriorityHigh}

// Status is the parsed status.json. JSON tags mirror design/02.
type Status struct {
	ID             string    `json:"id"`
	State          TaskState `json:"state"`
	Priority       Priority  `json:"priority"`
	ParentGoal     string    `json:"parent_goal,omitempty"`
	BlockedOn      []string  `json:"blocked_on"`
	LinkedThreads  []string  `json:"linked_threads"`
	Assignee       string    `json:"assignee,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	DueAt          *time.Time `json:"due_at,omitempty"`
	OnComplete     []string  `json:"on_complete,omitempty"`
	OnKill         []string  `json:"on_kill,omitempty"`
	// MVP-only fields below — not part of the design schema but useful
	// for verbs that need to remember reasons across kill/defer.
	KilledReason   string     `json:"killed_reason,omitempty"`
	DeferredReason string     `json:"deferred_reason,omitempty"`
	DeferredUntil  *time.Time `json:"deferred_until,omitempty"`
}

// Validate checks invariants the CLI must enforce before writing.
func (s *Status) Validate() error {
	if !ids.ValidTaskID(s.ID) {
		return fmt.Errorf("invalid task id %q", s.ID)
	}
	if !isValidState(s.State) {
		return fmt.Errorf("invalid state %q (want one of %v)", s.State, ValidTaskStates)
	}
	if s.Priority != "" && !isValidPriority(s.Priority) {
		return fmt.Errorf("invalid priority %q (want one of %v)", s.Priority, ValidPriorities)
	}
	for _, b := range s.BlockedOn {
		if !ids.ValidTaskID(b) {
			return fmt.Errorf("invalid blocked_on entry %q", b)
		}
		if b == s.ID {
			return fmt.Errorf("task cannot block on itself: %s", s.ID)
		}
	}
	return nil
}

func isValidState(s TaskState) bool {
	for _, v := range ValidTaskStates {
		if v == s {
			return true
		}
	}
	return false
}

func isValidPriority(p Priority) bool {
	for _, v := range ValidPriorities {
		if v == p {
			return true
		}
	}
	return false
}

// TaskPaths bundles the on-disk locations for a task.
type TaskPaths struct {
	Dir       string
	Goal      string
	Status    string
	Log       string
	Artifacts string
}

// PathsFor returns the canonical paths for a task under stateRoot.
func PathsFor(stateRoot, taskID string) TaskPaths {
	dir := ids.TaskDir(stateRoot, taskID)
	return TaskPaths{
		Dir:       dir,
		Goal:      filepath.Join(dir, "goal.md"),
		Status:    filepath.Join(dir, "status.json"),
		Log:       filepath.Join(dir, "log.md"),
		Artifacts: filepath.Join(dir, "artifacts"),
	}
}

// CreateTask writes a fresh task directory with the provided status,
// goal title (used to build a minimal goal.md), and an empty log.
//
// It is an error if the task dir already exists.
func CreateTask(stateRoot string, st Status, goalBody string) error {
	if err := st.Validate(); err != nil {
		return err
	}
	p := PathsFor(stateRoot, st.ID)
	if _, err := os.Stat(p.Dir); err == nil {
		return fmt.Errorf("task %s already exists", st.ID)
	}
	if err := os.MkdirAll(p.Artifacts, 0o755); err != nil {
		return fmt.Errorf("mkdir task dir: %w", err)
	}
	if err := os.WriteFile(p.Goal, []byte(goalBody), 0o644); err != nil {
		return fmt.Errorf("write goal.md: %w", err)
	}
	if err := WriteStatus(stateRoot, &st); err != nil {
		return err
	}
	emptyLog := fmt.Sprintf("# %s — log\n\n", st.ID)
	if err := os.WriteFile(p.Log, []byte(emptyLog), 0o644); err != nil {
		return fmt.Errorf("write log.md: %w", err)
	}
	return nil
}

// ReadStatus parses status.json for the given task.
func ReadStatus(stateRoot, taskID string) (*Status, error) {
	p := PathsFor(stateRoot, taskID)
	b, err := os.ReadFile(p.Status)
	if err != nil {
		return nil, fmt.Errorf("read status.json for %s: %w", taskID, err)
	}
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse status.json for %s: %w", taskID, err)
	}
	if s.BlockedOn == nil {
		s.BlockedOn = []string{}
	}
	if s.LinkedThreads == nil {
		s.LinkedThreads = []string{}
	}
	return &s, nil
}

// WriteStatus atomically overwrites status.json. The caller is
// responsible for `git add` + commit.
func WriteStatus(stateRoot string, s *Status) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if s.BlockedOn == nil {
		s.BlockedOn = []string{}
	}
	if s.LinkedThreads == nil {
		s.LinkedThreads = []string{}
	}
	p := PathsFor(stateRoot, s.ID)
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}
	b = append(b, '\n')
	tmp := p.Status + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, p.Status); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// AppendLog appends a timestamped entry to log.md.
func AppendLog(stateRoot, taskID, actor, body string) error {
	p := PathsFor(stateRoot, taskID)
	entry := fmt.Sprintf("\n## %s — %s\n%s\n", time.Now().UTC().Format("2006-01-02T15:04Z"), actor, strings.TrimSpace(body))
	f, err := os.OpenFile(p.Log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log.md: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(entry); err != nil {
		return fmt.Errorf("append log: %w", err)
	}
	return nil
}

// ListTasks returns every task ID under state/tasks/ in lexical order
// (which for T-<n> means numerical ascending up to T-9, then lexical).
func ListTasks(stateRoot string) ([]string, error) {
	root := ids.TasksRoot(stateRoot)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if ids.ValidTaskID(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		// numerical sort on the trailing integer
		ni := taskNum(out[i])
		nj := taskNum(out[j])
		return ni < nj
	})
	return out, nil
}

func taskNum(id string) int {
	// id is T-<n>, validated by caller.
	var n int
	fmt.Sscanf(id, "T-%d", &n)
	return n
}

// LoadAll returns every parsed Status, in stable order.
func LoadAll(stateRoot string) ([]*Status, error) {
	ids, err := ListTasks(stateRoot)
	if err != nil {
		return nil, err
	}
	out := make([]*Status, 0, len(ids))
	for _, id := range ids {
		s, err := ReadStatus(stateRoot, id)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// GoalBody reads goal.md for a task.
func GoalBody(stateRoot, taskID string) (string, error) {
	p := PathsFor(stateRoot, taskID)
	b, err := os.ReadFile(p.Goal)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// LogBody reads log.md for a task.
func LogBody(stateRoot, taskID string) (string, error) {
	p := PathsFor(stateRoot, taskID)
	b, err := os.ReadFile(p.Log)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// RemoveBlockedOn removes target from src's blocked_on list. If src was
// blocked solely on target, also flips state from blocked → ready.
// Returns true if the status was modified.
func RemoveBlockedOn(s *Status, target string) bool {
	out := s.BlockedOn[:0]
	removed := false
	for _, b := range s.BlockedOn {
		if b == target {
			removed = true
			continue
		}
		out = append(out, b)
	}
	s.BlockedOn = out
	if removed && len(s.BlockedOn) == 0 && s.State == StateBlocked {
		s.State = StateReady
	}
	return removed
}

// AddBlockedOn appends target to src's blocked_on list, deduping. If
// target is added and src was ready/in-progress, state flips to blocked.
func AddBlockedOn(s *Status, target string) bool {
	for _, b := range s.BlockedOn {
		if b == target {
			return false
		}
	}
	s.BlockedOn = append(s.BlockedOn, target)
	if s.State == StateReady || s.State == StateInProgress {
		s.State = StateBlocked
	}
	return true
}
