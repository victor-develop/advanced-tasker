// Package render assembles the read-only human/LLM-facing views the CLI
// exposes via `harness render ...`. The functions in this package are
// pure over the state directory — they read files and produce strings,
// never mutate anything.
//
// Three views are rendered: the full commander dashboard, a cold-start
// "brief" view, and a `pickup` listing. The dashboard surfaces every
// design/04 section (PINNED / DELTA / TASKS / THREADS / PENDING REVIEW
// / RECENT TICK LOG / AVAILABLE COMMANDS) and marks each empty bucket
// explicitly so the contract holds even on a fresh state directory.
package render

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/victor-develop/advanced-tasker/internal/store"
)

// DashboardOptions configures Dashboard. Now is injected so tests can
// produce stable golden files.
type DashboardOptions struct {
	Budget int
	Now    time.Time
}

// Dashboard returns the rendered commander dashboard string per
// design/04 §Dashboard format.
func Dashboard(stateRoot string, opts DashboardOptions) (string, error) {
	if opts.Budget <= 0 {
		opts.Budget = 8000
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}

	tasks, err := store.LoadAll(stateRoot)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	header(&b, opts)
	pinnedSection(&b, stateRoot)
	deltaSection(&b, stateRoot)
	tasksSection(&b, stateRoot, tasks)
	threadsSection(&b, stateRoot)
	pendingReviewSection(&b, stateRoot)
	tickLogSection(&b, stateRoot)
	commandsHint(&b)

	// Token-budget allocation per design/04: if over budget, demote
	// progressively (archive-ready tasks → unchanging threads → DELTA
	// collapse → emit OVER BUDGET warning).
	used := approxTokens(b.String())
	if used > opts.Budget {
		b2 := strings.Builder{}
		b2.WriteString("⚠ OVER BUDGET — compress tasks/threads (commander: prune killed/done, archive stale threads)\n\n")
		b2.WriteString(b.String())
		out := strings.Replace(b2.String(), "USED_TOKENS", fmt.Sprintf("%d", used), 1)
		return out, nil
	}
	out := strings.Replace(b.String(), "USED_TOKENS", fmt.Sprintf("%d", used), 1)
	return out, nil
}

func header(b *strings.Builder, opts DashboardOptions) {
	fmt.Fprintf(b, "=== HARNESS DASHBOARD — %s ===\n", opts.Now.UTC().Format("2006-01-02T15:04Z"))
	fmt.Fprintf(b, "Token budget: %d / used: ~USED_TOKENS\n", opts.Budget)
	b.WriteString("You are the COMMANDER. Read in order, then act.\n")
	b.WriteString("Phases: Survey → Drill → Reconcile → Act → Log → Exit.\n")
	b.WriteString("DO NOT skip phases. DO NOT spawn subprocesses that wait for replies.\n")
	b.WriteString("All side effects via `harness <cmd>`.\n\n")
}

func pinnedSection(b *strings.Builder, stateRoot string) {
	b.WriteString("──── PINNED (from humans) ──────────────────────────────────\n")
	entries := listInbox(stateRoot, "human")
	if len(entries) == 0 {
		b.WriteString("(none)\n\n")
		return
	}
	for _, e := range entries {
		fmt.Fprintf(b, "- %s\n", e)
	}
	b.WriteString("\n")
}

func deltaSection(b *strings.Builder, stateRoot string) {
	b.WriteString("──── DELTA ─────────────────────────────────────────────────\n")
	newItems := listInbox(stateRoot, "new")
	reports := listInbox(stateRoot, "agent-reports")
	anomalies := listInbox(stateRoot, "anomalies")
	updates := listInbox(stateRoot, "updates")
	total := len(newItems) + len(reports) + len(anomalies) + len(updates)
	if total == 0 {
		b.WriteString("(no inbox activity)\n\n")
		return
	}
	if len(newItems) > 0 {
		fmt.Fprintf(b, "inbox/new: %d item(s)\n", len(newItems))
	}
	if len(updates) > 0 {
		fmt.Fprintf(b, "inbox/updates: %d item(s)\n", len(updates))
	}
	if len(reports) > 0 {
		fmt.Fprintf(b, "inbox/agent-reports: %d item(s)\n", len(reports))
	}
	if len(anomalies) > 0 {
		fmt.Fprintf(b, "inbox/anomalies: %d item(s)\n", len(anomalies))
	}
	b.WriteString("\n")
}

func tasksSection(b *strings.Builder, stateRoot string, tasks []*store.Status) {
	active := []*store.Status{}
	for _, t := range tasks {
		if t.State == store.StateKilled || t.State == store.StateDone {
			continue
		}
		active = append(active, t)
	}
	fmt.Fprintf(b, "──── TASKS (%d active) ─────────────────────────────────────\n", len(active))
	if len(active) == 0 {
		b.WriteString("(none)\n\n")
		return
	}

	// Group by parent goal.
	roots := []*store.Status{}
	children := map[string][]*store.Status{}
	for _, t := range active {
		if t.ParentGoal == "" {
			roots = append(roots, t)
		} else {
			children[t.ParentGoal] = append(children[t.ParentGoal], t)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return taskNum(roots[i].ID) < taskNum(roots[j].ID) })
	for _, r := range roots {
		// Goal row.
		flags := drift(r)
		fmt.Fprintf(b, "%-6s %-6s %-12s %q%s\n", r.ID, "goal", string(r.State), goalSummary(stateRoot, r.ID), flags)
		ch := children[r.ID]
		sort.Slice(ch, func(i, j int) bool { return taskNum(ch[i].ID) < taskNum(ch[j].ID) })
		for i, c := range ch {
			prefix := " ├─"
			indent := " │  "
			if i == len(ch)-1 {
				prefix = " └─"
				indent = "    "
			}
			fmt.Fprintf(b, "%s%-6s %-6s %-12s %q%s\n", prefix, c.ID, "task", string(c.State), goalSummary(stateRoot, c.ID), drift(c))
			// blocked-on hint (per design/04 example): ↳blocked-on=T-9 (deferred)
			for _, dep := range c.BlockedOn {
				note := taskStateNote(tasks, dep)
				fmt.Fprintf(b, "%s↳blocked-on=%s%s\n", indent, dep, note)
			}
			// source thread hint: ↳from <thread-id>
			for _, th := range c.LinkedThreads {
				fmt.Fprintf(b, "%s↳from %s\n", indent, th)
			}
			// owner: ↳owner=<assignee>
			if c.Assignee != "" {
				fmt.Fprintf(b, "%s↳owner=%s\n", indent, c.Assignee)
			}
		}
	}
	// Orphans whose parent_goal points at a missing/killed task.
	orphans := []*store.Status{}
	for parent, ch := range children {
		found := false
		for _, r := range roots {
			if r.ID == parent {
				found = true
				break
			}
		}
		if !found {
			orphans = append(orphans, ch...)
		}
	}
	if len(orphans) > 0 {
		b.WriteString("(orphan tasks — parent goal not active)\n")
		sort.Slice(orphans, func(i, j int) bool { return taskNum(orphans[i].ID) < taskNum(orphans[j].ID) })
		for _, c := range orphans {
			fmt.Fprintf(b, "  %-6s %-12s %q\n", c.ID, string(c.State), goalSummary(stateRoot, c.ID))
		}
	}
	b.WriteString("\n")
}

// drift returns a "⚠" suffix if the task looks stuck or accreting.
func drift(t *store.Status) string {
	if t.State == store.StateInProgress && !t.UpdatedAt.IsZero() && time.Since(t.UpdatedAt) > 7*24*time.Hour {
		return " ⚠"
	}
	return ""
}

// taskStateNote returns " (deferred)" style suffix for blocked-on
// references when the blocker is in a notable state.
func taskStateNote(all []*store.Status, id string) string {
	for _, t := range all {
		if t.ID == id {
			switch t.State {
			case store.StateDeferred, store.StateKilled, store.StateDone:
				return fmt.Sprintf(" (%s)", t.State)
			}
		}
	}
	return ""
}

func threadsSection(b *strings.Builder, stateRoot string) {
	threads := listThreads(stateRoot)
	fmt.Fprintf(b, "──── THREADS (%d tracked) ───────────────────────────────────\n", len(threads))
	if len(threads) == 0 {
		b.WriteString("(none)\n\n")
		return
	}
	for _, id := range threads {
		state := readRollupState(stateRoot, id)
		owner, lastEv := readThreadMetaPair(stateRoot, id)
		// Format: <id>  <state>  ↳<owner_task>  <last_event_hint>
		var ownerHint string
		if owner != "" {
			ownerHint = "↳" + owner
		}
		fmt.Fprintf(b, "%s  %s  %s  %s\n", id, state, ownerHint, lastEv)
	}
	b.WriteString("\n")
}

// readRollupState pulls the `state:` field from the rollup.md YAML
// frontmatter (best-effort).
func readRollupState(stateRoot, threadID string) string {
	body, err := os.ReadFile(filepath.Join(stateRoot, "threads", threadID, "rollup.md"))
	if err != nil {
		return ""
	}
	lines := strings.Split(string(body), "\n")
	inFront := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "---" {
			if inFront {
				return ""
			}
			inFront = true
			continue
		}
		if !inFront {
			continue
		}
		if strings.HasPrefix(line, "state:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "state:"))
		}
	}
	return ""
}

// readThreadMetaPair returns (owner_task, last_event_hint) from meta.json.
func readThreadMetaPair(stateRoot, threadID string) (string, string) {
	body, err := os.ReadFile(filepath.Join(stateRoot, "threads", threadID, "meta.json"))
	if err != nil {
		return "", ""
	}
	owner := ""
	hint := ""
	// Tiny JSON peek to avoid bringing in encoding/json structs here.
	if idx := strings.Index(string(body), `"owner_task":`); idx >= 0 {
		rest := string(body)[idx+len(`"owner_task":`):]
		owner = pluckQuoted(rest)
	}
	if idx := strings.Index(string(body), `"last_event_at":`); idx >= 0 {
		rest := string(body)[idx+len(`"last_event_at":`):]
		hint = pluckQuoted(rest)
	}
	return owner, hint
}

func pluckQuoted(s string) string {
	first := strings.Index(s, `"`)
	if first < 0 {
		return ""
	}
	rest := s[first+1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func pendingReviewSection(b *strings.Builder, stateRoot string) {
	reports := listDir(filepath.Join(stateRoot, "jobs", "done"))
	fmt.Fprintf(b, "──── PENDING REVIEW (%d) ────────────────────────────────────\n", len(reports))
	if len(reports) == 0 {
		b.WriteString("(none)\n\n")
		return
	}
	for _, r := range reports {
		fmt.Fprintf(b, "- %s\n", r)
	}
	b.WriteString("\n")
}

func tickLogSection(b *strings.Builder, stateRoot string) {
	b.WriteString("──── RECENT TICK LOG (last 3) ──────────────────────────────\n")
	entries := listDir(filepath.Join(stateRoot, "tick-log"))
	// Sorted by name ascending; reverse and take last 3.
	if len(entries) == 0 {
		b.WriteString("(empty)\n\n")
		return
	}
	sort.Sort(sort.Reverse(sort.StringSlice(entries)))
	if len(entries) > 3 {
		entries = entries[:3]
	}
	for _, e := range entries {
		fmt.Fprintf(b, "- %s\n", e)
	}
	b.WriteString("\n")
}

func commandsHint(b *strings.Builder) {
	// Verbatim from design/04 §"AVAILABLE COMMANDS" — do not paraphrase.
	b.WriteString("──── AVAILABLE COMMANDS ────────────────────────────────────\n")
	b.WriteString("harness triage <id> --action=...\n")
	b.WriteString("harness dispatch T-<n> --role=...\n")
	b.WriteString("harness review J-<id> --action=...\n")
	b.WriteString("harness outbox send --to=... --thread=... --body=... --risk=...\n")
	b.WriteString("harness task update|kill|defer|merge|split|link|unlink\n")
	b.WriteString("harness pin renew P-<id>\n")
	b.WriteString("harness rollup flush <thread-id>\n")
	b.WriteString("harness tick-log append \"...\"\n")
	b.WriteString("harness tick end --summary \"...\"\n")
	b.WriteString("(use --help on any verb for full args)\n")
}

// Brief returns a cold-start summary for an external agent picking up
// work without prior context. Much shorter than Dashboard.
func Brief(stateRoot string) (string, error) {
	tasks, err := store.LoadAll(stateRoot)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "=== HARNESS BRIEF — %s ===\n", time.Now().UTC().Format("2006-01-02T15:04Z"))
	fmt.Fprintf(&b, "state root: %s\n", stateRoot)
	fmt.Fprintf(&b, "tasks: %d total\n", len(tasks))
	active := 0
	for _, t := range tasks {
		if t.State != store.StateKilled && t.State != store.StateDone {
			active++
		}
	}
	fmt.Fprintf(&b, "tasks active: %d\n", active)
	fmt.Fprintf(&b, "threads tracked: %d\n", len(listThreads(stateRoot)))
	fmt.Fprintf(&b, "inbox/new: %d\n", len(listInbox(stateRoot, "new")))
	fmt.Fprintf(&b, "pending review: %d\n", len(listDir(filepath.Join(stateRoot, "jobs", "done"))))
	b.WriteString("\nNext step: `harness render dashboard` for the full view, or `harness pickup` to see available roles.\n")
	return b.String(), nil
}

// Pickup lists the available role files under state/roles/. It does not
// recommend; the design forbids it (design/03).
func Pickup(stateRoot string) (string, error) {
	dir := filepath.Join(stateRoot, "roles")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Available roles in %s:\n", dir)
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		fmt.Fprintf(&b, "  - %s\n", e.Name())
	}
	b.WriteString("\nNo recommendation. Pick the role appropriate for your task.\n")
	return b.String(), nil
}

// --- helpers --------------------------------------------------------

func listInbox(stateRoot, bucket string) []string {
	return listDir(filepath.Join(stateRoot, "inbox", bucket))
}

// listDir returns the basenames of non-hidden, non-directory entries.
func listDir(p string) []string {
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// listThreads returns one-line summaries of threads/<id>/ directories.
func listThreads(stateRoot string) []string {
	dir := filepath.Join(stateRoot, "threads")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func goalSummary(stateRoot, taskID string) string {
	b, err := os.ReadFile(filepath.Join(stateRoot, "tasks", taskID, "goal.md"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		if line == "" {
			continue
		}
		// Drop the "T-<n> — " prefix if present.
		if idx := strings.Index(line, " — "); idx > 0 {
			line = line[idx+len(" — "):]
		}
		return line
	}
	return ""
}

func taskNum(id string) int {
	var n int
	fmt.Sscanf(id, "T-%d", &n)
	return n
}

func approxTokens(s string) int {
	return (len(s) + 3) / 4
}
