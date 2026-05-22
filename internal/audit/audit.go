// Package audit implements the audit agent's signals computation and
// report writing (design/11). Signals are computed in Go; the LLM's
// only job is to write the narrative.
package audit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/victor-develop/advanced-tasker/internal/inbox"
	"github.com/victor-develop/advanced-tasker/internal/llm"
	"github.com/victor-develop/advanced-tasker/internal/store"
)

// DefaultChecklist is the YAML body seeded into state/audit/checklist.yaml
// on `harness init`. Editable by the user.
const DefaultChecklist = `# Audit checklist — what the audit agent looks at every run.
# Each check has: name, severity_on_hit, query (computed in Go), hint.

checks:
  - name: stuck_in_progress
    severity_on_hit: watch
    query: tasks_in_progress_unchanged_for_days
    args:
      days: 7
    hint: "consider splitting, killing, or deferring tasks that have not moved."

  - name: idle_drift
    severity_on_hit: watch
    query: consecutive_idle_threshold
    args:
      threshold: 8
    hint: "commander cadence may be too aggressive vs signal volume."

  - name: anomalies_pile
    severity_on_hit: problem
    query: anomalies_count_threshold
    args:
      threshold: 10
    hint: "investigate inbox/anomalies/ — there are unresolved validation failures."

  - name: review_backlog
    severity_on_hit: watch
    query: pending_review_threshold
    args:
      threshold: 5
    hint: "jobs/done/ has accumulated; run harness review."

  - name: self_loop
    severity_on_hit: problem
    query: dispatch_loop_threshold
    args:
      same_role_same_task_repeats: 3
    hint: "commander appears to be re-dispatching the same job without progress."
`

// ChecklistFile is the relative path to the checklist.
const ChecklistFile = "audit/checklist.yaml"

// Checklist is the parsed audit/checklist.yaml.
type Checklist struct {
	Checks []Check `yaml:"checks"`
}

// Check is one entry.
type Check struct {
	Name          string            `yaml:"name"`
	SeverityOnHit string            `yaml:"severity_on_hit"`
	Query         string            `yaml:"query"`
	Args          map[string]any    `yaml:"args"`
	Hint          string            `yaml:"hint"`
}

// LoadChecklist reads state/audit/checklist.yaml.
func LoadChecklist(stateRoot string) (*Checklist, error) {
	b, err := os.ReadFile(filepath.Join(stateRoot, ChecklistFile))
	if err != nil {
		return nil, err
	}
	var cl Checklist
	if err := yaml.Unmarshal(b, &cl); err != nil {
		return nil, err
	}
	return &cl, nil
}

// Finding is one computed audit observation.
type Finding struct {
	Check    string
	Severity string // "healthy" | "watch" | "problem"
	Detail   string
	Hint     string
}

// ComputeSignals walks state/ and applies every check's deterministic
// query. Each query has a hard-coded implementation here; the LLM
// (downstream) is only responsible for the narrative.
func ComputeSignals(stateRoot string, cl *Checklist) []Finding {
	var out []Finding
	for _, c := range cl.Checks {
		switch c.Query {
		case "tasks_in_progress_unchanged_for_days":
			days := intArg(c.Args, "days", 7)
			tasks, _ := store.LoadAll(stateRoot)
			var stuck []string
			cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
			for _, t := range tasks {
				if t.State == store.StateInProgress && !t.UpdatedAt.IsZero() && t.UpdatedAt.Before(cutoff) {
					stuck = append(stuck, t.ID)
				}
			}
			sev := "healthy"
			detail := "no in-progress tasks are stuck"
			if len(stuck) > 0 {
				sev = c.SeverityOnHit
				sort.Strings(stuck)
				detail = fmt.Sprintf("in-progress for > %dd: %s", days, strings.Join(stuck, ", "))
			}
			out = append(out, Finding{Check: c.Name, Severity: sev, Detail: detail, Hint: c.Hint})

		case "anomalies_count_threshold":
			thr := intArg(c.Args, "threshold", 10)
			names, _ := inbox.List(stateRoot, inbox.BucketAnomalies)
			sev := "healthy"
			detail := fmt.Sprintf("anomalies present: %d", len(names))
			if len(names) >= thr {
				sev = c.SeverityOnHit
			}
			out = append(out, Finding{Check: c.Name, Severity: sev, Detail: detail, Hint: c.Hint})

		case "review_backlog":
			fallthrough
		case "pending_review_threshold":
			thr := intArg(c.Args, "threshold", 5)
			entries, _ := os.ReadDir(filepath.Join(stateRoot, "jobs", "done"))
			n := len(entries)
			sev := "healthy"
			detail := fmt.Sprintf("pending review: %d", n)
			if n >= thr {
				sev = c.SeverityOnHit
			}
			out = append(out, Finding{Check: c.Name, Severity: sev, Detail: detail, Hint: c.Hint})

		case "consecutive_idle_threshold":
			thr := intArg(c.Args, "threshold", 8)
			cnt := readConsecutiveIdle(stateRoot)
			sev := "healthy"
			detail := fmt.Sprintf("consecutive_idle: %d", cnt)
			if cnt >= thr {
				sev = c.SeverityOnHit
			}
			out = append(out, Finding{Check: c.Name, Severity: sev, Detail: detail, Hint: c.Hint})

		case "dispatch_loop_threshold":
			thr := intArg(c.Args, "same_role_same_task_repeats", 3)
			hits := findDispatchLoops(stateRoot, thr)
			sev := "healthy"
			detail := "no dispatch loops detected"
			if len(hits) > 0 {
				sev = c.SeverityOnHit
				detail = "repeated dispatches: " + strings.Join(hits, "; ")
			}
			out = append(out, Finding{Check: c.Name, Severity: sev, Detail: detail, Hint: c.Hint})

		// Threads dimension (compute always — useful free signal).
		default:
			// Unknown query — surface as a watch finding so users know
			// to update Go-side support.
			out = append(out, Finding{Check: c.Name, Severity: "watch", Detail: "unknown query (Go-side stub)", Hint: c.Hint})
		}
	}
	return out
}

func intArg(args map[string]any, key string, def int) int {
	if args == nil {
		return def
	}
	switch x := args[key].(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return def
}

// readConsecutiveIdle parses engine.consecutive_idle from state/engine.json
// if present; missing → 0.
func readConsecutiveIdle(stateRoot string) int {
	b, err := os.ReadFile(filepath.Join(stateRoot, "engine.json"))
	if err != nil {
		return 0
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		return 0
	}
	switch x := m["consecutive_idle"].(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

// findDispatchLoops scans jobs/done + jobs/failed for repeated
// (role, task_id) tuples and returns short descriptors.
func findDispatchLoops(stateRoot string, threshold int) []string {
	type key struct{ role, task string }
	counts := map[key]int{}
	for _, dir := range []string{"done", "failed", "in-flight", "pending"} {
		entries, _ := os.ReadDir(filepath.Join(stateRoot, "jobs", dir))
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(stateRoot, "jobs", dir, e.Name()))
			if err != nil {
				continue
			}
			var j struct {
				Role   string `yaml:"role"`
				TaskID string `yaml:"task_id"`
			}
			if err := yaml.Unmarshal(b, &j); err != nil {
				continue
			}
			counts[key{j.Role, j.TaskID}]++
		}
	}
	var out []string
	for k, n := range counts {
		if n >= threshold {
			out = append(out, fmt.Sprintf("%s on %s ×%d", k.role, k.task, n))
		}
	}
	sort.Strings(out)
	return out
}

// Run executes one audit pass. It computes signals, calls the driver
// for narrative, writes the report, drops an anomaly inbox entry, and
// optionally escalates Problem findings to inbox/human/.
func Run(ctx context.Context, stateRoot string, driver llm.Driver, escalateProblems bool) (string, error) {
	cl, err := LoadChecklist(stateRoot)
	if err != nil {
		return "", fmt.Errorf("load checklist: %w", err)
	}
	findings := ComputeSignals(stateRoot, cl)
	prompt := buildPrompt(findings)
	auditID := time.Now().UTC().Format("2006-01-02T15-04-05Z")
	streamPath := filepath.Join(stateRoot, "telemetry", "audits", auditID+".jsonl")

	res, err := driver.Invoke(ctx, prompt, llm.InvokeOptions{
		Role:           llm.RoleAuditor,
		Timeout:        2 * time.Minute,
		StreamJSONPath: streamPath,
	})
	if err != nil {
		// LLM failure is non-fatal: fall back to the Go-rendered table
		// so we still produce a usable report (design/11 stub mode).
		res.Output = "(LLM unavailable; signals only)"
	}

	body := renderReport(auditID, driver.Name(), res, findings)
	reportDir := filepath.Join(stateRoot, "audit", "reports")
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		return "", err
	}
	reportPath := filepath.Join(reportDir, auditID+".md")
	if err := os.WriteFile(reportPath, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("write report: %w", err)
	}
	// Emit anomaly signal.
	_, _ = inbox.AppendAnomaly(stateRoot, "audit-"+auditID, summary(findings))

	if escalateProblems && hasProblem(findings) {
		humanPath := filepath.Join(stateRoot, "inbox", "human", "audit-"+auditID+".md")
		_ = os.MkdirAll(filepath.Dir(humanPath), 0o755)
		humanBody := fmt.Sprintf("---\npriority: now\nscope: global\ncreated_at: %s\ncreated_by: auditor\n---\n\nAudit %s found Problem-severity issues.\nSee %s\n", time.Now().UTC().Format(time.RFC3339), auditID, reportPath)
		_ = os.WriteFile(humanPath, []byte(humanBody), 0o644)
	}
	return reportPath, nil
}

func buildPrompt(findings []Finding) string {
	var b strings.Builder
	b.WriteString("You are the AUDITOR. Read the precomputed signals below and write a short narrative.\n")
	b.WriteString("Group into ✅ Healthy / ⚠ Watch / ❌ Problem. Keep total ≤400 words.\n\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "- [%s] %s — %s\n", f.Severity, f.Check, f.Detail)
		if f.Hint != "" {
			fmt.Fprintf(&b, "    hint: %s\n", f.Hint)
		}
	}
	return b.String()
}

func renderReport(id, driverName string, res llm.InvokeResult, findings []Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\naudit_id: %s\ndriver: %s\ncost_usd: %g\nduration_ms: %d\n---\n\n", id, driverName, res.CostUSD, res.DurationMS)
	fmt.Fprintf(&b, "## Signals (Go-computed)\n\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "- [%s] %s — %s\n", iconFor(f.Severity), f.Check, f.Detail)
	}
	b.WriteString("\n## Narrative\n\n")
	b.WriteString(strings.TrimSpace(res.Output))
	b.WriteString("\n")
	return b.String()
}

func iconFor(sev string) string {
	switch sev {
	case "healthy":
		return "OK"
	case "watch":
		return "WATCH"
	case "problem":
		return "PROBLEM"
	}
	return sev
}

func summary(findings []Finding) string {
	var p, w int
	for _, f := range findings {
		switch f.Severity {
		case "problem":
			p++
		case "watch":
			w++
		}
	}
	return fmt.Sprintf("audit signals: %d problems, %d watches", p, w)
}

func hasProblem(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == "problem" {
			return true
		}
	}
	return false
}

// ListReports returns the basenames of audit reports, sorted newest
// first.
func ListReports(stateRoot string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(stateRoot, "audit", "reports"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}
