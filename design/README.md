# Design Documents

Read in this order:

1. **[01-overview.md](./01-overview.md)** — Problem, design principles,
   process topology. Start here.
2. **[02-state-and-schemas.md](./02-state-and-schemas.md)** — Every file
   in `state/` and its schema. The contract between all components.
3. **[03-harness-cli.md](./03-harness-cli.md)** — Full CLI command
   surface.
4. **[04-commander-tick.md](./04-commander-tick.md)** — Tick lifecycle
   and dashboard rendering.
5. **[05-rollup-updater.md](./05-rollup-updater.md)** — Rollup update
   contract + append-only ledger enforcement via git.
6. **[06-worker-protocol.md](./06-worker-protocol.md)** — Job/report
   schemas, dispatch/review flow.
7. **[07-outbox.md](./07-outbox.md)** — Outbox + risk model + revoke.
8. **[08-slack-poller.md](./08-slack-poller.md)** — Track B spec
   (isolated).
9. **[09-github-poller.md](./09-github-poller.md)** — Track C spec
   (isolated).
10. **[10-bootstrap-and-driver-modes.md](./10-bootstrap-and-driver-modes.md)**
    — Day-1 init flow + autopilot/manual/hybrid driver modes.

## How to use these docs

- If you're a **human reviewing the design**: read in order.
- If you're an **AI agent implementing a track**: read `01`, `02`, `03`,
  then the doc(s) for your track per [../TASKS.md](../TASKS.md). Cross-
  reference others as needed but stay in your lane.
- If you want to **change a schema or contract**: open a PR against the
  relevant doc *before* coding. The docs are the source of truth.

## What's NOT specified yet

These are deliberately open until first-implementation feedback:

- **LLM driver shimming.** How `autopilot` actually invokes `claude -p`
  vs. another agent vs. local OSS model. The interface is "render a
  prompt, capture an exit + report file". Track A implements one
  reference driver (claude-p); others can follow.
- **Authentication for shared state.** Multi-machine setups are described
  but the security model (who can claim, who can approve outbox) is
  treated as ambient — `state/` filesystem permissions, no in-band auth.
- **Metric / telemetry collection.** Useful but not v1. Tick logs + git
  history are the audit substrate.
- **Backup / restore.** `state/` is a git repo — push to a remote is the
  primary recovery story. Formal backup beyond this is unspecified.
