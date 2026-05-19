// Package jobs reads/writes worker job YAMLs and reports under
// state/jobs/{pending,in-flight,done,failed}/ and emits the matching
// signals into state/inbox/agent-reports/. The shape follows
// design/02 §jobs and design/06 §"Worker protocol".
package jobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// State enumerates the queue directories a job may live in.
type State string

const (
	StatePending  State = "pending"
	StateInFlight State = "in-flight"
	StateDone     State = "done"
	StateFailed   State = "failed"
)

// AllStates is the canonical list of possible job states (queue dirs).
var AllStates = []State{StatePending, StateInFlight, StateDone, StateFailed}

// Job is one entry under state/jobs/<state>/<id>.yaml.
type Job struct {
	ID         string    `yaml:"id"`
	CreatedAt  time.Time `yaml:"created_at"`
	CreatedBy  string    `yaml:"created_by"`
	Role       string    `yaml:"role"`
	TaskID     string    `yaml:"task_id"`
	Priority   string    `yaml:"priority,omitempty"`
	Timeout    string    `yaml:"timeout,omitempty"`
	ClaimedBy  string    `yaml:"claimed_by,omitempty"`
	ClaimedPID int       `yaml:"claimed_pid,omitempty"`
	LeaseUntil time.Time `yaml:"lease_until,omitempty"`

	Instruction string  `yaml:"instruction"`
	Context     Context `yaml:"context"`
	Expects     Expects `yaml:"expects"`

	// Set when the worker submits a report.
	Report *Report `yaml:"report,omitempty"`
}

// Context whitelists the rollups, tasks, files, and prior reports the
// worker is allowed to see (per design/06 §"Worker input assembly").
type Context struct {
	Rollups      []string `yaml:"rollups"`
	Tasks        []string `yaml:"tasks"`
	Files        []string `yaml:"files"`
	PriorReports []string `yaml:"prior_reports"`
}

// Expects defines what the worker's report must look like.
type Expects struct {
	OutcomeEnum []string `yaml:"outcome_enum"`
	Artifacts   []string `yaml:"artifacts,omitempty"`
}

// Report is the worker's structured submission.
type Report struct {
	JobID      string    `yaml:"job_id"`
	FinishedAt time.Time `yaml:"finished_at"`
	Outcome    string    `yaml:"outcome"`
	Confidence string    `yaml:"confidence"`
	TLDR       string    `yaml:"tldr"`
	Next       []Next    `yaml:"next,omitempty"`
	Evidence   []string  `yaml:"evidence,omitempty"`
	Artifacts  []string  `yaml:"artifacts,omitempty"`
	Details    string    `yaml:"details,omitempty"`
}

// Next is one item in a report's next[] array.
type Next struct {
	Action string                 `yaml:"action"`
	Risk   string                 `yaml:"risk,omitempty"`
	Args   map[string]interface{} `yaml:"args,omitempty"`
}

// PathFor returns the on-disk path for a job in the given state.
func PathFor(stateRoot string, st State, id string) string {
	return filepath.Join(stateRoot, "jobs", string(st), id+".yaml")
}

// JobsDir returns the parent directory for jobs in the given state.
func JobsDir(stateRoot string, st State) string {
	return filepath.Join(stateRoot, "jobs", string(st))
}

// Write atomically writes the job YAML to dst.
func Write(dst string, j *Job) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(j)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Read parses a job YAML file.
func Read(src string) (*Job, error) {
	b, err := os.ReadFile(src)
	if err != nil {
		return nil, err
	}
	var j Job
	if err := yaml.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("parse job %s: %w", src, err)
	}
	return &j, nil
}

// FindJob searches every state dir for a job by id. Returns the
// resolved state and path. Returns ErrNotFound if absent everywhere.
func FindJob(stateRoot, id string) (State, string, error) {
	for _, st := range AllStates {
		p := PathFor(stateRoot, st, id)
		if _, err := os.Stat(p); err == nil {
			return st, p, nil
		}
	}
	return "", "", ErrNotFound
}

// ErrNotFound is returned by FindJob when the job is absent.
var ErrNotFound = errors.New("job not found")

// Move atomically moves a job from one state dir to another.
func Move(stateRoot string, j *Job, from, to State) (string, error) {
	src := PathFor(stateRoot, from, j.ID)
	dst := PathFor(stateRoot, to, j.ID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	// Re-marshal so any in-memory edits (claim metadata, report) land.
	if err := Write(dst, j); err != nil {
		return "", err
	}
	if src != dst {
		if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("remove old job file: %w", err)
		}
	}
	return dst, nil
}

// ListByState returns the IDs of jobs in the given state, sorted.
func ListByState(stateRoot string, st State) ([]string, error) {
	dir := JobsDir(stateRoot, st)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(ids)
	return ids, nil
}

// ParseTimeout returns the job's Timeout as a duration; default 30m on
// parse failure.
func (j *Job) ParseTimeout() time.Duration {
	if j.Timeout == "" {
		return 30 * time.Minute
	}
	d, err := time.ParseDuration(j.Timeout)
	if err != nil {
		return 30 * time.Minute
	}
	return d
}
