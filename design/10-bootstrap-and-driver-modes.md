# 10 — Bootstrap and Driver Modes

## Day-1: from empty dir to running harness

```bash
# 1. Install the harness binary (Go build → ~/bin/harness)
go install github.com/victor-develop/advanced-tasker/cmd/harness@latest
# (or build from source — see top-level README)

# 2. Initialize state
harness init --state ~/.harness
# Creates ~/.harness/{config.yaml, threads/, tasks/, inbox/, jobs/, outbox/,
#                    tick-log/, sources/, roles/} and runs `git init` inside.

# 3. Configure secrets and models
harness config set models.commander claude-opus-4-7
harness config set models.updater claude-haiku-4-5
harness config set models.worker_default claude-sonnet-4-6

# Secrets via env (recommended) or local config:
export SLACK_BOT_TOKEN=xoxb-...
export GITHUB_TOKEN=ghp_...
export ANTHROPIC_API_KEY=sk-ant-...
# Or:
harness config set --local secrets.slack_bot_token xoxb-...   # written to config.local.yaml, gitignored

# 4. Register sources to watch
harness watch slack-channel C0492 --reason "data team alerts"
harness watch github-repo acme/api

# 5. Seed an initial goal
harness goal create "Improve ingest reliability"
# → returns T-1

# 6. Choose driver mode
harness config set mode hybrid    # autopilot | manual | hybrid
harness autopilot start            # if mode != manual

# 7. (Optional) Sanity check
harness pickup
harness render dashboard
```

After this, the harness is running. Pollers will start landing events; the
rollup updater will compress them; the commander will tick on schedule (or
on demand for manual mode).

## What `harness init` does

```
state/
├── .git/                                 ← git init
├── .gitignore                            ← excludes inbox/, jobs/, outbox/, etc.
├── .git/hooks/post-commit                ← installs ledger guards
├── config.yaml                           ← default config (see 02)
├── roles/
│   ├── pr-reviewer.md                    ← seeded role prompts
│   ├── slack-drafter.md
│   ├── planner.md
│   ├── researcher.md
│   └── summarizer.md
├── threads/                              ← empty
├── tasks/                                ← empty
├── inbox/{new,updates,human,agent-reports,anomalies}/
├── jobs/{pending,in-flight,done,failed}/
├── outbox/{pending,awaiting-human,sent,failed}/
├── tick-log/                             ← empty
└── sources/{slack,github}/{cursors,config.yaml}
```

Then:
```
$ cd state && git add -A && git commit -m "harness init"
```

Tracked items are committed; queue items are gitignored.

## Driver modes

### `autopilot`

Daemons run autonomously:
- `slack-poller`, `github-poller` — continuous, per their intervals
- `rollup-updater` — watches `.dirty`, debounces, invokes Haiku-class
  model, writes new rollup via CLI
- `worker-runner` — watches `jobs/pending/`, claims jobs, invokes
  Sonnet-class model with `harness render worker-input`, submits report
- `commander-scheduler` — triggers commander tick on:
  - Every N minutes (configurable, default 1h)
  - Inbox delta exceeding threshold (e.g., 3 new items)
  - Worker report submission
- `outbox-sender` — watches `outbox/pending/`, sends, moves to `sent/`

All daemons run as goroutines in one `harness autopilot` process, OR as
separate processes managed externally (systemd, launchd, supervisor) — TBD
per implementer; goroutine model is simpler for v1.

### `manual`

All daemons paused. Nothing happens automatically.

To drive the system, an external agent or human runs:
```
harness pickup                              # see what's available
harness claim commander --as=me --ttl=15m   # take the commander role
# ... read render dashboard, run harness verbs ...
harness tick end --summary "..."            # release lock
```

Or to take a worker job:
```
harness job ls --state=pending
harness claim job J-abc --as=me --ttl=30m
harness render worker-input J-abc           # get prompt
# ... do the work ...
harness submit-report J-abc --file=report.yaml
```

Pollers MAY still run in manual mode (so the state stays current). The
config flag `manual.daemons_paused` controls this granularly:

```yaml
manual:
  daemons_paused:
    pollers: false        # keep pulling Slack/GH
    rollup_updater: true  # pause LLM-driven summarization
    worker_runner: true   # pause worker dispatch
    commander_scheduler: true
    outbox_sender: false  # let queued sends through (humans still gate high-risk)
```

### `hybrid`

Daemons run AND humans/external agents can claim work at any time. The
claim mechanism handles conflict:
- A worker daemon will not claim a job already in `in-flight/`
- A scheduler will not start a tick if `commander` is already claimed

This is the recommended default. It lets you intervene at any moment without
fighting the autopilot.

## Switching modes mid-flight

```
harness config set mode manual
harness autopilot pause
# ... do manual work ...
harness autopilot resume
harness config set mode hybrid
```

State is unchanged across mode switches. No restart required.

## Health checks

```
harness doctor
```

Reports:
- Git repo health (uncommitted changes, broken hooks)
- Source configs valid (tokens present, channels accessible)
- Lock state (any stale leases?)
- Schema validation on all `threads/*/rollup.md` and `tasks/*/status.json`
- Daemon liveness (autopilot only)

## Multi-machine support

The user mentioned running another machine's Claude Code against the same
harness. Two viable patterns:

### Pattern A — shared filesystem (recommended for v1)
Both machines mount `~/.harness/` from the same source (NFS, syncthing,
git-pull-on-cron, etc.). Only one machine should run autopilot. The
other(s) use `manual` mode with their own claims.

### Pattern B — server + clients
Future work: expose `harness` over a thin HTTP/grpc layer so clients hit a
single state server. Out of scope for v1.

## A worked first-tick example

Right after `harness init` + `harness goal create "X"`:

```
$ harness render dashboard
=== HARNESS DASHBOARD — 2026-05-19T11:00:00Z ===
Token budget: 8000 / used: ~600

──── PINNED ────
(none)

──── DELTA ────
First tick. No prior state.

──── TASKS (1 active) ────
T-1 goal  ready  "Improve ingest reliability"

──── THREADS (0 tracked) ────

──── PENDING REVIEW (0) ────

──── RECENT TICK LOG ────
(empty)
...
```

The commander's typical first action: dispatch a `planner` worker on T-1.

```
harness dispatch T-1 --role=planner
# → J-001 in jobs/pending/
```

Worker daemon picks it up, planner LLM returns a report with `next: [
task.create, task.create, task.create, task.link, ... ]` proposing the
decomposition. Commander reviews and accepts.

The whole loop is now live.
