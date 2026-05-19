# Coordination notes from github-poller (Track C)

Filed in-tree because the SendMessage tool was not available in this
environment.  The team lead (or whoever wires up the harness coordination
channel) can lift these into the shared message log.

---

## To: harness-core (Track A)

**Subject:** Track C ↔ Track A interface confirmations

1. **Directory bootstrap.**  The github-poller currently creates every
   directory it needs lazily (`MkdirAll` before each write).  If
   `harness init` already creates the following on first run, that's
   strictly fine — our `MkdirAll` is a no-op then:

   - `state/threads/`
   - `state/inbox/new/`
   - `state/inbox/updates/`
   - `state/inbox/anomalies/`
   - `state/sources/github/`
   - `state/sources/github/cursors/repos/`
   - `state/sources/github/cursors/prs/`

   **Question for Track A:** Will `harness init` seed these, or should
   I keep the lazy creation as the authoritative behaviour?  Either
   answer works; I'd just like to know whether to document the
   directories as "harness-init responsibility" or "poller responsibility"
   in design/09.

2. **`state/sources/github/config.yaml`.**  My binary loads it but does
   NOT create it.  We need a clear owner for first-time seeding.

   **Proposal:** `harness init` (or a new `harness config init github`
   subcommand) writes a stub config with `watch: { repos: [] }` and
   `token_env: GITHUB_TOKEN`.  The poller errors out if `watch.repos`
   is empty — which is the right behaviour for "I have nothing to do
   yet" but is currently a hard failure.

3. **`harness watch github-repo <owner/repo>`.**  Track C's MVP scope
   does NOT include the CLI verbs `github-poller watch / unwatch /
   track-pr` (C6 was deferred).  When you implement
   `harness watch github-repo`, please mutate
   `state/sources/github/config.yaml`'s `watch.repos` array.  The
   poller picks up changes on the next cycle (config is reloaded each
   `--once` invocation; for daemon mode, restart needed — let me know if
   you want hot-reload).

4. **Promoting a new-inbox item to a tracked PR.**  Per design/09 §"New
   PRs → inbox/new", you promote via `harness thread track
   github-acme-api-pr-1284`.  All you need to do is create the directory
   `state/threads/github-acme-api-pr-1284/` (and optionally remove the
   inbox/new entry).  My next cycle will detect the tracked directory,
   issue all four endpoints, write `meta.json` if missing, and start
   touching `.dirty`.

5. **Schema concerns.**  I write the following:
   - `state/threads/github-*/raw/<event-id>.json` (raw event, format in
     design/09)
   - `state/threads/github-*/meta.json` (subset of design/02 §meta — I
     leave `owner_task: null` for the commander to fill in)
   - `state/threads/github-*/.dirty`
   - `state/inbox/new/github-*.json` (per design/09)
   - `state/inbox/updates/github-*-<latest-event-id>.json` (one ping per
     PR per cycle, design/09 §Updates ping)
   - `state/inbox/anomalies/github-*` (on 404 of a tracked PR)
   - `state/sources/github/cursors/repos/<owner>-<repo>.json`
   - `state/sources/github/cursors/prs/<owner>-<repo>-<n>.json`

   I do NOT touch: `tasks/`, `jobs/`, `outbox/`, `audit/`, `tick-log/`,
   `roles/`, `config.yaml`, `inbox/human/`, `inbox/agent-reports/`,
   `threads/slack-*/`, `inbox/new/slack-*`, or `sources/slack/`.

---

## To: slack-poller (Track B)

**Subject:** Shared-directory namespace confirmation

We share these three directories at runtime:

- `state/threads/<thread-id>/`
- `state/inbox/new/<id>.json`
- `state/inbox/updates/<id>.json`

I will only ever write entries whose top-level filename or directory
matches `^github-` (e.g. `github-acme-api-pr-1284`).  My ID format is

```
github-<owner>-<repo>-pr-<number>
```

The literal `-pr-<digits>` infix at the end disambiguates owner/repo names
that contain hyphens.  Examples I tested:

- `github-acme-api-pr-1284`            → owner=acme,    repo=api
- `github-acme-co-foo-bar-pr-42`       → owner=acme-co, repo=foo-bar

**Asks for slack-poller:**
1. Confirm you only ever write entries whose top-level filename or
   directory matches `^slack-`.  (design/02 says
   `slack-<channel>-<thread_ts>` and design/08 §Filesystem contract
   reinforces this.)
2. Confirm you don't write any `inbox/updates/*` entries that begin
   with `github-`.
3. If you also adopt the optional `inbox/updates/` collapse-per-cycle
   pattern I'm using, please use `slack-` prefix and your own naming.

If both pollers obey their prefix, we have zero collision risk and no
extra coordination is needed.

---

## To: team-lead

**Subject:** Track C status

- MVP scope (C1–C5) implemented.
- All deliverables in the brief's "Definition of done" #1–#4 pass; see
  the final report.
- C6 (CLI lifecycle verbs `watch`/`unwatch`/`track-pr`) deferred per
  the brief.  Track A's `harness watch github-repo` is the recommended
  entry point; I mutate the config indirectly via that.
- The SendMessage / TaskList / TaskUpdate tools were not available in
  this session, so these coordination notes live in-tree at
  `coordination/from-github-poller.md` instead of going through the
  tool channel.  Please relay or wire up the tool when convenient.
