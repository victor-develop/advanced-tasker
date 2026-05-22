# Coordination notes from slack-poller (Track B, rounds 2 + 3)

Filed in-tree because `SendMessage` / `TaskList` / `TaskUpdate` were not
available in this session (confirmed via `ToolSearch select:...` returning
only `EnterWorktree` in round 2 and only `EnterWorktree` again in round 3).
The team lead (or whoever wires up the harness coordination channel) can
lift these into the shared message log.

> **Round 3 update (see end of doc for the full §"Round 3 additions"
> section).** The slack-poller now ships a `doctor` first-boot sanity
> subcommand, harder operator-actionable auth-fail messages, and a real
> Slack acceptance script for the lead-driven e2e. The mock acceptance
> golden hash is unchanged (`a1bcd615...d5da`).

Round-2 scope delivered: B1 (config init integration), B2-B4 verified
post-rebase, B5 lifecycle CLIs (watch/unwatch/track-thread/untrack-thread/
status/force-poll), graceful SIGTERM, rate-limit handling, anomaly
writing, and a golden-snapshot acceptance script. All five round-1
integration tests still pass; B5 + graceful + rate-limit + golden tests
added in round 2.

Branch: `track-b/slack-mvp` (PR #2). Rebased onto `origin/main` after
design PR #4 merged; no conflicts.

---

## To: harness-core-r2 (Track A)

### 1. Subject: schemas the slack-poller writes (exact field set)

These are the filesystem contracts you can rely on. All JSON files use
2-space indented, trailing-newline output (Go `json.MarshalIndent` +
`\n`). All file writes are atomic (write to `.tmp` → fsync → rename).

#### `state/threads/slack-<channel>-<thread_ts>/raw/<ts>.json`

```json
{
  "id": "slack-C0492-1700000000.000000",
  "source": "slack",
  "captured_at": "2026-05-19T12:21:18Z",
  "channel": "C0492",
  "ts": "1700000050.000000",
  "thread_ts": "1700000000.000000",
  "user": "U_BOB",
  "user_name": "",
  "text": "acceptance reply",
  "blocks": null,
  "reactions": null,
  "subtype": "",
  "is_top_level_in_thread": false,
  "permalink": "https://acme.slack.com/archives/C0492/p1700000050000000"
}
```

- `id` is the parent thread ID (matches the dir name), not the per-event
  ID. (This matches design/08 §"File contents" — the `id` example there
  uses the parent thread's id.)
- `blocks` and `reactions` are `json.RawMessage` — present only when
  Slack returned non-empty values (`omitempty`).
- `user_name`, `subtype` are present-but-empty when omitted, since they
  are not on the omitempty path. `text` is always present.
- `is_top_level_in_thread` is `true` for the thread root, `false` for
  replies. Set deterministically from `ts == thread_ts`.

#### `state/threads/slack-<channel>-<thread_ts>/meta.json`

```json
{
  "id": "slack-C0492-1700000000.000000",
  "source": "slack",
  "url": "https://acme.slack.com/archives/C0492/p1700000050000000",
  "created_at": "2023-11-14T22:14:10Z",
  "last_event_at": "2023-11-14T22:14:10Z",
  "owner_task": null,
  "participants": ["U_BOB"],
  "tracking_since": "2026-05-19T12:21:18Z"
}
```

- `owner_task` is always `null` from the poller; the commander fills it
  via `harness thread link`.
- `participants` is a sorted dedup'd union over all observed users.
- `tracking_since` is wall-clock at first write. Subsequent updates only
  touch `last_event_at`, `url` (if previously empty), and `participants`.
- `created_at` is derived from the earliest Slack `ts` seen — converted
  via `time.Unix(int64(f), int64((f-floor(f))*1e9)).UTC().Format(RFC3339)`.

#### `state/threads/slack-<channel>-<thread_ts>/.dirty`

Empty file. Presence + mtime are the signal. Cleared by your rollup
updater. We `Chtimes` to refresh mtime on every touch.

#### `state/inbox/new/slack-<channel>-<ts>.json`

```json
{
  "id": "slack-C0492-1700000100.000100",
  "source": "slack",
  "kind": "new",
  "received_at": "2026-05-19T12:21:18Z",
  "summary": "acceptance new message",
  "ref": {
    "channel": "C0492",
    "ts": "1700000100.000100",
    "thread_ts": null,
    "user": "U_ALICE"
  },
  "raw_inline": {
    "permalink": "https://acceptance.test/...",
    "text": "acceptance new message"
  }
}
```

- `kind` is always `"new"`. (design/02 enum: `new | update | human-directive |
  agent-report | anomaly`.)
- `ref.thread_ts` is `null` for true top-level messages and the parent
  `thread_ts` for replies to an untracked thread (we land both in
  inbox/new and let the commander decide whether to promote).
- `raw_inline` always contains `text` and `permalink`. May contain
  `blocks` and `reactions` if Slack returned them. **Note**: design/02 has
  a `raw_path` field; we instead use `raw_inline` per design/08, which
  says the new-thread case has no raw/ entry yet. Track A's
  `harness triage` should read `raw_inline` (not `raw_path`) for slack
  inbox/new items, OR we can add a `raw_path` shim that points to a
  yet-to-exist file — please advise via the lead if you need a different
  shape.

#### `state/inbox/updates/slack-<thread-id>-<latest-ts>.json` (optional, gated by `write_update_pings`)

```json
{
  "id": "slack-C0492-1700000000.000000-1700000050.000000",
  "source": "slack",
  "kind": "update",
  "received_at": "2026-05-19T12:21:18Z",
  "thread_id": "slack-C0492-1700000000.000000",
  "latest_ts": "1700000050.000000",
  "new_count": 1
}
```

Disabled by default. Operators flip `write_update_pings: true` in
`state/sources/slack/config.yaml` to enable. One file per (thread, poll
cycle); subsequent identical pings dedup via the filename.

#### `state/inbox/anomalies/slack-<scope>.json` (new in round 2)

```json
{
  "id": "slack-channel-access-C0492",
  "source": "slack",
  "kind": "channel_access",
  "scope": "channel:C0492",
  "reason": "slack reported: not_in_channel",
  "occurred_at": "2026-05-19T12:30:00Z",
  "channel": "C0492"
}
```

ID formats by kind:

- `channel_access` → `slack-channel-access-<channel>`
- `rate_limit_exhausted` → `slack-rate-limit-exhausted-<scope>`
- `malformed_message` → `slack-malformed-<channel>-<ts>` (with `.` and
  `-` preserved)
- `thread_not_found` → `slack-thread-not-found-<thread-id>`

The marker file IS the signal — re-occurrences of the same anomaly are
no-ops. The poller mutes the source for this process lifetime
(`channel_access`) or simply continues with backoff
(`rate_limit_exhausted`); we do not mutate `config.yaml`.

#### `state/sources/slack/cursors/channels/<channel>.json`

```json
{ "last_ts": "1700000100.000100" }
```

#### `state/sources/slack/cursors/threads/<thread-id>.json`

```json
{ "last_reply_ts": "1700000050.000000" }
```

All cursor writes are crash-safe (`.tmp` → fsync → rename).

### 2. Subject: `harness config init slack` stub compatibility

Per design/03 + design/10 + the brief, my loader exits 1 with the
literal message:

```
run 'harness config init slack' to seed config
```

…when `state/sources/slack/config.yaml` is missing. This wording is the
contract — please don't paraphrase it on your side either. My loader
expects the following YAML shape (lifted from
`internal/slack/example_config.yaml`, which is the canonical reference):

```yaml
auth:
  token_env: SLACK_BOT_TOKEN     # required (or token_file / token)
watch:
  channels: []                   # populate via `harness watch slack-channel <id>`
                                 # or `slack-poller watch <id>`
poll_interval: 30s
max_concurrent_thread_polls: 4
backoff:
  on_rate_limit: 60s
  on_error: 5s
  max_backoff: 5m
# write_update_pings: false      # optional; default false
```

**Required keys for compatibility:** `auth.token_env`, `watch.channels`.
Everything else has a defaults path in `(*Config).applyDefaults`. If
`watch.channels` is empty, the poller logs a warning but does not fail
(the brief says fail; design/08 §Validate says "almost certainly a
misconfiguration on first start … log a warning instead of failing").
Please match this in your stub generator: emit `watch: {channels: []}`
rather than omitting `watch` entirely.

**I cannot read `coordination/from-harness-core.md`** at the time of
writing — your branch did not have it yet when I rebased. If your stub
differs from the shape above (extra/renamed keys), please flag in your
own coordination notes; I have NOT silently widened the loader to
accommodate unknown stubs.

### 3. Subject: do I read `state/config.yaml`?

**No.** The `slack-poller` binary reads only
`state/sources/slack/config.yaml`. It does NOT read or depend on
`state/config.yaml` for global limits. If you later need the poller to
honor a global `limits.outbox_per_thread_per_hour` or similar, that's a
new feature for round 3, not round 2.

### 4. Subject: `harness watch slack-channel <id>` interop

I also implement `slack-poller watch <channel-id>` and
`slack-poller unwatch <channel-id>`, both mutating
`state/sources/slack/config.yaml`. If Track A's
`harness watch slack-channel <id>` also writes to that file, please use
the same YAML shape (especially `watch.channels[].id` and
`watch.channels[].reason`). My code preserves all keys it doesn't
understand on rewrite (via `yaml.Unmarshal` → `yaml.Marshal`), so
forward-compat should be cheap.

### 5. Subject: `harness thread track` interop

design/08 says `harness thread track` (yours) and `slack-poller
track-thread` (mine) are alternative entry points. Mine reads
`state/inbox/new/slack-<C>-<ts>.json`, writes
`state/threads/slack-<C>-<ts>/raw/<ts>.json` + `meta.json` + `.dirty`,
then deletes the inbox entry. If yours does anything else (e.g. emits a
git commit), my code does not — operators using my CLI must commit
manually afterwards. (My CLI does not touch git, by design — the poller
binary is git-naive.)

---

## To: github-poller-r2 (Track C)

### Namespace discipline confirmations

1. **I only ever write entries whose top-level filename or directory
   matches `^slack-`.** I have verified this manually and via the
   acceptance test (only `slack-*` paths appear under the produced
   `state/`). My ID format is the literal `slack-<channel-id>-<thread_ts>`,
   with `-` separators. Channel IDs in Slack don't contain `-` (they
   are uppercase alphanumeric, like `C0492`), so the `LastIndex("-")`
   split is safe.

2. **I never write under `state/inbox/{new,updates,anomalies}/github-*`.**
   The Writer's path constants are hard-coded with the `slack-` prefix
   baked into the `id`, so there is no execution path that produces a
   `github-` filename. (See `internal/slack/writer.go` —
   `WriteInboxNew` uses `item.ID + ".json"`, and the only place that ID
   is constructed is `buildInboxItem(...)` which prepends `slack-`.)

3. **Updates ping naming:** I use `slack-<thread-id>-<latest-ts>.json`,
   which itself starts with `slack-` (the `thread-id` already has the
   `slack-` prefix). So the full filename like
   `slack-C0492-1700000000.000000-1700000050.000000.json` is always
   `slack-` prefixed. We do not collide with your `github-*` paths.

4. **Anomalies:** I introduce `state/inbox/anomalies/slack-*.json`. Your
   round-1 notes mention `state/inbox/anomalies/github-*` for tracked-PR
   404s — same directory, distinct prefixes, no collision risk.

If you ever find a `non-^slack-` file under one of the shared dirs that
my code wrote, that's a bug — please ping me and I will fix.

---

## To: harness-core-r2 — `slack-poller force-poll` semantics

`slack-poller force-poll [<channel-id> | <thread-id>]` triggers exactly
one poll cycle bypassing the schedule. The arg distinguishes
channel-vs-thread by the `slack-` prefix:

- `slack-poller force-poll C0492` → only that channel polled (no thread
  polls)
- `slack-poller force-poll slack-C0492-1700000000.000000` → only that
  thread polled (no channel polls)
- `slack-poller force-poll` (no arg) → equivalent to `--once`

`status [--json]` prints config path, tracked channels with cursor +
last poll time (cursor file mtime), tracked threads with cursor +
last_event_at + owner_task. `--json` emits a single `StatusReport`
JSON object — see `internal/slack/cli/status.go` for the schema. Useful
for `harness doctor` to compose with.

---

## DESIGN AMBIGUITY — LEAD MUST RESOLVE

(None found during round-2 implementation that prevented progress. The
following are documentation gaps I worked around but flag for awareness;
none require an unblock.)

### 1. design/02 §"inbox/<bucket>/<id>.json" `raw_path` vs design/08 `raw_inline` — RESOLVED (round 3)

**Resolved by design PR #5 (round-3 clarifications, merged).** design/02
§"inbox/<bucket>/<id>.json" now shows `raw_inline: { text, permalink }`
and adds a new §"Raw event location — inbox/new vs threads/" subsection
explicitly stating that pollers MUST use `raw_inline` for the
new-untracked stage. My round-2 implementation was already aligned; the
round-3 D1 verification adds an explicit test
(`internal/slack/raw_inline_alignment_test.go`) that loads a
poller-produced inbox/new entry and asserts the JSON contains
`raw_inline.text` + `raw_inline.permalink` and does NOT contain
`raw_path`. No code change needed.

### 2. `harness config init slack` exact stub shape — RESOLVED

**Update after Track A pushed `coordination/from-harness-core.md`:**
Track A's stub is

```yaml
auth:
  token_env: SLACK_BOT_TOKEN
watch:
  channels: []
poll_interval: 30s
```

This is **compatible** with my loader. `applyDefaults` fills the missing
`max_concurrent_thread_polls` (4) and `backoff.{on_rate_limit,on_error,
max_backoff}` (60s, 5s, 5m). My `Validate` only fails on a non-empty
channel with empty id, so `watch.channels: []` validates fine. No loader
change needed.

Their `harness watch slack-channel <id> [--reason ...]` writes
`{id, reason?}` mappings into `watch.channels`, matching my
`WatchedChannel` struct. Atomic-write semantics not explicitly
documented on their side, but they git-commit each mutation so torn
writes can be diagnosed via `git status` / `git diff`. My
`slack-poller watch/unwatch` uses `SaveConfig` which does atomic
`.tmp` → rename. Mixing both verbs on the same machine should be safe.

### 3. `slack-poller status` and `harness watch` ownership overlap

design/03 §"Source management (poller-facing)" defines `harness watch
slack-channel <id>`; design/08 §"Tracking lifecycle commands" defines
`slack-poller watch <id>`. Both mutate the same file. The brief says
both must exist. I implemented both endpoints on my side; please make
sure yours does the same atomic-write semantics so concurrent invocation
doesn't tear the YAML. (My `SaveConfig` uses the same atomic-rename as
cursors.)

---

## Round-2 deliverable inventory (PR #2)

- `cmd/slack-poller/main.go` — thin wrapper, delegates to `internal/slack/cli`.
- `cmd/acceptance-slack-poller/main.go` — deterministic golden-snapshot driver.
- `internal/slack/cli/` — cobra subcommand tree (watch, unwatch,
  track-thread, untrack-thread, status, force-poll, plus the default
  poll/daemon action).
- `internal/slack/{config,client,poller,writer,cursors,anomaly,
  normalize}.go` — package internals.
- `internal/slack/cli/{cli_test.go, integration_test.go}` — unit + B5 +
  graceful + rate-limit + golden tests.
- `internal/slack/testdata/golden/snapshot.hash` — pinned golden hash.
- `scripts/acceptance-slack-poller.sh` — acceptance script
  (Definition-of-Done #5).
- `internal/slack/example_config.yaml` — canonical stub format
  reference for Track A.

## Definition-of-done evidence

```
$ go build ./...
$ go vet ./...
$ go vet -tags=integration ./...
$ go test ./...
ok  	github.com/victor-develop/advanced-tasker/internal/slack
ok  	github.com/victor-develop/advanced-tasker/internal/slack/cli
$ go test -tags=integration ./...
ok  	github.com/victor-develop/advanced-tasker/internal/slack
ok  	github.com/victor-develop/advanced-tasker/internal/slack/cli
$ bash scripts/acceptance-slack-poller.sh
==> building slack-poller...
==> running acceptance driver...
computed hash: a1bcd61571d69e299b765e18c6413c3d78cca741bf1e227ab4bf89af9f14d5da
PASS: hash matches golden
==> OK
```

Reproduced clean across 3 consecutive invocations of the acceptance
script.

---

# Round 3 additions

PR #2 stays on `track-b/slack-mvp`, rebased onto `origin/main` after design
PR #5 merged (no conflicts; PR #5 touched only `design/*`). Four
deliverables from round-3 brief §D1–D4:

## D1. `raw_inline` alignment verified against merged design PR #5

Design PR #5 unified design/02 with design/08 in favor of `raw_inline`.
The round-2 implementation was already correct (`raw_inline` field tag on
`InboxItem.RawInline`; no `raw_path` anywhere in the source tree).

**Verification:**

- `grep -rn "raw_inline\\|raw_path" --include="*.go"` shows zero
  `raw_path` references; the only matches are `raw_inline` in
  `internal/slack/writer.go` (struct tag), `internal/slack/cli/track.go`
  (consumer for promotion), and comments / doc strings.
- New test `internal/slack/raw_inline_alignment_test.go`:
  - `TestRawInlineAlignmentWithDesign` writes an inbox/new entry via
    `Writer.WriteInboxNew`, reads it back, generically unmarshals the
    JSON, and asserts:
    1. `raw_path` key is absent
    2. `raw_inline` key is present and is a JSON object
    3. `raw_inline.text` is a non-empty string
    4. `raw_inline.permalink` is a non-empty string
  - `TestInboxItemMarshalUsesRawInlineFieldName` guards the struct tag
    on `InboxItem.RawInline` against accidental rename to `raw_path`.

Golden mock-acceptance hash unchanged
(`a1bcd61571d69e299b765e18c6413c3d78cca741bf1e227ab4bf89af9f14d5da`)
since no on-disk output for the canned scenario changed.

## D2. `slack-poller doctor` subcommand — for the lead's e2e

**New cobra subcommand.** Performs first-boot sanity checks against the
configured Slack workspace. The lead's REAL e2e against channel
`C0B071K1SFP` should call this before `slack-poller --once`.

### Signature

```
slack-poller doctor [--json]

global flags:
  --state-dir <path>     state/ directory (env HARNESS_STATE; default ./state)
  --api-url <url>        override slack.com (testing)
  --log-level <level>    debug|info|warn|error (default info)
```

### Exit codes

| Code | Meaning |
|---|---|
| 0 | all checks pass |
| 1 | hard failure (token missing/invalid, channel inaccessible, missing scope) |
| 2 | soft signal only (no channels configured, channel exists but history is empty) |

### Checks (in order)

1. **Token check** — resolve via the same path the daemon uses:
   `auth.token_env`, then `auth.token`, then `auth.token_file`. Reports
   the source as `env $SLACK_BOT_TOKEN` / `config auth.token` / `file
   <path>` and the token byte length on success.
2. **Auth check** — calls Slack's `auth.test`. Reports
   `authenticated as <bot_name> in workspace <team_name>` on success.
   On failure, reports the Slack error code (`invalid_auth`,
   `token_revoked`, `missing_scope`, etc.).
3. **Channel ping** — calls `conversations.info` for
   `watch.channels[0]`. Reports `[ok] channel <id> (<name>) -- bot has
   access` on success; on `not_in_channel`, prints `hint: invite the
   bot via /invite @<bot> in #<channel>`.
4. **History ping** — calls `conversations.history?limit=1` for the
   same channel. An empty result is reported as a soft signal (exit 2),
   not a failure.

### Output (human-readable)

```
config: /path/to/state/sources/slack/config.yaml
[ok]   token from env $SLACK_BOT_TOKEN (length=64)
[ok]   authenticated as tasker-bot in workspace Acme
[ok]   channel C0B071K1SFP (project-ai-pioneers) -- bot has access
summary: all checks passed (exit 0)
```

### Output (`--json`)

```json
{
  "config_path": "...",
  "token":    { "ok": true, "source": "env $SLACK_BOT_TOKEN", "length": 64 },
  "auth":     { "ok": true, "bot_name": "tasker-bot", "team_name": "Acme",
                "bot_id": "B0...", "user_id": "U0..." },
  "channels": [
    { "id": "C0B071K1SFP", "name": "project-ai-pioneers", "ok": true,
      "bot_has_access": true, "history_ok": true }
  ],
  "summary":  "all checks passed",
  "exit_code": 0
}
```

The schema is `DoctorReport` in `internal/slack/cli/doctor.go`. Tests:
`internal/slack/cli/doctor_test.go` covers happy path, token-missing,
auth-fail (invalid token), channel-not-found, not-in-channel-with-hint,
empty-history soft exit, no-channels-configured soft exit, missing_scope
extraction, and JSON output round-trip.

## D3. Actionable auth-fail error messages

### Stderr line on auth failure (exit 3)

Updated `internal/slack/cli/root.go` Execute() to route auth-fail errors
through a new `FormatAuthError` helper. Mapping:

| Slack error code | Stderr line | Exit |
|---|---|---|
| `invalid_auth`, `token_revoked`, `token_expired`, `not_authed`, `account_inactive` | `slack-poller: token invalid (check SLACK_BOT_TOKEN, bot must be in channel)` | 3 |
| `missing_scope` (with `needed:` clause) | `slack-poller: missing scope: <scope> (grant via Slack app config and reinstall)` | 3 |
| Any other error | `slack-poller: <verbatim error>` | per CommandError |

### `not_in_channel` → operator-actionable anomaly

`internal/slack/poller.go` now routes `not_in_channel` through a new
`formatChannelAccessReason` helper. The on-disk anomaly file
(`state/inbox/anomalies/slack-channel-access-<channel>.json`) reason
field reads:

```
bot not in channel; invite via /invite @<bot> in #<channel>
```

Previously: `slack reported: not_in_channel` (less actionable). The
poller still disables the channel for the rest of the process lifetime;
other watched channels keep polling.

### `missing_scope` → exit 3 with scope name

`internal/slack/client.go` exports `MissingScope(err) string` which
parses slack-go's error text `"missing_scope, needed: <scope>, provided:
<other>"`. The CLI uses this to inject the missing scope name into the
stderr line on exit.

### Tests

- `internal/slack/cli/auth_errors_test.go`:
  `TestFormatAuthError_InvalidAuth` (covers all 5 fatal auth codes),
  `TestFormatAuthError_MissingScope`,
  `TestFormatAuthError_NonAuthErrorPassThrough`,
  `TestFormatAuthError_NilSafe`, `TestAuthFailCode`,
  `TestMissingScope_Extraction`.
- `internal/slack/anomaly_actionable_test.go`:
  `TestFormatChannelAccessReason_NotInChannel` (exact string match),
  `TestFormatChannelAccessReason_ChannelNotFound`,
  `TestFormatChannelAccessReason_Unknown`,
  `TestAnomalyChannelAccess_FileContents` (writes through the writer
  and asserts JSON on disk).
- `internal/slack/cli/doctor_test.go`: `TestDoctor_AuthFail_InvalidToken`
  drives the doctor against a stub client returning `invalid_auth`
  and asserts the stdout summary includes the canonical line.

## D4. Acceptance split — mock + real

### `scripts/acceptance-slack-poller.sh` (unchanged from round 2)

Still runs the deterministic httptest-backed driver via
`cmd/acceptance-slack-poller`. Golden hash
`a1bcd61571d69e299b765e18c6413c3d78cca741bf1e227ab4bf89af9f14d5da`
unchanged after the round-3 changes — verified post-rebase and
post-implementation.

### `scripts/acceptance-slack-poller-real.sh` (new)

Hits the REAL Slack workspace. **Skips cleanly with exit 0 + a clear
message when `SLACK_BOT_TOKEN` or `TEST_SLACK_CHANNEL` is unset, so CI
default behavior is unchanged.**

Steps when env is present:

1. Build the binary into a tmp dir
2. Mkdir a fresh state dir + write `state/sources/slack/config.yaml`
   with `auth.token_env: SLACK_BOT_TOKEN` and
   `watch.channels: [{id: $TEST_SLACK_CHANNEL}]`. (We deliberately do
   NOT shell out to `harness config init slack` — that would couple
   tracks; cf. round-2 coordination §2 on the canonical config shape,
   which we mirror verbatim.)
3. Run `slack-poller doctor` — exit non-zero on hard failure (mapped
   to exit code 3 in the script)
4. Run `slack-poller --once`
5. Assert:
   - `state/sources/slack/cursors/channels/$TEST_SLACK_CHANNEL.json`
     exists and `last_ts` is non-empty
   - At least one of: `state/inbox/new/slack-$TEST_SLACK_CHANNEL-*.json`
     OR `state/threads/slack-$TEST_SLACK_CHANNEL-*/` (both are valid
     depending on channel activity; empty channel is allowed)
   - If a sample inbox/new entry exists, it contains `"raw_inline"`
     and NOT `"raw_path"` (D1 schema check applied to real output)
6. Lint the compiled binary for forbidden Slack endpoint references
   (`chat.postMessage`, `chat.update`, `chat.delete`, `reactions.add`,
   `reactions.remove`) via `strings <binary> | grep`. ZERO matches
   required.
7. Print a summary of every file written under the temp state dir;
   exit 0.

### Script-level lint test

`internal/slack/cli/scripts_lint_test.go`
`TestRealAcceptanceScriptIsReadOnly` reads
`scripts/acceptance-slack-poller-real.sh` and asserts:

- the file exists and is executable
- the skip-if-missing-env path is present (`SLACK_BOT_TOKEN`,
  `TEST_SLACK_CHANNEL`, `SKIP:`, `exit 0`)
- it invokes `slack-poller doctor` and `--once`
- it contains NO `curl chat.postMessage`-style invocations (we do allow
  the endpoint names to appear in the explicit `forbidden_endpoints`
  deny-list array that the script greps the binary against — that is
  the read-only invariant check itself)

---

# To: harness-core-r3

## Namespace discipline (still holds)

All files written by `slack-poller` continue to have basenames or
directory names matching `^slack-`. Round-3 additions (doctor command,
acceptance-slack-poller-real script) write nothing — doctor is
read-only against Slack; the real-acceptance script writes only into
its own tmp state dir.

## Doctor signature for harness composability

If `harness doctor` wants to compose doctor results from each poller,
`slack-poller doctor --json` emits the `DoctorReport` JSON shown in §D2
above. Schema is stable for round 3; if it needs to change for round 4
we'll bump a version field.

## Stub config compatibility (unchanged)

Round-2 coordination §2 still applies: `auth.token_env`,
`watch.channels`, `poll_interval` are the required keys; everything
else has a defaults path. Track A's `harness config init slack` stub
remains compatible.

---

# To: github-poller-r3

## Namespace discipline (still holds)

All files written by `slack-poller` continue to match `^slack-`. The
round-3 additions (doctor + real acceptance) do not write under
`state/inbox/`, `state/threads/`, or `state/sources/github/`. The
doctor subcommand is read-only against Slack and writes nothing locally.

No collision risk under `state/inbox/anomalies/` — slack anomalies all
start with `slack-`, e.g. `slack-channel-access-C0492.json`,
`slack-rate-limit-exhausted-<scope>.json`.

---

# DESIGN AMBIGUITY — LEAD MUST RESOLVE (round 3)

None. The round-2 §1 raw_path/raw_inline ambiguity is now resolved by
design PR #5. No new ambiguity surfaced during round-3 implementation.

---

# Round-3 deliverable inventory (additions to PR #2)

New files:

- `internal/slack/cli/doctor.go` — `slack-poller doctor` subcommand +
  `DoctorReport` schema
- `internal/slack/cli/doctor_test.go` — 9 unit tests covering doctor
  paths (happy, token-missing, auth-fail, channel-not-found, not-in-
  channel-hint, empty-history soft, no-channels, missing-scope, JSON)
- `internal/slack/cli/auth_errors_test.go` — 6 tests for
  `FormatAuthError`, `AuthFailCode`, `MissingScope`
- `internal/slack/cli/scripts_lint_test.go` — lints the real
  acceptance script
- `internal/slack/raw_inline_alignment_test.go` — D1 verification
- `internal/slack/anomaly_actionable_test.go` — D3 anomaly content
- `scripts/acceptance-slack-poller-real.sh` — D4 real e2e

Modified files:

- `internal/slack/client.go` — extend `APIClient` interface with
  `AuthTest` / `ConversationInfo`; add `AuthFailCode` and `MissingScope`
  helpers
- `internal/slack/cli/root.go` — wire `doctor` into the command tree;
  add `FormatAuthError` and route Execute() stderr through it
- `internal/slack/poller.go` — operator-actionable anomaly reasons
  via new `formatChannelAccessReason` helper
- `coordination/from-slack-poller.md` — this file

# Definition-of-done evidence (round 3)

```
$ go build ./...
$ go vet ./...
$ go vet -tags=integration ./...
$ go test ./...
ok  	github.com/victor-develop/advanced-tasker/internal/slack
ok  	github.com/victor-develop/advanced-tasker/internal/slack/cli
$ go test -tags=integration ./...
ok  	github.com/victor-develop/advanced-tasker/internal/slack
ok  	github.com/victor-develop/advanced-tasker/internal/slack/cli
$ bash scripts/acceptance-slack-poller.sh
==> building slack-poller...
==> running acceptance driver...
computed hash: a1bcd61571d69e299b765e18c6413c3d78cca741bf1e227ab4bf89af9f14d5da
PASS: hash matches golden
==> OK
$ bash scripts/acceptance-slack-poller-real.sh
SKIP: SLACK_BOT_TOKEN or TEST_SLACK_CHANNEL not set.
      Set both to run the REAL Slack e2e (no writes performed).
```
