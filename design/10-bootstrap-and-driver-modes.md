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

# 4. Seed source configs, then register sources to watch
harness config init slack         # writes state/sources/slack/config.yaml stub
harness config init github        # writes state/sources/github/config.yaml stub
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
├── .gitignore                            ← excludes inbox/, jobs/, outbox/pending|*|failed/, telemetry/
├── .git/hooks/post-commit                ← installs ledger + human-pin guards
├── config.yaml                           ← default config (see 02)
├── roles/
│   ├── pr-reviewer.md                    ← seeded role prompts
│   ├── slack-drafter.md
│   ├── planner.md
│   ├── researcher.md
│   ├── summarizer.md
│   └── auditor.md
├── audit/{checklist.yaml, reports/}      ← seeded default checklist (design/11)
├── threads/                              ← empty
├── tasks/                                ← empty
├── inbox/{new,updates,human,agent-reports,anomalies}/
├── jobs/{pending,in-flight,done,failed}/
├── outbox/{pending,awaiting-human,sent,failed}/
├── tick-log/                             ← empty
├── telemetry/{ticks,workers}/            ← empty, gitignored
└── sources/{slack,github}/cursors/       ← directories only
```

Then:
```
$ cd state && git add -A && git commit -m "harness init"
```

Tracked items are committed; queue items are gitignored.

**Note: `state/sources/<source>/config.yaml` is NOT created by
`harness init`.** Source configs are opt-in: run `harness config init slack`
and/or `harness config init github` to seed them. The poller binaries
MUST error helpfully (exit 1, message "run `harness config init <source>`
to seed config") when the file is absent — never auto-create a
non-functional default at poll time, and never silently no-op.

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

### LLM driver interface

Every place the harness invokes an LLM (commander tick, rollup updater,
worker, audit) goes through one Go interface so the loop is testable
without network calls and the driver is swappable:

```go
// internal/llm/driver.go
package llm

import "context"

type Role string  // "commander" | "updater" | "worker" | "auditor"

type InvokeOptions struct {
    Role           Role          // selects model + stream-json capture path
    Model          string        // override (else config.yaml model for the role)
    SystemPrompt   string        // optional; appended to the role prompt
    Timeout        time.Duration // hard cutoff
    StreamJSONPath string        // if non-empty, tee raw stream-json to this file
}

type InvokeResult struct {
    Output       string   // the LLM's final textual output
    SessionID    string   // opaque, from stream-json result event (empty if N/A)
    CostUSD      float64  // parsed from stream-json result event (0 if N/A)
    DurationMS   int64
    IsError      bool
    RawArtifact  string   // path to the captured JSONL (== StreamJSONPath when set)
}

type Driver interface {
    // Invoke runs one bounded LLM call. Prompt is the full prompt to send;
    // for commander ticks this is the rendered dashboard. The driver MUST
    // be re-entrant and safe for concurrent calls from different roles.
    Invoke(ctx context.Context, prompt string, opts InvokeOptions) (InvokeResult, error)

    // Name returns the driver's stable identifier ("claude-p", "fake", etc.)
    Name() string
}
```

Two implementations ship in v1:

**`claude-p`** — execs `claude -p --print --output-format stream-json
--verbose` (or the equivalent for `worker` role: `claude -p --print
--system-prompt <roles/...>`). Stdin gets `prompt`, stdout JSONL is teed
to `StreamJSONPath`, the final `{"type":"result", ...}` line is parsed
into `InvokeResult`. Used by autopilot in production.

**`fake`** — deterministic, scripted driver for tests and `--driver fake`
acceptance runs. Reads scripted responses from
`state/.test/fake-driver/<role>/<call-index>.txt` (or a per-test fixture
dir passed via option). Records each Invoke into an in-memory log
inspectable via `Driver.(*Fake).Calls()`. No subprocess, no network.
Tests targeting the loop (autopilot, rollup updater, worker runner,
audit) use this driver exclusively.

Configuration (in `state/config.yaml`):

```yaml
models:
  driver: claude-p           # claude-p | fake
  commander: claude-opus-4-7
  updater: claude-haiku-4-5
  worker_default: claude-sonnet-4-6
  auditor: claude-haiku-4-5
```

The driver is selected once at process start; long-running daemons do
not hot-swap drivers. To swap, restart the autopilot.

CLI override for ad-hoc runs:

```
harness autopilot start --driver fake [--duration 60s]
```

The `--driver` flag wins over `config.yaml`. The `--duration` flag (only
honored with `--driver fake`, or under `HARNESS_TEST_DURATION`) bounds
the autopilot lifetime so acceptance scripts can exercise the full loop
in seconds.

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
