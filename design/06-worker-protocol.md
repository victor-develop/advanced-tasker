# 06 — Worker Protocol (Jobs and Reports)

This document specifies the async work loop: how the commander dispatches
work, how workers execute it, and how reports flow back.

## Roles

| Role | Tools (suggested) | Typical work |
|---|---|---|
| `pr-reviewer` | Read, Grep, Bash(git/harness:*) | Evaluate a PR diff + comments |
| `slack-drafter` | Read | Draft a reply for a Slack thread |
| `planner` | Read, Grep | Decompose a goal into subtasks |
| `researcher` | Read, Grep, WebFetch | Investigate a question |
| `summarizer` | Read | Rollup updater (see 05) |

Roles are extensible. Adding a new role means adding a `roles/<name>.md`
system-prompt file and (optionally) a model preference in `config.yaml`.

## Job lifecycle

```
   commander
       │
       │  harness dispatch T-12 --role=pr-reviewer ...
       ▼
  jobs/pending/J-abc.yaml
       │
       │  worker daemon (or external agent via `harness claim job`)
       ▼
  jobs/in-flight/J-abc.yaml  (claimed_by, lease_until set)
       │
       │  worker LLM runs: reads `harness render worker-input`, does work
       │  writes report.yaml + artifacts under tasks/<id>/artifacts/
       │  calls `harness submit-report J-abc --file=report.yaml`
       ▼
  jobs/done/J-abc.yaml
       │
       │  inbox/agent-reports/J-abc.json signal written
       ▼
  next commander tick → `harness review J-abc --action=...`
```

## Job file schema (commander writes)

See [02-state-and-schemas.md](./02-state-and-schemas.md#jobsstateidyaml)
for the canonical YAML.

Key constraints:
- `instruction` is ≤500 chars. Long context belongs in the `context.*`
  whitelist, not the instruction.
- `context.rollups`, `context.tasks`, `context.files` are **explicit
  whitelists**. The worker sees nothing else from `state/`.
- `expects.outcome_enum` constrains the report. If the worker's outcome
  doesn't match, `submit-report` rejects.
- `timeout` triggers automatic move to `jobs/failed/` after expiry.

## Worker input assembly

`harness render worker-input J-<id>` builds:

```
You are a <role> worker for harness. Your job is to complete the task
described below and submit a structured report via:

  harness submit-report J-<id> --file=report.yaml

You may use these tools: <from role profile>
You may NOT call other `harness` verbs that mutate state — your only side
effect is `submit-report`.

────────────────────────────────────────────────────────────────────────
INSTRUCTION (from commander):
<job.instruction>

REQUIRED OUTCOME — must be one of: <expects.outcome_enum>

────────────────────────────────────────────────────────────────────────
ROLE SYSTEM PROMPT:
<contents of roles/<role>.md>

────────────────────────────────────────────────────────────────────────
CONTEXT (whitelisted):

Thread rollups in scope:
<for each rollup, full rollup.md content>

Linked tasks:
<for each task, goal.md + status.json + log.md>

Files in scope:
<for each file, content (or excerpt if large)>

Prior worker reports for this task:
<for each prior_report, the report.yaml>

────────────────────────────────────────────────────────────────────────
REPORT SCHEMA (your final action):
<see report schema in design/02 + this doc>
```

## Report schema (worker writes)

```yaml
job_id: J-abc123
finished_at: 2026-05-19T10:31:00Z

outcome: <one of expects.outcome_enum>
confidence: low | med | high      # mandatory

tldr: |
  ≤200 chars. The commander reads only this by default.

next:
  - action: <enum>
    risk: low | normal | high     # required if action is outbox.send
    args: { ... }
  - ...

evidence:
  - <file:lineRange or thread snapshot reference>

artifacts:
  - <relative path under tasks/<id>/artifacts/>

details: |
  Optional long form. Not loaded by default.
```

### `next.action` enum and required args

| action | required args | risk required? |
|---|---|---|
| `outbox.send` | `thread`, `body_file`, `in_reply_to?` | **yes** |
| `task.update` | `id`, `state?`, `priority?`, `due?` | no |
| `task.create` | `title`, `parent?`, `priority?` | no |
| `task.kill` | `id`, `reason` | no |
| `task.defer` | `id`, `until?`, `reason` | no |
| `task.link` | `from`, `to` (blocked-on) | no |
| `task.unlink` | `from`, `to` | no |
| `rollup.note` | `thread`, `verbatim` | no |
| `dispatch` | `task`, `role`, `instruction` | no |
| `ask-human` | `question`, `scope?` | no |

### Validation on submit-report

`harness submit-report` rejects if:
- `outcome` not in `expects.outcome_enum`
- `confidence` missing or invalid
- `tldr` empty or >200 chars
- `next[i].action` invalid or missing required args
- `next[i]` is `outbox.send` without `risk`
- `artifacts[i]` paths outside `tasks/<task_id>/artifacts/`

## Review and acceptance

`harness review J-<id> --action=accept` executes the `next[]` actions per
the safety policy:

### Safety policy
1. **Internal-only actions** (`task.update`, `task.create`, `task.link`,
   etc.) — executed immediately.
2. **`outbox.send` with risk=low** — queued to `outbox/pending/`, sender
   daemon will send. Commander has reviewed.
3. **`outbox.send` with risk=normal|high** — queued to
   `outbox/awaiting-human/`. Commander acceptance is not sufficient.
4. **`ask-human`** — written as a pinned note to the human inbox, no
   automatic action.
5. **`dispatch`** — writes a new job file. Commander does not chain
   workers in one tick; the new job is processed on the next loop.

### Partial accept

```
harness review J-abc --action=accept --only=0,2
```
Executes `next[0]` and `next[2]` only. `next[1]` is dropped (logged in
review history). This lets the commander accept some recommendations
without all-or-nothing.

### Reject

```
harness review J-abc --action=reject --reason="confidence too low"
```
Job stays in `jobs/done/` but tagged superseded. Commander may re-dispatch
a similar job with refined instructions.

## Worker failure modes

| Failure | Detector | Handling |
|---|---|---|
| Timeout (no submit-report by `created_at + timeout`) | Worker daemon scanner | Move job to `failed/`, write `inbox/agent-reports/J-<id>.failed.json` |
| LLM crash / process exit | Worker daemon | Same as timeout |
| `submit-report` rejected (schema) | CLI | Job stays in `in-flight/`, worker can retry. After N retries, daemon moves to `failed/`. |
| Worker exceeds tool budget | Worker daemon (counts subprocess calls) | Configurable — soft warn or hard fail |
| Worker self-reports `blocked` | (legitimate outcome) | Normal report path; commander decides |

## Lease and claim

Worker daemon (or external agent) claims a pending job by:

```
harness claim job J-<id> --as=<agent-id> --ttl=30m
```

This atomically moves `jobs/pending/J-<id>.yaml` to
`jobs/in-flight/J-<id>.yaml` and writes `claimed_by`/`lease_until`. If the
move fails (already claimed), `harness claim` returns exit 3.

Expired leases (lease_until < now) are auto-released by the daemon
scanner: job moves back to `pending/` and lease cleared.

## Concurrency limits

- One commander at a time (commander lock).
- Multiple workers can run in parallel; default cap of 2 in autopilot
  config, configurable.
- A single task may have multiple in-flight jobs (e.g., a researcher and a
  pr-reviewer both working on T-12). Worker daemon does not serialize
  per-task.

## Why not let workers chain workers?

A worker dispatching its own follow-up worker breaks the commander's
visibility into the work graph. If a `researcher` finds it needs a
`pr-reviewer` to continue, it must:
- Submit a report with `next: [dispatch: ...]`
- Commander on next tick approves (or not) the chained dispatch

This costs latency but preserves the commander's centrality. The
alternative — workers spawning workers — leads to graphs the commander
can't see and budgets it can't control.
