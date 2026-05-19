# 11 — Audit Agent

The audit agent is a **meta-monitor**. It is an independent process,
running on a cheap model (Haiku-class), whose only job is to look at the
commander's recent behavior and flag drift, anti-patterns, or stuck state
that the commander itself cannot see (because the commander *is* the
thing drifting).

Pattern: a separate daemon, run on a slower cadence than the commander,
with a different model and a strictly read-only view.

## Why a separate agent

The commander cannot reliably audit itself:
- It has too much else to do per tick — auditing would steal token budget
- It is the source of the drift — asking it to spot its own bad habits
  is asking the same model to defeat the same blind spot
- An audit needs **historical view** across many ticks; the commander
  only sees its current dashboard

A separate, cheap, read-only process solves all three.

## What it checks

Each audit run produces a short report classifying observations into:

**✅ Healthy** — nothing of note in this category
**⚠ Watch** — pattern that may matter, surface it
**❌ Problem** — clear violation or stuck state, escalate

### Dimensions audited (default checklist)

1. **Task progress** — Are any tasks unchanged for > N days while
   `in-progress`? Is `consecutive_idle` rising tick-over-tick? Are any
   `blocked-on` chains stuck because the blocker itself was killed?
2. **Communication discipline** — Are outbox sends following the role's
   stated risk guidance? Are messages going to the right thread? Are
   `[harness]` (or configured) prefixes consistent?
3. **Cost trend** — Single-tick cost vs. rolling average. Spikes are
   flagged. Cumulative cost vs. budget.
4. **Workflow integrity** — Were all due hooks updated in the last N
   ticks? Did dispatched jobs return reports in time? Are anomalies
   accumulating in `inbox/anomalies/`?
5. **Omissions** — Are there pending review items the commander has
   ignored for > N ticks? Inbox items un-triaged for > M ticks?
   Tracked threads with no events for > P days that should be archived?
6. **Self-loop detection** — Is the commander re-dispatching the same
   worker job repeatedly with no progress? Re-triaging the same inbox?

The checklist is in `state/audit/checklist.yaml`, editable. Each item
has a severity, a query (over state/ + tick logs), and a remediation
hint shown in the report.

## When it runs

Default: every 6 ticks of the commander, or every 4 hours, whichever is
less frequent. In autopilot mode, an `audit-daemon` process schedules
itself. In manual mode, a human runs `harness audit run`.

The audit cadence is intentionally slower than the commander's. The
audit is a strategic-altitude review, not a per-event check.

## Input — read-only

The audit agent reads:
- `state/tick-log/` (last ~20 entries)
- `state/tasks/*/status.json` + `log.md` (state snapshots)
- `state/threads/*/meta.json` (timestamps only, not full rollups)
- `state/inbox/anomalies/`
- `state/inbox/agent-reports/` (last ~10)
- `state/outbox/sent/` (last day's history)
- `state/audit/checklist.yaml`
- `state/audit/last-N-reports.md` (its own prior reports, for trend)

It does **not** read full rollups, full task logs, or any worker job
internals. Its input budget should be small (< 4k tokens). If audit
input grows large, that itself is a signal — surface it.

## Output — a single report

`harness audit run` writes:

```
state/audit/reports/<iso-timestamp>.md
```

```markdown
---
audit_id: 2026-05-19T14-00-00Z
model: claude-haiku-4-5
cost_usd: 0.012
duration_ms: 1820
trend_window: last 24h
---

## ✅ Healthy
- Outbox discipline: 12 sends, all matched stated risk levels
- Hook freshness: all due hooks updated within window

## ⚠ Watch
- T-9 has been `deferred` for 11 days. Deferral pin P-09 expired
  2026-05-17 but task never returned to active triage.
- Consecutive idle ticks rising: 0 → 2 → 4 → 5 over last 4 audits.
  Commander may be running too frequently for actual signal volume.

## ❌ Problem
- Re-dispatch loop: J-abc123 was re-dispatched 3 times for T-12 with
  the same instruction and the same `low-confidence` outcome. Suggests
  worker cannot answer the question as posed. Suggest:
  `harness task split T-12 ...` and re-scope.
```

Each finding includes (when relevant) a **suggested CLI command** — but
the audit agent does not execute. It can only write the report.

## How findings reach the commander

After writing the report, the audit agent writes:

```
state/inbox/anomalies/audit-<audit-id>.json
```

with a summary and pointer to the full report. The commander's next
tick sees this in the DELTA section.

For severity `❌ Problem` findings, the audit agent also optionally
writes a `state/inbox/human/audit-<audit-id>.md` with `priority: now`
— a hint to the human watching, not just the commander. Configurable in
`config.yaml`:

```yaml
audit:
  problems_escalate_to_human: true
  watch_only_to_commander: true
```

## What the audit agent cannot do

- Mutate state (other than writing its own reports and inbox notes)
- Send outbox messages
- Claim commander or worker locks
- Dispatch jobs
- Touch rollups

CLI enforcement: the audit role's whitelist excludes all mutating verbs
except `harness audit report write` (its own command).

## Tools allowed to the audit role

```
roles/auditor.md
```

System prompt declares:
- Read-only mode
- Output schema (Healthy/Watch/Problem)
- Word limit (≤ 400 words for the body)

Allowed tools for the LLM:
- Read
- `Bash(harness audit:*)` — only the audit subcommands
- `Bash(harness *show|ls|render*)` — read-only `harness` verbs

The audit agent does NOT have shell access to arbitrary commands.

## CLI surface

```
harness audit run [--model haiku]
   # Run one audit immediately. Writes a report + anomaly.

harness audit ls
   # List recent reports.

harness audit show <audit-id>
   # Display a past report.

harness audit checklist edit
   # Open $EDITOR on checklist.yaml.

harness audit-daemon start|stop|status
   # In autopilot, the audit daemon.
```

## Cost expectations

- Model: Haiku-class
- Input: ~3k tokens (tick logs + status snapshots)
- Output: ~500 tokens (report)
- Cadence: every 4h or every 6 ticks
- Daily cost ceiling: typically < $0.50

If costs exceed this, audit checklist is likely over-scoped — simplify
or make audit cadence slower.

## Why not run audit on the commander's model?

We considered using the same Opus-class model. Rejected because:
1. Audit findings are "look back across many ticks" — cheap model is
   sufficient since it's just pattern matching against the checklist
2. A different model adds a real second opinion — Opus and Haiku
   diverge on some judgment calls in useful ways
3. Cost — Opus 6x/day for audit alone would exceed the commander's
   own budget

If a finding seems important and Haiku-level reasoning is insufficient,
the audit agent can escalate via `next: ask-human`-equivalent (write
to `inbox/human/`). The human can then promote it to a commander
dispatch.

## Implementation notes for Track A

- `audit-daemon` is a separate goroutine/process from `worker-runner`
  and `rollup-updater-daemon`. Different schedule, different model
  config.
- The checklist is interpreted by Go code: each check has a name, a
  query function over state/, and a templated hint. The LLM's job is
  to *evaluate severity* and *write the narrative*, not to compute the
  raw signals — those are pre-computed and passed in.
- This split (Go computes signals, LLM writes report) means most of the
  audit logic is deterministic and testable. The LLM is just the
  prose generator.
