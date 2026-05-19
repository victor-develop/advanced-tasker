# advanced-tasker

A file-based harness for driving LLM agents through complex, long-running task
management. The core problem: LLM context windows are finite, but real work
spans many threads, PRs, and days. This system uses the filesystem as the
durable world model and treats each LLM activation as a bounded, stateless
"tick" against that model.

**Status:** Design phase. No code yet. See [design/](./design/) for the full
specification.

## In one paragraph

A long-running scheduler watches Slack channels and GitHub PRs and writes raw
events to disk. A lightweight summarizer LLM keeps a structured **rollup** per
tracked thread (the world model). A **commander** LLM is activated
periodically — it reads a rendered **dashboard** that compresses the entire
world into a fixed token budget, then issues actions via a `harness` CLI:
update tasks, rewire dependencies, dispatch worker LLMs, reply via outbox. All
side effects go through the CLI. All state lives in a git-tracked directory.
Workers run async; the commander never waits. The whole loop is driver-agnostic
— `claude -p`, another agent, or a human can pick it up at any time.

## Why

Real ops/SWE work has three properties that break naïve LLM agents:
1. **Long-horizon** — context window can't hold a week of Slack
2. **Multi-stream** — Slack threads, PRs, and tasks all evolve in parallel
3. **Mixed authority** — humans need to steer mid-flight without restarting

The harness handles all three by externalizing state to files, compressing
ruthlessly, and making every action go through a CLI that humans, daemons, and
LLMs can all use.

## Architecture (one screen)

```
┌──── Driver layer (replaceable) ───────────────────────────────────┐
│  claude -p autopilot  │  human at CLI  │  another agent  │  mix   │
└─────────────────────────────┬─────────────────────────────────────┘
                              │  (only via)
                              ▼
┌──── Protocol layer ───────────────────────────────────────────────┐
│  harness CLI  (Go binary, all side effects go here)                │
└─────────────────────────────┬─────────────────────────────────────┘
                              ▼
┌──── State layer (git-tracked) ────────────────────────────────────┐
│  state/threads/<id>/{rollup.md, raw/, meta.json}                   │
│  state/tasks/<id>/{goal.md, status.json, log.md, artifacts/}       │
│  state/inbox/{new,updates,human,agent-reports}/                    │
│  state/jobs/{pending,in-flight,done,failed}/                       │
│  state/outbox/{pending,awaiting-human,sent,failed}/                │
│  state/tick-log/<timestamp>.md                                     │
│  state/sources/{slack,github}/...cursors                           │
└────────────────────────────────────────────────────────────────────┘
                              ▲
                              │  (write only)
┌──── Ingestion layer (no LLM) ─────────────────────────────────────┐
│  slack-poller │ github-poller │ (extensible)                       │
└────────────────────────────────────────────────────────────────────┘
```

## Quickstart (planned)

```bash
harness init
harness config set slack.token ... github.token ...
harness watch slack-channel C0492
harness watch github-repo org/repo
harness goal create "first objective"
harness autopilot start
# or
harness pickup   # for manual / external agent driving
```

## For AI agents implementing this repo

See [AGENTS.md](./AGENTS.md) and [TASKS.md](./TASKS.md). Work is broken into
three parallel-implementable tracks:
- **Harness core** — the CLI, state schema, commander tick rendering
- **Slack poller** — Slack ingestion daemon
- **GitHub PR tracker** — GH PR ingestion daemon

Each track has an isolated spec under [design/](./design/) and well-defined
file-system contracts so they can be built independently.
