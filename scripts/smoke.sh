#!/usr/bin/env bash
# Manual smoke test: spin up a local httpserver returning canned PR data
# and drive the binary against it.  Verifies a tracked PR results in a
# raw event + .dirty marker on disk.
#
# Run from the worktree root:
#   ./scripts/smoke.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SMOKE_DIR="$(mktemp -d -t gh-poller-smoke.XXXXXX)"
echo "smoke dir: $SMOKE_DIR"

mkdir -p "$SMOKE_DIR/state/sources/github"
mkdir -p "$SMOKE_DIR/state/threads/github-acme-api-pr-1284/raw"

cat > "$SMOKE_DIR/state/sources/github/config.yaml" <<EOF
auth:
  token_env: SMOKE_TOKEN
watch:
  repos: [acme/api]
poll_interval: 60s
new_pr_lookback: 7d
EOF

# Start a tiny stub HTTP server.
python3 - "$SMOKE_DIR" <<'PY' &
import http.server, json, sys, os
state_dir = sys.argv[1]

class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path == "/repos/acme/api/pulls":
            data = b'[]'
        elif path == "/repos/acme/api/pulls/1284":
            data = json.dumps({
                "id": 1, "number": 1284, "state": "open", "title": "x",
                "html_url": "https://example.com/acme/api/pull/1284",
                "user": {"id": 1, "login": "alice"},
                "created_at": "2026-05-18T00:00:00Z",
                "updated_at": "2026-05-19T00:00:00Z",
            }).encode()
        elif path == "/repos/acme/api/issues/1284/comments":
            data = json.dumps([{
                "id": 99001, "user": {"id": 1, "login": "alice"},
                "body": "smoke comment",
                "created_at": "2026-05-19T01:00:00Z",
                "updated_at": "2026-05-19T01:00:00Z",
            }]).encode()
        elif path == "/repos/acme/api/pulls/1284/comments" or path == "/repos/acme/api/pulls/1284/reviews":
            data = b'[]'
        else:
            self.send_response(404); self.end_headers(); return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
    def log_message(self, *a, **k): pass

http.server.HTTPServer(("127.0.0.1", 18787), H).serve_forever()
PY
SERVER_PID=$!
trap 'kill $SERVER_PID 2>/dev/null || true' EXIT
sleep 0.5

cd "$ROOT"
go build -o "$SMOKE_DIR/github-poller" ./cmd/github-poller

SMOKE_TOKEN=stub "$SMOKE_DIR/github-poller" \
    --state-root="$SMOKE_DIR/state" \
    --github-base-url="http://127.0.0.1:18787/" \
    --once

THREAD="$SMOKE_DIR/state/threads/github-acme-api-pr-1284"
test -f "$THREAD/.dirty" || { echo "FAIL: .dirty missing"; exit 1; }
test -f "$THREAD/meta.json" || { echo "FAIL: meta.json missing"; exit 1; }
test -f "$THREAD/raw/issue-comment-99001.json" || { echo "FAIL: raw event missing"; exit 1; }

echo
echo "OK: raw event + .dirty + meta.json written"
echo "raw files:"
ls -la "$THREAD/raw/"
