# 02 — State Layout and File Schemas

This document defines every file in `state/`. These schemas are the contract
between components. Changing them is a breaking change.

## Top-level layout

```
state/
├── .git/                       # state itself is a git repo
├── config.yaml                 # global harness config
├── threads/                    # tracked Slack threads, PRs (long-lived)
│   └── <thread-id>/
│       ├── meta.json
│       ├── rollup.md
│       └── raw/
│           └── <event-id>.json
├── tasks/                      # internal work items
│   └── <task-id>/
│       ├── goal.md
│       ├── status.json
│       ├── log.md
│       └── artifacts/
├── inbox/
│   ├── new/                    # never-before-seen signals (poller writes)
│   ├── updates/                # informational ping that a tracked thread changed
│   ├── human/                  # human directives (pin/now/soon/fyi)
│   ├── agent-reports/          # worker completion signals
│   └── anomalies/              # CLI/validator rejections needing review
├── jobs/
│   ├── pending/                # commander wrote; worker daemon picks up
│   ├── in-flight/              # claimed
│   ├── done/
│   └── failed/
├── outbox/
│   ├── pending/                # ready to send (low-risk)
│   ├── awaiting-human/         # high-risk, needs explicit approval
│   ├── sent/
│   └── failed/
├── tick-log/
│   └── <iso-timestamp>.md
├── sources/
│   ├── slack/                  # poller cursors + config
│   └── github/
├── audit/
│   ├── checklist.yaml          # what the auditor checks (editable)
│   └── reports/                # past audit reports (git-tracked)
│       └── <iso-timestamp>.md
├── telemetry/                  # captured stream-json from each tick (gitignored)
│   ├── ticks/<iso>.jsonl
│   ├── workers/<job-id>.jsonl
│   └── summary.log             # one-line cost/duration/exit per run
└── roles/                      # worker role definitions (system prompts)
    ├── pr-reviewer.md
    ├── slack-drafter.md
    ├── planner.md
    ├── researcher.md
    ├── summarizer.md           # rollup updater role
    └── auditor.md              # audit agent role
```

Items under `inbox/`, `jobs/`, `outbox/pending|awaiting-human|failed/`, and
`telemetry/` are short-lived; they are NOT checked into git (see
[01-overview.md](./01-overview.md) P5 — git tracks the durable world model,
not transient queues). The `.gitignore` inside `state/` excludes them.

Items under `threads/`, `tasks/`, `tick-log/`, `audit/reports/`,
`outbox/sent/`, `roles/`, and `config.yaml` ARE git-tracked.

`telemetry/` is gitignored because it can grow large (per-tick stream-json
captures). Rotate/archive via `harness telemetry rotate` if needed.

## ID conventions

| Kind | Format | Example |
|---|---|---|
| Thread (Slack) | `slack-<channel>-<thread_ts>` | `slack-C0492-1715814123.001200` |
| Thread (GitHub PR) | `github-<owner>-<repo>-pr-<num>` | `github-acme-api-pr-1284` |
| Task | `T-<n>` (monotonic) | `T-12` |
| Job | `J-<short-uuid>` | `J-abc123` |
| Inbox item | `<source>-<event-id>` | `slack-C0492-1715814999.000400` |
| Outbox item | `O-<short-uuid>` | `O-xyz789` |
| Tick | `<iso-utc>` (filename) | `2026-05-19T10-23-00Z.md` |

## config.yaml (git-tracked)

```yaml
mode: hybrid                 # autopilot | manual | hybrid
git:
  auto_commit: true          # CLI commits state/ on every mutation
models:
  commander: claude-opus-4-7
  updater: claude-haiku-4-5
  worker_default: claude-sonnet-4-6
limits:
  dashboard_token_budget: 8000
  rollup_current_ask_max_lines: 3
  rollup_open_questions_max_lines: 5
  outbox_per_thread_per_hour: 5
pins:
  default_ttl: 7d
sources:
  slack:
    enabled: true
  github:
    enabled: true
```

Secrets (tokens) go in `state/config.local.yaml` (gitignored) or env vars.

## threads/&lt;id&gt;/meta.json

```json
{
  "id": "slack-C0492-1715814123.001200",
  "source": "slack",
  "url": "https://acme.slack.com/archives/C0492/p1715814123001200",
  "created_at": "2026-05-14T09:12:00Z",
  "last_event_at": "2026-05-19T09:12:00Z",
  "owner_task": "T-12",
  "participants": ["alice", "bob", "me"],
  "tracking_since": "2026-05-14T09:15:00Z"
}
```

## threads/&lt;id&gt;/rollup.md

```yaml
---
id: github-acme-api-pr-1284
source: github
url: https://github.com/acme/api/pull/1284
state: awaiting-author-response    # enum, see below
last_event: 2026-05-19T09:12Z reviewer-2 left 3 comments
owner_task: T-12
participants: [alice, bob, me]
---

## Goal (stable)
Replace fixed-backoff retry in ingest pipeline with jittered exponential.

## Current ask (mutable, ≤3 lines)
- Alice asks max_retries be configurable
- CI is red — check if new tests are flaky

## Open questions (mutable, ≤5 lines)
- [ ] Keep old fixed-backoff path as fallback? (bob, not yet answered)

## Decisions ledger (append-only)
- 2026-05-14: Use jittered exponential per AWS architecture blog
- 2026-05-16: max_retries default = 5

## Verbatim pins (high-signal originals, never auto-deleted)
> "this needs to ship before friday cutoff" — alice, 2026-05-17
> "we cannot break legacy clients" — bob, 2026-05-18 (— pinned by human)
```

### `state` enum
One of:
`new`, `awaiting-our-response`, `awaiting-author-response`,
`awaiting-external`, `in-progress`, `blocked`, `resolved`, `archived`.

### Ledger and pin invariants (CLI-enforced via git diff)
- The `## Decisions ledger` section may only have lines **appended**. Any
  edit/removal of an existing ledger line is rejected.
- Lines in `## Verbatim pins` marked `— pinned by human` may only be added,
  never removed by the updater.

## tasks/&lt;id&gt;/goal.md

Free-form prose statement of the goal. Stable — only the commander rewrites
it via `harness task restate-goal`.

```markdown
# T-12 — Refactor ingest retry to jittered exponential

Replace the fixed-backoff retry logic in src/ingest/retry.go with a
jittered exponential strategy. Must remain backwards-compatible with
existing call sites. Configurable max_retries.
```

## tasks/&lt;id&gt;/status.json

```json
{
  "id": "T-12",
  "state": "in-progress",
  "priority": "normal",
  "parent_goal": "T-1",
  "blocked_on": ["T-9"],
  "linked_threads": ["github-acme-api-pr-1284"],
  "assignee": "commander",
  "created_at": "2026-05-14T08:00:00Z",
  "updated_at": "2026-05-19T09:30:00Z",
  "due_at": "2026-05-22T17:00:00Z",
  "on_complete": ["unblock:T-15", "notify:thread:github-acme-api-pr-1284"],
  "on_kill": ["unblock:T-15"]
}
```

`state` enum: `ready`, `in-progress`, `blocked`, `deferred`, `done`, `killed`.

### Declarative completion hooks

`on_complete` and `on_kill` are arrays of side-effect specs that the CLI
executes automatically when `harness task check` (state → `done`) or
`harness task kill` is called. Supported specs:

| Spec | Effect |
|---|---|
| `unblock:T-<n>` | Remove the current task from `T-<n>.blocked_on`; if it was the only blocker, transition `T-<n>` from `blocked` → `ready` |
| `notify:thread:<id>` | Enqueue an outbox draft (low-risk) acknowledging completion in the thread (commander still reviews unless config says auto-send) |
| `dispatch:<role>:<task>` | Queue a worker job (still subject to commander review on next tick) |

These run inside the CLI transaction, in the same git commit as the
status change. If a hook fails (e.g., target task doesn't exist), the
whole mutation aborts with exit 2.

## tasks/&lt;id&gt;/log.md

Append-only narrative log. Commander and CLI both append. Each entry:

```markdown
## 2026-05-19T09:30Z — commander
Dispatched J-abc123 to pr-reviewer to evaluate PR-1284 v2.
```

## inbox/&lt;bucket&gt;/&lt;id&gt;.json (poller / CLI writes)

```json
{
  "id": "slack-C0492-1715814999.000400",
  "source": "slack",
  "kind": "new",
  "received_at": "2026-05-19T10:15:00Z",
  "summary": "@me asked about ingest pipeline status in #data",
  "ref": {
    "channel": "C0492",
    "ts": "1715814999.000400",
    "thread_ts": null,
    "user": "alice"
  },
  "raw_path": "threads/slack-C0492-.../raw/1715814999.000400.json"
}
```

`kind` is one of `new`, `update`, `human-directive`, `agent-report`,
`anomaly`.

### Human directives (inbox/human/)

```yaml
---
priority: pin | now | soon | fyi
scope: T-12 | github-acme-api-pr-1284 | global
expires: 2026-05-26
created_at: 2026-05-19T11:00:00Z
created_by: victor
---
Push PR-1284 to merge today. Defer T-9 refactor until next sprint.
```

## jobs/&lt;state&gt;/&lt;id&gt;.yaml

```yaml
id: J-abc123
created_at: 2026-05-19T10:23:00Z
created_by: commander          # commander | human | <worker-id> (rare)
role: pr-reviewer
task_id: T-12
priority: normal               # low | normal | high
timeout: 30m
claimed_by: null               # set when moved to in-flight/
lease_until: null

instruction: |
  PR-1284 author pushed v2. Evaluate whether alice's max_retries feedback
  is addressed. If yes, recommend approve; if no, list remaining gaps.

context:
  rollups:
    - threads/github-acme-api-pr-1284
  tasks:
    - T-12
  files:
    - src/ingest/retry.go
  prior_reports: []

expects:
  outcome_enum: [approve, request-changes, blocked]
  artifacts: [review-comments.md]
```

## job reports (worker writes via CLI)

```yaml
job_id: J-abc123
finished_at: 2026-05-19T10:31:00Z
outcome: approve
confidence: med                # low | med | high

tldr: |
  v2 adds RetryConfig with default max_retries=5, configurable via env.
  Addresses alice's feedback. Recommend approve.

next:
  - action: outbox.send
    risk: low
    args:
      thread: github-acme-api-pr-1284
      body_file: artifacts/approval-comment.md
  - action: task.update
    args:
      id: T-12
      state: ready-to-merge

evidence:
  - src/ingest/retry.go:42-78
  - threads/github-acme-api-pr-1284 rollup as of 2026-05-19T10:23Z

artifacts:
  - artifacts/approval-comment.md
  - artifacts/diff-analysis.md

details: |
  ...optional long-form...
```

### `next.action` enum
- `outbox.send` (requires `risk` field)
- `task.update`, `task.create`, `task.kill`, `task.defer`
- `task.link`, `task.unlink`
- `rollup.note` (add a Verbatim pin via summarizer hint)
- `dispatch` (suggest a follow-up worker)
- `ask-human` (worker is blocked, escalate)

## outbox/&lt;state&gt;/&lt;id&gt;.yaml

```yaml
id: O-xyz789
created_at: 2026-05-19T11:30:00Z
created_by: J-abc123           # job ID or commander or human
to: slack                      # slack | github-pr-comment | github-pr-review
ref:
  thread: github-acme-api-pr-1284
  in_reply_to: <source-event-id>
body_file: artifacts/approval-comment.md
risk: low                      # low | normal | high
revoke_window: 5m
```

Risk semantics: see [07-outbox.md](./07-outbox.md).

## tick-log/&lt;iso-timestamp&gt;.md

```markdown
---
tick_id: 2026-05-19T10-23-00Z
duration_ms: 4321
commander_model: claude-opus-4-7
cost_usd: 0.42
idle: false             # true if no state-changing actions taken
consecutive_idle: 0     # running counter; resets when idle=false
session_id: <opaque ID from driver, optional>
---

## What I saw
- 2 new inbox items (1 slack new, 1 agent report)
- T-12 PR-1284 state: awaiting-author-response → ready-to-merge
- pin from victor: ship PR-1284 today

## What I did
- Reviewed J-abc123, accepted next actions (outbox + task update)
- Triaged inbox slack item to new task T-15
- Deferred T-9 per human pin

## What I'm leaving for next tick
- T-15 needs planner dispatch
- Watching for J-abc123 outbox send confirmation
```

## sources/slack/config.yaml

```yaml
auth:
  token_env: SLACK_BOT_TOKEN
watch:
  channels:
    - id: C0492
      reason: "data team alerts"
poll_interval: 30s
```

## sources/slack/cursors/

```
sources/slack/cursors/
├── channels/
│   └── C0492.json              # { last_ts: "1715814999.000400" }
└── threads/
    └── 1715814123.001200.json  # { last_reply_ts: "1715814500.001500" }
```

## sources/github/config.yaml

```yaml
auth:
  token_env: GITHUB_TOKEN
watch:
  repos:
    - "acme/api"
poll_interval: 60s
```

## sources/github/cursors/

```
sources/github/cursors/
└── prs/
    └── acme-api-1284.json
        # { last_polled_at, last_event_id_per_endpoint: {...} }
```

## .dirty marker

```
state/threads/<id>/.dirty
```

Touched by pollers after appending to `raw/`. Read and cleared by the
rollup updater daemon. Empty file; presence is the signal.

## File mutation rules (enforced by CLI)

1. **No direct writes to git-tracked files outside the CLI.** Polling and
   updater daemons write to git-tracked files via `harness <verb>`. Direct
   filesystem writes by daemons are limited to gitignored areas (`raw/`,
   `.dirty`, `inbox/new/`).
2. **Every CLI mutation that touches git-tracked files commits.** Commit
   message format: `<verb> <object>: <short summary>`.
3. **Append-only invariants are validated post-commit.** A pre-push or
   post-commit hook re-runs the ledger/pin guards (see
   [05-rollup-updater.md](./05-rollup-updater.md)) and reverts if violated.
