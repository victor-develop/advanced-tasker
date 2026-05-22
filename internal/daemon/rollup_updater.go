package daemon

import (
	"context"
	"path/filepath"
	"time"

	"github.com/victor-develop/advanced-tasker/internal/llm"
	"github.com/victor-develop/advanced-tasker/internal/threads"
)

// RollupUpdater is the daemon that watches state/threads/*/.dirty,
// renders the updater input, invokes the LLM driver with role=updater,
// and pipes the response through `harness rollup update`.
type RollupUpdater struct {
	Bus      *Bus
	Debounce time.Duration
	Interval time.Duration
}

// NewRollupUpdater constructs a RollupUpdater with defaults from
// design/05 (30s debounce, 5s poll interval).
func NewRollupUpdater(bus *Bus) *RollupUpdater {
	return &RollupUpdater{Bus: bus, Debounce: 30 * time.Second, Interval: 2 * time.Second}
}

// Run blocks until ctx is cancelled.
func (r *RollupUpdater) Run(ctx context.Context) error {
	for {
		if err := sleepCtx(ctx, r.Interval); err != nil {
			return nil
		}
		ids, _ := threads.List(r.Bus.StateRoot)
		for _, id := range ids {
			if !threads.IsDirty(r.Bus.StateRoot, id) {
				continue
			}
			if err := r.process(ctx, id); err != nil {
				r.Bus.Log("rollup-updater %s: %v", id, err)
			}
		}
	}
}

func (r *RollupUpdater) process(ctx context.Context, id string) error {
	// Read the prompt.
	prompt, _, err := runHarness(r.Bus, "", "rollup", "render-input", id)
	if err != nil {
		return err
	}
	streamPath := filepath.Join(r.Bus.StateRoot, "telemetry", "workers", "updater-"+id+"-"+time.Now().UTC().Format("20060102T150405Z")+".jsonl")
	res, err := r.Bus.Driver.Invoke(ctx, prompt, llm.InvokeOptions{
		Role:           llm.RoleUpdater,
		Timeout:        60 * time.Second,
		StreamJSONPath: streamPath,
	})
	if err != nil || res.Output == "" {
		// Persistent failure: log + exponential backoff handled by
		// caller's loop (next pass picks up .dirty again).
		return err
	}
	// Pipe stdout through the CLI for validation + commit.
	_, stderr, err := runHarness(r.Bus, res.Output, "rollup", "update", id, "--file=-")
	if err != nil {
		r.Bus.Log("rollup-updater %s: CLI rejected: %s", id, stderr)
		return err
	}
	return nil
}
