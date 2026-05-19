package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v75/github"
)

// OverlapBuffer is the design/09 §Dedup `since=last_polled_at - 60s` slack.
const OverlapBuffer = 60 * time.Second

// Poller orchestrates a single poll cycle (or a daemon loop).
type Poller struct {
	Config  *Config
	Client  *Client
	Cursors *CursorStore
	Writer  *Writer
	Logger  *slog.Logger
	Now     func() time.Time

	// MaxPagesPerEndpoint caps how many pages we'll walk in one cycle to
	// keep a single PR from monopolising the rate budget.  Default 5.
	MaxPagesPerEndpoint int
}

// CycleStats summarises a single cycle for logging / tests.
type CycleStats struct {
	ReposPolled       int
	PRsDiscovered     int
	PRsPolled         int
	NewInboxItems     int
	UpdateInboxItems  int
	RawEventsWritten  int
	NotModifiedCount  int
	AnomaliesRecorded int
	Errors            int
}

// RunOnce executes one complete poll cycle: discover PRs for each repo, then
// poll all tracked PRs.  Errors per repo/PR are logged but do not abort the
// cycle (the goal is best-effort progress).
func (p *Poller) RunOnce(ctx context.Context) (*CycleStats, error) {
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.MaxPagesPerEndpoint == 0 {
		p.MaxPagesPerEndpoint = 5
	}
	stats := &CycleStats{}

	for _, repo := range p.Config.Repos() {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		stats.ReposPolled++
		if err := p.discoverPRs(ctx, repo, stats); err != nil {
			stats.Errors++
			p.Logger.Error("repo discovery failed",
				"repo", repo.String(), "error", err)
		}
	}

	// Track all known PR threads across all configured repos.
	type prJob struct {
		repo   RepoRef
		number int
	}
	var jobs []prJob
	for _, repo := range p.Config.Repos() {
		nums, err := p.listTrackedPRs(repo)
		if err != nil {
			p.Logger.Error("list tracked PRs",
				"repo", repo.String(), "error", err)
			stats.Errors++
			continue
		}
		for _, n := range nums {
			jobs = append(jobs, prJob{repo, n})
		}
	}

	// Concurrency bound.
	sem := make(chan struct{}, p.Config.MaxConcurrent)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(j prJob) {
			defer wg.Done()
			defer func() { <-sem }()
			local := &CycleStats{}
			if err := p.pollPR(ctx, j.repo, j.number, local); err != nil {
				local.Errors++
				p.Logger.Error("pr poll failed",
					"repo", j.repo.String(), "number", j.number, "error", err)
			}
			mu.Lock()
			stats.PRsPolled += local.PRsPolled
			stats.RawEventsWritten += local.RawEventsWritten
			stats.NotModifiedCount += local.NotModifiedCount
			stats.AnomaliesRecorded += local.AnomaliesRecorded
			stats.UpdateInboxItems += local.UpdateInboxItems
			stats.Errors += local.Errors
			mu.Unlock()
		}(j)
	}
	wg.Wait()

	return stats, nil
}

// listTrackedPRs returns PR numbers for which a thread directory exists.
func (p *Poller) listTrackedPRs(r RepoRef) ([]int, error) {
	prefix := fmt.Sprintf("github-%s-%s-pr-", r.Owner, r.Repo)
	dir := p.Writer.StateRoot + "/threads"
	entries, err := readDirNoFail(dir)
	if err != nil {
		return nil, err
	}
	var nums []int
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(e.Name(), prefix+"%d", &n); err != nil {
			continue
		}
		nums = append(nums, n)
	}
	return nums, nil
}

// discoverPRs runs the repo-level open-PR scan and seeds inbox/new entries
// for PRs that aren't yet tracked.
func (p *Poller) discoverPRs(ctx context.Context, r RepoRef, stats *CycleStats) error {
	cursor, err := p.Cursors.LoadRepo(r)
	if err != nil {
		return fmt.Errorf("load repo cursor: %w", err)
	}
	lookbackCutoff := p.Now().Add(-p.Config.NewPRLookback.Duration)

	page := 1
	for pageCount := 0; pageCount < p.MaxPagesPerEndpoint; pageCount++ {
		etag := cursor.PullsETag
		if pageCount > 0 {
			// Only use the ETag for the first page - subsequent pages
			// have distinct ETags and we don't store them all.
			etag = ""
		}
		res, err := p.Client.ListOpenPRs(ctx, r, etag, page)
		if err != nil {
			if ok, retryAfter := IsRateLimit(err); ok {
				p.Logger.Warn("rate limited; backing off",
					"repo", r.String(), "retry_after", retryAfter)
				return nil
			}
			if IsUnauthorized(err) {
				return fmt.Errorf("unauthorized: %w", err)
			}
			return fmt.Errorf("list open prs: %w", err)
		}
		if res.NotModified {
			stats.NotModifiedCount++
			cursor.PullsETag = res.ETag
			break
		}
		// First successful page on this cycle: capture the ETag.
		if pageCount == 0 {
			cursor.PullsETag = res.ETag
		}

		stoppedEarly := false
		for _, pr := range res.PRs {
			updatedAt := pr.GetUpdatedAt().Time
			if updatedAt.Before(lookbackCutoff) {
				// PRs are sorted updated-desc; once we're below the
				// lookback window we can stop entirely.
				stoppedEarly = true
				break
			}
			id := ThreadID(r, pr.GetNumber())
			exists, err := p.Writer.ThreadExists(id)
			if err != nil {
				p.Logger.Error("thread exists check", "id", id, "error", err)
				stats.Errors++
				continue
			}
			if exists {
				continue
			}
			path, wrote, err := p.Writer.WriteInboxNew(pr, r, p.Now())
			if err != nil {
				p.Logger.Error("write inbox new", "id", id, "error", err)
				stats.Errors++
				continue
			}
			if wrote {
				stats.PRsDiscovered++
				stats.NewInboxItems++
				p.Logger.Info("new PR discovered",
					"id", id, "title", pr.GetTitle(),
					"author", pr.GetUser().GetLogin(), "path", path)
			}
		}
		if stoppedEarly || res.NextPage == 0 {
			break
		}
		page = res.NextPage
	}

	cursor.LastPRDiscoveryAt = p.Now()
	if err := p.Cursors.SaveRepo(r, cursor); err != nil {
		return fmt.Errorf("save repo cursor: %w", err)
	}
	return nil
}

// pollPR runs the four endpoints for one tracked PR.
func (p *Poller) pollPR(ctx context.Context, r RepoRef, number int, stats *CycleStats) error {
	stats.PRsPolled++
	id := ThreadID(r, number)
	cursor, err := p.Cursors.LoadPR(r, number)
	if err != nil {
		return fmt.Errorf("load pr cursor: %w", err)
	}

	// 1. PR metadata (also gives us a non-nil PullRequest for EnsureMeta).
	pull, etag, notModified, err := p.fetchPull(ctx, r, number, cursor.Endpoints.PullETag)
	if err != nil {
		if IsNotFound(err) {
			p.Logger.Warn("PR returned 404; marking as anomaly", "id", id)
			anomalyPath, werr := p.Writer.WriteAnomaly(id, map[string]any{
				"kind":     "github-pr-404",
				"id":       id,
				"observed": p.Now().Format(time.RFC3339),
			})
			if werr != nil {
				return fmt.Errorf("write anomaly: %w", werr)
			}
			stats.AnomaliesRecorded++
			p.Logger.Info("404 anomaly written", "path", anomalyPath)
			return nil
		}
		if ok, retryAfter := IsRateLimit(err); ok {
			p.Logger.Warn("PR get rate-limited; will retry next cycle",
				"id", id, "retry_after", retryAfter)
			return nil
		}
		return fmt.Errorf("get pull: %w", err)
	}
	cursor.Endpoints.PullETag = etag

	var prSnapshotWritten bool
	var latestEventID string
	now := p.Now()

	if pull != nil {
		// Make sure meta exists.  If the thread directory was hand-rolled
		// by a teammate or by `harness thread track`, meta may not exist yet.
		if _, err := p.Writer.EnsureMeta(id, pull, now); err != nil {
			return fmt.Errorf("ensure meta: %w", err)
		}

		// Detect state change via updated_at.
		updated := pull.GetUpdatedAt().Time
		if updated.After(cursor.Endpoints.PRUpdatedAt) {
			ev := buildPRStateEvent(r, pull, now)
			path, wrote, err := p.Writer.WriteRawEvent(id, ev)
			if err != nil {
				return fmt.Errorf("write pr-state: %w", err)
			}
			if wrote {
				stats.RawEventsWritten++
				prSnapshotWritten = true
				latestEventID = ev.ID
				p.Logger.Info("pr-state snapshot",
					"id", id, "state", pull.GetState(), "path", path)
			}
			cursor.Endpoints.PRUpdatedAt = updated
		}
	} else if notModified {
		stats.NotModifiedCount++
	}

	// 2. Issue comments
	if newEv, anyWrote, err := p.pollIssueComments(ctx, r, number, id, cursor, stats); err != nil {
		p.Logger.Error("issue comments", "id", id, "error", err)
		stats.Errors++
	} else if anyWrote {
		latestEventID = newEv
	}

	// 3. Review comments
	if newEv, anyWrote, err := p.pollReviewComments(ctx, r, number, id, cursor, stats); err != nil {
		p.Logger.Error("review comments", "id", id, "error", err)
		stats.Errors++
	} else if anyWrote {
		latestEventID = newEv
	}

	// 4. Reviews
	if newEv, anyWrote, err := p.pollReviews(ctx, r, number, id, cursor, stats); err != nil {
		p.Logger.Error("reviews", "id", id, "error", err)
		stats.Errors++
	} else if anyWrote {
		latestEventID = newEv
	}

	cursor.LastPolledAt = now
	if err := p.Cursors.SavePR(r, number, cursor); err != nil {
		return fmt.Errorf("save pr cursor: %w", err)
	}

	if latestEventID != "" || prSnapshotWritten {
		if err := p.Writer.TouchDirty(id); err != nil {
			return fmt.Errorf("touch dirty: %w", err)
		}
		// Optional collapsed-per-cycle inbox/updates ping.
		summary := fmt.Sprintf("%d new event(s) for %s", stats.RawEventsWritten, id)
		_, err := p.Writer.WriteInboxUpdate(id, latestEventID, summary,
			fmt.Sprintf("threads/%s/raw/%s.json", id, latestEventID), now)
		if err != nil {
			p.Logger.Error("write inbox update", "id", id, "error", err)
			stats.Errors++
		} else {
			stats.UpdateInboxItems++
		}
		// Best-effort participant update: read latest events would be expensive,
		// so we just merge the PR author and let the rollup updater enrich.
		if pull != nil {
			_ = p.Writer.UpdateMeta(id, func(m *Meta) {
				m.LastEventAt = now
				m.Participants = MergeParticipants(m.Participants, pull.GetUser().GetLogin())
			})
		}
	}

	return nil
}

func (p *Poller) fetchPull(ctx context.Context, r RepoRef, number int, etag string) (*gh.PullRequest, string, bool, error) {
	res, err := p.Client.GetPull(ctx, r, number, etag)
	if err != nil {
		return nil, etag, false, err
	}
	if res.NotModified {
		return nil, res.ETag, true, nil
	}
	return res.Pull, res.ETag, false, nil
}

func (p *Poller) pollIssueComments(ctx context.Context, r RepoRef, number int, id string, cursor *PRCursor, stats *CycleStats) (string, bool, error) {
	since := cursor.Endpoints.IssueCommentsSince
	if !since.IsZero() {
		since = since.Add(-OverlapBuffer)
	}
	page := 1
	var latestEventID string
	var newest time.Time
	var anyWrote bool
	for pageCount := 0; pageCount < p.MaxPagesPerEndpoint; pageCount++ {
		etag := ""
		if pageCount == 0 {
			etag = cursor.Endpoints.IssueCommentsETag
		}
		res, err := p.Client.ListIssueComments(ctx, r, number, since, etag, page)
		if err != nil {
			if ok, _ := IsRateLimit(err); ok {
				p.Logger.Warn("issue comments rate-limited", "id", id)
				return latestEventID, anyWrote, nil
			}
			return latestEventID, anyWrote, err
		}
		if res.NotModified {
			stats.NotModifiedCount++
			if pageCount == 0 {
				cursor.Endpoints.IssueCommentsETag = res.ETag
			}
			break
		}
		if pageCount == 0 {
			cursor.Endpoints.IssueCommentsETag = res.ETag
		}
		for _, ic := range res.Comments {
			eventID := fmt.Sprintf("issue-comment-%d", ic.GetID())
			exists, err := p.Writer.EventExists(id, eventID)
			if err != nil {
				return latestEventID, anyWrote, fmt.Errorf("exists check: %w", err)
			}
			if exists {
				continue
			}
			ev := buildIssueCommentEvent(r, number, ic, p.Now())
			_, wrote, err := p.Writer.WriteRawEvent(id, ev)
			if err != nil {
				return latestEventID, anyWrote, err
			}
			if wrote {
				stats.RawEventsWritten++
				anyWrote = true
				latestEventID = eventID
				if t := ic.GetUpdatedAt().Time; t.After(newest) {
					newest = t
				}
			}
		}
		if res.NextPage == 0 {
			break
		}
		page = res.NextPage
	}
	if !newest.IsZero() {
		cursor.Endpoints.IssueCommentsSince = newest
	}
	return latestEventID, anyWrote, nil
}

func (p *Poller) pollReviewComments(ctx context.Context, r RepoRef, number int, id string, cursor *PRCursor, stats *CycleStats) (string, bool, error) {
	since := cursor.Endpoints.ReviewCommentsSince
	if !since.IsZero() {
		since = since.Add(-OverlapBuffer)
	}
	page := 1
	var latestEventID string
	var newest time.Time
	var anyWrote bool
	for pageCount := 0; pageCount < p.MaxPagesPerEndpoint; pageCount++ {
		etag := ""
		if pageCount == 0 {
			etag = cursor.Endpoints.ReviewCommentsETag
		}
		res, err := p.Client.ListReviewComments(ctx, r, number, since, etag, page)
		if err != nil {
			if ok, _ := IsRateLimit(err); ok {
				p.Logger.Warn("review comments rate-limited", "id", id)
				return latestEventID, anyWrote, nil
			}
			return latestEventID, anyWrote, err
		}
		if res.NotModified {
			stats.NotModifiedCount++
			if pageCount == 0 {
				cursor.Endpoints.ReviewCommentsETag = res.ETag
			}
			break
		}
		if pageCount == 0 {
			cursor.Endpoints.ReviewCommentsETag = res.ETag
		}
		for _, rc := range res.Comments {
			eventID := fmt.Sprintf("review-comment-%d", rc.GetID())
			exists, err := p.Writer.EventExists(id, eventID)
			if err != nil {
				return latestEventID, anyWrote, err
			}
			if exists {
				continue
			}
			ev := buildReviewCommentEvent(r, number, rc, p.Now())
			_, wrote, err := p.Writer.WriteRawEvent(id, ev)
			if err != nil {
				return latestEventID, anyWrote, err
			}
			if wrote {
				stats.RawEventsWritten++
				anyWrote = true
				latestEventID = eventID
				if t := rc.GetUpdatedAt().Time; t.After(newest) {
					newest = t
				}
			}
		}
		if res.NextPage == 0 {
			break
		}
		page = res.NextPage
	}
	if !newest.IsZero() {
		cursor.Endpoints.ReviewCommentsSince = newest
	}
	return latestEventID, anyWrote, nil
}

func (p *Poller) pollReviews(ctx context.Context, r RepoRef, number int, id string, cursor *PRCursor, stats *CycleStats) (string, bool, error) {
	page := 1
	var latestEventID string
	var anyWrote bool
	for pageCount := 0; pageCount < p.MaxPagesPerEndpoint; pageCount++ {
		etag := ""
		if pageCount == 0 {
			etag = cursor.Endpoints.ReviewsETag
		}
		res, err := p.Client.ListReviews(ctx, r, number, etag, page)
		if err != nil {
			if ok, _ := IsRateLimit(err); ok {
				p.Logger.Warn("reviews rate-limited", "id", id)
				return latestEventID, anyWrote, nil
			}
			return latestEventID, anyWrote, err
		}
		if res.NotModified {
			stats.NotModifiedCount++
			if pageCount == 0 {
				cursor.Endpoints.ReviewsETag = res.ETag
			}
			break
		}
		if pageCount == 0 {
			cursor.Endpoints.ReviewsETag = res.ETag
		}
		allSeen := true
		for _, rv := range res.Reviews {
			if cursor.HasReviewSeen(rv.GetID()) {
				continue
			}
			allSeen = false
			eventID := fmt.Sprintf("review-%d", rv.GetID())
			exists, err := p.Writer.EventExists(id, eventID)
			if err != nil {
				return latestEventID, anyWrote, err
			}
			if !exists {
				ev := buildReviewEvent(r, number, rv, p.Now())
				_, wrote, err := p.Writer.WriteRawEvent(id, ev)
				if err != nil {
					return latestEventID, anyWrote, err
				}
				if wrote {
					stats.RawEventsWritten++
					anyWrote = true
					latestEventID = eventID
				}
			}
			cursor.AddReviewSeen(rv.GetID())
		}
		// design/09 hint: pre-fetch one page; if all IDs already seen,
		// skip the rest.
		if allSeen && pageCount == 0 {
			break
		}
		if res.NextPage == 0 {
			break
		}
		page = res.NextPage
	}
	return latestEventID, anyWrote, nil
}

// --- event builders --------------------------------------------------------

func buildIssueCommentEvent(r RepoRef, number int, ic *gh.IssueComment, capturedAt time.Time) RawEvent {
	ev := RawEvent{
		ID:         fmt.Sprintf("issue-comment-%d", ic.GetID()),
		Source:     "github",
		CapturedAt: capturedAt,
		Kind:       "issue-comment",
		Actor:      ic.GetUser().GetLogin(),
		ActorID:    ic.GetUser().GetID(),
		CreatedAt:  ic.GetCreatedAt().Time,
		UpdatedAt:  ic.GetUpdatedAt().Time,
		Body:       ic.GetBody(),
		HTMLURL:    ic.GetHTMLURL(),
		Raw:        ic,
	}
	ev.PR.Owner = r.Owner
	ev.PR.Repo = r.Repo
	ev.PR.Number = number
	return ev
}

func buildReviewCommentEvent(r RepoRef, number int, rc *gh.PullRequestComment, capturedAt time.Time) RawEvent {
	ev := RawEvent{
		ID:         fmt.Sprintf("review-comment-%d", rc.GetID()),
		Source:     "github",
		CapturedAt: capturedAt,
		Kind:       "review-comment",
		Actor:      rc.GetUser().GetLogin(),
		ActorID:    rc.GetUser().GetID(),
		CreatedAt:  rc.GetCreatedAt().Time,
		UpdatedAt:  rc.GetUpdatedAt().Time,
		Body:       rc.GetBody(),
		HTMLURL:    rc.GetHTMLURL(),
		Raw:        rc,
	}
	ev.PR.Owner = r.Owner
	ev.PR.Repo = r.Repo
	ev.PR.Number = number
	return ev
}

func buildReviewEvent(r RepoRef, number int, rv *gh.PullRequestReview, capturedAt time.Time) RawEvent {
	ev := RawEvent{
		ID:         fmt.Sprintf("review-%d", rv.GetID()),
		Source:     "github",
		CapturedAt: capturedAt,
		Kind:       "review",
		Actor:      rv.GetUser().GetLogin(),
		ActorID:    rv.GetUser().GetID(),
		CreatedAt:  rv.GetSubmittedAt().Time,
		UpdatedAt:  rv.GetSubmittedAt().Time,
		Body:       rv.GetBody(),
		HTMLURL:    rv.GetHTMLURL(),
		Raw:        rv,
	}
	ev.PR.Owner = r.Owner
	ev.PR.Repo = r.Repo
	ev.PR.Number = number
	return ev
}

func buildPRStateEvent(r RepoRef, pr *gh.PullRequest, capturedAt time.Time) RawEvent {
	id := fmt.Sprintf("pr-state-%s", pr.GetUpdatedAt().Format("2006-01-02T15-04-05Z"))
	body := fmt.Sprintf("state=%s, merged=%v, draft=%v",
		pr.GetState(), pr.GetMerged(), pr.GetDraft())
	ev := RawEvent{
		ID:         id,
		Source:     "github",
		CapturedAt: capturedAt,
		Kind:       "pr-state",
		Actor:      pr.GetUser().GetLogin(),
		ActorID:    pr.GetUser().GetID(),
		CreatedAt:  pr.GetCreatedAt().Time,
		UpdatedAt:  pr.GetUpdatedAt().Time,
		Body:       body,
		HTMLURL:    pr.GetHTMLURL(),
		Raw:        pr,
	}
	ev.PR.Owner = r.Owner
	ev.PR.Repo = r.Repo
	ev.PR.Number = pr.GetNumber()
	return ev
}
