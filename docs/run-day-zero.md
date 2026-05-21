# Run day zero

How to take a fresh checkout of `advanced-tasker` to a 5-minute supervised
autopilot run against the real Slack, GitHub, and Anthropic APIs.

If you have not read `design/01-overview.md`, do that first. This doc is
operator notes only.

## 1. Prereqs

You need three env vars. Only the first is mandatory.

| Env var | Required? | Where to get it |
|---|---|---|
| `ANTHROPIC_API_KEY` | yes — the commander/auditor/updater all call this | <https://console.anthropic.com/settings/keys> |
| `SLACK_BOT_TOKEN` | optional — Slack-poller will be skipped without it | Slack app config → OAuth & Permissions → `xoxb-...` |
| `GITHUB_TOKEN` | optional — GitHub-poller will be skipped without it | `gh auth token`, or a fine-grained PAT with PR read scope |

Install the three binaries (Track A core + the two pollers):

```
go install github.com/victor-develop/advanced-tasker/cmd/harness@latest
# Track B (Slack poller) and Track C (GitHub poller) install commands
# come from those teams; see their READMEs.
```

Confirm:

```
harness version
```

## 2. `harness boot`

`harness boot` is a pure-Go interactive first-run command. It does NOT
call the LLM. It walks you through eight steps and forces
`outbox.sender_enabled=false` so nothing leaves the host until you opt
in.

Example session (your answers in `>`):

```
$ harness boot
[detect] state directory /Users/you/state is not initialized.
> Run `harness init` at /Users/you/state? [Y/n]
> 
[ok] initialized state at /Users/you/state
[ok] ANTHROPIC_API_KEY is set
[ok] SLACK_BOT_TOKEN is set
[missing] GITHUB_TOKEN is not set — GitHub will be skipped
> Watch a Slack channel? Enter channel ID (e.g. C0492) or blank to skip:
> C0492
>   --reason (optional, blank for none): data team alerts
[ok] watching slack/C0492
> First goal title [My first goal]:
> Ship the ingest retry refactor
[ok] created T-1 — "Ship the ingest retry refactor"
[safety] outbox.sender_enabled set to false — no external messages will be sent until you opt in
> Run `harness autopilot start --driver claude-p --duration 5m` now? [y/N]
> n

Summary: watch list: slack=[C0492], github=[]; goal=T-1; sender_enabled=false; next: harness autopilot start --driver claude-p --duration 5m
```

State changes after this boot:
- `state/` initialized with the full design/02 skeleton + `.git`.
- `state/sources/slack/config.yaml` seeded and channel `C0492` added to
  `watch.channels`.
- `state/tasks/T-1/{goal.md,status.json,log.md}` created.
- `state/config.yaml` has `outbox.sender_enabled: false`.

Non-interactive variant (for fixtures, CI, internal scripting):

```
harness boot --non-interactive
```

It takes the default for every prompt — including `skip` for the
channel/repo watches — so you end up with: state initialized, goal=T-1
"My first goal", `sender_enabled=false`. Use this when you want a
known-state harness without manual input.

## 3. First autopilot run (5 minutes)

```
harness autopilot start --driver claude-p --duration 5m
```

While the run is in progress, the daemons spin in goroutines:

- `commander-scheduler` ticks the commander at the cadence in
  `state/config.yaml::schedule` (default 3m in the active window, 30m
  outside it; first tick fires immediately).
- `rollup-updater` watches `state/threads/*/.dirty` and re-summarizes
  threads when pollers append raw events.
- `worker-runner` claims jobs out of `state/jobs/pending/`.
- `outbox-sender` watches `state/outbox/pending/`. Because
  `sender_enabled=false`, it **moves items to
  `state/outbox/awaiting-human/`** and logs to stderr; it does NOT call
  Slack or GitHub.
- `audit-daemon` runs the meta-review at its slow cadence
  (`audit.cadence: 4h`); for a 5-minute run you typically get one
  audit signal pass.

What to expect in `state/` after the run:

- At least one `state/tick-log/*.md` file (one per commander tick).
- At least one `state/audit/reports/*.md` (audit narrative + signals).
- One line in `state/telemetry/summary.log` per tick / worker / audit,
  each with a real `cost=$...` since this is a real `claude-p` run.
  Inspect with `harness telemetry cost` or `tail state/telemetry/summary.log`.
- Possibly entries in `state/threads/*/rollup.md` if the pollers
  landed events the rollup-updater could chew on.
- **Nothing in `state/outbox/sent/`** because `sender_enabled=false`.
  If the commander queued any low-risk outbox items, they land in
  `state/outbox/awaiting-human/` instead.

## 4. How to stop

```
harness autopilot stop
```

This removes `state/autopilot.lock`. In v1 (per design/10 §"v1 ships
goroutine-model + duration-bounded testing"), the running goroutines
finish their current iteration and then exit on the next cancellation
check. In-flight work:

- A commander tick in progress finishes its current call (no kill mid-LLM).
- A worker job in `state/jobs/in-flight/` stays there until its lease
  expires; the next autopilot start can re-claim it.
- Anything in `state/outbox/pending/` or `state/outbox/awaiting-human/`
  stays put — those are queues, not in-flight requests.

If you want a hard stop, just kill the process; the next run will
inspect leases via `harness doctor`.

## 5. How to inspect

```
harness render dashboard      # the same view the commander reads at each tick
harness telemetry cost        # aggregate $ from summary.log
harness telemetry cost --since 2026-05-22T00:00:00Z   # bound it
harness inbox ls --bucket new
harness inbox ls --bucket anomalies
harness outbox ls --state awaiting-human
git -C state/ log --oneline   # full audit trail
git -C state/ show <sha>      # see exactly what one commit did
```

The dashboard is the canonical "what is going on right now" view. It
lists active tasks, tracked threads, pending review (worker jobs whose
reports the commander has not yet accepted/rejected), the last 3 tick
logs, and the AVAILABLE COMMANDS cheat sheet from design/04.

If `harness doctor` reports anything other than `doctor: OK`, fix that
before opting into real sending.

## 6. How to safely opt into real sending

Once you have one supervised dry run under your belt and you have
inspected `outbox/awaiting-human/` and confirmed the commander is
proposing reasonable messages:

```
harness config set outbox.sender_enabled true
```

This commits a one-line change to `state/config.yaml`. The next
outbox-sender iteration (within ~2 seconds) will see the new value and
start calling Slack / GitHub. The other risk gates remain:

- **Risk classification** (`design/07`). Workers and the commander tag
  every outbox item `low`, `normal`, or `high`. Only `low`-risk items
  go directly to `outbox/pending/`; `normal` and `high` go to
  `outbox/awaiting-human/` and require `harness outbox approve <O-id>`
  before they can be sent. With `sender_enabled=true` the daemon will
  process `pending/` automatically — but anything risky still waits for
  you.
- **Rate limits** (`design/07` + `internal/outbox/limits.go`).
  Per-thread / per-channel / global per-hour caps are enforced before
  every send. Violations log to stderr and write inbox anomalies.
- **Duplicate guard** — identical bodies to the same thread within 10
  minutes are rejected.
- **Revoke window** (`outbox.revoke_window`, default 5 minutes).
  `harness outbox revoke <O-id>` within the window deletes the pending
  file before the sender picks it up.

To send a low-risk message yourself (e.g. as a smoke test of the real
provider path), use:

```
harness outbox send --to slack --thread slack-C0492-1715814123.001200 \
  --risk low --body /tmp/message.md
```

If `sender_enabled=true`, the message will be delivered on the next
sender iteration. Otherwise it lands in `awaiting-human/`. Either way,
the file is committed to `state/` so the audit trail is complete.

When in doubt, set `sender_enabled` back to `false`:

```
harness config set outbox.sender_enabled false
```

That is the round-3 safety net. Use it.
