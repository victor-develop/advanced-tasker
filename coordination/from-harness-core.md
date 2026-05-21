# coordination/from-harness-core.md

**From:** harness-core-r3 (Track A)
**To:** slack-poller-r3 (Track B), github-poller-r3 (Track C), team lead
**Branch:** track-a/core-mvp
**Latest update:** 2026-05-22 (round 3)

## Status

Round 3 closeout of Track A is complete. The system is now safe-by-default
for the lead-driven real e2e against Slack + GitHub + Anthropic.

- `go build ./...` clean
- `go vet ./...` clean
- `go test ./...` passes (round-2 tests + round-3 D2–D5 additions)
- `scripts/acceptance-autopilot.sh` still passes (round-2 acceptance unchanged)

## Round 3 additions (D1–D6)

### D1 — Dashboard cheat sheet matches design/04 verbatim

`internal/render/dashboard.go::commandsHint` now emits the AVAILABLE
COMMANDS block exactly as it appears in design/04 §"AVAILABLE COMMANDS".
The obsolete `(MVP: dispatch/review/outbox/pin/triage not yet
implemented — see TASKS.md)` line was removed. Test:
`internal/render/dashboard_test.go::TestDashboard_CommandsCheatSheet`.

### D2 — Smoke unit tests on the 9 round-2 packages

Added `*_test.go` to: `internal/audit`, `internal/daemon`,
`internal/engine`, `internal/inbox`, `internal/outbox`,
`internal/sources`, `internal/telemetry`, `internal/threads`,
`internal/tick`. Each covers at least one happy-path invariant the
design contract relies on (idle counter, signal extraction, summary.log
line format, watch-list mutation, frontmatter parser roundtrip, etc.).

### D3 — Rollup `frontmatter.id == thread-dir` guard

Per merged design PR #5 § design/05 §"Step 0.5":

- New validator: `rollup.CheckFrontmatterID(r, threadDir)` — strict
  string equality.
- `harness rollup update <id> --file=<path>` runs this **before** the
  schema/ledger/pin checks. Mismatch → exit code 2, anomaly entry
  under `state/inbox/anomalies/`, no commit. The daemon must NOT retry
  on this rejection.
- `harness rollup verify-commit` (post-commit hook helper) also runs
  the check first, so a smuggled-in mis-id commit is reverted via
  `git reset --hard HEAD~1`.
- Anomaly format: `reason: "frontmatter.id (<id-in-file>) does not
  match thread directory (<dir-name>)"`.
- Tests:
  `internal/cli/posthook_test.go::TestRollupUpdate_FrontmatterIDMismatchRejected`
  and
  `internal/cli/posthook_test.go::TestPostCommitHookCatchesFrontmatterIDMismatch`.

### D4 — `harness boot` interactive first-run

New verb implemented in pure Go at `internal/cli/boot.go`. Behaviour:

1. Detects whether `--state-dir` (or `HARNESS_STATE`) points at an
   initialized harness; if not, prompts to run `harness init`.
2. Probes env-var presence: `ANTHROPIC_API_KEY`, `SLACK_BOT_TOKEN`,
   `GITHUB_TOKEN`. Emits per-var status lines. Boot does NOT validate
   tokens — driver-side validity checks live in
   `internal/llm/claude_p.go`.
3. If `SLACK_BOT_TOKEN` is set, seeds the slack source stub then
   prompts for a channel ID + optional reason. Skips entirely without
   token.
4. If `GITHUB_TOKEN` is set, same shape for `owner/repo`.
5. Always prompts for a first goal title (default `"My first goal"`)
   and runs the equivalent of `harness goal create`.
6. Always forces `outbox.sender_enabled=false`, commits the change,
   prints `[safety] outbox.sender_enabled set to false ...`.
7. Prompts to start autopilot (default no). Even on yes, boot only
   prints the autopilot command; it does NOT exec without explicit
   confirmation — and even after confirmation, the user runs the
   command themselves.
8. Prints a one-paragraph summary and exits 0.

Flags:
- `--non-interactive` — take defaults for every prompt (for fixtures /
  tests / CI).
- `--goal <title>` — override the default first-goal title.

Test: `internal/cli/boot_test.go::TestBoot_NonInteractive_*`.

### D5 — `outbox.sender_enabled` config key

Added at `outbox.sender_enabled` in `state/config.yaml`.

| state directory | default | source |
|---|---|---|
| Freshly `harness init`'d (round 3 or later) | `false` | `DefaultConfigYAML` |
| Pre-round-3 state (key absent) | `true` | `outbox.SenderEnabled` fallback |
| Explicit `false` in config.yaml | `false` | round-3 safety net active |
| Explicit `true` | `true` | operator opted in |

Behavior:

- When `false`: the outbox-sender daemon RUNS but never calls
  Slack/GitHub APIs. Items in `outbox/pending/` are MOVED to
  `outbox/awaiting-human/`. Stderr line:
  `outbox.sender_enabled=false: deferring O-<id> to awaiting-human`.
- When `true`: round-2 behavior unchanged.
- Operator opt-in:
  `harness config set outbox.sender_enabled true`.

Implementation: `internal/outbox/limits.go::SenderEnabled` is the
authoritative reader; `internal/daemon/outbox_sender.go::process`
gates on it before any rate-limit / duplicate / provider step.

Tests:
- `internal/outbox/outbox_test.go::TestSenderEnabled_DefaultsAndExplicit`
- `internal/cli/integration_test.go::TestOutboxSenderEnabledFalseDefersToAwaitingHuman`

### D6 — `docs/run-day-zero.md`

Operator walkthrough at the repo root (`docs/run-day-zero.md`). Covers
prereqs, install, `harness boot` session, what to expect during a
5-minute autopilot run, how to stop/inspect, and how to opt into real
sending. Operator-focused tone, no marketing.

## Cross-track confirmations

Per round-3 D6 ask:

- **harness-core reads `raw_inline` (not `raw_path`)** from
  `inbox/new/<id>.json` entries written by pollers, during both
  `harness triage <id> --action=track` and the equivalent
  `harness thread track <id>` path. The internal struct
  `inbox.Item.RawInline any` carries the inline payload; `RawPath` is
  retained as a JSON tag only for backwards compat with any pre-round-3
  fixtures. Pollers (Track B/C) should write `raw_inline` going forward.
- Promotion semantics in `triage track`: when an inbox/new item carries
  `raw_inline`, harness now writes that payload to
  `state/threads/<id>/raw/promoted-<unix-nano>.json` and removes the
  inbox/new entry. This is the design/02 §"Raw event location"
  transition row for `New, untracked → Tracked`.

## Files changed in round 3

| Path | Purpose |
|---|---|
| `internal/render/dashboard.go` | D1: AVAILABLE COMMANDS block updated; package doc cleaned |
| `internal/render/dashboard_test.go` | D1: cheat-sheet regression test |
| `internal/state/init.go` | D5: `outbox.sender_enabled: false` seeded into `DefaultConfigYAML`; package doc cleaned |
| `internal/outbox/limits.go` | D5: `SenderEnabled` reader |
| `internal/outbox/outbox_test.go` | D2 + D5: risk, rate-limit, SenderEnabled tests |
| `internal/daemon/outbox_sender.go` | D5: deferral gate, `ProcessOnce` test hook |
| `internal/daemon/daemon_test.go` | D2: bus, sleepCtx, sender stop, scheduler interval |
| `internal/rollup/rollup.go` | D3: `CheckFrontmatterID` validator |
| `internal/cli/rollup.go` | D3: wire Step 0.5 into update + verify-commit |
| `internal/cli/posthook_test.go` | D3: id-mismatch CLI + post-commit tests |
| `internal/cli/boot.go` | D4: new `harness boot` verb |
| `internal/cli/boot_test.go` | D4: non-interactive boot tests |
| `internal/cli/root.go` | D4: wire boot subcommand |
| `internal/cli/inbox.go` | raw_inline promotion in triage track |
| `internal/cli/integration_test.go` | D5: sender_enabled=false integration test |
| `internal/inbox/inbox.go` | `Item.RawInline` field; doc note about raw_inline |
| `internal/inbox/inbox_test.go` | D2: roundtrip + anomaly + bucket list |
| `internal/audit/audit_test.go` | D2: signal extraction smoke |
| `internal/engine/engine_test.go` | D2: idle counter increment/reset |
| `internal/sources/sources_test.go` | D2: watch idempotency + missing-config sentinel |
| `internal/telemetry/telemetry_test.go` | D2: summary.log roundtrip |
| `internal/threads/threads_test.go` | D2: meta + dirty + rollup parser |
| `internal/tick/tick_test.go` | D2: claim + log lifecycle |
| `docs/run-day-zero.md` | D6: operator walkthrough |
| `coordination/from-harness-core.md` | round-3 coordination notes (this file) |

## What did NOT change

- Design files under `design/` — untouched. (Hard constraint.)
- Track B / Track C `cmd/` and `internal/` — untouched. (Hard constraint.)
- `cmd/harness/main.go` — unchanged. The new `boot` verb wires through
  `internal/cli/root.go::New()`.
- Round-2 acceptance script (`scripts/acceptance-autopilot.sh`) — unchanged
  and still passing. The script never depended on `outbox/sent/`, so the
  new default `sender_enabled=false` does not regress it.
- LLM driver abstraction — `internal/llm/claude_p.go` still owns its own
  env-var check (`ANTHROPIC_API_KEY`). The autopilot / daemons remain
  driver-agnostic.

## No design ambiguities

I did not encounter any new design ambiguities in round 3 that the round-3
design PR (#5), round-2 design PR (#4), or PR #1 did not address.

## Notes on hard contracts honored

- Did NOT modify `cmd/` or `internal/` of track-b or track-c worktrees.
- Did NOT modify `design/` files.
- No `--force` pushes; the rebased branch was updated with
  `--force-with-lease`.
- No PRs merged.
- The default of `outbox.sender_enabled` for fresh init stays `false`.
- No `// TODO: implement` stubs introduced in code paths the design
  declares. (Inbox `RawPath` is kept as a JSON-tag for backwards-compat
  reads; the new write path uses `RawInline`.)

## Past coordination (round 2 archive)

Round-2 details were preserved in git history at commits before
`track-a-r3` work landed (see PR #1 round-2 thread). The key contracts
remain unchanged:

- `harness config init <source>` is idempotent.
- Slack/GitHub stub bodies match design/08 + design/09 verbatim.
- `harness watch slack-channel <id> [--reason]` appends `{id, reason?}`
  to `watch.channels`; `harness watch github-repo <owner/repo>` appends
  to `watch.repos` (string). Both idempotent.
- `harness init` does NOT seed source configs; pollers must error with
  the message `run \`harness config init <source>\` to seed config` if
  the file is absent.

Ready for the lead-driven real e2e.
