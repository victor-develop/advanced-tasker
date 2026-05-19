package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	sgs "github.com/slack-go/slack"
)

// APIClient is the subset of Slack API surface the poller uses. Defined
// as an interface so tests can substitute a mock.
type APIClient interface {
	// History fetches conversations.history page for a channel.
	History(ctx context.Context, params HistoryParams) (HistoryPage, error)
	// Replies fetches conversations.replies page for a thread.
	Replies(ctx context.Context, params RepliesParams) (RepliesPage, error)
	// Permalink optionally fetches the permalink for a message. Errors are
	// non-fatal; the poller falls back to a blank permalink.
	Permalink(ctx context.Context, channel, ts string) (string, error)
}

// HistoryParams are the inputs to conversations.history.
type HistoryParams struct {
	ChannelID string
	Oldest    string
	Cursor    string
	Limit     int
}

// HistoryPage is one page of conversations.history.
type HistoryPage struct {
	Messages   []Message
	HasMore    bool
	NextCursor string
}

// RepliesParams are the inputs to conversations.replies.
type RepliesParams struct {
	ChannelID string
	ThreadTS  string
	Oldest    string
	Cursor    string
	Limit     int
}

// RepliesPage is one page of conversations.replies.
type RepliesPage struct {
	Messages   []Message
	HasMore    bool
	NextCursor string
}

// Message is the poller's internal representation of a Slack message. We
// preserve unparsed JSON for blocks/reactions so downstream consumers can
// inspect everything Slack returned.
type Message struct {
	ChannelOverride string // optional: set by poller when channel is not on the wire (replies omit it)
	TS              string
	ThreadTS        string
	User            string
	Text            string
	Subtype         string
	Blocks          json.RawMessage
	Reactions       json.RawMessage
	Permalink       string
}

// EffectiveThreadTS returns ThreadTS if non-empty, else TS. A top-level
// message in a channel that has not yet spawned replies has ThreadTS=="".
func (m Message) EffectiveThreadTS() string {
	if m.ThreadTS != "" {
		return m.ThreadTS
	}
	return m.TS
}

// IsTopLevelInThread reports whether this message is the thread root
// (ts == thread_ts).
func (m Message) IsTopLevelInThread() bool {
	if m.ThreadTS == "" {
		return true
	}
	return m.TS == m.ThreadTS
}

// SlackGoClient is an APIClient backed by github.com/slack-go/slack.
type SlackGoClient struct {
	C *sgs.Client
	// LastResponseHeaders stores the most recent HTTP response headers seen
	// from Slack. Used to surface Retry-After info on 429 errors. Best
	// effort; not thread-safe across distinct call sites.
	LastResponseHeaders http.Header
}

// SlackGoClientConfig wires retry + transport settings.
type SlackGoClientConfig struct {
	// APIURL overrides slack.com (testing).
	APIURL string
	// MaxRetries429 is the number of automatic in-client retries on 429.
	// 0 disables (the caller handles 429s explicitly).
	MaxRetries429 int
	// RateLimitFallback is the wait when Slack omits Retry-After.
	RateLimitFallback time.Duration
	// MaxBackoff caps the in-client retry sleep.
	MaxBackoff time.Duration
}

// NewSlackGoClient constructs a client. apiURL is optional — when non-empty
// the client points there instead of slack.com, useful for httptest. This
// signature is preserved for the existing callers; new code should use
// NewSlackGoClientWithConfig.
func NewSlackGoClient(token, apiURL string) *SlackGoClient {
	return NewSlackGoClientWithConfig(token, SlackGoClientConfig{APIURL: apiURL})
}

// NewSlackGoClientWithConfig constructs a client with retry / rate-limit
// behavior wired in. The client retries 429s up to cfg.MaxRetries429 times,
// honoring Retry-After when present, falling back to cfg.RateLimitFallback
// (capped by cfg.MaxBackoff) otherwise. On a 401 (token invalid), the
// underlying call surfaces a SlackErrorResponse with code "invalid_auth";
// callers should map that to ErrFatalAuth via IsFatalAuthError.
func NewSlackGoClientWithConfig(token string, cfg SlackGoClientConfig) *SlackGoClient {
	c := &SlackGoClient{}
	var opts []sgs.Option
	if cfg.APIURL != "" {
		opts = append(opts, sgs.OptionAPIURL(cfg.APIURL))
	}
	opts = append(opts, sgs.OptionOnResponseHeaders(func(_ string, headers http.Header) {
		c.LastResponseHeaders = headers.Clone()
	}))
	if cfg.MaxRetries429 > 0 {
		retryCfg := sgs.RetryConfig{
			MaxRetries:         cfg.MaxRetries429,
			RetryAfterDuration: cfg.RateLimitFallback,
			BackoffInitial:     cfg.RateLimitFallback,
			BackoffMax:         cfg.MaxBackoff,
		}
		opts = append(opts, sgs.OptionRetryConfig(retryCfg))
	}
	c.C = sgs.New(token, opts...)
	return c
}

func (s *SlackGoClient) History(ctx context.Context, p HistoryParams) (HistoryPage, error) {
	limit := p.Limit
	if limit == 0 {
		limit = 100
	}
	resp, err := s.C.GetConversationHistoryContext(ctx, &sgs.GetConversationHistoryParameters{
		ChannelID: p.ChannelID,
		Oldest:    p.Oldest,
		Cursor:    p.Cursor,
		Limit:     limit,
	})
	if err != nil {
		return HistoryPage{}, fmt.Errorf("conversations.history %s: %w", p.ChannelID, err)
	}
	if !resp.Ok {
		return HistoryPage{}, fmt.Errorf("conversations.history %s: %s", p.ChannelID, resp.Error)
	}
	msgs := make([]Message, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		msgs = append(msgs, convertMsg(m, p.ChannelID))
	}
	return HistoryPage{
		Messages:   msgs,
		HasMore:    resp.HasMore,
		NextCursor: resp.ResponseMetaData.NextCursor,
	}, nil
}

func (s *SlackGoClient) Replies(ctx context.Context, p RepliesParams) (RepliesPage, error) {
	limit := p.Limit
	if limit == 0 {
		limit = 200
	}
	msgs, hasMore, next, err := s.C.GetConversationRepliesContext(ctx, &sgs.GetConversationRepliesParameters{
		ChannelID: p.ChannelID,
		Timestamp: p.ThreadTS,
		Oldest:    p.Oldest,
		Cursor:    p.Cursor,
		Limit:     limit,
	})
	if err != nil {
		return RepliesPage{}, fmt.Errorf("conversations.replies %s ts=%s: %w", p.ChannelID, p.ThreadTS, err)
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, convertMsg(m, p.ChannelID))
	}
	return RepliesPage{Messages: out, HasMore: hasMore, NextCursor: next}, nil
}

func (s *SlackGoClient) Permalink(ctx context.Context, channel, ts string) (string, error) {
	link, err := s.C.GetPermalinkContext(ctx, &sgs.PermalinkParameters{
		Channel: channel,
		Ts:      ts,
	})
	if err != nil {
		return "", err
	}
	return link, nil
}

// convertMsg converts a slack-go Message to our internal Message. We
// re-marshal blocks/reactions to json.RawMessage to keep them opaque.
func convertMsg(m sgs.Message, channel string) Message {
	out := Message{
		ChannelOverride: channel,
		TS:              m.Timestamp,
		ThreadTS:        m.ThreadTimestamp,
		User:            m.User,
		Text:            m.Text,
		Subtype:         m.SubType,
		Permalink:       m.Permalink,
	}
	if len(m.Blocks.BlockSet) > 0 {
		if b, err := json.Marshal(m.Blocks); err == nil && !isEmptyJSON(b) {
			out.Blocks = b
		}
	}
	if len(m.Reactions) > 0 {
		if b, err := json.Marshal(m.Reactions); err == nil && !isEmptyJSON(b) {
			out.Reactions = b
		}
	}
	return out
}

func isEmptyJSON(b []byte) bool {
	s := strings.TrimSpace(string(b))
	return s == "" || s == "null" || s == "[]" || s == "{}"
}

// ErrFatalAuth is returned by the client when Slack reports an
// unrecoverable auth error. The poller should exit non-zero on this.
var ErrFatalAuth = errors.New("slack auth error: token invalid or revoked")

// fatalAuthCodes are Slack `error` codes that indicate the token cannot be
// repaired by waiting/retrying — only by rotating credentials.
var fatalAuthCodes = map[string]struct{}{
	"invalid_auth":      {},
	"not_authed":        {},
	"token_revoked":     {},
	"token_expired":     {},
	"account_inactive":  {},
	"missing_scope":     {},
}

// IsFatalAuthError reports whether err is a Slack auth failure that should
// terminate the process (vs. backing off and retrying).
func IsFatalAuthError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrFatalAuth) {
		return true
	}
	var sre sgs.SlackErrorResponse
	if errors.As(err, &sre) {
		if _, ok := fatalAuthCodes[sre.Err]; ok {
			return true
		}
	}
	// String fallback for the simpler errors slack-go surfaces.
	msg := err.Error()
	for code := range fatalAuthCodes {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}

// rateLimitedRetryAfter pulls the wait duration off a *sgs.RateLimitedError
// if err is one (or wraps one). Returns ok=false if not a rate-limit error.
func rateLimitedRetryAfter(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	var rl *sgs.RateLimitedError
	if errors.As(err, &rl) {
		return rl.RetryAfter, true
	}
	return 0, false
}

// RateLimitWait reports the Retry-After-derived wait if err is a rate-limit
// error. Exported for the daemon loop.
func RateLimitWait(err error) (time.Duration, bool) {
	return rateLimitedRetryAfter(err)
}

// channelAccessError returns the matching anomaly reason if err indicates
// the bot cannot access the channel (channel_not_found, not_in_channel,
// missing_scope), and "" otherwise.
func channelAccessError(err error) string {
	if err == nil {
		return ""
	}
	codes := []string{"channel_not_found", "not_in_channel", "is_archived"}
	msg := err.Error()
	for _, c := range codes {
		if strings.Contains(msg, c) {
			return c
		}
	}
	return ""
}
