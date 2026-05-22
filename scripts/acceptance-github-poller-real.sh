#!/usr/bin/env bash
# Round-3 REAL acceptance harness for Track C (GitHub PR poller).
#
# Drives the actual github-poller binary against api.github.com using a
# token that the lead obtains via `gh auth token` (or any other PAT with
# `repo` scope) and a repo specified by TEST_GH_REPO.  This is the
# closing-round companion to scripts/acceptance-github-poller.sh, which
# is mock-only and remains the CI default.
#
# Required env vars:
#   GITHUB_TOKEN   PAT (or token from `gh auth token`); scope: repo
#   TEST_GH_REPO   owner/name (e.g., "victor-develop/advanced-tasker")
#
# Skip semantics:
#   When either env var is unset/empty, the script exits 0 with a clear
#   message.  This keeps the script safe to invoke from CI matrices.
#
# What it does:
#   1. Builds the binary
#   2. Seeds a tmp state dir + state/sources/github/config.yaml with
#      auth.token_env=GITHUB_TOKEN, watch.repos=[$TEST_GH_REPO]
#   3. Runs `github-poller doctor` — fails closed if any check fails
#   4. Runs `github-poller --once` (= run --once)
#   5. Asserts:
#        a. (no minimum) state/inbox/new/github-*.json may exist
#        b. cursor file at state/sources/github/cursors/repos/<o>-<r>.json
#           exists with last_pr_discovery_at non-empty
#        c. cursor has an ETag-equivalent field for the discovery endpoint
#           (`pulls_etag`)
#        d. any threads/ directories that the cycle created have a valid
#           meta.json
#   6. Verifies the poller binary issues ZERO write HTTP verbs by
#      grepping the source tree
#
# ZERO writes to GitHub (no comment posts, no review submits, no merges)
# — the poller is read-only by design.  The grep guard in step 6 enforces
# this at the source level.
#
# Exit codes:
#   0  all assertions passed (or skip due to missing env)
#   non-zero  first failing step

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

# --- skip-if-missing-env ---------------------------------------------------
if [ -z "${GITHUB_TOKEN:-}" ] || [ -z "${TEST_GH_REPO:-}" ]; then
    echo "==> REAL acceptance SKIPPED"
    echo "    GITHUB_TOKEN and TEST_GH_REPO must both be set to run this."
    echo "    GITHUB_TOKEN: $( [ -z "${GITHUB_TOKEN:-}" ] && echo "(unset)" || echo "(set, length=${#GITHUB_TOKEN})" )"
    echo "    TEST_GH_REPO: ${TEST_GH_REPO:-(unset)}"
    echo
    echo "    Hint: GITHUB_TOKEN=\$(gh auth token) TEST_GH_REPO=victor-develop/advanced-tasker $0"
    exit 0
fi

# --- sanity: TEST_GH_REPO shape -------------------------------------------
case "$TEST_GH_REPO" in
    */*) : ;;
    *)
        echo "FAIL: TEST_GH_REPO must be owner/repo, got: $TEST_GH_REPO" >&2
        exit 2
        ;;
esac
OWNER="${TEST_GH_REPO%/*}"
REPO="${TEST_GH_REPO#*/}"

# --- 1. Build -------------------------------------------------------------
echo "==> 1/6 building github-poller"
go build ./...

TMP="$(mktemp -d -t gh-poller-real.XXXXXX)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

STATE="$TMP/state"
mkdir -p "$STATE/sources/github"
mkdir -p "$STATE/inbox/new"
mkdir -p "$STATE/inbox/updates"
mkdir -p "$STATE/inbox/anomalies"
mkdir -p "$STATE/threads"
mkdir -p "$STATE/sources/github/cursors/repos"
mkdir -p "$STATE/sources/github/cursors/prs"

cat > "$STATE/sources/github/config.yaml" <<EOF
auth:
  token_env: GITHUB_TOKEN
  type: pat
watch:
  repos:
    - $TEST_GH_REPO
poll_interval: 60s
new_pr_lookback: 7d
max_concurrent_pr_polls: 2
backoff:
  on_rate_limit: 60s
  on_error: 5s
  max_backoff: 5m
EOF

# Build a dedicated binary copy so we're not racing with concurrent builds.
BIN="$TMP/github-poller"
go build -o "$BIN" ./cmd/github-poller

# --- 2. Doctor preflight --------------------------------------------------
echo "==> 2/6 running 'github-poller doctor' against $TEST_GH_REPO"
if ! "$BIN" --state-dir "$STATE" doctor; then
    echo "FAIL: doctor preflight failed; see output above" >&2
    exit 2
fi

# --- 3. One-shot poll -----------------------------------------------------
echo "==> 3/6 running 'github-poller --once' against $TEST_GH_REPO"
if ! "$BIN" --state-dir "$STATE" --once; then
    echo "FAIL: poller --once returned non-zero exit" >&2
    exit 3
fi

# --- 4. Assertions on filesystem output -----------------------------------
echo "==> 4/6 asserting filesystem output contract"

# 4a. inbox/new/github-*.json: may be present, count not constrained.
INBOX_NEW_COUNT=$(find "$STATE/inbox/new" -name 'github-*.json' -type f 2>/dev/null | wc -l | tr -d ' ')
echo "    inbox/new entries: $INBOX_NEW_COUNT"

# 4b. cursor file exists.
CURSOR="$STATE/sources/github/cursors/repos/${OWNER}-${REPO}.json"
if [ ! -f "$CURSOR" ]; then
    echo "FAIL: missing repo cursor $CURSOR" >&2
    exit 4
fi

# 4c. cursor has last_pr_discovery_at + ETag.
python3 - "$CURSOR" <<'PY'
import json, sys
path = sys.argv[1]
with open(path) as f:
    cur = json.load(f)
lpda = cur.get("last_pr_discovery_at", "")
if not lpda:
    print(f"FAIL: cursor missing last_pr_discovery_at: {cur}", file=sys.stderr)
    sys.exit(5)
etag = cur.get("pulls_etag", "")
if not etag:
    # GitHub may legitimately omit ETag in some cases, but we require it
    # for the round-3 real acceptance contract.  If this trips, capture
    # the bare response so the lead can investigate.
    print(f"FAIL: cursor missing pulls_etag (ETag): {cur}", file=sys.stderr)
    sys.exit(5)
print(f"    cursor.last_pr_discovery_at: {lpda}")
print(f"    cursor.pulls_etag: {etag[:32]}{'...' if len(etag)>32 else ''}")
PY

# 4d. any thread dirs that exist must have valid meta.json.
THREAD_COUNT=0
INVALID_META=0
for d in "$STATE/threads"/github-*; do
    [ -d "$d" ] || continue
    THREAD_COUNT=$((THREAD_COUNT + 1))
    if [ ! -f "$d/meta.json" ]; then
        echo "FAIL: thread $d missing meta.json" >&2
        INVALID_META=$((INVALID_META + 1))
        continue
    fi
    if ! python3 -c "import json,sys; m=json.load(open(sys.argv[1])); assert m.get('id') and m.get('source')=='github'" "$d/meta.json"; then
        echo "FAIL: thread $d has invalid meta.json" >&2
        INVALID_META=$((INVALID_META + 1))
    fi
done
echo "    tracked thread dirs: $THREAD_COUNT (expected 0 on fresh run)"
if [ "$INVALID_META" -ne 0 ]; then
    exit 4
fi

# --- 5. ZERO GitHub writes: source-level guard ----------------------------
echo "==> 5/6 grepping source tree for forbidden write verbs"
# We scan the poller code (cmd/github-poller + internal/github) for any
# call that would issue POST/PUT/PATCH/DELETE against repo endpoints.
# go-github's typed methods carry the HTTP verb implicitly, so we match
# both raw NewRequest calls and the typed mutate methods.
#
# Whitelist:
#   - test files (*_test.go) — they may construct synthetic responses
#   - vcr / cassettes — they may replay any verb
#
# To allow grep to find both lowercase ("post") and uppercase ("POST"),
# we look for the standard quoted strings as they appear in NewRequest
# calls.
FOUND=0
for verb in POST PUT PATCH DELETE; do
    HITS=$(grep -RIn --include='*.go' \
            --exclude='*_test.go' \
            --exclude='vcr_*.go' \
            -E "NewRequest\([^,]*,\s*\"$verb\"|Method:\s*\"$verb\"|http\\.Method$verb" \
            cmd/github-poller internal/github 2>/dev/null || true)
    if [ -n "$HITS" ]; then
        echo "FAIL: found $verb usage in non-test code:" >&2
        echo "$HITS" >&2
        FOUND=$((FOUND + 1))
    fi
done

# Also check that no typed mutating method names from go-github are used.
# We scan a conservative list of known mutators on PullRequests / Issues
# / Repositories — extend as needed.
MUTATORS='\.Create(|\.Edit(|\.Delete(|\.Merge(|\.UpdateBranch(|\.RequestReviewers(|\.RemoveReviewers(|\.CreateReview(|\.SubmitReview(|\.DismissReview(|\.UpdateReview('
HITS=$(grep -RIn --include='*.go' \
        --exclude='*_test.go' \
        --exclude='vcr_*.go' \
        -E "PullRequests$MUTATORS|Issues$MUTATORS|Repositories$MUTATORS" \
        cmd/github-poller internal/github 2>/dev/null || true)
if [ -n "$HITS" ]; then
    echo "FAIL: found go-github mutating method in non-test code:" >&2
    echo "$HITS" >&2
    FOUND=$((FOUND + 1))
fi

if [ "$FOUND" -gt 0 ]; then
    exit 6
fi
echo "    ok: no POST/PUT/PATCH/DELETE verbs in poller source"

# --- 6. Summary -----------------------------------------------------------
echo
echo "==> 6/6 ALL GOOD"
echo "    binary built"
echo "    doctor preflight: ok"
echo "    --once cycle: ok"
echo "    inbox/new entries: $INBOX_NEW_COUNT"
echo "    repo cursor: $CURSOR (ETag captured)"
echo "    tracked thread dirs: $THREAD_COUNT"
echo "    source-level GitHub write guard: ok"
