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

harness tick-log append "<text>"
  # Appends to the current tick's log file.

harness tick end [--summary "..."]
  # Closes the tick: writes summary, releases lock, commits tick-log.
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
harness claim commander --as=<agent-id> [--ttl 10m]
harness claim job J-<id> --as=<agent-id> [--ttl 30m]
harness release commander
harness release job J-<id>
```

Leases are TTL'd. Expired leases auto-release on next claim attempt.

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
