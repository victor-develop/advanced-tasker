package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ghp "github.com/victor-develop/advanced-tasker/internal/github"
)

// seedConfig writes a minimal state/sources/github/config.yaml with the
// given repos.
func seedConfig(t *testing.T, stateRoot string, repos ...string) {
	t.Helper()
	cfgDir := filepath.Join(stateRoot, "sources", "github")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "auth:\n  token_env: GITHUB_TOKEN\nwatch:\n  repos:\n"
	for _, r := range repos {
		body += "    - " + r + "\n"
	}
	if len(repos) == 0 {
		body = "auth:\n  token_env: GITHUB_TOKEN\nwatch:\n  repos: []\n"
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDoWatch_AppendsRepo(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir)
	if err := DoWatch(dir, "acme/api"); err != nil {
		t.Fatal(err)
	}
	root, err := ghp.LoadConfigRaw(ghp.DefaultConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	got := ghp.WatchRepos(root)
	if len(got) != 1 || got[0] != "acme/api" {
		t.Errorf("watch.repos got %v", got)
	}
	// Cursor directories created.
	for _, sub := range []string{
		"sources/github/cursors/repos",
		"sources/github/cursors/prs",
	} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
			t.Errorf("missing cursor dir %s: %v", sub, err)
		}
	}
}

func TestDoWatch_Idempotent(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir, "acme/api")
	if err := DoWatch(dir, "acme/api"); err != nil {
		t.Fatalf("first watch: %v", err)
	}
	if err := DoWatch(dir, "acme/api"); err != nil {
		t.Fatalf("idempotent re-watch: %v", err)
	}
	root, _ := ghp.LoadConfigRaw(ghp.DefaultConfigPath(dir))
	got := ghp.WatchRepos(root)
	if len(got) != 1 {
		t.Errorf("expected exactly 1 repo; got %v", got)
	}
}

func TestDoWatch_MissingConfig(t *testing.T) {
	dir := t.TempDir()
	err := DoWatch(dir, "acme/api")
	if err == nil || !strings.Contains(err.Error(), "harness config init github") {
		t.Fatalf("expected ErrConfigMissing; got %v", err)
	}
}

func TestDoUnwatch_RemovesRepoAndCursors(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir, "acme/api", "acme/ingest")

	// Pre-seed a fake PR cursor under acme/api.
	cursors := ghp.NewCursorStore(dir)
	r := ghp.RepoRef{Owner: "acme", Repo: "api"}
	c := &ghp.PRCursor{}
	c.Endpoints.IssueCommentsETag = `"x"`
	if err := cursors.SavePR(r, 1284, c); err != nil {
		t.Fatal(err)
	}
	repoCur := &ghp.RepoCursor{PullsETag: `"r"`}
	if err := cursors.SaveRepo(r, repoCur); err != nil {
		t.Fatal(err)
	}

	if err := DoUnwatch(dir, "acme/api"); err != nil {
		t.Fatal(err)
	}
	root, _ := ghp.LoadConfigRaw(ghp.DefaultConfigPath(dir))
	got := ghp.WatchRepos(root)
	if len(got) != 1 || got[0] != "acme/ingest" {
		t.Errorf("expected only acme/ingest remaining; got %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "sources", "github", "cursors", "repos", "acme-api.json")); !os.IsNotExist(err) {
		t.Errorf("repo cursor should be removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sources", "github", "cursors", "prs", "acme-api-1284.json")); !os.IsNotExist(err) {
		t.Errorf("pr cursor should be removed: %v", err)
	}
}

func TestDoUnwatch_NotWatchedIsNoOp(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir, "acme/api")
	if err := DoUnwatch(dir, "other/repo"); err != nil {
		t.Fatal(err)
	}
	root, _ := ghp.LoadConfigRaw(ghp.DefaultConfigPath(dir))
	if got := ghp.WatchRepos(root); len(got) != 1 {
		t.Errorf("expected unchanged repos; got %v", got)
	}
}

func TestDoTrackPR_PromotesInboxNew(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir, "acme/api")

	// Seed an inbox/new entry.
	inboxDir := filepath.Join(dir, "inbox", "new")
	if err := os.MkdirAll(inboxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	item := map[string]any{
		"id":          "github-acme-api-pr-1284",
		"source":      "github",
		"kind":        "new",
		"received_at": "2026-05-19T10:15:00Z",
		"summary":     "alice opened PR 1284",
		"ref": map[string]any{
			"owner":  "acme",
			"repo":   "api",
			"number": 1284,
			"title":  "x",
			"author": "alice",
			"url":    "https://github.com/acme/api/pull/1284",
			"state":  "open",
			"draft":  false,
		},
	}
	data, _ := json.Marshal(item)
	if err := os.WriteFile(filepath.Join(inboxDir, "github-acme-api-pr-1284.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := DoTrackPR(dir, "acme/api", 1284); err != nil {
		t.Fatal(err)
	}
	// Thread dir + meta.json exist.
	threadDir := filepath.Join(dir, "threads", "github-acme-api-pr-1284")
	if _, err := os.Stat(filepath.Join(threadDir, "meta.json")); err != nil {
		t.Errorf("meta.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(threadDir, ".dirty")); err != nil {
		t.Errorf(".dirty missing: %v", err)
	}
	// Inbox entry removed.
	if _, err := os.Stat(filepath.Join(inboxDir, "github-acme-api-pr-1284.json")); !os.IsNotExist(err) {
		t.Errorf("inbox/new entry should be removed: %v", err)
	}
}

func TestDoTrackPR_NoInboxEntryStillSucceeds(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir, "acme/api")
	// No inbox entry — track-pr is idempotent and should still create
	// the thread dir + meta.json (operator may have run `harness thread
	// track` first).
	if err := DoTrackPR(dir, "acme/api", 9999); err != nil {
		t.Fatal(err)
	}
	threadDir := filepath.Join(dir, "threads", "github-acme-api-pr-9999")
	if _, err := os.Stat(filepath.Join(threadDir, "meta.json")); err != nil {
		t.Errorf("meta.json missing: %v", err)
	}
}

func TestDoTrackPR_Idempotent(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir, "acme/api")
	if err := DoTrackPR(dir, "acme/api", 1284); err != nil {
		t.Fatal(err)
	}
	// Capture meta to confirm second call doesn't clobber.
	writer := ghp.NewWriter(dir)
	meta1, _ := writer.LoadMeta("github-acme-api-pr-1284")
	if err := DoTrackPR(dir, "acme/api", 1284); err != nil {
		t.Fatalf("re-track failed: %v", err)
	}
	meta2, _ := writer.LoadMeta("github-acme-api-pr-1284")
	if !meta1.TrackingSince.Equal(meta2.TrackingSince) {
		t.Error("re-track clobbered tracking_since")
	}
}

func TestDoUntrackPR_WithArchive(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir, "acme/api")
	if err := DoTrackPR(dir, "acme/api", 1284); err != nil {
		t.Fatal(err)
	}
	// Pre-seed a cursor so we can verify it's removed.
	cursors := ghp.NewCursorStore(dir)
	if err := cursors.SavePR(ghp.RepoRef{Owner: "acme", Repo: "api"}, 1284, &ghp.PRCursor{}); err != nil {
		t.Fatal(err)
	}

	if err := DoUntrackPR(dir, "acme/api", 1284, true); err != nil {
		t.Fatal(err)
	}
	// Thread dir should be moved out of threads/<id>.
	if _, err := os.Stat(filepath.Join(dir, "threads", "github-acme-api-pr-1284")); !os.IsNotExist(err) {
		t.Errorf("thread should be gone from threads/: %v", err)
	}
	// Archived copy exists.
	matches, _ := filepath.Glob(filepath.Join(dir, "threads", "_archive", "github-acme-api-pr-1284-*"))
	if len(matches) != 1 {
		t.Errorf("expected 1 archive entry; got %v", matches)
	}
	// Cursor gone.
	if _, err := os.Stat(filepath.Join(dir, "sources", "github", "cursors", "prs", "acme-api-1284.json")); !os.IsNotExist(err) {
		t.Errorf("cursor should be removed: %v", err)
	}
}

func TestDoUntrackPR_NoArchive(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir, "acme/api")
	if err := DoTrackPR(dir, "acme/api", 1284); err != nil {
		t.Fatal(err)
	}
	if err := DoUntrackPR(dir, "acme/api", 1284, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "threads", "github-acme-api-pr-1284")); err != nil {
		t.Errorf("thread dir should remain when not archiving: %v", err)
	}
}

func TestBuildStatus_JSON(t *testing.T) {
	dir := t.TempDir()
	seedConfig(t, dir, "acme/api", "acme/ingest")
	cursors := ghp.NewCursorStore(dir)
	r := ghp.RepoRef{Owner: "acme", Repo: "api"}
	c := &ghp.PRCursor{}
	c.Endpoints.IssueCommentsETag = `"x"`
	c.AddReviewSeen(1)
	c.AddReviewSeen(2)
	if err := cursors.SavePR(r, 1284, c); err != nil {
		t.Fatal(err)
	}

	rep, err := BuildStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Repos) != 2 {
		t.Errorf("expected 2 repos; got %v", rep.Repos)
	}
	var apiRepo *RepoStatus
	for i := range rep.Repos {
		if rep.Repos[i].Repo == "acme/api" {
			apiRepo = &rep.Repos[i]
			break
		}
	}
	if apiRepo == nil {
		t.Fatal("acme/api missing from status")
	}
	if len(apiRepo.Tracked) != 1 || apiRepo.Tracked[0].Number != 1284 {
		t.Errorf("expected #1284 tracked; got %v", apiRepo.Tracked)
	}
	if !apiRepo.Tracked[0].ETags.IssueComments {
		t.Error("expected IssueComments ETag flag")
	}
	if apiRepo.Tracked[0].ReviewsSeenCount != 2 {
		t.Errorf("reviews_seen_count: %d", apiRepo.Tracked[0].ReviewsSeenCount)
	}
}

func TestBuildStatus_MissingConfigIsTolerated(t *testing.T) {
	dir := t.TempDir()
	rep, err := BuildStatus(dir)
	if err != nil {
		t.Fatalf("status should tolerate missing config: %v", err)
	}
	if len(rep.Repos) != 0 {
		t.Errorf("expected no repos; got %v", rep.Repos)
	}
}
