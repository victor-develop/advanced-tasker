package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/victor-develop/advanced-tasker/internal/jobs"
	"github.com/victor-develop/advanced-tasker/internal/llm"
	"github.com/victor-develop/advanced-tasker/internal/telemetry"
)

// WorkerRunner is the daemon that picks up pending jobs, claims them,
// invokes the LLM driver with role=worker, and submits the report
// via the CLI.
type WorkerRunner struct {
	Bus      *Bus
	AgentID  string
	Interval time.Duration
	Timeout  time.Duration
}

// NewWorkerRunner constructs a WorkerRunner.
func NewWorkerRunner(bus *Bus) *WorkerRunner {
	return &WorkerRunner{
		Bus:      bus,
		AgentID:  "autopilot-worker",
		Interval: 2 * time.Second,
		Timeout:  5 * time.Minute,
	}
}

// Run blocks until ctx is cancelled.
func (w *WorkerRunner) Run(ctx context.Context) error {
	for {
		if err := sleepCtx(ctx, w.Interval); err != nil {
			return nil
		}
		ids, _ := jobs.ListByState(w.Bus.StateRoot, jobs.StatePending)
		for _, id := range ids {
			if err := w.process(ctx, id); err != nil {
				w.Bus.Log("worker-runner %s: %v", id, err)
			}
		}
	}
}

func (w *WorkerRunner) process(ctx context.Context, id string) error {
	// Claim.
	_, _, err := runHarness(w.Bus, "", "claim", "job", id, "--as", w.AgentID, "--ttl", "5m", "--pid", fmt.Sprintf("%d", os.Getpid()))
	if err != nil {
		// Likely contention; move on.
		return nil
	}
	// Render worker input.
	prompt, _, err := runHarness(w.Bus, "", "render", "worker-input", id)
	if err != nil {
		return err
	}
	streamPath := filepath.Join(w.Bus.StateRoot, "telemetry", "workers", id+".jsonl")
	res, err := w.Bus.Driver.Invoke(ctx, prompt, llm.InvokeOptions{
		Role:           llm.RoleWorker,
		Timeout:        w.Timeout,
		StreamJSONPath: streamPath,
	})
	if err != nil || res.Output == "" {
		w.Bus.Log("worker-runner %s: LLM failure: %v", id, err)
		return err
	}
	_ = telemetry.AppendSummary(w.Bus.StateRoot, "worker", res.CostUSD, res.DurationMS, res.IsError, res.SessionID)
	// Pipe through submit-report.
	_, stderr, err := runHarness(w.Bus, res.Output, "submit-report", id, "--file=-")
	if err != nil {
		w.Bus.Log("worker-runner %s: submit-report rejected: %s", id, stderr)
		return err
	}
	return nil
}
