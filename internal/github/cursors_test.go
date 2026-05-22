package github

import (
	"testing"
	"time"
)

func TestCursorStore_PR_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewCursorStore(dir)
	r := RepoRef{Owner: "acme", Repo: "api"}

	loaded, err := s.LoadPR(r, 1284)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.LastPolledAt.IsZero() {
		t.Errorf("empty load should give zero cursor; got %+v", loaded)
	}

	now := time.Date(2026, 5, 19, 10, 15, 0, 0, time.UTC)
	loaded.LastPolledAt = now
	loaded.Endpoints.IssueCommentsSince = now.Add(-time.Minute)
	loaded.AddReviewSeen(4567)
	loaded.AddReviewSeen(4568)
	loaded.Endpoints.IssueCommentsETag = `W/"abc"`
	if err := s.SavePR(r, 1284, loaded); err != nil {
		t.Fatal(err)
	}

	reloaded, err := s.LoadPR(r, 1284)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.LastPolledAt.Equal(now) {
		t.Errorf("last_polled_at: got %s want %s", reloaded.LastPolledAt, now)
	}
	if !reloaded.HasReviewSeen(4567) || !reloaded.HasReviewSeen(4568) {
		t.Errorf("review seen ids lost: %+v", reloaded.Endpoints.ReviewsSeenIDs)
	}
	if reloaded.Endpoints.IssueCommentsETag != `W/"abc"` {
		t.Errorf("etag lost: %q", reloaded.Endpoints.IssueCommentsETag)
	}
}

func TestAddReviewSeen_Idempotent(t *testing.T) {
	c := &PRCursor{}
	if !c.AddReviewSeen(42) {
		t.Error("first add should return true")
	}
	if c.AddReviewSeen(42) {
		t.Error("second add should return false")
	}
	if len(c.Endpoints.ReviewsSeenIDs) != 1 {
		t.Errorf("expected 1 id; got %v", c.Endpoints.ReviewsSeenIDs)
	}
}

func TestCursorStore_Repo_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewCursorStore(dir)
	r := RepoRef{Owner: "acme", Repo: "api"}

	c, err := s.LoadRepo(r)
	if err != nil {
		t.Fatal(err)
	}
	if !c.LastPRDiscoveryAt.IsZero() {
		t.Errorf("expected zero cursor; got %+v", c)
	}
	now := time.Now().UTC().Truncate(time.Second)
	c.LastPRDiscoveryAt = now
	c.PullsETag = `"deadbeef"`
	if err := s.SaveRepo(r, c); err != nil {
		t.Fatal(err)
	}
	c2, err := s.LoadRepo(r)
	if err != nil {
		t.Fatal(err)
	}
	if !c2.LastPRDiscoveryAt.Equal(now) {
		t.Errorf("LastPRDiscoveryAt: got %s want %s", c2.LastPRDiscoveryAt, now)
	}
	if c2.PullsETag != `"deadbeef"` {
		t.Errorf("PullsETag: got %q", c2.PullsETag)
	}
}
