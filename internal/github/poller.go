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

// MaxRateLimitSleep caps how long we'll block in a single endpoint call
// waiting for a GitHub rate-limit reset.  GitHub's primary REST limit
// resets hourly; if the reported reset is further than this cap we
// surrender the cycle instead of blocking the binary for a long time.
const MaxRateLimitSleep = 15 * time.Minute

// MaxRateLimitRetries bounds how often a single endpoint will retry after
// honoring a 403 + rate-limit response in the same cycle.  Prevents an
// infinite loop when an upstream serves rate-limit headers indefinitely
// (e.g. misconfigured proxy in tests).
const MaxRateLimitRetries = 2

// sleepCtx is like time.Sleep but cancellable.  Returns ctx.Err() if the
// context fires first, nil if the full duration elapses.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// honorRateLimit sleeps until the X-RateLimit-Reset epoch reported by the
// upstream error (capped by MaxRateLimitSleep), then returns `retry=true`
// so the caller can retry the in-progress request.  If the reported reset
// is missing or in the past, it sleeps for the configured
// `backoff.on_rate_limit` minimum.  If the cap is exceeded, it returns
// `retry=false` and the caller should abort the cycle gracefully.
//
// Returns a non-nil error only if the context is cancelled during sleep,
// in which case the caller should propagate cancellation upward.
func (p *Poller) honorRateLimit(ctx context.Context, err error, endpoint, target string) (retry bool, sleepErr error) {
	reset := RateLimitReset(err)
	now := p.Now()
	var d time.Duration
	if !reset.IsZero() {
		d = reset.Sub(now)
	}
	if d <= 0 {
		d = p.Config.Backoff.OnRateLimit.Duration
	}
	if d > MaxRateLimitSleep {
		p.Logger.Warn("rate-limit reset farther than cap; surrendering cycle",
			"endpoint", endpoint, "target", target,
			"reset", reset, "want_sleep", d, "cap", MaxRateLimitSleep)
		return false, nil
	}
	p.Logger.Warn("rate limited; sleeping until reset",
		"endpoint", endpoint, "target", target, "sleep", d, "reset", reset)
	if err := sleepCtx(ctx, d); err != nil {
		return false, err
	}
	return true, nil
}

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

// archiveDeletedPR is the 404-on-tracked-PR handler.  It:
//  1. archives the thread directory to state/threads/_archive/<id>-<ts>/
//  2. writes a stable anomaly note under inbox/anomalies/
//  3. drops the PR cursor so the next cycle doesn't re-poll
//
// All three steps are best-effort: if archiving fails (e.g., concurrent
// hand-edit), the anomaly still gets written and the cursor still gets
// dropped so the loop converges.  Caller still updates stats.
func (p *Poller) archiveDeletedPR(id string, r RepoRef, number int, cursor *PRCursor) error {
	if _, err := p.Writer.ArchiveThread(id); err != nil {
		p.Logger.Error("thread archive failed; recording anomaly anyway",
			"id", id, "error", err)
	}
	_, werr := p.Writer.WriteAnomalyStable(id, "pr-404", map[string]any{
		"kind":     "github-pr-404",
		"id":       id,
		"repo":     r.String(),
		"number":   number,
		"observed": p.Now().Format(time.RFC3339),
		"summary":  fmt.Sprintf("tracked PR %s returned 404; archived", id),
	})
	if werr != nil {
		return fmt.Errorf("write anomaly: %w", werr)
	}
	// Wipe the cursor so the deferred SavePR in pollPR doesn't recreate it.
	*cursor = PRCursor{}
	if err := p.Cursors.DeletePR(r, number); err != nil {
		p.Logger.Error("cursor delete failed", "id", id, "error", err)
	}
	return nil
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
// for PRs that aren't yet tracked.  Cursor save is deferred so partial
// progress is persisted on SIGTERM / cancellation.
func (p *Poller) discoverPRs(ctx context.Context, r RepoRef, stats *CycleStats) (retErr error) {
	cursor, err := p.Cursors.LoadRepo(r)
	if err != nil {
		return fmt.Errorf("load repo cursor: %w", err)
	}
	defer func() {
		if saveErr := p.Cursors.SaveRepo(r, cursor); saveErr != nil {
			if retErr == nil {
				retErr = fmt.Errorf("save repo cursor: %w", saveErr)
			} else {
				p.Logger.Error("repo cursor save failed alongside other error",
					"repo", r.String(), "save_error", saveErr, "primary", retErr)
			}
		}
	}()
	lookbackCutoff := p.Now().Add(-p.Config.NewPRLookback.Duration)

	page := 1
	rateLimitRetries := 0
	for pageCount := 0; pageCount < p.MaxPagesPerEndpoint; pageCount++ {
		etag := cursor.PullsETag
		if pageCount > 0 {
			// Only use the ETag for the first page - subsequent pages
			// have distinct ETags and we don't store them all.
			etag = ""
		}
		res, err := p.Client.ListOpenPRs(ctx, r, etag, page)
		if err != nil {
			if ok, _ := IsRateLimit(err); ok {
				retry, sleepErr := p.honorRateLimit(ctx, err, "list open prs", r.String())
				if sleepErr != nil {
					return sleepErr
				}
				if retry && rateLimitRetries < MaxRateLimitRetries {
					rateLimitRetries++
					continue // retry same page after sleep
				}
				p.Logger.Warn("rate-limit retries exhausted; surrendering page",
					"repo", r.String(), "retries", rateLimitRetries)
				return nil
			}
			if IsUnauthorized(err) {
				return fmt.Errorf("unauthorized: %w", err)
			}
			if IsMalformed(err) {
				p.Logger.Warn("malformed request to list open prs; recording anomaly",
					"repo", r.String(), "error", err)
				_, werr := p.Writer.WriteAnomaly(fmt.Sprintf("github-%s-%s-list-open-prs", r.Owner, r.Repo),
					map[string]any{
						"kind":     "github-422-list-open-prs",
						"repo":     r.String(),
						"observed": p.Now().Format(time.RFC3339),
						"error":    err.Error(),
					})
				if werr != nil {
					return werr
				}
				stats.AnomaliesRecorded++
				return nil
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
	return nil
}

// pollPR runs the four endpoints for one tracked PR.  The cursor is
// persisted in a deferred block so that even if a downstream call panics or
// errors after partial progress has been made (e.g., SIGTERM mid-cycle),
// the last-completed batch is still durable on disk per the round-2
// graceful-shutdown requirement.
//
// The `archived` flag suppresses the deferred cursor save when the 404
// archive path has already deleted the cursor file (we don't want to
// resurrect it).
func (p *Poller) pollPR(ctx context.Context, r RepoRef, number int, stats *CycleStats) (retErr error) {
	stats.PRsPolled++
	id := ThreadID(r, number)
	cursor, err := p.Cursors.LoadPR(r, number)
	if err != nil {
		return fmt.Errorf("load pr cursor: %w", err)
	}
	archived := false
	defer func() {
		if archived {
			return
		}
		if saveErr := p.Cursors.SavePR(r, number, cursor); saveErr != nil {
			if retErr == nil {
				retErr = fmt.Errorf("save pr cursor: %w", saveErr)
			} else {
				p.Logger.Error("cursor save failed alongside other error",
					"id", id, "save_error", saveErr, "primary", retErr)
			}
		}
	}()

	// 1. PR metadata (also gives us a non-nil PullRequest for EnsureMeta).
	var pull *gh.PullRequest
	var etag string
	var notModified bool
	for attempt := 0; attempt < 3; attempt++ {
		var err error
		pull, etag, notModified, err = p.fetchPull(ctx, r, number, cursor.Endpoints.PullETag)
		if err == nil {
			break
		}
		if IsNotFound(err) {
			// PR deleted upstream — archive the thread, write an anomaly,
			// remove the cursor.  Per design/09 §"Error handling" and the
			// round-2 hardening requirement.
			if err := p.archiveDeletedPR(id, r, number, cursor); err != nil {
				return fmt.Errorf("archive deleted PR: %w", err)
			}
			archived = true
			stats.AnomaliesRecorded++
			p.Logger.Info("PR 404; thread archived and anomaly written", "id", id)
			return nil
		}
		if ok, _ := IsRateLimit(err); ok {
			retry, sleepErr := p.honorRateLimit(ctx, err, "get pull", id)
			if sleepErr != nil {
				return sleepErr
			}
			if retry {
				continue
			}
			p.Logger.Warn("rate-limit reset out of range; skipping PR for this cycle",
				"id", id)
			return nil
		}
		if IsUnauthorized(err) {
			return fmt.Errorf("unauthorized: %w", err)
		}
		if IsMalformed(err) {
			p.Logger.Warn("malformed get-pull response; recording anomaly",
				"id", id, "error", err)
			_, werr := p.Writer.WriteAnomaly(id+"-get-pull-422", map[string]any{
				"kind":     "github-422-get-pull",
				"id":       id,
				"observed": p.Now().Format(time.RFC3339),
				"error":    err.Error(),
			})
			if werr != nil {
				return fmt.Errorf("write anomaly: %w", werr)
			}
			stats.AnomaliesRecorded++
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
	rateLimitRetries := 0
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
				retry, sleepErr := p.honorRateLimit(ctx, err, "issue comments", id)
				if sleepErr != nil {
					return latestEventID, anyWrote, sleepErr
				}
				if retry && rateLimitRetries < MaxRateLimitRetries {
					rateLimitRetries++
					continue
				}
				return latestEventID, anyWrote, nil
			}
			if IsMalformed(err) {
				_, _ = p.Writer.WriteAnomalyStable(id, "issue-comments-422",
					map[string]any{
						"kind":     "github-422-issue-comments",
						"id":       id,
						"observed": p.Now().Format(time.RFC3339),
						"error":    err.Error(),
					})
				stats.AnomaliesRecorded++
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
	rateLimitRetries := 0
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
				retry, sleepErr := p.honorRateLimit(ctx, err, "review comments", id)
				if sleepErr != nil {
					return latestEventID, anyWrote, sleepErr
				}
				if retry && rateLimitRetries < MaxRateLimitRetries {
					rateLimitRetries++
					continue
				}
				return latestEventID, anyWrote, nil
			}
			if IsMalformed(err) {
				_, _ = p.Writer.WriteAnomalyStable(id, "review-comments-422",
					map[string]any{
						"kind":     "github-422-review-comments",
						"id":       id,
						"observed": p.Now().Format(time.RFC3339),
						"error":    err.Error(),
					})
				stats.AnomaliesRecorded++
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
	rateLimitRetries := 0
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
				retry, sleepErr := p.honorRateLimit(ctx, err, "reviews", id)
				if sleepErr != nil {
					return latestEventID, anyWrote, sleepErr
				}
				if retry && rateLimitRetries < MaxRateLimitRetries {
					rateLimitRetries++
					continue
				}
				return latestEventID, anyWrote, nil
			}
			if IsMalformed(err) {
				_, _ = p.Writer.WriteAnomalyStable(id, "reviews-422",
					map[string]any{
						"kind":     "github-422-reviews",
						"id":       id,
						"observed": p.Now().Format(time.RFC3339),
						"error":    err.Error(),
					})
				stats.AnomaliesRecorded++
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

	labels := make([]string, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		labels = append(labels, l.GetName())
	}
	reviewers := make([]string, 0, len(pr.RequestedReviewers))
	for _, u := range pr.RequestedReviewers {
		reviewers = append(reviewers, u.GetLogin())
	}
	snap := &PRStateSnapshot{
		State:              pr.GetState(),
		Merged:             pr.GetMerged(),
		Labels:             labels,
		HeadSHA:            pr.GetHead().GetSHA(),
		BaseSHA:            pr.GetBase().GetSHA(),
		RequestedReviewers: reviewers,
		Draft:              pr.GetDraft(),
	}
	if pr.Mergeable != nil {
		v := *pr.Mergeable
		snap.Mergeable = &v
	}

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
		Snapshot:   snap,
		Raw:        pr,
	}
	ev.PR.Owner = r.Owner
	ev.PR.Repo = r.Repo
	ev.PR.Number = pr.GetNumber()
	return ev
}
