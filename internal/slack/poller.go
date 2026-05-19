package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Poller drives one full poll cycle across all watched channels and
// tracked threads. It is safe to construct once and call PollOnce multiple
// times sequentially; concurrency within a single PollOnce is bounded by
// MaxConcurrentThreadPolls.
type Poller struct {
	Cfg     *Config
	Client  APIClient
	Cursors *CursorStore
	Writer  *Writer
	Logger  *slog.Logger

	// disabled holds channel IDs we have stopped polling for this process
	// lifetime (e.g. channel_not_found). Per design/08 §"Error handling",
	// such channels are skipped without mutating config.yaml.
	disabledMu sync.RWMutex
	disabled   map[string]struct{}

	// OnlyTargets restricts a single PollOnce to a subset of channels and/or
	// threads. Used by `slack-poller force-poll <id>`. Empty means
	// "everything in scope". Reset between cycles by the caller.
	OnlyTargets *PollTargets
}

// PollTargets restricts a single PollOnce to a subset of channels/threads.
type PollTargets struct {
	Channels map[string]struct{}
	Threads  map[string]struct{}
}

// disabledHas reports whether a channel is in the disabled set.
func (p *Poller) disabledHas(channel string) bool {
	p.disabledMu.RLock()
	defer p.disabledMu.RUnlock()
	_, ok := p.disabled[channel]
	return ok
}

// disable marks a channel as not-to-be-polled this process lifetime.
func (p *Poller) disable(channel string) {
	p.disabledMu.Lock()
	defer p.disabledMu.Unlock()
	if p.disabled == nil {
		p.disabled = make(map[string]struct{})
	}
	p.disabled[channel] = struct{}{}
}

// PollResult summarizes one PollOnce call. Counters are aggregate across
// channels and threads.
type PollResult struct {
	ChannelsPolled      int
	ThreadsPolled       int
	RawEventsWritten    int
	InboxNewWritten     int
	UpdatePingsWritten  int
	AnomaliesWritten    int
	Errors              int
}

// PollOnce runs one full poll cycle: every watched channel + every tracked
// thread. Returns a result summary and an error only if the cycle could
// not start (e.g., listing tracked threads failed) or if the context was
// canceled. Per-target errors are counted but do not abort the cycle. If
// p.OnlyTargets is non-nil, polling is restricted to the named channels
// and threads (used by `force-poll`).
//
// Fatal auth errors are propagated as p's return value so the daemon can
// exit immediately. Per design/08 §"Error handling".
func (p *Poller) PollOnce(ctx context.Context) (PollResult, error) {
	result := PollResult{}
	logger := p.logger()

	// Channel-level polling: sequential per channel; pagination loops
	// within each. Channel volume is bounded by the watch list so the
	// extra parallelism is not worth the complexity.
	for _, ch := range p.Cfg.Watch.Channels {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if p.disabledHas(ch.ID) {
			continue
		}
		if p.OnlyTargets != nil && !p.OnlyTargets.includesChannel(ch.ID) {
			continue
		}
		r, err := p.pollChannel(ctx, ch.ID)
		mergeResult(&result, r)
		result.ChannelsPolled++
		if err != nil {
			if IsFatalAuthError(err) {
				return result, fmt.Errorf("channel %s: %w", ch.ID, ErrFatalAuth)
			}
			if reason := channelAccessError(err); reason != "" {
				p.handleChannelAccessError(&result, ch.ID, reason)
				continue
			}
			// Rate-limit errors propagate so the daemon can back off
			// rather than spin. Per design/08 §"Error handling".
			if _, ok := RateLimitWait(err); ok {
				return result, err
			}
			result.Errors++
			logger.Error("channel poll failed",
				slog.String("channel", ch.ID),
				slog.String("err", err.Error()))
		}
	}

	// Thread-level polling: bounded concurrency.
	threadIDs, err := p.Writer.ListTrackedThreads()
	if err != nil {
		return result, fmt.Errorf("list tracked threads: %w", err)
	}

	// Apply OnlyTargets filter to threads if present.
	if p.OnlyTargets != nil {
		filtered := threadIDs[:0]
		for _, id := range threadIDs {
			if p.OnlyTargets.includesThread(id) {
				filtered = append(filtered, id)
			}
		}
		threadIDs = filtered
	}

	if len(threadIDs) == 0 {
		return result, nil
	}

	concurrency := p.Cfg.MaxConcurrentThreadPolls
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var fatalErr error

	for _, id := range threadIDs {
		if err := ctx.Err(); err != nil {
			break
		}
		id := id
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r, err := p.pollThread(ctx, id)
			mu.Lock()
			defer mu.Unlock()
			mergeResult(&result, r)
			result.ThreadsPolled++
			if err != nil {
				if IsFatalAuthError(err) && fatalErr == nil {
					fatalErr = fmt.Errorf("thread %s: %w", id, ErrFatalAuth)
				}
				if reason := channelAccessError(err); reason != "" {
					p.handleThreadAccessError(&result, id, reason)
					return
				}
				if _, ok := RateLimitWait(err); ok && fatalErr == nil {
					// Propagate the first rate-limit error so the daemon
					// can sleep and write the anomaly. Subsequent threads
					// in this cycle will be skipped (next iteration of the
					// loop), but in-flight goroutines already started will
					// complete normally.
					fatalErr = err
					return
				}
				result.Errors++
				logger.Error("thread poll failed",
					slog.String("thread", id),
					slog.String("err", err.Error()))
			}
		}()
	}
	wg.Wait()
	if fatalErr != nil {
		return result, fatalErr
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

// includesChannel reports whether the targets set includes channel.
func (t *PollTargets) includesChannel(channel string) bool {
	if t == nil {
		return true
	}
	if len(t.Channels) == 0 && len(t.Threads) == 0 {
		return true
	}
	_, ok := t.Channels[channel]
	return ok
}

// includesThread reports whether the targets set includes threadID.
func (t *PollTargets) includesThread(threadID string) bool {
	if t == nil {
		return true
	}
	if len(t.Channels) == 0 && len(t.Threads) == 0 {
		return true
	}
	_, ok := t.Threads[threadID]
	return ok
}

// handleChannelAccessError records an anomaly and removes the channel from
// active polling for the rest of this process. Config is not mutated.
func (p *Poller) handleChannelAccessError(result *PollResult, channel, reason string) {
	a := AnomalyChannelAccess(channel, "slack reported: "+reason)
	wrote, err := p.Writer.WriteAnomaly(a)
	if err != nil {
		p.logger().Error("write channel-access anomaly",
			slog.String("channel", channel),
			slog.String("err", err.Error()))
		result.Errors++
		return
	}
	if wrote {
		result.AnomaliesWritten++
	}
	p.disable(channel)
	p.logger().Warn("disabling channel for remainder of process",
		slog.String("channel", channel),
		slog.String("reason", reason))
}

// handleThreadAccessError records an anomaly for a 404 / not_in_channel on
// a tracked thread. We do not remove tracked-thread dirs (commander's job)
// — we just stop returning errors for this thread by writing the anomaly
// marker; future polls will simply repeat the API attempt.
func (p *Poller) handleThreadAccessError(result *PollResult, threadID, reason string) {
	a := AnomalyThreadNotFound(threadID, "slack reported: "+reason)
	wrote, err := p.Writer.WriteAnomaly(a)
	if err != nil {
		p.logger().Error("write thread-not-found anomaly",
			slog.String("thread", threadID),
			slog.String("err", err.Error()))
		result.Errors++
		return
	}
	if wrote {
		result.AnomaliesWritten++
	}
	p.logger().Warn("thread access error logged",
		slog.String("thread", threadID),
		slog.String("reason", reason))
}

// WriteRateLimitAnomaly records that we gave up on persistent 429s. The
// daemon loop calls this from outside PollOnce after exhausting backoff.
func (p *Poller) WriteRateLimitAnomaly(scope string, retryAfter time.Duration) (PollResult, error) {
	result := PollResult{}
	reason := fmt.Sprintf("persistent 429 after %s; retry-after=%s",
		p.Cfg.Backoff.MaxBackoff.Duration, retryAfter)
	a := AnomalyRateLimitExhausted(scope, reason)
	wrote, err := p.Writer.WriteAnomaly(a)
	if err != nil {
		return result, err
	}
	if wrote {
		result.AnomaliesWritten++
	}
	return result, nil
}

// IsContextErr reports whether err is a context cancellation/deadline.
func IsContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// pollChannel runs the conversations.history loop for one channel and
// dispatches each message to either a tracked-thread raw write or an
// inbox/new write. Cursor is advanced only after the full batch is
// successfully persisted (write-on-success per design/08 §Dedup).
func (p *Poller) pollChannel(ctx context.Context, channelID string) (PollResult, error) {
	logger := p.logger().With(slog.String("channel", channelID))
	result := PollResult{}

	oldest, err := p.Cursors.GetChannelCursor(channelID)
	if err != nil {
		return result, fmt.Errorf("read channel cursor: %w", err)
	}

	var (
		cursor       string
		newestTS     = oldest // we never advance past the messages we successfully wrote
		batch        []Message
	)

	for {
		page, err := p.Client.History(ctx, HistoryParams{
			ChannelID: channelID,
			Oldest:    oldest,
			Cursor:    cursor,
		})
		if err != nil {
			return result, err
		}
		batch = append(batch, page.Messages...)
		if !page.HasMore || page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	logger.Info("channel page",
		slog.String("oldest", oldest),
		slog.Int("messages", len(batch)))

	for _, m := range batch {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		ts := m.TS
		threadTS := m.EffectiveThreadTS()
		threadID := ThreadID(channelID, threadTS)

		// If we already track this thread, write into raw/. Otherwise
		// the message is "new" and lands in inbox/new/ — even if the
		// message has a thread_ts (i.e., is a reply to an untracked
		// thread); commander decides whether to promote.
		if p.Writer.ThreadIsTracked(threadID) {
			ev := messageToEvent(m, channelID)
			if !p.Writer.RawEventExists(threadID, ts) {
				if ev.Permalink == "" {
					ev.Permalink = p.tryPermalink(ctx, channelID, ts)
				}
				wrote, err := p.Writer.WriteRawEvent(threadID, ev)
				if err != nil {
					return result, fmt.Errorf("write raw event: %w", err)
				}
				if wrote {
					result.RawEventsWritten++
				}
			}
			if err := p.Writer.EnsureMeta(threadID, channelID, threadTS,
				ev.Permalink, ts, ts, participantsOf(m)); err != nil {
				return result, fmt.Errorf("ensure meta: %w", err)
			}
			if err := p.Writer.TouchDirty(threadID); err != nil {
				return result, fmt.Errorf("touch dirty: %w", err)
			}
		} else if m.IsTopLevelInThread() {
			// Brand-new top-level message in an un-tracked channel-level
			// conversation. Land it in inbox/new/.
			permalink := p.tryPermalink(ctx, channelID, ts)
			item := buildInboxItem(m, channelID, permalink)
			wrote, err := p.Writer.WriteInboxNew(item)
			if err != nil {
				return result, fmt.Errorf("write inbox new: %w", err)
			}
			if wrote {
				result.InboxNewWritten++
			}
		} else {
			// It's a reply to an untracked thread surfaced via the
			// channel feed. We treat it the same as a new top-level
			// item — commander decides whether to track the parent.
			permalink := p.tryPermalink(ctx, channelID, ts)
			item := buildInboxItem(m, channelID, permalink)
			wrote, err := p.Writer.WriteInboxNew(item)
			if err != nil {
				return result, fmt.Errorf("write inbox new: %w", err)
			}
			if wrote {
				result.InboxNewWritten++
			}
		}

		if ts > newestTS {
			newestTS = ts
		}
	}

	// Write-on-success: advance the cursor only after the whole batch
	// landed without an error path returning early.
	if newestTS != "" && newestTS != oldest {
		if err := p.Cursors.SetChannelCursor(channelID, newestTS); err != nil {
			return result, fmt.Errorf("write channel cursor: %w", err)
		}
	}
	return result, nil
}

// pollThread runs conversations.replies for one tracked thread and writes
// replies into raw/. The thread cursor is advanced on success.
func (p *Poller) pollThread(ctx context.Context, threadID string) (PollResult, error) {
	result := PollResult{}

	channel, threadTS, ok := ParseThreadID(threadID)
	if !ok {
		return result, fmt.Errorf("invalid thread id %q", threadID)
	}

	oldest, err := p.Cursors.GetThreadCursor(threadID)
	if err != nil {
		return result, fmt.Errorf("read thread cursor: %w", err)
	}

	var (
		cursor   string
		batch    []Message
	)
	for {
		page, err := p.Client.Replies(ctx, RepliesParams{
			ChannelID: channel,
			ThreadTS:  threadTS,
			Oldest:    oldest,
			Cursor:    cursor,
		})
		if err != nil {
			return result, err
		}
		batch = append(batch, page.Messages...)
		if !page.HasMore || page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	// conversations.replies returns the thread root first as a courtesy.
	// We accept it idempotently — dedup in WriteRawEvent handles the
	// no-op case.
	newestTS := oldest
	earliestTS := ""
	var participantsAll []string
	var latestPermalink string

	wroteAny := false
	for _, m := range batch {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		ts := m.TS
		// Skip pre-cursor messages. Slack's conversations.replies
		// always returns the thread root regardless of the `oldest`
		// param; without this we would do a redundant permalink lookup
		// and rely on WriteRawEvent's existence check for the dedup.
		if oldest != "" && tsLessThanOrEq(ts, oldest) {
			continue
		}
		ev := messageToEvent(m, channel)
		ev.ThreadTS = threadTS // ensure thread linkage for poller-derived top-level messages
		ev.IsTopLevelInThread = (ts == threadTS)

		if !p.Writer.RawEventExists(threadID, ts) {
			if ev.Permalink == "" {
				ev.Permalink = p.tryPermalink(ctx, channel, ts)
			}
			latestPermalink = ev.Permalink
			wrote, err := p.Writer.WriteRawEvent(threadID, ev)
			if err != nil {
				return result, fmt.Errorf("write raw event: %w", err)
			}
			if wrote {
				result.RawEventsWritten++
				wroteAny = true
			}
		}
		if earliestTS == "" || tsLessThan(ts, earliestTS) {
			earliestTS = ts
		}
		if ts > newestTS {
			newestTS = ts
		}
		participantsAll = append(participantsAll, participantsOf(m)...)
	}

	if earliestTS == "" {
		earliestTS = threadTS
	}

	if err := p.Writer.EnsureMeta(threadID, channel, threadTS, latestPermalink,
		earliestTS, newestTS, participantsAll); err != nil {
		return result, fmt.Errorf("ensure meta: %w", err)
	}

	if wroteAny {
		if err := p.Writer.TouchDirty(threadID); err != nil {
			return result, fmt.Errorf("touch dirty: %w", err)
		}
		if err := p.Writer.WriteUpdatePing(threadID, newestTS, result.RawEventsWritten); err != nil {
			return result, fmt.Errorf("write update ping: %w", err)
		}
		result.UpdatePingsWritten++
	}

	if newestTS != "" && newestTS != oldest {
		if err := p.Cursors.SetThreadCursor(threadID, newestTS); err != nil {
			return result, fmt.Errorf("write thread cursor: %w", err)
		}
	}
	return result, nil
}

func (p *Poller) tryPermalink(ctx context.Context, channel, ts string) string {
	if p.Client == nil {
		return ""
	}
	link, err := p.Client.Permalink(ctx, channel, ts)
	if err != nil {
		// Non-fatal: permalink is best-effort.
		return ""
	}
	return link
}

func (p *Poller) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// messageToEvent renders an internal Message as the on-disk Event shape.
func messageToEvent(m Message, channel string) *Event {
	threadTS := m.ThreadTS
	if threadTS == "" {
		threadTS = m.TS
	}
	return &Event{
		ID:                 ThreadID(channel, threadTS),
		Source:             "slack",
		Channel:            channel,
		TS:                 m.TS,
		ThreadTS:           threadTS,
		User:               m.User,
		Text:               m.Text,
		Blocks:             m.Blocks,
		Reactions:          m.Reactions,
		Subtype:            m.Subtype,
		IsTopLevelInThread: m.IsTopLevelInThread(),
		Permalink:          m.Permalink,
	}
}

// buildInboxItem builds an inbox/new entry for a never-before-seen
// top-level message.
func buildInboxItem(m Message, channel, permalink string) *InboxItem {
	var threadTS *string
	if m.ThreadTS != "" && m.ThreadTS != m.TS {
		t := m.ThreadTS
		threadTS = &t
	}
	summary := truncate(m.Text, 200)
	inline := map[string]any{
		"text":      m.Text,
		"permalink": permalink,
	}
	if len(m.Blocks) > 0 {
		inline["blocks"] = json.RawMessage(m.Blocks)
	}
	if len(m.Reactions) > 0 {
		inline["reactions"] = json.RawMessage(m.Reactions)
	}
	return &InboxItem{
		ID:      "slack-" + channel + "-" + m.TS,
		Source:  "slack",
		Kind:    "new",
		Summary: summary,
		Ref: InboxRef{
			Channel:  channel,
			TS:       m.TS,
			ThreadTS: threadTS,
			User:     m.User,
		},
		RawInline: inline,
	}
}

func participantsOf(m Message) []string {
	if m.User == "" {
		return nil
	}
	return []string{m.User}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return s[:n-1] + "…"
}

// tsLessThan returns true if a < b treating both as numeric Slack ts.
// Lexicographic comparison works because both are fixed-width formats
// like "1715814123.001200".
func tsLessThan(a, b string) bool {
	return a < b
}

func tsLessThanOrEq(a, b string) bool {
	return a <= b
}

func mergeResult(dst *PollResult, src PollResult) {
	dst.RawEventsWritten += src.RawEventsWritten
	dst.InboxNewWritten += src.InboxNewWritten
	dst.UpdatePingsWritten += src.UpdatePingsWritten
	dst.AnomaliesWritten += src.AnomaliesWritten
	// ChannelsPolled, ThreadsPolled, Errors maintained by caller.
}

// SleepWithJitter sleeps for d ± up to 10%. Used to spread out polls when
// the daemon loop is restarted.
func SleepWithJitter(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
