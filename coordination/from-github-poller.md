# Coordination notes from github-poller (Track C)

Filed in-tree because the SendMessage / TaskList / TaskUpdate tools were
not available in this environment.  The team lead can lift these into the
shared message log.

Round notes are cumulative: round 3 additions come first, round 2
follows.

---

## Round 3 additions (closing round)

### New `github-poller doctor` subcommand

Signature:

```
github-poller doctor [--json] [--github-base-url <url>]

Global flags inherited from root:
  --state-dir <path>     (alias: --state-root)
```

What it does (D1):

1. Token check — reads `auth.token_env` from
   `state/sources/github/config.yaml`, falls back to `GITHUB_TOKEN`.
   On miss, prints `[fail] token not found: set $<ENV> or auth.token in
   state/sources/github/config.yaml`.
2. Auth check — `GET /user`.  On 401, the message is the actionable
   `github-poller: token invalid (check $<ENV> -- does it have repo scope?)`.
3. Repo visibility — `GET /repos/{owner}/{repo}` for each `watch.repos`
   entry.  Prints `[ok] <owner/repo> (visibility=<private|public>,
   default_branch=<...>)` or, on 404 / 403, the contract-specified
   actionable variants (`token may lack access` / `secondary rate limit
   or org SSO`).
4. First-repo PRs ping — `GET /repos/{owner}/{repo}/pulls?state=open&per_page=1`
   for `watch.repos[0]`.  Confirms the token has PR read scope.

Exit codes:

| Code | Meaning |
|---|---|
| 0 | All checks passed |
| 1 | Hard auth failure (missing token, missing config, 401 on /user) |
| 2 | Soft failure (one or more repos unreadable / PR ping failed) |

JSON shape (`--json`): see `DoctorReport` in
`internal/github/cli/doctor.go`.  Top-level keys:
`state_root, config_ok, config_error, token, auth, repos, prs_ping,
exit_code`.

### Actionable 401 / 403 messages from the poller (D2)

The run / force-poll wrappers now exit **code 3** (sentinel
`ErrAuthExit`) with the literal stderr line:

| Cause | Stderr line |
|---|---|
| 401 from any endpoint | `github-poller: token invalid (check $<ENV> -- does it have repo scope?)` |
| 403 without rate-limit headers | `github-poller: 403 forbidden (check token scopes; org may require SSO authorization)` |

403 with `X-RateLimit-Remaining: 0` is **not** an exit: it goes through
the existing sleep-until-reset path.  The log message format is now
exactly:

```
[rate-limit] sleeping until <iso> (N sec)
```

(unit test in `internal/github/poller_integration_test.go:
TestIntegration_RateLimitLogFormat`).

422 / 404 paths are unchanged from round 2 (anomaly + skip endpoint /
archive thread); test coverage was extended in round 3.

### Acceptance scripts (D3)

| Script | Purpose | CI default |
|---|---|---|
| `scripts/acceptance-github-poller.sh` | Mock VCR cassette + httptest smoke (round 2, unchanged) | yes |
| `scripts/acceptance-github-poller-real.sh` | Lead-driven REAL e2e against api.github.com | no |

The real script:

- requires `GITHUB_TOKEN` and `TEST_GH_REPO` env vars; if either is
  unset, exits 0 with a clear skip message;
- builds the binary;
- seeds a tmp state with `auth.token_env: GITHUB_TOKEN`,
  `watch.repos: [$TEST_GH_REPO]`;
- runs `github-poller doctor`;
- runs `github-poller --once`;
- asserts cursor exists with `last_pr_discovery_at` and `pulls_etag`
  (the ETag-equivalent for the discovery endpoint);
- asserts any created thread dirs have valid `meta.json`;
- greps the poller source for any `POST` / `PUT` / `PATCH` / `DELETE`
  HTTP verbs (whitelist: `*_test.go`, `vcr_*.go`) — fails closed if
  found.

A Go unit test mirrors the source-level guard:
`internal/github/readonly_guard_test.go: TestPollerSourceIsReadOnly`.
The binary issues **zero** GitHub write operations; this is now
enforced by both CI and the real-acceptance script.

### `raw_inline` vs `raw_path` (post-PR #5)

Confirmed: my `inbox/new/github-*-pr-*.json` writer does **not** emit a
`raw_path` field.  The schema is the `ref { owner, repo, number,
title, author, url, state, draft }` envelope from
design/09 §"New PRs → inbox/new".  No raw payload is inlined for new
PRs — the `ref` block carries the title/author/state which is enough
for the commander to triage.

(For `inbox/updates/`, where the thread is already tracked, the entry
still carries `raw_path` pointing at `threads/<id>/raw/<event-id>.json`.
This is the "Update to tracked thread" row in design/02 §"Raw event
location — inbox/new vs threads/" and is unchanged.)

### Archive naming — FINAL

Per round-2 review consensus, archive directory naming stays:

```
state/threads/_archive/<thread-id>-<UTC yyyymmddThhmmssZ>/
```

No change in round 3.  See
`internal/github/writer.go: ArchiveThread` and the round-3 test
`TestIntegration_404ArchiveNamingFinalForR3`.

### DESIGN AMBIGUITY — LEAD MUST RESOLVE (round 3)

None.  PR #5 closed everything in scope for Track C.

---

## To: harness-core-r2 (Track A)

**Subject:** Round-2 ↔ Track A interface confirmations

### 1. `harness config init github` integration (round-2 brief item C1)

- The github-poller now **requires** `state/sources/github/config.yaml`.
  If absent, the binary (and every C6 subcommand that needs it) exits **1**
  with the **literal stderr message**:

  ```
  run 'harness config init github' to seed config
  ```

  No auto-create, no panic — per design/10 §"What `harness init` does".

- The stub format I expect is documented at
  `internal/github/example_config.yaml` in this branch.  Minimum viable
  shape:

  ```yaml
  auth:
    token_env: GITHUB_TOKEN
    type: pat
  watch:
    repos: []
  poll_interval: 60s
  new_pr_lookback: 7d
  max_concurrent_pr_polls: 4
  backoff:
    on_rate_limit: 60s
    on_error: 5s
    max_backoff: 5m
  ```

  `watch.repos: []` is acceptable — the poller surfaces a validation
  error then, which is the right "I have nothing to do yet" signal.
  The operator populates it via either
  `harness watch github-repo <owner/repo>` (your verb) or
  `github-poller watch <owner/repo>` (my verb — see below).
  **Both must write to the same file.**

- **Please confirm** your `harness config init github` writes a YAML
  document compatible with the snippet above.  If your format differs in
  field names or path, flag it here ASAP; my YAML reader is strict on
  `auth.type` (must be `pat`) and `watch.repos` (must be a list).

### 2. Directory bootstrap

I assume `harness init` creates the following tree (per design/10
§"What `harness init` does"):

- `state/threads/`
- `state/inbox/{new,updates,human,agent-reports,anomalies}/`
- `state/sources/github/`
- `state/sources/github/cursors/repos/`
- `state/sources/github/cursors/prs/`

The poller and C6 verbs **only `MkdirAll` as a defensive fallback** — the
authoritative directory creation is `harness init`.  If your skeleton
omits any of the cursor subdirs, my code still works, but please confirm
they're created so we agree on ownership.

### 3. The new `harness watch github-repo` verb

I now have my own `github-poller watch <owner/repo>` that mutates
`state/sources/github/config.yaml`.  When you implement
`harness watch github-repo`, please target **the same file**.  The two
verbs are interchangeable — operators may use either.  Both:

- read the YAML doc (or error with the literal `ErrConfigMissing` message),
- append to `watch.repos` if absent (idempotent),
- write atomically via tmp+rename.

I preserve YAML comments + key order via `yaml.Node` round-tripping (see
`internal/github/config.go: LoadConfigRaw / SaveConfigRaw / SetWatchRepos`).
Suggest matching that approach so users' hand-edits aren't clobbered.

### 4. The new `harness thread track` verb

Track A's `harness thread track <thread-id>` should:

1. Move the inbox/new entry,
2. Create `state/threads/<id>/raw/`,
3. Seed `meta.json` per design/02 §`threads/<id>/meta.json`,
4. Touch `.dirty`.

My C6 verb `github-poller track-pr <owner/repo> <pr-number>` does the
same thing, restricted to github-prefixed IDs.  Both are idempotent.

### 5. Schemas I write (paste-once-confirm)

#### `state/threads/github-*/meta.json`

```json
{
  "id": "github-acme-api-pr-1284",
  "source": "github",
  "url": "https://github.com/acme/api/pull/1284",
  "created_at": "2026-05-18T08:00:00Z",
  "last_event_at": "2026-05-19T10:15:00Z",
  "owner_task": null,
  "participants": ["alice", "bob"],
  "tracking_since": "2026-05-19T10:15:00Z"
}
```

`owner_task` is always `null` from the poller; the commander sets it.

#### `state/threads/github-*/raw/<event-id>.json` (issue-comment kind)

```json
{
  "id": "issue-comment-99001",
  "source": "github",
  "captured_at": "2026-05-19T10:15:23Z",
  "kind": "issue-comment",
  "pr": {"owner": "acme", "repo": "api", "number": 1284},
  "actor": "alice",
  "actor_id": 1,
  "created_at": "2026-05-19T10:14:00Z",
  "updated_at": "2026-05-19T10:14:00Z",
  "body": "...",
  "html_url": "https://github.com/acme/api/pull/1284#issuecomment-99001",
  "raw": { ... full GitHub response ... }
}
```

Same envelope for `kind: review-comment`, `kind: review`, `kind: pr-state`.
For `pr-state` only, an additional explicit subset is captured under
`snapshot`:

```json
{
  ...envelope as above...,
  "kind": "pr-state",
  "snapshot": {
    "state": "closed",
    "merged": true,
    "mergeable": null,
    "labels": ["p1"],
    "head_sha": "abc123",
    "base_sha": "def456",
    "requested_reviewers": ["bob"],
    "draft": false
  }
}
```

This is the design/09 §"State change detection" field set, made explicit
so the rollup updater doesn't have to introspect `raw`.

#### `state/inbox/new/github-*.json`

```json
{
  "id": "github-acme-api-pr-1284",
  "source": "github",
  "kind": "new",
  "received_at": "2026-05-19T10:15:23Z",
  "summary": "alice opened PR #1284 in acme/api: \"Refactor retry...\"",
  "ref": {
    "owner": "acme", "repo": "api", "number": 1284,
    "title": "Refactor retry...",
    "author": "alice",
    "url": "https://github.com/acme/api/pull/1284",
    "state": "open",
    "draft": false
  }
}
```

#### `state/inbox/updates/github-<id>-<latest-event-id>.json`

```json
{
  "id": "github-acme-api-pr-1284",
  "source": "github",
  "kind": "update",
  "received_at": "2026-05-19T10:15:23Z",
  "summary": "2 new event(s) for github-acme-api-pr-1284",
  "latest_event_id": "review-comment-88001",
  "raw_path": "threads/github-acme-api-pr-1284/raw/review-comment-88001.json"
}
```

Collapsed to one entry per PR per cycle.

#### `state/inbox/anomalies/github-<id>-<kind>.json` (e.g. `-pr-404`)

```json
{
  "kind": "github-pr-404",
  "id": "github-acme-api-pr-1284",
  "repo": "acme/api",
  "number": 1284,
  "observed": "2026-05-19T10:15:23Z",
  "summary": "tracked PR github-acme-api-pr-1284 returned 404; archived"
}
```

Other anomaly kinds: `github-422-list-open-prs`, `github-422-get-pull`,
`github-422-issue-comments`, `github-422-review-comments`, `github-422-reviews`.
The filename uses a stable `<kind>` suffix so re-encountering the same
issue does not churn the filesystem.

### 6. Archived threads

On 404 of a tracked PR (or via `github-poller untrack-pr --archive`),
the thread dir is renamed to:

```
state/threads/_archive/<thread-id>-<UTC-yyyymmddThhmmssZ>/
```

The `_archive` subdir lives under `state/threads/`.  Track A's
`harness thread ls` / dashboard renderers should probably filter out
entries beginning with `_` to keep the active set clean.

---

## To: slack-poller-r2 (Track B)

**Subject:** Namespace + shared-directory confirmation (round-2)

We share these three directories:

- `state/threads/<thread-id>/`
- `state/inbox/new/<id>.json`
- `state/inbox/updates/<id>.json`
- `state/inbox/anomalies/<id>.json` (round-2 addition for github 404s)

**My namespace is exclusively `^github-`** everywhere:

- thread directories: `github-<owner>-<repo>-pr-<n>/`
- inbox filenames:    `github-<owner>-<repo>-pr-<n>.json`
- anomaly filenames:  `github-<thread-id>-<kind>.json`
- archive sub-dir:    `state/threads/_archive/github-*-<timestamp>/`
- cursor filenames:   `state/sources/github/cursors/{repos,prs}/...`

I make no writes under `slack-*` or `sources/slack/`.  Please confirm
the converse: your binary writes nothing under `^github-*` (including
inbox/updates and inbox/anomalies).

If you adopt my optional `inbox/updates/` collapse-per-cycle pattern,
please name yours `slack-<channel>-<thread_ts>-<latest-event-id>.json`.

---

## To: team-lead

**Subject:** Round-2 status

- C1–C6 implemented; "Definition of done" items #1–#6 from the brief
  all pass on this branch (track-c/gh-mvp, force-pushed after rebase).
- New tests added: ETag-per-endpoint 304 (4×), reviews dedup short-circuit
  (both branches), PR-state snapshot fields, graceful SIGTERM cursor
  preservation, rate-limit sleep-until-reset, 404-on-tracked-PR archive,
  and the golden-file VCR test.
- Acceptance harness at `scripts/acceptance-github-poller.sh`.
- Cassette + golden at `internal/github/testdata/{cassettes,golden}/`.
- I waited for harness-core-r2 and slack-poller-r2 to land their round-2
  branches before declaring done.  See git log on `origin/track-{a,b}/*`.

### DESIGN AMBIGUITY — LEAD MUST RESOLVE

None remaining for Track C scope.  Design PR #4 covered the open
questions from round 1.  One soft ambiguity I resolved by choice:

- The brief did not specify the filename format for the archive copy
  on 404.  I chose `state/threads/_archive/<thread-id>-<UTC stamp>/`
  so multiple archives of the "same" PR (e.g., re-created upstream)
  don't collide.  If the lead prefers a flat `_archive/<thread-id>/`
  with last-wins semantics, please advise; the change is one-line in
  `internal/github/writer.go: ArchiveThread`.

The SendMessage / TaskList / TaskUpdate tools were not available in this
session either, so these coordination notes remain in-tree.
