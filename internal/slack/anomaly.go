package slack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Anomaly is the inbox/anomalies/<id>.json schema. It records a non-fatal
// runtime issue (channel access denied, malformed payload, persistent rate
// limit, etc.) so the commander can surface it on the next tick. The
// poller writes one anomaly per (kind, scope) per process lifetime — the
// file is the marker.
//
// File path:
//
//	state/inbox/anomalies/slack-<scope>.json
//
// Scope conventions:
//
//	channel-access:<channel>           channel not in workspace / not_in_channel
//	rate-limit-exhausted:<scope>       gave up after max_backoff
//	malformed:<channel>:<ts>           per-event malformed response
//	thread-not-found:<thread-id>       tracked thread is 404
//
// Subsequent occurrences of the same anomaly are no-ops (the marker file is
// the signal); the OccurredAt field captures the first sighting.
type Anomaly struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	Kind       string `json:"kind"`
	Scope      string `json:"scope"`
	Reason     string `json:"reason"`
	OccurredAt string `json:"occurred_at"`
	// HTTPStatus is optional — set for rate-limit / API errors. 0 omitted.
	HTTPStatus int `json:"http_status,omitempty"`
	// Channel / ThreadID provide structured pointers when applicable.
	Channel  string `json:"channel,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
}

// AnomaliesDir returns state/inbox/anomalies.
func (s StateLayout) AnomaliesDir() string {
	return filepath.Join(s.StateRoot, "inbox", "anomalies")
}

// WriteAnomaly writes an anomaly marker file if one does not already exist
// for the given anomaly. The filename slot is the anomaly ID — caller is
// responsible for crafting a stable ID (we don't merge with a clock so that
// re-reading is trivial).
//
// Returns wrote=true if a new file was created, wrote=false if a marker
// already existed.
func (w *Writer) WriteAnomaly(a *Anomaly) (wrote bool, err error) {
	if a.ID == "" {
		return false, fmt.Errorf("anomaly.ID required")
	}
	if a.Source == "" {
		a.Source = "slack"
	}
	if a.OccurredAt == "" {
		a.OccurredAt = w.now().Format(time.RFC3339)
	}
	dir := w.Layout.AnomaliesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir anomalies dir: %w", err)
	}
	path := filepath.Join(dir, a.ID+".json")
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return false, err
	}
	if err := atomicWrite(path, append(b, '\n'), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// AnomalyChannelAccess builds an anomaly for a channel the bot cannot
// access (channel_not_found, not_in_channel, etc.).
func AnomalyChannelAccess(channel, reason string) *Anomaly {
	return &Anomaly{
		ID:      "slack-channel-access-" + channel,
		Kind:    "channel_access",
		Scope:   "channel:" + channel,
		Channel: channel,
		Reason:  reason,
	}
}

// AnomalyRateLimitExhausted builds an anomaly for giving up on persistent
// 429s after the configured max_backoff.
func AnomalyRateLimitExhausted(scope, reason string) *Anomaly {
	return &Anomaly{
		ID:     "slack-rate-limit-exhausted-" + safeID(scope),
		Kind:   "rate_limit_exhausted",
		Scope:  scope,
		Reason: reason,
	}
}

// AnomalyMalformed builds an anomaly for a malformed message payload.
// Each (channel, ts) gets its own marker.
func AnomalyMalformed(channel, ts, reason string) *Anomaly {
	return &Anomaly{
		ID:      "slack-malformed-" + safeID(channel+"-"+ts),
		Kind:    "malformed_message",
		Scope:   "message:" + channel + ":" + ts,
		Channel: channel,
		Reason:  reason,
	}
}

// AnomalyThreadNotFound builds an anomaly for a tracked thread that the
// API reports as 404 / channel_not_found.
func AnomalyThreadNotFound(threadID, reason string) *Anomaly {
	return &Anomaly{
		ID:       "slack-thread-not-found-" + safeID(threadID),
		Kind:     "thread_not_found",
		Scope:    "thread:" + threadID,
		ThreadID: threadID,
		Reason:   reason,
	}
}

// safeID maps a string to a filename-safe form. We keep '.' and '-' since
// Slack ts uses them; other special chars become '_'.
func safeID(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '.', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
