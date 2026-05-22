package outbox

import (
	"errors"
	"fmt"
	"strings"

	slackgo "github.com/slack-go/slack"
)

// ErrUnsupportedProvider is returned by SlackProviderSend when the item's
// `to` field is not one of slack | github-pr-comment | github-pr-review.
// The daemon falls back to handleFailure on this.
var ErrUnsupportedProvider = errors.New("unsupported provider")

// SlackProviderSend posts the item body to Slack via chat.postMessage and
// returns a sender_response per design/07. token is the Slack bot token
// resolved from $SLACK_BOT_TOKEN or sources/slack/config.yaml.
//
// Per design/07 §Slack:
//   - to: slack
//   - ref.thread: slack-<channel>-<thread_ts>   (parses out channel + ts)
//   - posts with thread_ts set so replies thread correctly
//
// The body is the raw markdown contents of body_file (Slack does NOT
// render markdown; the caller is responsible for formatting). For the
// outbox-test acceptance suite, callers pass body bodies prefixed with
// [harness-test] so test messages are visually distinguishable.
func SlackProviderSend(token, bodyText string, it *Item) (map[string]any, error) {
	if it.To != "slack" {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedProvider, it.To)
	}
	channel, threadTS, err := parseSlackThreadID(it.Ref.Thread)
	if err != nil {
		return nil, err
	}
	api := slackgo.New(token)
	opts := []slackgo.MsgOption{
		slackgo.MsgOptionText(bodyText, false),
		slackgo.MsgOptionDisableLinkUnfurl(),
		slackgo.MsgOptionAsUser(false),
	}
	if threadTS != "" {
		opts = append(opts, slackgo.MsgOptionTS(threadTS))
	}
	_, ts, err := api.PostMessage(channel, opts...)
	if err != nil {
		return nil, fmt.Errorf("chat.postMessage: %w", err)
	}
	return map[string]any{
		"provider":   "slack",
		"channel":    channel,
		"thread_ts":  threadTS,
		"message_ts": ts,
	}, nil
}

// SlackProviderDelete performs `chat.delete` for revoke-within-window.
// Used by `harness outbox revoke` when the item is in sent/ and the
// revoke_window has not yet elapsed (design/07 §Revoke).
func SlackProviderDelete(token string, channel, messageTS string) error {
	api := slackgo.New(token)
	_, _, err := api.DeleteMessage(channel, messageTS)
	if err != nil {
		return fmt.Errorf("chat.delete: %w", err)
	}
	return nil
}

// parseSlackThreadID extracts the channel and thread_ts out of an ID
// shaped slack-<channel>-<thread_ts>. The thread_ts portion may itself
// contain a dot (Slack timestamps look like 1715814123.001200).
func parseSlackThreadID(threadID string) (channel, threadTS string, err error) {
	if !strings.HasPrefix(threadID, "slack-") {
		return "", "", fmt.Errorf("not a slack thread id: %q", threadID)
	}
	rest := strings.TrimPrefix(threadID, "slack-")
	// Channel ID is the next dash-separated token; anything after is the ts.
	idx := strings.Index(rest, "-")
	if idx < 0 {
		return "", "", fmt.Errorf("malformed slack thread id: %q", threadID)
	}
	return rest[:idx], rest[idx+1:], nil
}
