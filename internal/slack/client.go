package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
}

// NewSlackGoClient constructs a client. apiURL is optional — when non-empty
// the client points there instead of slack.com, useful for httptest.
func NewSlackGoClient(token, apiURL string) *SlackGoClient {
	var opts []sgs.Option
	if apiURL != "" {
		opts = append(opts, sgs.OptionAPIURL(apiURL))
	}
	return &SlackGoClient{C: sgs.New(token, opts...)}
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
