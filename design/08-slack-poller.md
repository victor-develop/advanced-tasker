# 08 — Slack Poller (Track B)

This is the **isolated spec** for the Slack ingestion daemon. Implementing
this track does not require understanding the rollup updater, commander, or
worker protocol — only the filesystem output contract below.

## Scope

The Slack poller:
1. Reads `state/sources/slack/config.yaml` for tracked channels + auth
2. Periodically polls Slack for new messages and thread replies
3. Writes raw events to the filesystem
4. Maintains cursors to avoid re-fetching

The Slack poller does NOT:
- Send messages (outbox does this, separately)
- Parse or summarize content
- Read or write task / rollup files
- Make any LLM calls

## Configuration

```yaml
# state/sources/slack/config.yaml
auth:
  token_env: SLACK_BOT_TOKEN
watch:
  channels:
    - id: C0492
      reason: "data team alerts"
    - id: C1234
      reason: "PR notifications"
poll_interval: 30s
backoff:
  on_rate_limit: 60s
  on_error: 5s
  max_backoff: 5m
```

Auth options (in order of preference):
1. Env var named by `token_env`
2. `state/config.local.yaml` `secrets.slack_bot_token`
3. Fail to start if neither present

## Two-level polling

Slack APIs don't give "all updates to all my threads" — we must split:

### Channel-level poll
For each tracked channel:
- `conversations.history` with `oldest=<last_ts>`
- Returns top-level messages in the channel (new threads or replies to
  un-tracked threads count as top-level here if `thread_ts == ts`)
- Update channel cursor to the newest `ts` seen

### Thread-level poll
For each **tracked thread** (anything under `state/threads/slack-*/`):
- `conversations.replies` with `channel=<C>`, `ts=<thread_ts>`,
  `oldest=<last_reply_ts>`
- Returns all replies after the cursor
- Update thread cursor

### Why both
- Channel poll discovers *new* threads we're not yet tracking
- Thread poll catches *new replies* on threads we already follow
- A thread that hasn't been tracked yet would only show top-level
  messages; once promoted, switch to thread-level polling for replies

## Filesystem output contract (HARD CONTRACT)

### Raw event files
For each event (new or reply), write:

```
state/threads/<thread-id>/raw/<ts>.json
```

where:
- `<thread-id>` is `slack-<channel-id>-<thread_ts>` (use the parent
  thread_ts; for top-level messages without a thread, use `ts` itself)
- `<ts>` is the Slack timestamp string (already unique)

File contents (JSON):

```json
{
  "id": "slack-C0492-1715814999.000400",
  "source": "slack",
  "captured_at": "2026-05-19T10:15:23Z",
  "channel": "C0492",
  "ts": "1715814999.000400",
  "thread_ts": "1715814123.001200",
  "user": "U07ABCDEF",
  "user_name": "alice",
  "text": "raw text content here",
  "blocks": [...],
  "reactions": [...],
  "subtype": null,
  "is_top_level_in_thread": false,
  "permalink": "https://acme.slack.com/archives/C0492/p1715814999000400"
}
```

`blocks`, `reactions`, `subtype` may be null/empty. Preserve everything
Slack gives us; downstream may need it.

### .dirty marker
After writing one or more raw events to a thread, touch:

```
state/threads/<thread-id>/.dirty
```

Just an empty file. Its presence (mtime) signals the rollup updater to
re-run. Don't delete it yourself — the updater clears it.

### Meta initialization
If `state/threads/<thread-id>/meta.json` does not exist (new thread you're
tracking), create it:

```json
{
  "id": "slack-C0492-1715814123.001200",
  "source": "slack",
  "url": "<permalink>",
  "created_at": "<earliest ts ISO>",
  "last_event_at": "<latest ts ISO>",
  "owner_task": null,
  "participants": ["alice"],
  "tracking_since": "<now ISO>"
}
```

Update `last_event_at` and `participants` on subsequent writes.

### New (untracked) thread → inbox/new
For a top-level message in a channel where no `state/threads/slack-<C>-<ts>/`
exists yet, the poller does NOT auto-create a thread directory. Instead:

```
state/inbox/new/slack-<C>-<ts>.json
```

with contents:

```json
{
  "id": "slack-C0492-1715814999.000400",
  "source": "slack",
  "kind": "new",
  "received_at": "2026-05-19T10:15:23Z",
  "summary": "alice in #data-alerts: \"hey, what's the status of...\"",
  "ref": {
    "channel": "C0492",
    "ts": "1715814999.000400",
    "thread_ts": null,
    "user": "U07ABCDEF"
  },
  "raw_inline": {
    "text": "...",
    "permalink": "..."
  }
}
```

The commander decides whether to promote it to a tracked thread (via
`harness thread track`) or drop it.

### Tracked thread promotion
When `harness thread track slack-C0492-<ts>` is called:
- CLI creates `state/threads/<id>/` with `meta.json`
- CLI moves the raw payload from `inbox/new/.../raw_inline` into
  `state/threads/<id>/raw/<ts>.json`
- CLI touches `.dirty`
- Poller picks up the new thread on next interval and starts thread-level
  polling

The poller itself does not promote — it only writes new-thread items to
`inbox/new/`.

### Updates ping
When a tracked thread receives new replies, optionally write a lightweight
informational item:

```
state/inbox/updates/<thread-id>-<latest-ts>.json
```

This is a hint to the commander that something changed, useful when the
rollup updater hasn't run yet. Single file per thread per poll cycle (don't
spam — collapse multiple new replies into one update ping).

## Cursors

```
state/sources/slack/cursors/
├── channels/
│   └── C0492.json        # { "last_ts": "1715814999.000400" }
└── threads/
    └── slack-C0492-1715814123.001200.json
        # { "last_reply_ts": "1715814555.001500" }
```

Atomic update: write to `<file>.tmp`, then rename. Crash-safe.

## Dedup

Slack `ts` is unique per channel. The poller MUST check `raw/<ts>.json`
existence before writing. If exists → skip silently (overlap is normal).

Cursor advances are write-on-success: only after a batch is fully written
do we update the cursor file.

## Tracking lifecycle commands (CLI)

These are implemented by the poller binary OR delegated to `harness` (TBD
by Track A coordination):

```
slack-poller watch <channel-id> [--reason "..."]
slack-poller unwatch <channel-id>
slack-poller status                  # show channels + cursors
slack-poller force-poll [<channel-id>]
```

Implementer's choice whether these are part of `harness` (e.g.,
`harness watch slack-channel C0492`) or a separate binary. Both must
update `state/sources/slack/config.yaml`.

## Error handling

| Condition | Action |
|---|---|
| 429 rate limit | Sleep `Retry-After` then backoff per config |
| 401 / token invalid | Log + exit; do not retry |
| Network error | Exponential backoff, max 5m |
| Channel not in workspace | Log to `inbox/anomalies/`, remove from active polling |
| Malformed Slack response | Skip event, log + 1 anomaly per occurrence |

## Daemon process model

- Single binary `slack-poller` (or subcommand of `harness`)
- Single goroutine pool: one channel-poll job + N thread-poll jobs in
  parallel (config: `max_concurrent_thread_polls`, default 4)
- Graceful shutdown on SIGTERM: finish in-flight HTTP, write cursors, exit
- Log to stderr in structured JSON
- `--once` flag for one-shot polling (useful for testing)

## Testing

Required integration tests:
1. **Fresh start** — empty cursors → poll → events land in
   `inbox/new/`
2. **Overlap idempotency** — re-run with same cursors → no duplicate
   files
3. **Thread promotion** — manually create a thread dir → next poll
   uses thread-level for replies, not channel-level
4. **Dedup** — write a raw file manually → poll same window → file
   unchanged
5. **Mock Slack API** — use `slacktest` or a recording proxy; don't hit
   real Slack in CI

## Out of scope for this track

- Threading/parent linking heuristics (we use raw `thread_ts` only)
- Reaction-based signals ("👍" as approval) — possible future feature
- File uploads / Slack attachments beyond URL preservation
- DMs (multi-party direct messages) — not in v1

## Implementation hints

- `github.com/slack-go/slack` is the canonical Go client
- Pagination: `conversations.history` returns up to 100 messages; loop
  with `cursor` until exhausted within a single poll cycle
- Convert Slack `ts` to ISO via `strconv.ParseFloat(ts, 64)` →
  `time.Unix(int64(f), int64((f-math.Floor(f))*1e9))`
- Be defensive: a top-level message can have `thread_ts == ts` even when
  there are replies; treat thread root as a tracked-thread candidate the
  moment any reply arrives
