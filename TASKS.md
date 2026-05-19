# TASKS.md — Implementation work breakdown

This document partitions the work into three parallel-implementable tracks.
Each track has a primary design doc, a defined filesystem contract, and a
target deliverable. Tracks can be built independently and integrated later.

> Before starting any track: read [AGENTS.md](./AGENTS.md) and the required
> design docs.

---

## Track A — Harness Core
**Owner agent team:** core
**Primary spec:** [design/03-harness-cli.md](./design/03-harness-cli.md),
[design/04-commander-tick.md](./design/04-commander-tick.md),
[design/05-rollup-updater.md](./design/05-rollup-updater.md),
[design/06-worker-protocol.md](./design/06-worker-protocol.md),
[design/07-outbox.md](./design/07-outbox.md),
[design/10-bootstrap-and-driver-modes.md](./design/10-bootstrap-and-driver-modes.md)

### Deliverables (MVP order)

**A1. Repo skeleton + state init**
- Go module, `cmd/harness/main.go`, subcommand framework (cobra or stdlib)
- `harness init` creates state/ directory and runs `git init` inside it
- `harness config get|set` reads/writes a YAML config file under state/
- Acceptance: empty dir → `harness init` → valid state/ with .git

**A2. Task and goal CRUD**
- `harness goal create`, `harness task create|update|kill|defer|split|merge`
- `harness link <a> blocked-on <b>`, `harness unlink`
- Writes goal.md / status.json / log.md per
  [design/02-state-and-schemas.md](./design/02-state-and-schemas.md)
- Every mutation: validate → write → `git add && git commit` inside state/
- Acceptance: tests covering each verb + DAG cycle rejection

**A3. Dashboard rendering**
- `harness render dashboard` → stdout, fixed token budget, per
  [design/04-commander-tick.md](./design/04-commander-tick.md)
- `harness render brief` → cold-start agent simpler view
- `harness pickup` → just lists what's available, no recommendation
- Acceptance: golden-file tests for various state snapshots

**A4. Inbox triage**
- `harness inbox ls`, `harness triage <inbox-id> --action=...`
- Acceptance: tests covering each action path

**A5. Worker protocol (job/report)**
- `harness dispatch <task> --role=<role> [--input=...]` writes job file
- `harness render worker-input <job-id>` outputs worker prompt
- `harness submit-report <job-id> --file=report.yaml` validates + writes
- `harness review <job-id> --action=accept|reject` per
  [design/06-worker-protocol.md](./design/06-worker-protocol.md)
- Acceptance: end-to-end test with a stub worker

**A6. Outbox + risk model**
- `harness outbox send|approve|revoke` per
  [design/07-outbox.md](./design/07-outbox.md)
- Acceptance: tests for each risk path; high-risk requires human

**A7. Rollup updater driver**
- Daemon that watches `state/threads/<id>/.dirty` and invokes the configured
  summarizer LLM with old rollup + new raw messages
- `git commit` hook validates ledger-append-only + pinned lines preserved;
  on violation, `git reset --hard` and write to `inbox/anomalies/`
- Acceptance: integration test with a mock updater that tries to delete a
  ledger entry → CLI must reject

**A8. Autopilot driver**
- Scheduler that triggers commander ticks via `claude -p`
- Worker daemon that picks up jobs and runs them as `claude -p`
- `harness autopilot start|stop|pause|resume`
- Acceptance: dry-run mode that prints intended subprocess calls

### Track A non-goals
- Do not implement Slack or GitHub API calls (Tracks B and C)
- Do not build a UI

---

## Track B — Slack Poller
**Owner agent team:** slack-poller
**Primary spec:** [design/08-slack-poller.md](./design/08-slack-poller.md)

### Deliverables

**B1. `slack-poller` binary**
- Standalone Go binary (or subcommand of `harness`, TBD by core team)
- Reads `state/sources/slack/config.yaml` for tracked channels + auth
- Maintains per-channel and per-thread cursors

**B2. Two-level polling**
- Channel-level: `conversations.history` for new top-level messages
- Thread-level: `conversations.replies` for each tracked thread

**B3. Event normalization + persistence**
- Writes raw events to
  `state/threads/slack-<channel>-<thread_ts>/raw/<ts>.json`
- For new threads (channel-level message that isn't in a tracked thread):
  writes to `state/inbox/new/slack-<channel>-<ts>.json`
- After writing, `touch state/threads/<id>/.dirty` to signal updater

**B4. Dedup + idempotency**
- Slack `ts` is the unique key; re-runs over overlapping windows must not
  duplicate

**B5. Tracking lifecycle**
- `slack-poller watch <channel-id>` and `unwatch`
- `slack-poller track-thread <channel> <thread_ts>` (promote a new-inbox
  item to a tracked thread)

### Track B non-goals
- Do not parse rollups or invoke LLMs
- Do not send messages (that's outbox, Track A)
- Do not handle GitHub

### Hard contracts with other tracks
- Output file paths and schemas: see
  [design/08-slack-poller.md](./design/08-slack-poller.md) §"Filesystem
  output contract"
- Must not write outside `state/threads/`, `state/inbox/new/`, or
  `state/sources/slack/`

---

## Track C — GitHub PR Tracker
**Owner agent team:** github-poller
**Primary spec:** [design/09-github-poller.md](./design/09-github-poller.md)

### Deliverables

**C1. `github-poller` binary**
- Reads `state/sources/github/config.yaml` for tracked repos + auth (PAT or
  GH App)

**C2. Multi-source polling per PR**
- PR review comments (`/pulls/{n}/comments`)
- PR issue comments (`/issues/{n}/comments`)
- PR state changes (open/closed/merged via `/pulls/{n}` `updated_at`)
- Optional: PR review summaries, check runs

**C3. New PR discovery**
- Per repo: `/pulls?state=open&sort=updated` to find new tracked PRs
- New PRs land in `state/inbox/new/github-<repo>-<pr>.json` until commander
  decides to track

**C4. Event persistence**
- Same shape as Slack: `state/threads/github-<repo>-pr-<n>/raw/<event-id>.json`
- `touch .dirty` after write

**C5. Dedup**
- Use GitHub's comment IDs / event IDs; never trust `since=` alone (small
  timestamp jitter observed)

**C6. Tracking lifecycle**
- `github-poller watch <owner/repo>`, `unwatch`
- `github-poller track-pr <owner/repo> <pr-number>`

### Track C non-goals
- Do not implement GitHub Issues, Discussions, Actions
- Do not post comments (outbox)

### Hard contracts with other tracks
- See [design/09-github-poller.md](./design/09-github-poller.md) §"Filesystem
  output contract"
- Must not write outside `state/threads/`, `state/inbox/new/`, or
  `state/sources/github/`

---

## Integration milestones

**M1 (Walking skeleton):** Track A1+A2+A3 done. Empty state, manual task
creation, dashboard renders.

**M2 (Manual driver):** Track A1–A4 done. A human can `harness pickup`,
read a fake inbox item, triage it.

**M3 (Async work):** Track A5 + A6 done. Worker reports loop closed.

**M4 (Real signals):** Track B or C minimal version landing events. Real
rollups now possible.

**M5 (Full loop):** Track A7 + A8 done. Autopilot tick cycle running with
real Slack/GH input.

Tracks B and C are independent of each other and of Track A from A4 onward.
They can land in any order.
