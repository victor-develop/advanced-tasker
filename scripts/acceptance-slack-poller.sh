#!/usr/bin/env bash
# scripts/acceptance-slack-poller.sh — Track B acceptance script.
#
# Builds slack-poller, then runs cmd/acceptance-slack-poller, which:
#   1. Spins up a deterministic httptest mock Slack server
#   2. Seeds a temp state/ directory with one tracked thread + one watched
#      channel
#   3. Runs `slack-poller --once --api-url <mock>` against the mock
#   4. Computes a normalized hash over the produced files (raw/<ts>.json,
#      meta.json, .dirty, inbox/new/*.json, cursors) — wall-clock fields
#      are stripped before hashing so the result is reproducible
#   5. Compares to the golden hash at
#      internal/slack/testdata/golden/snapshot.hash
#
# Exit 0 on match; non-zero on mismatch or setup error.
#
# Use --update to overwrite the golden file (rebaseline).
set -euo pipefail

cd "$(dirname "$0")/.."
repo_root="$(pwd)"

build_dir="$(mktemp -d)"
trap 'rm -rf "$build_dir"' EXIT

binary="$build_dir/slack-poller"
echo "==> building slack-poller..."
go build -o "$binary" ./cmd/slack-poller

golden="$repo_root/internal/slack/testdata/golden/snapshot.hash"
mkdir -p "$(dirname "$golden")"

extra_args=()
if [[ "${1:-}" == "--update" ]]; then
  extra_args+=(-update)
fi

echo "==> running acceptance driver..."
go run ./cmd/acceptance-slack-poller \
  -binary "$binary" \
  -golden "$golden" \
  ${extra_args[@]+"${extra_args[@]}"}

echo "==> OK"
