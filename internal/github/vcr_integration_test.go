//go:build integration

package github

// This file verifies that the poller wires through an arbitrary
// http.RoundTripper, which means it can be driven by `go-vcr` cassettes
// or any other recorder.  We use a small in-memory roundtripper here
// (no cassette file) to keep the test hermetic; the same pattern can be
// swapped in for `vcr.New(...).GetDefaultClient()` once cassettes are
// recorded against a real GitHub PAT.
//
// See https://github.com/dnaeon/go-vcr for the recommended cassette
// workflow; cassettes live under `testdata/cassettes/` and are recorded
// once with a personal token then committed for offline replay.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/dnaeon/go-vcr.v4/pkg/recorder"
)

// TestIntegration_GoVCRWiring boots up an httptest server, records its
// interactions into a temp cassette using go-vcr ModeRecordOnce, and then
// replays them through the poller.  This proves the wiring path that real
// cassettes will travel.
func TestIntegration_GoVCRWiring(t *testing.T) {
	now := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)

	// Real-ish upstream that go-vcr will record.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[`))
			_, _ = w.Write(stubPRJSON(7777, "acme", "api", "VCR test PR",
				"alice", "open", now.Add(-2*time.Hour)))
			_, _ = w.Write([]byte(`]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cassetteDir := t.TempDir()
	cassettePath := filepath.Join(cassetteDir, "fresh-start")

	// First pass: record against the upstream.
	rec, err := recorder.New(cassettePath, recorder.WithMode(recorder.ModeRecordOnce))
	if err != nil {
		t.Fatal(err)
	}
	httpClient := rec.GetDefaultClient()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfgPath, []byte(`
auth:
  token_env: TEST_VCR_TOKEN
watch:
  repos: [acme/api]
poll_interval: 60s
new_pr_lookback: 7d
max_concurrent_pr_polls: 1
`), 0o644)
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// Point the github client's underlying http.Client at the recorder
	// AND set the base URL to the upstream so go-vcr captures the
	// request.  We disable retries (per_page paging beyond first page,
	// PR cursor follow-on calls) by not preparing a tracked PR.
	client, err := NewClient("test-token", upstream.URL+"/", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	p := &Poller{
		Config:  cfg,
		Client:  client,
		Cursors: NewCursorStore(dir),
		Writer:  NewWriter(dir),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     func() time.Time { return now },
	}
	stats, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.PRsDiscovered != 1 {
		t.Errorf("recording pass: expected 1 PR discovered; got %d", stats.PRsDiscovered)
	}
	if err := rec.Stop(); err != nil {
		t.Fatal(err)
	}

	// Verify the cassette file actually got written.
	cassetteFile := cassettePath + ".yaml"
	if _, err := os.Stat(cassetteFile); err != nil {
		t.Fatalf("cassette file missing: %v", err)
	}
	info, _ := os.Stat(cassetteFile)
	if info.Size() == 0 {
		t.Fatal("cassette file is empty")
	}

	// Now shut down the upstream and replay-only.
	upstream.Close()
	rec2, err := recorder.New(cassettePath, recorder.WithMode(recorder.ModeReplayOnly))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rec2.Stop() }()
	httpClient2 := rec2.GetDefaultClient()
	client2, err := NewClient("test-token", upstream.URL+"/", httpClient2)
	if err != nil {
		t.Fatal(err)
	}
	dir2 := t.TempDir()
	cfgPath2 := filepath.Join(dir2, "config.yaml")
	_ = os.WriteFile(cfgPath2, []byte(`
auth:
  token_env: TEST_VCR_TOKEN
watch:
  repos: [acme/api]
poll_interval: 60s
new_pr_lookback: 7d
max_concurrent_pr_polls: 1
`), 0o644)
	cfg2, _ := LoadConfig(cfgPath2)
	p2 := &Poller{
		Config:  cfg2,
		Client:  client2,
		Cursors: NewCursorStore(dir2),
		Writer:  NewWriter(dir2),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     func() time.Time { return now },
	}
	stats2, err := p2.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}
	if stats2.PRsDiscovered != 1 {
		t.Errorf("replay pass: expected 1 PR discovered; got %d", stats2.PRsDiscovered)
	}
}
