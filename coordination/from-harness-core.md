# coordination/from-harness-core.md

**From:** harness-core-r2 (Track A)
**To:** slack-poller-r2 (Track B), github-poller-r2 (Track C), team lead
**Branch:** track-a/core-mvp
**Date:** 2026-05-19

## Status

Round 2 implementation of Track A is complete. `harness autopilot start --driver fake --duration 60s` exercises the full loop with no panic, `harness doctor` reports clean state, and all unit/integration tests pass.

Acceptance script: `scripts/acceptance-autopilot.sh` (bash). It builds the binary, seeds a fixture, runs `autopilot start --driver fake --duration 12s`, and asserts: tick-log written, audit report written, telemetry/summary.log present, rollup updater succeeded against a scripted fixture, dispatched worker job moved to done/, no panic in stderr, doctor OK.

## Mandatory coordination answers

### 1. `harness config init <source>` exists and is idempotent

Confirmed for both:
- `harness config init slack` → seeds `state/sources/slack/config.yaml`
- `harness config init github` → seeds `state/sources/github/config.yaml`

Re-running on an existing file is a no-op: exit code 0, output `already exists: <path>`, no overwrite. Test coverage: `internal/cli/integration_test.go::TestConfigInitIdempotent`.

### 2. Literal stub content

Track B and C should match these exactly. Source is `internal/sources/sources.go`.

#### slack stub (`state/sources/slack/config.yaml`)

```yaml
# state/sources/slack/config.yaml — Slack poller config.
# Seeded by `harness config init slack`. Edit `watch.channels`
# directly OR use `harness watch slack-channel <id>`.
auth:
  token_env: SLACK_BOT_TOKEN
watch:
  channels: []
poll_interval: 30s
```

#### github stub (`state/sources/github/config.yaml`)

```yaml
# state/sources/github/config.yaml — GitHub poller config.
# Seeded by `harness config init github`. Edit `watch.repos`
# directly OR use `harness watch github-repo <owner/repo>`.
auth:
  token_env: GITHUB_TOKEN
watch:
  repos: []
poll_interval: 60s
```

These match design/08 (slack) and design/09 (github) schemas: `auth.token_env`, `watch.{channels,repos}`, `poll_interval`. `harness init` does NOT create either file — pollers MUST exit with the error `run \`harness config init <source>\` to seed config` when the file is absent.

### 3. `harness watch` mutators

Both verbs mutate the existing `state/sources/<src>/config.yaml`:

- `harness watch slack-channel <channel-id> [--reason "..."]` appends `{id: <channel-id>, reason: "..."}` (without reason: just `{id: ...}`) to `watch.channels`. Idempotent: re-running with the same ID does not duplicate.
- `harness watch github-repo <owner/repo>` appends `"owner/repo"` (string) to `watch.repos`. Idempotent.
- `harness unwatch slack-channel <id>` / `harness unwatch github-repo <owner/repo>` remove the entry.
- After every mutation, the CLI runs `git add sources/<src>/config.yaml && git commit -m "watch ..."`.

The pollers see the change on next reload of the YAML file. Test coverage: `TestWatchChannelMutatesSourceConfig`.

**Note for pollers:** the `channels` list is a YAML list of mappings (each `{id, reason?}`); the `repos` list is a YAML list of strings. If your poller currently reads either as strings-only, please align with this shape.

## Newly available verbs the pollers can use

| Verb | Purpose |
|---|---|
| `harness inbox ls --bucket new` | Confirm poller writes are landing |
| `harness thread track <id>` | Poller can promote new-inbox to tracked thread |
| `harness rollup show <id>` | Verify rollup file shape after updater runs |
| `harness sources ls` | List the watch set (handy for poller status pages) |
| `harness doctor` | Clean state check |
| `harness telemetry ls` / `cost` | If your poller wants to emit cost numbers |

## What changed since round 1

A1 retrofit:
- `--state` → `--state-dir` (global). Tests updated; the local `--state <enum>` on `task update` still works.
- `harness config init <source>` added (was missing).
- `harness init` now seeds `audit/checklist.yaml` and installs a real post-commit hook (was a placeholder stub).
- Source configs are NOT seeded by `init`; opt-in only.

A3 retrofit (dashboard):
- THREADS rows show `<id>  <state>  ↳<owner_task>  <last_event_hint>` parsed from rollup frontmatter + meta.json.
- TASKS rows tree-formatted with `├─`, `│`, `└─`; blocked-on hints show blocker state in `(deferred)` form; linked threads and assignee surface as `↳from ...` and `↳owner=...`.
- HOOK AUDIT banner emitted by `tick start` when daemons look stale.
- Token-budget over-cap emits `OVER BUDGET` warning at top.
- Inline drift `⚠` on in-progress tasks unchanged > 7d.

A4: inbox + triage (drop/track/attach/task) with required sub-args.

A5: dispatch, render worker-input, submit-report (all validation rules from design/06), review (with risk-raise but never risk-downgrade), claim/release job with PID-aware stale-lock detection.

A6: outbox send/approve/reject/revoke/ls/flush, sender daemon with dry-run, rate-limit + duplicate guards.

A7: rollup update CLI + render-input, rollup-updater daemon (debounced .dirty watcher), post-commit hook with real ledger/pin validators (and the smuggled-violation test in `internal/cli/posthook_test.go`).

A8: `internal/llm.Driver` interface, `NewClaudeP`, `NewFake`. Autopilot wires all five daemons as goroutines with `--driver` and `--duration` flags. Telemetry capture per design/10 — stream-json teed to `state/telemetry/{ticks,workers,audits}/`, summary.log appended.

A9: audit signals + report writer, audit-daemon (slow cadence, escalates ❌ Problem to inbox/human/).

## No design ambiguities

I did not encounter any new design ambiguities round 2 that the round-2 design PR did not address.

## Notes on hard contracts honored

- Did NOT modify `cmd/` or `internal/` of track-b or track-c worktrees.
- Did NOT modify `design/` files.
- No `--force` pushes; the rebased branch was updated with `--force-with-lease`.
- No PRs merged.
- No `// TODO: implement` stubs left in code paths the design declares. The autopilot `pause`/`resume` are no-ops in v1 (design/10 says "TBD"); status reports running/not-running; this matches the round-2 ambiguity resolution that v1 ships goroutine-model + duration-bounded testing.

Ready for your verification.
