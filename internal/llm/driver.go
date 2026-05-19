// Package llm defines the LLM driver interface and ships two
// implementations: claude-p (production, execs `claude -p`) and fake
// (deterministic, scripted, for tests and `--driver fake` runs).
//
// Per design/10 §"LLM driver interface", every LLM invocation in the
// system (commander tick, rollup updater, worker, audit) flows through
// this interface. No `claude -p` strings should appear elsewhere.
package llm

import (
	"context"
	"time"
)

// Role identifies which logical agent is making the call. Drivers may
// use it to select the model and to choose the stream-json capture
// path.
type Role string

// Canonical roles. Workers use their role name (e.g. "pr-reviewer") as
// the Role string; the four below are reserved for the daemons.
const (
	RoleCommander Role = "commander"
	RoleUpdater   Role = "updater"
	RoleWorker    Role = "worker"
	RoleAuditor   Role = "auditor"
)

// InvokeOptions configures a single Invoke call.
type InvokeOptions struct {
	Role           Role          // selects model + capture path
	Model          string        // override; if empty, driver picks from config
	SystemPrompt   string        // appended after the role prompt if non-empty
	Timeout        time.Duration // hard cutoff
	StreamJSONPath string        // tee raw stream-json here if non-empty
}

// InvokeResult is the parsed outcome of one LLM call.
type InvokeResult struct {
	Output      string  // the LLM's final textual output
	SessionID   string  // opaque, from stream-json result event
	CostUSD     float64 // parsed from result event
	DurationMS  int64
	IsError     bool
	RawArtifact string // path to JSONL (== StreamJSONPath when set)
}

// Driver is the only interface daemons or the CLI use to talk to a
// model. It MUST be safe for concurrent calls from different roles.
type Driver interface {
	Invoke(ctx context.Context, prompt string, opts InvokeOptions) (InvokeResult, error)
	Name() string
}
