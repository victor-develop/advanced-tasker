# 09 — GitHub PR Poller (Track C)

This is the **isolated spec** for the GitHub PR ingestion daemon. Like the
Slack poller, this can be implemented without knowledge of other tracks if
the filesystem output contract below is honored.

## Scope

The GitHub poller:
1. Reads `state/sources/github/config.yaml` for tracked repos + auth
2. Discovers new PRs in tracked repos
3. Polls each tracked PR for: review comments, issue comments, state changes,
   check runs
4. Writes raw events + state to filesystem
5. Maintains cursors

The GitHub poller does NOT:
- Post comments (outbox does this)
- Trigger workflows / actions
- Handle Issues, Discussions, Projects
- Make LLM calls

## Configuration

```yaml
# state/sources/github/config.yaml
auth:
  token_env: GITHUB_TOKEN
  type: pat                       # pat | app
watch:
  repos:
    - "acme/api"
    - "acme/ingest"
poll_interval: 60s
new_pr_lookback: 7d               # how far back to look for new PRs on startup
backoff:
  on_rate_limit: 60s
  on_error: 5s
  max_backoff: 5m
```

## Two-level polling (analogous to Slack)

### Repo-level poll
For each tracked repo:
- `GET /repos/{owner}/{repo}/pulls?state=open&sort=updated&direction=desc`
- Look for PRs not yet in `state/threads/github-<owner>-<repo>-pr-<n>/`
- Write to `inbox/new/github-<owner>-<repo>-pr-<n>.json`

### PR-level poll
For each tracked PR (i.e., `state/threads/github-<owner>-<repo>-pr-<n>/`
exists), call all four endpoints with `since=<last_polled_at>`:

1. **Issue comments** (general PR conversation)
   `GET /repos/{owner}/{repo}/issues/{n}/comments?since=...`
2. **Review comments** (inline code comments)
   `GET /repos/{owner}/{repo}/pulls/{n}/comments?since=...`
3. **PR metadata** (state, mergeable, head/base, labels)
   `GET /repos/{owner}/{repo}/pulls/{n}` (no since; check `updated_at`)
4. **Reviews** (approve/request-changes/comment summaries)
   `GET /repos/{owner}/{repo}/pulls/{n}/reviews` (no since support;
   dedup by `id`)
5. **Check runs** (optional)
   `GET /repos/{owner}/{repo}/commits/{sha}/check-runs`

## Filesystem output contract (HARD CONTRACT)

### Thread ID format
```
github-<owner>-<repo>-pr-<number>
```

Example: `github-acme-api-pr-1284`.

### Raw event files

```
state/threads/<thread-id>/raw/<event-id>.json
```

`<event-id>` uses the source endpoint as a prefix to avoid collisions:

- `issue-comment-<id>.json` (from issue comments endpoint)
- `review-comment-<id>.json` (from PR review comments endpoint)
- `review-<id>.json` (from reviews endpoint)
- `pr-state-<updated_at-iso>.json` (state snapshots — multiple over time)
- `check-run-<id>-<conclusion>.json` (terminal check states)

Each file is the raw GitHub JSON, lightly normalized:

```json
{
  "id": "issue-comment-1234567",
  "source": "github",
  "captured_at": "2026-05-19T10:15:23Z",
  "kind": "issue-comment",
  "pr": {
    "owner": "acme",
    "repo": "api",
    "number": 1284
  },
  "actor": "alice",
  "actor_id": 12345,
  "created_at": "2026-05-19T10:14:00Z",
  "updated_at": "2026-05-19T10:14:00Z",
  "body": "...",
  "html_url": "https://github.com/acme/api/pull/1284#issuecomment-1234567",
  "raw": { ...full GitHub response... }
}
```

### .dirty marker
After writing any event for a tracked PR, touch:

```
state/threads/<thread-id>/.dirty
```

### Meta initialization
If `state/threads/<thread-id>/meta.json` does not exist, create:

```json
{
  "id": "github-acme-api-pr-1284",
  "source": "github",
  "url": "https://github.com/acme/api/pull/1284",
  "created_at": "<PR.created_at>",
  "last_event_at": "<latest event ISO>",
  "owner_task": null,
  "participants": ["alice", "bob"],
  "tracking_since": "<now>"
}
```

Update `last_event_at` and `participants` (union) on subsequent writes.

### New PRs → inbox/new
For PRs discovered via repo-level poll but not yet tracked:

```
state/inbox/new/github-acme-api-pr-<n>.json
```

```json
{
  "id": "github-acme-api-pr-1284",
  "source": "github",
  "kind": "new",
  "received_at": "2026-05-19T10:15:23Z",
  "summary": "alice opened PR #1284: 'Refactor retry to jittered exponential'",
  "ref": {
    "owner": "acme",
    "repo": "api",
    "number": 1284,
    "title": "Refactor retry to jittered exponential",
    "author": "alice",
    "url": "https://github.com/acme/api/pull/1284",
    "state": "open",
    "draft": false
  }
}
```

The commander promotes via `harness thread track github-acme-api-pr-1284`.

### Updates ping
When new events arrive for a tracked PR (within one poll cycle), write at
most one:

```
state/inbox/updates/github-acme-api-pr-<n>-<latest-event-id>.json
```

Collapse multiple new events into one ping per PR per cycle.

## Cursors

```
state/sources/github/cursors/
├── repos/
│   └── acme-api.json
│       # { "last_pr_discovery_at": "2026-05-19T10:00:00Z" }
└── prs/
    └── acme-api-1284.json
        # {
        #   "last_polled_at": "2026-05-19T10:15:00Z",
        #   "endpoints": {
        #     "issue_comments_since": "2026-05-19T10:14:00Z",
        #     "review_comments_since": "2026-05-19T10:13:00Z",
        #     "reviews_seen_ids": [4567, 4568, 4569],
        #     "pr_updated_at": "2026-05-19T10:12:00Z"
        #   }
        # }
```

Atomic update: write `.tmp`, rename.

## Dedup — IMPORTANT

GitHub's `since` filter has small (~1-2s) timestamp jitter at boundaries.
DO NOT trust it alone. The poller MUST:

1. Use `since=<last_polled_at - 60s>` (overlap buffer)
2. Dedup by event ID via `raw/<event-id>.json` existence check
3. For reviews (no `since` param), track seen IDs in the cursor

## State change detection

For PR metadata, the poller compares `pr.updated_at` against
`cursor.endpoints.pr_updated_at`. If newer:
- Write `pr-state-<updated_at>.json` capturing fields:
  `state`, `merged`, `mergeable`, `labels`, `head_sha`, `base_sha`,
  `requested_reviewers`, `draft`
- Update cursor

The rollup updater consumes these to refresh `meta.state` in rollups.

## Tracking lifecycle

```
github-poller watch <owner/repo>
github-poller unwatch <owner/repo>
github-poller status
github-poller force-poll [<owner/repo> | <owner/repo>#<pr>]
```

Or delegated to `harness watch github-repo <owner/repo>` (coordinated with
Track A).

## Error handling

| Condition | Action |
|---|---|
| 403 with rate-limit headers | Sleep until `X-RateLimit-Reset` |
| 404 on a tracked PR | PR deleted; archive thread; log to anomalies |
| 401 / token invalid | Log + exit |
| 422 / malformed request | Skip endpoint for this cycle, log |
| Network error | Exponential backoff |

## Token / auth notes

- PAT: single-user, simple, fine for first version
- GH App: required for higher rate limits and audit; future work
- Required PAT scopes:
  - `repo` (private repos) or `public_repo` (public only)
  - `read:org` if you want team membership in `participants`

## Performance: conditional requests

GitHub supports `If-None-Match` / `ETag` and `If-Modified-Since`. The
poller SHOULD use these to avoid burning rate limit on no-change polls.
Store `ETag` alongside cursor data; 304 responses cost ~no rate budget.

## Daemon process model

- Single binary `github-poller`
- Goroutine pool: N concurrent PR polls (`max_concurrent_pr_polls`,
  default 4)
- Graceful shutdown on SIGTERM
- `--once` flag for testing
- Structured JSON logs to stderr

## Testing

1. **Fresh start** — new repo, lookback `7d` → discovers existing PRs into
   `inbox/new/`
2. **Cursor overlap** — re-run; no duplicate `raw/` files written
3. **State change** — PR closes between polls → state snapshot file
   written
4. **PR deleted** — 404 → handled gracefully
5. **Rate limit** — mock 429 → backoff respected

Use `go-vcr` or a recording proxy. Don't hit real GH in CI.

## Out of scope for this track

- Issues (non-PR)
- Discussions
- Actions / Workflows
- Branch protections
- Repository settings

## Implementation hints

- `github.com/google/go-github/v62/github` (or latest) is the canonical
  client. Supports ETag handling out of the box.
- Use the `Listing` pagination wrapper; loop until `resp.NextPage == 0`
  in a single poll cycle.
- Reviews endpoint has no `since` — minimize cost by caching seen IDs.
- Pre-fetch one page; if all IDs already seen, skip the rest.
