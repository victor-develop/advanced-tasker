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

# 6. Set scheduler cadence (active vs inactive windows)
harness config set schedule.active_window.timezone Asia/Singapore
harness config set schedule.active_window.start 08:00
harness config set schedule.active_window.end   20:00
harness config set schedule.active_window.interval 3m
harness config set schedule.inactive_window.interval 30m

# 7. Choose driver mode
harness config set mode hybrid    # autopilot | manual | hybrid
harness autopilot start            # if mode != manual

# 8. (Optional) Sanity check
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
- `commander-scheduler` — triggers commander tick per cadence
  ([see design/04 §Cadence](./04-commander-tick.md#cadence-autopilot-scheduler)):
  - Two-band time window (active vs inactive), separate intervals
  - Event triggers: inbox delta, worker report ready (debounced)
- `outbox-sender` — watches `outbox/pending/`, sends, moves to `sent/`
- `audit-daemon` — periodic meta-review (Haiku-class), much slower
  cadence than commander ([see design/11](./11-audit-agent.md))

All daemons run as goroutines in one `harness autopilot` process, OR as
separate processes managed externally (systemd, launchd, supervisor) — TBD
per implementer; goroutine model is simpler for v1.

### Telemetry capture (autopilot only)

The scheduler invokes `claude -p` with `--output-format stream-json
--verbose` and tees the JSONL output to:

```
state/telemetry/ticks/<iso-timestamp>.jsonl
```

After the process exits, the scheduler:
1. Parses the final `{"type":"result", ...}` line
2. Extracts `total_cost_usd`, `duration_ms`, `is_error`, `session_id`
3. Calls `harness tick end --idle|--summary --cost-usd <f> --duration-ms <i>`
   (the commander's own final action also calls `tick end`; the
   scheduler's call is a safety net that respects whichever fired
   first — same end state)
4. Appends a one-line summary to `state/telemetry/summary.log`:
   ```
   2026-05-19T10:23Z tick    cost=$0.42  dur=4321ms  err=false  session=...
   ```

The worker daemon does the same for each worker invocation, writing to
`state/telemetry/workers/<job-id>.jsonl` and `summary.log`.

This costs essentially nothing and gives `harness telemetry cost` real
data day one.

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

## Human intervention — three tiers

Three escalating ways for a human to inject intent. Use the lightest one
that fits.

### Tier 1 — drop an inbox directive (no interruption)
```
echo "Push PR-1284 today; defer T-9." | harness pin --scope T-12 --ttl 3d
# or just write a file under state/inbox/human/ with frontmatter
```
The commander sees this on its next scheduled tick. Lowest disruption,
fully audited.

### Tier 2 — directly mutate state via CLI (immediate, no LLM)
```
harness task kill T-3 --reason "scope cut"
harness task defer T-9 --until 2026-06-01
harness outbox revoke O-xyz789
```
No tick required; the change is committed. The next commander tick
sees the new state in DELTA. Use when you know exactly what should
change and don't need agent judgment.

### Tier 3 — take over the commander seat (heaviest)
```
harness autopilot pause
harness claim commander --as=victor --ttl=15m
harness render dashboard
# ... read, run harness verbs as you see fit, drive a full tick by hand ...
harness tick end --summary "manual tick: rebalanced priorities after
                            customer call"
harness release commander
harness autopilot resume
```
Use when the model is clearly off the rails and you want to reset the
trajectory before the next autopilot tick. Tier 3 is the equivalent of
sitting in the chair. State is unchanged across the takeover; the next
autopilot tick continues from where you left off.

**An alternative for debugging only:** some drivers (notably
`claude -p`) support `--resume <session-id>` to continue a prior LLM
session. We do **not** recommend this in production — fresh ticks from
the dashboard are the design — but during debugging, attaching to the
most recent tick's session lets you observe the LLM's reasoning. The
session ID is in the tick-log frontmatter.

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
