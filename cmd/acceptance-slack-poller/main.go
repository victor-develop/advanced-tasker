// Command acceptance-slack-poller is the deterministic acceptance harness
// for the Track B slack-poller. It spins up an httptest server with a
// pre-baked Slack-API response set, seeds a state/ dir, runs
// slack-poller --once against the mock, then computes a normalized hash
// of the resulting filesystem and compares against the golden snapshot
// stored at testdata/golden/snapshot.hash.
//
// Exit codes:
//
//	0  hash matches golden (acceptance pass)
//	1  hash mismatches (acceptance fail) — diff printed to stderr
//	2  setup / build failure
//
// Invocation (typically from scripts/acceptance-slack-poller.sh):
//
//	go run ./cmd/acceptance-slack-poller \
//	    -binary <path-to-slack-poller> \
//	    -golden <repo>/internal/slack/testdata/golden/snapshot.hash \
//	    [-update]
//
// With -update, the hash file is overwritten with the freshly computed
// value (use only when intentionally rebaselining).
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "acceptance: %v\n", err)
		os.Exit(2)
	}
}

type opts struct {
	binary string
	golden string
	update bool
	keep   bool
}

func run() error {
	o := &opts{}
	flag.StringVar(&o.binary, "binary", "", "path to slack-poller binary (required)")
	flag.StringVar(&o.golden, "golden", "", "path to golden hash file (required)")
	flag.BoolVar(&o.update, "update", false, "overwrite golden with fresh hash")
	flag.BoolVar(&o.keep, "keep", false, "keep the temp state dir (debug)")
	flag.Parse()

	if o.binary == "" || o.golden == "" {
		flag.Usage()
		return errors.New("-binary and -golden are required")
	}
	if _, err := os.Stat(o.binary); err != nil {
		return fmt.Errorf("binary %s: %w", o.binary, err)
	}

	srv := startMock()
	defer srv.Close()

	stateDir, cleanup, err := setupState()
	if err != nil {
		return fmt.Errorf("setup state: %w", err)
	}
	if !o.keep {
		defer cleanup()
	} else {
		fmt.Fprintf(os.Stderr, "keeping state dir: %s\n", stateDir)
	}

	if err := runPoller(o.binary, stateDir, srv.URL+"/"); err != nil {
		return fmt.Errorf("slack-poller --once: %w", err)
	}

	got := slackpkg.NormalizedHash(stateDir)
	if got == "" {
		return errors.New("normalized hash is empty; poller wrote nothing")
	}
	fmt.Printf("computed hash: %s\n", got)

	if o.update {
		if err := os.WriteFile(o.golden, []byte(got+"\n"), 0o644); err != nil {
			return fmt.Errorf("write golden: %w", err)
		}
		fmt.Fprintln(os.Stderr, "golden updated.")
		return nil
	}

	wantBytes, err := os.ReadFile(o.golden)
	if err != nil {
		return fmt.Errorf("read golden %s: %w", o.golden, err)
	}
	want := strings.TrimSpace(string(wantBytes))
	if got != want {
		fmt.Fprintf(os.Stderr,
			"HASH MISMATCH\n  got:  %s\n  want: %s\n  golden: %s\n",
			got, want, o.golden)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "PASS: hash matches golden")
	return nil
}

// startMock launches the deterministic Slack mock. The response set is the
// minimum needed to exercise:
//   - one tracked-thread reply (raw/<ts>.json + meta.json + .dirty)
//   - one channel-level new message (inbox/new/<id>.json)
//   - channel + thread cursor advances
func startMock() *httptest.Server {
	const (
		threadTS = "1700000000.000000"
		replyTS  = "1700000050.000000"
		newMsgTS = "1700000100.000100"
	)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		endpoint := strings.TrimPrefix(r.URL.Path, "/")
		switch endpoint {
		case "conversations.history":
			writeJSON(w, map[string]any{
				"ok": true,
				"messages": []map[string]any{
					{
						"type": "message",
						"ts":   newMsgTS,
						"user": "U_ALICE",
						"text": "acceptance new message",
					},
				},
				"has_more": false,
				"response_metadata": map[string]any{"next_cursor": ""},
			})
		case "conversations.replies":
			writeJSON(w, map[string]any{
				"ok": true,
				"messages": []map[string]any{
					{
						"type":      "message",
						"ts":        replyTS,
						"thread_ts": threadTS,
						"user":      "U_BOB",
						"text":      "acceptance reply",
					},
				},
				"has_more": false,
				"response_metadata": map[string]any{"next_cursor": ""},
			})
		case "chat.getPermalink":
			writeJSON(w, map[string]any{
				"ok": true,
				"permalink": "https://acceptance.test/archives/" +
					r.Form.Get("channel") + "/p" +
					strings.ReplaceAll(r.Form.Get("message_ts"), ".", ""),
			})
		default:
			writeJSON(w, map[string]any{"ok": false, "error": "unknown:" + endpoint})
		}
	}))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// setupState creates a temp state dir and seeds:
//   - state/sources/slack/config.yaml (token_env: TEST_SLACK_TOKEN)
//   - state/threads/slack-C0492-1700000000.000000/ (pre-tracked thread)
func setupState() (string, func(), error) {
	dir, err := os.MkdirTemp("", "acceptance-slack-poller-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	if err := os.MkdirAll(filepath.Join(dir, "sources", "slack"), 0o755); err != nil {
		cleanup()
		return "", nil, err
	}
	cfg := strings.TrimLeft(`
auth:
  token_env: TEST_SLACK_TOKEN
watch:
  channels:
    - id: C0492
      reason: "acceptance"
poll_interval: 30s
backoff:
  on_rate_limit: 60s
  on_error: 5s
  max_backoff: 5m
`, "\n")
	if err := os.WriteFile(filepath.Join(dir, "sources", "slack",
		"config.yaml"), []byte(cfg), 0o644); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "threads",
		"slack-C0492-1700000000.000000"), 0o755); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

func runPoller(binary, stateDir, apiURL string) error {
	cmd := exec.Command(binary, "--state-dir", stateDir,
		"--api-url", apiURL, "--once", "--log-level", "error")
	cmd.Env = append(os.Environ(), "TEST_SLACK_TOKEN=xoxb-acceptance")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	deadline := time.Now().Add(30 * time.Second)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Until(deadline)):
		_ = cmd.Process.Kill()
		return errors.New("timeout waiting for slack-poller --once")
	}
}
