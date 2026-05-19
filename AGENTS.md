# AGENTS.md — For AI agents working on this repo

You are an AI agent picking up work on **advanced-tasker**, a file-based
harness for driving LLM agents through long-horizon task management. This
document tells you how to engage with this codebase. Read it before doing
anything else.

## What this repo is — in 60 seconds

This is **not** an LLM that does work. It is a **system around** an LLM that:
1. Captures signals from Slack/GitHub into files
2. Maintains compressed "rollups" of each tracked conversation/PR
3. Renders a fixed-budget dashboard for a periodic "commander" LLM tick
4. Lets the commander dispatch async worker LLMs and reply via outbox
5. Stores everything as git-tracked files so any agent or human can pick up

Every side effect goes through a single Go CLI: `harness`. State lives in a
git repo at `state/` (separate from this source repo). LLMs are pluggable
drivers, not core to the architecture.

## Required reading order

1. [README.md](./README.md) — pitch + architecture diagram
2. [design/01-overview.md](./design/01-overview.md) — design principles, why
   things are the way they are
3. [design/02-state-and-schemas.md](./design/02-state-and-schemas.md) — the
   `state/` directory and every file/YAML schema
4. [design/03-harness-cli.md](./design/03-harness-cli.md) — the CLI surface
5. The doc specific to your track (see [TASKS.md](./TASKS.md))

## Engagement rules

### Stay in your track
Work is split into three parallelizable tracks: **harness core**,
**slack poller**, **github poller**. Each has a dedicated design doc and a
clean filesystem contract with the others. **Do not cross track boundaries
unless coordinating.** If you find an inter-track contract issue, open an
issue/PR against the spec docs first.

### The schemas are the contract
The YAML/JSON schemas in [design/02-state-and-schemas.md](./design/02-state-and-schemas.md)
are load-bearing. They are how independent components communicate. **If you
need to change a schema, update the design doc in the same PR.** A
schema-breaking change without doc update is a regression.

### Side effects go through the CLI
Even tests should prefer running `harness <cmd>` over poking files directly.
The CLI is where validation, locking, and git commits live. Bypassing it
defeats the whole point of the design.

### Don't add what isn't specified
This system has a known failure mode: every feature you bolt on costs the
commander tokens. Resist scope creep. If a feature isn't in the design docs,
flag it for discussion before building.

### Tests over READMEs
Each track should have integration tests that exercise its filesystem
contract. A passing test that says "writes valid `raw/<event-id>.json` files"
is worth more than prose explaining intent. Aim for tests that another agent
could run to verify the contract.

### Git hygiene
- One feature/spec per PR
- Commit messages: `<track>: <imperative summary>` (e.g.
  `slack-poller: handle thread_ts cursor advance`)
- Don't `git push --force` to main
- The `state/` directory itself uses git as a substrate at runtime — that is
  a *different* git repo from this source repo. Don't conflate them.

## When you're stuck

The design docs are the source of truth for intent. If they're ambiguous or
contradictory, **stop and surface it**. Don't guess and code your way into a
divergent implementation that has to be undone later.

## Language and tooling

- **Go** for the harness CLI and pollers
- YAML for config and structured documents
- Markdown for rollups, goals, logs, tick logs
- The `state/` runtime repo uses plain git (no LFS, no submodules)
