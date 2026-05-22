package cli

import (
	"errors"
	"strings"
	"testing"

	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
)

// TestFormatAuthError_InvalidAuth verifies the operator-actionable stderr
// message when Slack rejects the token. Per round-3 §D3, the message must
// be exactly:
//
//	slack-poller: token invalid (check SLACK_BOT_TOKEN, bot must be in channel)
func TestFormatAuthError_InvalidAuth(t *testing.T) {
	for _, code := range []string{
		"invalid_auth",
		"token_revoked",
		"token_expired",
		"not_authed",
		"account_inactive",
	} {
		t.Run(code, func(t *testing.T) {
			err := &CommandError{Code: ExitAuthFatal, Err: errors.New(code)}
			got := FormatAuthError(err)
			want := "slack-poller: token invalid (check SLACK_BOT_TOKEN, bot must be in channel)"
			if got != want {
				t.Errorf("FormatAuthError(%s) = %q, want %q", code, got, want)
			}
		})
	}
}

// TestFormatAuthError_MissingScope verifies that a `missing_scope` Slack
// error surfaces the missing scope name in the stderr line.
func TestFormatAuthError_MissingScope(t *testing.T) {
	err := errors.New("missing_scope, needed: channels:history, provided: chat:write")
	got := FormatAuthError(err)
	if !strings.Contains(got, "missing scope: channels:history") {
		t.Errorf("FormatAuthError missing scope name: %q", got)
	}
}

// TestFormatAuthError_NonAuthErrorPassThrough verifies that non-auth errors
// are passed through with the "slack-poller:" prefix (no rewrite).
func TestFormatAuthError_NonAuthErrorPassThrough(t *testing.T) {
	err := errors.New("read config: yaml: line 3: did not find expected key")
	got := FormatAuthError(err)
	if !strings.HasPrefix(got, "slack-poller: ") {
		t.Errorf("missing prefix: %q", got)
	}
	if !strings.Contains(got, "did not find expected key") {
		t.Errorf("dropped original error: %q", got)
	}
}

// TestFormatAuthError_NilSafe ensures FormatAuthError handles a nil error.
func TestFormatAuthError_NilSafe(t *testing.T) {
	if got := FormatAuthError(nil); got != "" {
		t.Errorf("FormatAuthError(nil) = %q, want empty", got)
	}
}

// TestAuthFailCode covers the public helper used by daemon callers.
func TestAuthFailCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"invalid_auth", errors.New("invalid_auth"), "invalid_auth"},
		{"token_revoked", errors.New("token_revoked"), "token_revoked"},
		{"missing_scope", errors.New("missing_scope, needed: x"), "missing_scope"},
		{"benign", errors.New("network unreachable"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := slackpkg.AuthFailCode(tc.err); got != tc.want {
				t.Errorf("AuthFailCode = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMissingScope_Extraction verifies the slack-go error-text parser.
func TestMissingScope_Extraction(t *testing.T) {
	for _, tc := range []struct {
		err  string
		want string
	}{
		{"missing_scope, needed: channels:history, provided: chat:write", "channels:history"},
		{"slack reported: missing_scope, needed: channels:read", "channels:read"},
		{"some other error", ""},
		{"missing_scope but no needed clause", ""},
	} {
		got := slackpkg.MissingScope(errors.New(tc.err))
		if got != tc.want {
			t.Errorf("MissingScope(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
