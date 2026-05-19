# 07 — Outbox and the External Speaking Boundary

The outbox is the **only** way the harness communicates with the outside
world (Slack, GitHub comments, etc.). Nothing else writes externally. This
gives us a single auditable boundary and a place to enforce safety.

## Why an outbox at all

Direct calls from workers or commander to Slack/GH would:
1. Bypass review — a worker's hallucination becomes a public message
2. Lose auditability — no single place shows "what we said today"
3. Couple LLM cost to network failure — retries blow tokens
4. Make revocation impossible

The outbox solves all four.

## Lifecycle

```
   commander or worker (via review)
        │
        │  harness outbox send --risk=...
        ▼
   outbox/pending/ OR outbox/awaiting-human/
        │
        ├─→ (human approves if awaiting) → outbox/pending/
        │
        │  outbox-sender daemon
        ▼
   outbox/sent/    (success)  OR  outbox/failed/  (Slack/GH error)
```

## Risk classification

Every outbox item carries `risk: low | normal | high`. The classifier is
the **worker** when it proposes via `next: outbox.send`, or the
**commander** when it sends directly. The CLI rejects unsigned risk fields.

### Risk guidance (in `roles/<role>.md` for each worker role)

| Risk | Examples | Approval needed |
|---|---|---|
| **low** | Reply in a tracked thread to a routine question; ack a PR comment | None — commander review is sufficient |
| **normal** | Reply that commits us to a date or scope; first message to a new participant | Human approval required |
| **high** | Public-facing posts; messages to execs or external customers; anything sensitive | Human approval required |

### Commander cannot downgrade
The CLI enforces that **`harness review`** cannot lower a worker's stated
risk. It may only raise it. (Workers tend to under-classify; commander
should err on caution.) Commanders attempting to lower risk get exit 2.

### High-risk specifics
- Always lands in `outbox/awaiting-human/`
- Always sends an `inbox/human/<id>.json` ping requesting approval
- Requires `harness outbox approve O-<id>` from a human identity (the CLI
  refuses approval if `$HARNESS_AGENT_KIND=llm` or similar runtime hint)

## Limits and rate-limiting

In `config.yaml`:

```yaml
limits:
  outbox_per_thread_per_hour: 5
  outbox_per_channel_per_hour: 20
  outbox_global_per_hour: 100
  outbox_high_risk_require_human: true
  outbox_revoke_window: 5m
```

The sender daemon enforces these before each send. Exceeded → item stays
in `pending/`, sender writes `inbox/anomalies/` once per excess.

## Revoke

```
harness outbox revoke O-<id>
```

Behavior depends on state:
- `awaiting-human` or `pending` (not yet sent) → delete file, done
- `sent` AND within `revoke_window` → call provider's delete API:
  - Slack: `chat.delete` (requires bot to be in channel)
  - GitHub: `DELETE /repos/.../issues/comments/<id>` or
    `DELETE /repos/.../pulls/comments/<id>`
- `sent` AND past window → reject with exit 2 ("manual cleanup required")

Revoke is a human-friendly command but also callable by commander in case
of immediate regret. Workers cannot revoke.

## Auditability

Sent items in `outbox/sent/` are git-tracked (yes, git-tracked — unlike
other queue items). The history is the audit log. Each sent file has:

```yaml
id: O-xyz789
sent_at: 2026-05-19T11:32:00Z
sender_response:
  provider: slack
  message_ts: 1716123456.001200
  permalink: https://acme.slack.com/archives/C0492/p1716123456001200
created_by: J-abc123
risk: low
... (original fields)
```

This lets `git log -- outbox/sent/` show every external message ever sent,
who created it, what risk level, when.

## Sender daemon

The `outbox-sender` is a daemon (in autopilot mode) that:
1. Watches `outbox/pending/`
2. Validates rate limits
3. Calls Slack/GitHub API
4. On success: writes response into the file, `git mv` to `sent/`
5. On failure: increments `retry_count`, after N retries moves to `failed/`
   and writes `inbox/anomalies/`

In manual mode, the daemon doesn't run. A human or external agent can
trigger sends explicitly via `harness outbox flush` (a one-shot version of
the daemon).

## Per-provider details

### Slack

- Auth: bot token (`SLACK_BOT_TOKEN`)
- `to: slack`, `ref.thread: slack-<channel>-<thread_ts>` parses out channel
  and thread_ts
- Posts via `chat.postMessage` with `thread_ts` set
- If `in_reply_to` is provided, may use `chat.postMessage` with reply
  attachment block (TBD by sender impl)

### GitHub PR comments

- Auth: PAT or GH App token (`GITHUB_TOKEN`)
- `to: github-pr-comment` → general issue comment on PR
- `to: github-pr-review` → review comment (requires `in_reply_to`
  pointing at a code review thread)

## Anti-spam / sanity

Even with rate limits, the outbox should refuse to send:
- A message identical (content + thread + sender) to one sent in the last
  10 minutes (likely bug, not legitimate)
- A message > N tokens (configurable, default 500 words) without a human
  approval, regardless of risk classification (long messages are
  high-stakes by virtue of length)

These are hard guards. They generate `inbox/anomalies/` items.

## Implementation notes for Track A

- Sender is a separate goroutine/process from the worker daemon and the
  rollup updater daemon — different failure profiles, different rate
  budgets.
- Use `go-slack` and `go-github` libraries; both well-maintained.
- Idempotency: if sender crashes after API call but before file move, the
  next sender run sees the same `outbox/pending/<id>.yaml` and could
  re-send. Mitigation: write a pre-call sentinel file `<id>.sending`
  before the API call; on recovery, look up the provider's API by message
  body fingerprint to detect duplicate.
- Tests should cover: rate limit reject, risk downgrade reject, revoke
  within window, revoke past window.
