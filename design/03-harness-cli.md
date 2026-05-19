# 03 — Harness CLI Reference

The `harness` CLI is the only sanctioned mutator of `state/`. Every command
that changes git-tracked state ends with a `git commit` inside `state/`.

## Global flags
- `--state <path>` — override state directory (default: `$HARNESS_STATE` or
  `./state`)
- `--json` — machine-readable output
- `--dry-run` — print intended actions and git diff without applying

## Lifecycle

```
harness init [--state <path>]
  # Create state/ skeleton, git init inside it, write default config.yaml.

harness config get <key>
harness config set <key> <value>
  # Reads/writes config.yaml. <key> uses dotted path (e.g. models.commander).

harness version
```

## Tasks and goals

```
harness goal create "<title>"
  # Creates a root task (no parent). Returns T-<n>.

harness task create "<title>" [--parent T-<n>] [--due <iso>] [--priority ...]
harness task update T-<n> [--state ...] [--priority ...] [--due ...]
harness task restate-goal T-<n> "<new goal>"
harness task kill T-<n> [--reason "..."]
harness task defer T-<n> [--until <iso>] [--reason "..."]
harness task split T-<n> "<child-1>" "<child-2>" [...]
harness task merge T-<n> T-<m>           # T-<m> absorbed into T-<n>
harness task ls [--state ...] [--parent T-<n>] [--blocked]
harness task show T-<n>                  # full status + log + linked
```

### Dependencies

```
harness link T-<a> blocked-on T-<b>
harness unlink T-<a> T-<b>
harness deps show T-<n>                  # upstream + downstream
harness deps cycles                      # report any DAG cycles
```

`link` rejects edges that would create a cycle. Validation runs before
commit.

### Threads

```
harness thread track <thread-id>         # promote inbox/new item to thread
harness thread untrack <thread-id> [--archive]
harness thread show <thread-id>          # rollup + meta + recent raw
harness thread link <thread-id> T-<n>    # set owner_task
harness thread ls [--state ...] [--task T-<n>]
```

## Inbox and triage

```
harness inbox ls [--bucket new|updates|human|agent-reports|anomalies]
harness inbox show <inbox-id>
harness triage <inbox-id> --action=drop|track|attach|task
   # drop:    delete
   # track:   promote to a tracked thread
   # attach:  append to existing thread (--thread <id>)
   # task:    create a new task (--title "...")
```

## Human steering

```
harness pin "<text>" [--scope T-<n>|<thread-id>|global] [--ttl 7d]
harness pin ls
harness pin rm <pin-id>
harness pin renew <pin-id> [--ttl 7d]

harness note <thread-id> "<text>"        # adds a human-pinned Verbatim line

harness steer <kind> <ref> [<args>]      # sugar for pin/note/priority
   # e.g. harness steer pin T-12 "ship this week"
   # e.g. harness steer kill T-3 "scope cut"
```

## Worker dispatch and review

```
harness dispatch T-<n> --role=<role> [--input=<path>] \
                       [--rollups <id>,...] [--files <path>,...] \
                       [--timeout 30m] [--priority normal]
  # Writes jobs/pending/<id>.yaml. Returns J-<id>.

harness job ls [--state pending|in-flight|done|failed]
harness job show J-<id>
harness job cancel J-<id>

harness render worker-input J-<id>
  # Prints assembled prompt for the worker (used by autopilot worker daemon
  # and external agents).

harness submit-report J-<id> --file=<path>
  # Validates YAML, writes job report, moves job to done/, signals inbox.

harness review J-<id> --action=accept|reject [--only <indices>] \
                      [--reason "..."]
  # accept: execute the next[] actions per safety policy (see 07-outbox.md)
  # reject: keep job in done/ but mark superseded; commander may re-dispatch
```

## Outbox

```
harness outbox send --to=<channel> --thread=<id> --body=<path|->  \
                    --risk=low|normal|high [--in-reply-to <event-id>]
  # If risk=low and policy allows, queues to outbox/pending/.
  # Otherwise queues to outbox/awaiting-human/.

harness outbox ls [--state pending|awaiting-human|sent|failed]
harness outbox approve O-<id>            # human only; promotes to pending/
harness outbox reject O-<id>
harness outbox revoke O-<id>             # within revoke_window
```

## Rollup operations

```
harness rollup show <thread-id>          # full rollup
harness rollup flush <thread-id>         # force updater to re-summarize
harness rollup edit <thread-id>          # opens $EDITOR; CLI re-validates
harness rollup pin <thread-id> "<verbatim>"   # add a human-marked pin
```

## Tick and dashboard

```
harness render dashboard [--budget 8000] [--since <tick-id>]
  # Renders the commander's input. --since defaults to last tick's commit.

harness render brief                      # cold-start agent view
harness pickup                            # list available roles, no advice

harness tick start --as <agent-id> [--ttl 10m]
  # Claims the commander lock. Returns the dashboard prompt on stdout.
  # Also runs a "due-hook audit": checks every active hook/poller's
  # last-run timestamp against its expected window; any that missed
  # are surfaced at the top of the dashboard so the commander sees
  # missed signals before acting.

harness tick-log append "<text>"
  # Appends to the current tick's log file.

harness tick end [--idle | --summary "..."] [--cost-usd <f>] [--duration-ms <i>]
  # Closes the tick: writes tick-log file, releases lock, commits.
  # --idle marks the tick as no-op (no state-changing actions taken)
  # and increments engine.consecutive_idle. Without --idle,
  # consecutive_idle resets to 0. The driver (autopilot or external)
  # passes --cost-usd and --duration-ms when available, parsed from
  # the LLM's stream-json output. See design/10 for capture details.
```

## Render commands (for non-mutating prompt assembly)

```
harness render dashboard
harness render brief
harness render worker-input J-<id>
harness role show <role-name>            # outputs role's system prompt
```

These never mutate state. Pollers and external agents use them.

## Autopilot

```
harness autopilot start                  # starts scheduler + worker daemons
harness autopilot stop
harness autopilot pause                  # daemons keep running, just defer
harness autopilot resume
harness autopilot status
```

When paused, daemons will not claim new commander or worker work, but
existing in-flight jobs continue.

## Lock / claim mechanics

```
harness claim commander --as=<agent-id> [--ttl 10m] [--pid <pid>]
harness claim job J-<id> --as=<agent-id> [--ttl 30m] [--pid <pid>]
harness release commander
harness release job J-<id>
```

Leases are TTL'd. Expired leases auto-release on next claim attempt.

### PID-aware stale-lock detection
When a claim is attempted on a slot already held, the CLI first checks:
1. If `lease_until` is in the past → reclaim (auto-release)
2. If `pid` is recorded and `kill -0 <pid>` fails (process gone) →
   reclaim immediately, log a stale-lock anomaly
3. Otherwise → exit 3 (contention)

This dual mechanism (TTL + PID liveness) prevents both indefinite hangs
(if a process dies fast) and premature reclaims (if a slow process is
still alive past its TTL — though that itself merits investigation).

## Audit

```
harness audit run [--model haiku]
   # Run one audit immediately. Writes a report to state/audit/reports/
   # and an anomaly to state/inbox/anomalies/.

harness audit ls
harness audit show <audit-id>
harness audit checklist edit

harness audit-daemon start|stop|status
   # In autopilot, the audit daemon. Cadence configured in config.yaml.
```

See [11-audit-agent.md](./11-audit-agent.md).

## Telemetry

```
harness telemetry ls
harness telemetry show <tick-id|job-id>
harness telemetry rotate [--older-than 7d]
   # Move old stream-json captures out of state/telemetry/.

harness telemetry cost [--since <iso>]
   # Aggregate cost from telemetry/summary.log.
```

## Engine state

```
harness engine status
   # Show consecutive_idle, total_cycles, last_activated, mode.

harness engine idle-tick
   # Sugar over `tick end --idle` for non-tick increments (rare).

harness engine idle-reset
   # Reset consecutive_idle to 0. Manual override.
```

## Source management (poller-facing)

```
harness watch slack-channel <channel-id>
harness watch github-repo <owner/repo>
harness unwatch ...
harness sources ls
```

These update `sources/<src>/config.yaml`. Pollers reload on next interval.

## Output and exit codes

- Exit `0` — success
- Exit `1` — usage error
- Exit `2` — validation failure (e.g., DAG cycle, schema mismatch)
- Exit `3` — lock contention (couldn't claim)
- Exit `4` — git commit failed (state inconsistent — investigate)

`--json` output is structured:

```json
{
  "ok": true,
  "result": { ... },
  "git_commit": "<sha>"
}
```

## Implementation notes for Track A

- Use `cobra` or stdlib `flag` + subcommand router; cobra preferred for help
  generation.
- All state mutations should be wrapped in a `withStateLock` helper that
  acquires a file lock on `state/.harness-lock`.
- Use `git plumbing` commands (`git add`, `git commit`) rather than a Go
  library to keep state's git history readable by standard tools.
- Schema validation: `go-playground/validator` for struct tags, or
  hand-rolled — both fine. Reject early.
- The CLI must run with **no LLM dependency**. LLM calls happen in
  *autopilot* daemons or external drivers, not in CLI verbs.
