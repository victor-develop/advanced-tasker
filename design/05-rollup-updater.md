# 05 — Rollup Updater Contract

The rollup updater is the most frequent LLM call in the system. Quality of
rollups determines quality of the commander. This document specifies when
the updater runs, what it sees, what it produces, and how its output is
validated.

## What it is

A small-model (Haiku-class) worker whose job is to keep one thread's
`rollup.md` in sync with new raw events. It does **nothing else**.

## Trigger conditions

The updater daemon polls `state/threads/*/` for `.dirty` markers. A
`.dirty` marker is set by:

1. **Pollers** after appending one or more raw events
2. **Manual flush:** `harness rollup flush <thread-id>` touches `.dirty`
3. **State change hints:** poller writes a special raw event (e.g.,
   `pr_merged`, `thread_resolved`) and touches `.dirty`

The daemon applies a small debounce window (default 30s) to coalesce bursts
before running.

## Input the updater sees

`harness render updater-input <thread-id>` produces:

```
You are the ROLLUP UPDATER for <thread-id>. Update its rollup based on
new events. Output the FULL new rollup.md. Do not omit sections.

CONSTRAINTS (the CLI will reject violations):
- Decisions ledger lines must be APPEND-ONLY. You may add lines but never
  edit or remove existing ones.
- Verbatim pins marked "(— pinned by human)" must be preserved verbatim.
- `state` field must use the allowed enum.
- Current ask: ≤3 lines. Open questions: ≤5 lines.

THREAD GOAL (from owner task T-12):
<contents of tasks/T-12/goal.md if linked>

CURRENT ROLLUP:
<full current rollup.md>

NEW EVENTS SINCE LAST UPDATE:
<concatenated raw/*.json since last commit affecting this rollup,
 normalized to a readable form>

YOUR OUTPUT: the full new rollup.md, in the schema specified by
design/02-state-and-schemas.md. Nothing else.
```

Key points:
- Updater sees the **goal of the owning task**, so it can distinguish
  "noise" from "ask" — without the goal, "is this important?" is
  unanswerable.
- Updater does **not** see the dashboard, other threads, or other tasks.
- Updater does **not** see tick logs.

## Output: a full rollup file

Not a diff. Not a delta. The updater regenerates the entire file. This
simplifies the validation step.

The CLI receives the output via:
```
harness rollup update <thread-id> --file=<rollup.md>
```
or, the updater daemon pipes stdout directly to the CLI.

## Validation pipeline (CLI enforces, not the LLM)

When `harness rollup update` is called:

### Step 0.5: Frontmatter ID matches thread directory

Before any other validation, assert:

```
new_rollup.frontmatter["id"] == <thread directory name>
```

where the thread directory is `state/threads/<id>/` and `<id>` is the
path segment containing the `rollup.md` being written. The check is a
strict string equality — no trimming, no case folding.

Rationale: this catches a class of bug where the LLM produces a rollup
whose body looks valid but whose frontmatter `id` field points at a
different thread. Without this guard, the updater silently writes a
content-correct file under the wrong thread directory, and the
inconsistency only surfaces later when something cross-references the
two. The check is one line of Go in the validator and one line in the
post-commit hook.

On mismatch → reject with `exit code 2`, write to `inbox/anomalies/`
with `reason: "frontmatter.id (<id-in-file>) does not match thread
directory (<dir-name>)"`. No commit. The rollup-updater daemon must
NOT retry on this rejection — a mismatched id is an LLM output bug, not
a transient failure.

### Step 1: Schema check
- Required YAML frontmatter fields present
- `state` matches enum
- Sections present in order: Goal, Current ask, Open questions, Decisions
  ledger, Verbatim pins
- Line caps respected (Current ask ≤3, Open questions ≤5)

If fail → reject with `exit code 2`, write to `inbox/anomalies/`. No commit.

### Step 2: Append-only ledger check
- Parse `## Decisions ledger` from the OLD rollup (at `git HEAD`)
- Parse same section from the NEW proposed rollup
- Assert every old line is present in the new content **unchanged and in
  order**. New lines may only appear **after** all old lines.

```
old: A B C
new: A B C D        ✓
new: A B C' D       ✗ (C modified)
new: A C D          ✗ (B removed)
new: A C B D        ✗ (reordered)
new: D A B C        ✗ (appended in wrong position)
```

### Step 3: Human-pin preservation
- Parse `## Verbatim pins` from OLD rollup
- Extract every line tagged `(— pinned by human)`
- Assert every such line is present **verbatim** in the new content
- Non-human pins (LLM-added) may be added/removed freely

### Step 4: Write + commit
If all checks pass:
- Write new `rollup.md`
- `git add state/threads/<id>/rollup.md`
- `git commit -m "rollup <thread-id>: <auto-summary of delta>"`
- Clear `.dirty`
- Write `inbox/updates/<thread-id>-<commit-sha>.json` to signal the
  commander on next tick

### Step 5: Post-commit hook re-validates (belt + suspenders)
A post-commit hook in `state/.git/hooks/post-commit` re-runs steps 0.5, 2,
and 3 against `HEAD~1 vs HEAD`. If somehow a violation slipped through, the
hook calls `git reset --hard HEAD~1` and writes to `inbox/anomalies/`.

## Failure handling

| Failure | Action |
|---|---|
| Schema reject | Inbox anomaly. Updater daemon retries once with a stricter prompt; if still fails, escalates to commander. |
| Ledger violation | Same as above. Inbox anomaly clearly labeled. |
| LLM output truncated | Detect missing closing sections; retry; if persistent, escalate. |
| LLM unavailable | Daemon backs off (exponential, 30s → 5m); `.dirty` stays set. |

The commander, on its next tick, sees `inbox/anomalies/` entries in the
DELTA section and decides whether to manually edit (`harness rollup edit`),
flush again, or escalate to a human.

## Why not let the updater edit incrementally?

We considered "diff-format" output (e.g., "add this line to Current ask").
Rejected for three reasons:

1. **Validation complexity.** A full-file output makes diff checks trivial.
   Incremental edits require parsing the LLM's intent first.
2. **Drift recovery.** If the rollup ever gets corrupted, regenerating from
   scratch is the recovery path. Forcing every update through that path
   normalizes the operation.
3. **Determinism.** Asking the LLM to "output the full new file"
   constrains its output. "Output a diff" gives it more degrees of freedom
   to surprise.

The cost is more tokens per update. Acceptable for a small model.

## Cost / model choice

- Model: Haiku-class. Cheap, fast.
- Typical input: 1–3k tokens (old rollup + ~5 new messages + goal).
- Typical output: 500–1000 tokens (full new rollup).
- Frequency: as fast as raw events come in, debounced. Realistic peak:
  one update every 30s per active thread.

## Per-thread vs global updaters

One worker daemon process can serve many threads, processing them
sequentially. There is no benefit to per-thread daemons; the bottleneck is
the LLM API, not local IO.

## A non-trivial edge case: thread with no goal

If a thread has no `owner_task` (e.g., it was just promoted from inbox/new
without commander triage yet), the updater receives a placeholder goal:

```
THREAD GOAL: (not yet assigned — summarize neutrally; surface what
this thread seems to be about under Current ask so the commander
can decide.)
```

The updater is told to surface intent rather than try to filter by an
unknown goal.

## Implementation hints for Track A

- `harness render updater-input` is implemented as a pure function over
  `state/threads/<id>/` + the linked task's `goal.md`. No LLM call.
- The updater daemon is a separate goroutine/process from the worker
  daemon. They have different model configurations and cost profiles.
- Validation logic is shared between `harness rollup update` and the
  post-commit hook — keep it in one Go package.
- The post-commit hook is installed by `harness init`.
