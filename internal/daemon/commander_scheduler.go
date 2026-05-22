package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/victor-develop/advanced-tasker/internal/llm"
)

// CommanderScheduler ticks the commander at the configured cadence
// (two-band: active/inactive windows), pipes the dashboard prompt into
// the driver, and finalizes the tick via `harness tick end`.
type CommanderScheduler struct {
	Bus         *Bus
	AgentID     string
	MinInterval time.Duration
}

// NewCommanderScheduler constructs a scheduler with sensible defaults.
func NewCommanderScheduler(bus *Bus) *CommanderScheduler {
	return &CommanderScheduler{Bus: bus, AgentID: "autopilot-commander", MinInterval: 5 * time.Second}
}

// Run blocks until ctx is cancelled. It re-reads cadence config on
// every iteration so `harness config set schedule...` takes effect
// without a restart.
func (s *CommanderScheduler) Run(ctx context.Context) error {
	for {
		next := s.intervalFor(time.Now())
		if next < s.MinInterval {
			next = s.MinInterval
		}
		if err := s.runOnce(ctx); err != nil {
			s.Bus.Log("commander-scheduler: %v", err)
		}
		if err := sleepCtx(ctx, next); err != nil {
			return nil
		}
	}
}

func (s *CommanderScheduler) runOnce(ctx context.Context) error {
	// Claim → render dashboard → invoke → tick end.
	_, stderr, err := runHarness(s.Bus, "", "tick", "start", "--as", s.AgentID, "--ttl", "10m")
	if err != nil {
		// Likely contention; back off.
		s.Bus.Log("commander-scheduler: tick start: %s", stderr)
		return nil
	}
	prompt, _, err := runHarness(s.Bus, "", "render", "dashboard")
	if err != nil {
		// Release and continue.
		_, _, _ = runHarness(s.Bus, "", "release", "commander")
		return err
	}
	streamPath := filepath.Join(s.Bus.StateRoot, "telemetry", "ticks", time.Now().UTC().Format("20060102T150405Z")+".jsonl")
	res, _ := s.Bus.Driver.Invoke(ctx, prompt, llm.InvokeOptions{
		Role:           llm.RoleCommander,
		Timeout:        2 * time.Minute,
		StreamJSONPath: streamPath,
	})
	// Append the LLM's narrative to the tick log (best-effort).
	if res.Output != "" {
		_, _, _ = runHarness(s.Bus, "", "tick-log", "append", res.Output)
	}
	// Finalize. `harness tick end` is now the single writer of
	// telemetry/summary.log per tick (was duplicated here — see
	// design/10 §"Telemetry capture"). Pass session_id and is_error
	// through so the line carries the full record.
	endArgs := []string{"tick", "end", "--idle",
		fmt.Sprintf("--cost-usd=%g", res.CostUSD),
		fmt.Sprintf("--duration-ms=%d", res.DurationMS),
		fmt.Sprintf("--session-id=%s", res.SessionID),
	}
	if res.IsError {
		endArgs = append(endArgs, "--is-error")
	}
	_, _, _ = runHarness(s.Bus, "", endArgs...)
	return nil
}

// intervalFor returns the cadence per two-band schedule. Reads
// config.yaml fresh each call.
func (s *CommanderScheduler) intervalFor(now time.Time) time.Duration {
	cfgPath := filepath.Join(s.Bus.StateRoot, "config.yaml")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return 30 * time.Second
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return 30 * time.Second
	}
	sched, _ := cfg["schedule"].(map[string]any)
	if sched == nil {
		return 30 * time.Second
	}
	active, _ := sched["active_window"].(map[string]any)
	inactive, _ := sched["inactive_window"].(map[string]any)
	loc := time.Local
	if tz, _ := stringValue(active, "timezone"); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	local := now.In(loc)
	startHH := stringOr(active, "start", "08:00")
	endHH := stringOr(active, "end", "20:00")
	if isWithinHHMM(local, startHH, endHH) {
		if d, err := time.ParseDuration(stringOr(active, "interval", "3m")); err == nil {
			return d
		}
	}
	if d, err := time.ParseDuration(stringOr(inactive, "interval", "30m")); err == nil {
		return d
	}
	return 30 * time.Second
}

func stringValue(m map[string]any, k string) (string, bool) {
	if m == nil {
		return "", false
	}
	s, ok := m[k].(string)
	return s, ok
}

func stringOr(m map[string]any, k, fallback string) string {
	if v, ok := stringValue(m, k); ok {
		return v
	}
	return fallback
}

func isWithinHHMM(now time.Time, start, end string) bool {
	parse := func(s string) (int, int, bool) {
		var h, m int
		if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
			return 0, 0, false
		}
		return h, m, true
	}
	sh, sm, ok1 := parse(start)
	eh, em, ok2 := parse(end)
	if !ok1 || !ok2 {
		return false
	}
	cur := now.Hour()*60 + now.Minute()
	a := sh*60 + sm
	b := eh*60 + em
	return cur >= a && cur < b
}
