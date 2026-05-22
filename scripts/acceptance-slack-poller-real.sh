#!/usr/bin/env bash
# scripts/acceptance-slack-poller-real.sh -- Track B REAL acceptance.
#
# Round-3 §D4 (slack-poller-r3). This script hits a REAL Slack workspace
# using the operator-provided $SLACK_BOT_TOKEN and $TEST_SLACK_CHANNEL,
# and verifies the slack-poller produces the expected on-disk artifacts.
#
# Differences vs scripts/acceptance-slack-poller.sh (mock-based):
#   - Talks to slack.com, NOT an httptest mock.
#   - Skips cleanly when either env var is unset (so CI keeps green).
#   - Does NOT validate against a golden hash (real Slack data is
#     non-deterministic). Instead asserts file presence + cursor advance.
#   - Performs ZERO writes to Slack (no chat.postMessage, reactions.add,
#     chat.delete). The slack-poller binary is read-only by design.
#
# Required env:
#   SLACK_BOT_TOKEN     bot user OAuth token (xoxb-...)
#   TEST_SLACK_CHANNEL  channel ID (e.g. C0B071K1SFP)
#
# Optional:
#   ACCEPTANCE_KEEP_STATE=1   leave the temp state dir for inspection
#
# Exit codes:
#   0   all checks passed (or skipped because env was missing)
#   1   setup / build / hard failure
#   2   acceptance assertion failed (file missing, cursor not advanced)
#   3   slack-poller doctor reported a hard auth/channel failure
set -euo pipefail

cd "$(dirname "$0")/.."
repo_root="$(pwd)"

# -- Skip path -----------------------------------------------------------------
if [[ -z "${SLACK_BOT_TOKEN:-}" || -z "${TEST_SLACK_CHANNEL:-}" ]]; then
  echo "SKIP: SLACK_BOT_TOKEN or TEST_SLACK_CHANNEL not set."
  echo "      Set both to run the REAL Slack e2e (no writes performed)."
  exit 0
fi

# -- Sanity: bot tokens only ---------------------------------------------------
if [[ "$SLACK_BOT_TOKEN" != xoxb-* ]]; then
  echo "WARN: SLACK_BOT_TOKEN does not start with 'xoxb-'."
  echo "      slack-poller is designed for bot user tokens; user tokens may"
  echo "      pass auth.test but lack the right scopes."
fi

# -- Build ---------------------------------------------------------------------
build_dir="$(mktemp -d)"
state_dir="$(mktemp -d)"
cleanup() {
  rm -rf "$build_dir"
  if [[ "${ACCEPTANCE_KEEP_STATE:-0}" != "1" ]]; then
    rm -rf "$state_dir"
  else
    echo "(keeping state dir at $state_dir for inspection)"
  fi
}
trap cleanup EXIT

binary="$build_dir/slack-poller"
echo "==> building slack-poller..."
go build -o "$binary" ./cmd/slack-poller

# -- State scaffolding ---------------------------------------------------------
echo "==> seeding $state_dir/sources/slack/config.yaml..."
mkdir -p "$state_dir/sources/slack"
cat > "$state_dir/sources/slack/config.yaml" <<EOF
auth:
  token_env: SLACK_BOT_TOKEN
watch:
  channels:
    - id: $TEST_SLACK_CHANNEL
      reason: "round-3 real e2e"
poll_interval: 30s
backoff:
  on_rate_limit: 60s
  on_error: 5s
  max_backoff: 5m
EOF

# -- Doctor (real first-boot smoke) --------------------------------------------
echo "==> running 'slack-poller doctor'..."
set +e
"$binary" --state-dir "$state_dir" --log-level error doctor
doctor_code=$?
set -e
echo "    doctor exited with $doctor_code"
if [[ $doctor_code -eq 1 ]]; then
  echo "FAIL: doctor reported a hard failure (token / channel access)."
  exit 3
fi
if [[ $doctor_code -eq 2 ]]; then
  echo "WARN: doctor reported only soft signals (e.g. empty channel)."
fi
if [[ $doctor_code -gt 2 ]]; then
  echo "FAIL: unexpected doctor exit code $doctor_code"
  exit 1
fi

# -- One real poll cycle -------------------------------------------------------
echo "==> running 'slack-poller --once' against slack.com..."
set +e
"$binary" --state-dir "$state_dir" --log-level info --once
poll_code=$?
set -e
if [[ $poll_code -ne 0 ]]; then
  echo "FAIL: slack-poller --once exited $poll_code"
  exit 1
fi

# -- Assertions ----------------------------------------------------------------
echo "==> asserting filesystem outputs..."

cursor_file="$state_dir/sources/slack/cursors/channels/$TEST_SLACK_CHANNEL.json"
if [[ ! -f "$cursor_file" ]]; then
  echo "FAIL: cursor file missing: $cursor_file"
  exit 2
fi
last_ts="$(awk -F'"' '/last_ts/ {print $4; exit}' "$cursor_file")"
if [[ -z "$last_ts" ]]; then
  echo "FAIL: cursor file has empty last_ts: $cursor_file"
  cat "$cursor_file"
  exit 2
fi
echo "    cursor: $cursor_file last_ts=$last_ts"

# Either inbox/new entries OR tracked threads must exist (both are valid).
inbox_new_count=0
threads_count=0
if [[ -d "$state_dir/inbox/new" ]]; then
  inbox_new_count="$(find "$state_dir/inbox/new" -name "slack-$TEST_SLACK_CHANNEL-*.json" 2>/dev/null | wc -l | tr -d ' ')"
fi
if [[ -d "$state_dir/threads" ]]; then
  threads_count="$(find "$state_dir/threads" -maxdepth 1 -type d -name "slack-$TEST_SLACK_CHANNEL-*" 2>/dev/null | wc -l | tr -d ' ')"
fi
echo "    inbox/new slack-$TEST_SLACK_CHANNEL-*.json: $inbox_new_count"
echo "    threads/slack-$TEST_SLACK_CHANNEL-*: $threads_count"

if [[ "$inbox_new_count" -eq 0 && "$threads_count" -eq 0 ]]; then
  echo "NOTE: channel is empty (no messages); cursor advance is the only signal."
  echo "      This is allowed — soft pass."
fi

# Sample one inbox/new entry (if present) and assert raw_inline shape (D1 check)
sample_inbox="$(find "$state_dir/inbox/new" -name "slack-$TEST_SLACK_CHANNEL-*.json" 2>/dev/null | head -1 || true)"
if [[ -n "$sample_inbox" ]]; then
  if ! grep -q '"raw_inline"' "$sample_inbox"; then
    echo "FAIL: inbox/new entry missing raw_inline: $sample_inbox"
    cat "$sample_inbox"
    exit 2
  fi
  if grep -q '"raw_path"' "$sample_inbox"; then
    echo "FAIL: inbox/new entry contains forbidden raw_path: $sample_inbox"
    cat "$sample_inbox"
    exit 2
  fi
  echo "    sample inbox/new entry passes raw_inline schema check"
fi

# -- Read-only invariant -------------------------------------------------------
echo "==> verifying slack-poller binary has no write-to-Slack codepaths..."
forbidden_endpoints=(chat.postMessage chat.update chat.delete reactions.add reactions.remove)
violations=0
for ep in "${forbidden_endpoints[@]}"; do
  # The slack-go SDK uses these method names as Go function names; we grep
  # the compiled binary for references. Note: this is best-effort — Go
  # binaries can strip method names with -trimpath, but the default build
  # retains them.
  if strings "$binary" 2>/dev/null | grep -q "$ep"; then
    echo "FAIL: binary references forbidden Slack endpoint $ep"
    violations=$((violations+1))
  fi
done
if [[ $violations -gt 0 ]]; then
  echo "FAIL: slack-poller binary contains $violations write-to-Slack endpoint(s)"
  exit 2
fi
echo "    OK: binary contains no chat.postMessage / reactions.add / chat.delete refs"

# -- Summary -------------------------------------------------------------------
echo
echo "==> Files written under $state_dir:"
find "$state_dir" -type f 2>/dev/null | sed "s|^$state_dir|.|" | sort

echo
echo "==> PASS (real e2e against $TEST_SLACK_CHANNEL)"
exit 0
