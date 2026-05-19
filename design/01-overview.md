# 01 — Overview and Design Principles

## The problem

Real long-horizon agent work has three properties that defeat naïve "give an
LLM a tool and a goal" approaches:

1. **Long horizon.** A week of Slack threads + PR conversations does not fit
   in any context window. Summarization helps but drifts.
2. **Many parallel streams.** Slack threads, PRs, and internal tasks all
   evolve independently. A single agent that tries to hold them all loses
   coherence.
3. **Mixed authority.** Humans need to steer mid-flight (bump priority,
   redirect, override decisions) without restarting the agent or rebuilding
   context.

Existing autonomous-agent designs assume a single LLM with growing memory.
That breaks down at week-3.

## The approach in one sentence

**Externalize the world model to a git-tracked filesystem, treat each LLM
activation as a stateless tick against that model, and gate every side
effect through a single CLI.**

## Roles, in order of activation frequency

| Role | Frequency | Model class | Purpose |
|---|---|---|---|
| Pollers (no LLM) | Continuous | — | Capture Slack/GH events into raw/ |
| Rollup updater | Every Δ ≥ threshold | Small (Haiku-class) | Maintain structured per-thread summary |
| Worker | On dispatch | Mid (Sonnet-class) | Execute a bounded task and return a structured report |
| Commander | Periodic + event-triggered | Top (Opus-class) | Re-survey the world, reshape plans, dispatch workers |

The commander is the expensive one and runs **least often**. Most cycles are
small-model summarization and IO.

## Core architectural principles

### P1. State lives in files, not in memory
The world model is a git-tracked directory. The LLM is *replaceable*. If the
process dies, another agent (human or otherwise) can pick up `state/` and
keep going.

### P2. Each LLM activation is a bounded tick
Every commander activation is a single `claude -p` (or equivalent) call.
There is no "running session." The phase script for a tick is fixed:
**Survey → Drill → Reconcile → Act → Log → Exit**. See
[04-commander-tick.md](./04-commander-tick.md).

### P3. Commander surveys everything, every tick
Counterintuitive but load-bearing: every activation re-reads the whole
dashboard, re-evaluates dependencies, re-prioritizes. Token budget pressure
forces dashboard compression, not lazy queue-popping. The commander's job is
*re-calibration*, not just dispatch.

### P4. Compression is the central engineering problem
The quality of the dashboard determines the quality of the commander.
Rollups are not free-form prose; they are structured fields with explicit
slots (state, ask, decisions ledger, pinned verbatim). See
[05-rollup-updater.md](./05-rollup-updater.md).

### P5. Append-only ledgers beat mutable summaries
Decisions, once made, do not get rewritten — only superseded. This prevents
silent drift in the world model. Git provides the audit trail and the
enforcement mechanism (commit-hook diff validation).

### P6. Workers never wait, commanders never wait
The commander dispatches and exits. The worker runs async, writes a report,
signals via `inbox/agent-reports/`. The next commander tick sees it. No
agent blocks on another.

### P7. All side effects go through one CLI
Whether the actor is `claude -p`, another agent, or a human, the only way
to change state is `harness <cmd>`. The CLI is where validation, locking,
git commits, and audit live.

### P8. Workers see a curated, narrow context
A worker's input is a job file with an explicit whitelist of rollups and
files. It cannot wander the state tree. This both saves tokens and
prevents scope creep.

### P9. The outbox is the only external speaker
Anything that touches the outside world (Slack reply, PR comment) goes
through `state/outbox/`, has a risk classification, and may require human
approval. Workers cannot speak directly; they propose, commander or human
approves.

### P10. Driver-agnostic
The system runs the same whether autopilot daemons drive it, a human types
CLI commands, or a passing agent picks it up via `harness pickup`. Three
modes — `autopilot`, `manual`, `hybrid` — toggle daemon behavior without
touching state.

## Process topology

```
   ┌──────────────────┐         ┌──────────────────┐
   │ slack-poller     │         │ github-poller    │
   │ (Track B)        │         │ (Track C)        │
   └────────┬─────────┘         └─────────┬────────┘
            │  writes raw/ + .dirty       │
            ▼                              ▼
   ┌─────────────────────────────────────────────┐
   │  state/  (git repo)                         │
   │  threads/ tasks/ inbox/ jobs/ outbox/ ...   │
   └────────┬──────────┬──────────┬──────────────┘
            │          │          │
   reads    │          │          │  reads + writes
            ▼          ▼          ▼
   ┌─────────────┐ ┌──────────┐ ┌──────────┐
   │ rollup      │ │ worker   │ │ commander│
   │ updater     │ │ (async)  │ │ (tick)   │
   │ (daemon)    │ │ (daemon) │ │          │
   └─────────────┘ └────┬─────┘ └────┬─────┘
                        │            │
                        └──┬─────────┘  side effects via
                           ▼            harness CLI only
                     ┌──────────────┐
                     │ harness CLI  │
                     │ (Track A)    │
                     └──────┬───────┘
                            ▼
                     ┌──────────────┐
                     │ outbox       │
                     │ (sends to    │
                     │ Slack/GH)    │
                     └──────────────┘
```

## What this design explicitly is NOT

- **Not a chat UI.** No human-LLM conversation interface. Humans interact via
  `harness` CLI and by reading state/.
- **Not a single-agent system.** Roles are intentionally separated.
- **Not a long-running LLM session.** Each tick is a fresh process.
- **Not provider-locked.** `claude -p` is the reference driver; any agent
  that can read prompts and call a CLI works.

## Known open questions (tracked, not blockers)

- **TTL on human pins** — should pinned instructions auto-expire? Current
  default: 7 days, configurable. Subject to revision after first real usage.
- **Implicit dependency detection** — currently the commander handles this
  during its full survey. A dedicated linter agent may be added if the
  commander struggles at scale.
- **Multi-commander coordination** — current design assumes one commander
  lock. Multiple specialized commanders (e.g., one per domain) is a future
  consideration, not v1.
