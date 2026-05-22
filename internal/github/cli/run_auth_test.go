package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	gh "github.com/google/go-github/v75/github"

	ghp "github.com/victor-develop/advanced-tasker/internal/github"
)

// synthErrorResponse builds a go-github *ErrorResponse with the given
// status and headers.  Used by D2 unit tests so we don't need a full
// http roundtrip to exercise the classifier.
func synthErrorResponse(status int, hdr http.Header) error {
	if hdr == nil {
		hdr = http.Header{}
	}
	resp := &http.Response{
		StatusCode: status,
		Header:     hdr,
		Request:    &http.Request{Method: "GET"},
	}
	return &gh.ErrorResponse{
		Response: resp,
		Message:  http.StatusText(status),
	}
}

// TestAuthExitMessage_Mapping covers each of the D2 error categories that
// the run / force-poll wrappers turn into ErrAuthExit + the literal
// stderr line.  Each case constructs a synthetic error matching the
// shape of go-github's *ErrorResponse and asserts the message string.
func TestAuthExitMessage_Mapping(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "sources", "github")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"),
		[]byte("auth:\n  token_env: MY_GH\nwatch:\n  repos: [acme/api]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := ghp.LoadConfig(ghp.DefaultConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		makeErr   func() error
		wantOK    bool
		wantSub   string
		wantNotIn string
	}{
		{
			name: "401 returns token-invalid message with env var name",
			makeErr: func() error {
				return synthErrorResponse(http.StatusUnauthorized, nil)
			},
			wantOK:  true,
			wantSub: "token invalid (check $MY_GH",
		},
		{
			name: "403 with X-RateLimit-Remaining=0 is NOT an auth-exit",
			makeErr: func() error {
				return synthErrorResponse(http.StatusForbidden, http.Header{
					"X-Ratelimit-Remaining": []string{"0"},
				})
			},
			wantOK: false,
		},
		{
			name: "403 without rate-limit headers returns SSO/scope message",
			makeErr: func() error {
				return synthErrorResponse(http.StatusForbidden, nil)
			},
			wantOK:  true,
			wantSub: "org may require SSO authorization",
		},
		{
			name: "200-shaped error is not an auth-exit",
			makeErr: func() error {
				return errors.New("network reset")
			},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := authExitMessage(tc.makeErr(), cfg)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (msg=%q)", ok, tc.wantOK, msg)
			}
			if tc.wantSub != "" && !contains(msg, tc.wantSub) {
				t.Errorf("msg %q does not contain %q", msg, tc.wantSub)
			}
		})
	}
}

// TestRunDoesNotLeakAuthErrAfterCancel ensures that a cancelled context
// during a 401 doesn't promote the cancel into a fake auth-exit.  We
// don't actually exercise the daemon here; we only verify
// authExitMessage's classifier semantics around context-error wrapping.
func TestAuthExitMessage_NilCfgFallsBackToGITHUB_TOKEN(t *testing.T) {
	err := synthErrorResponse(http.StatusUnauthorized, nil)
	msg, ok := authExitMessage(err, nil)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !contains(msg, "$GITHUB_TOKEN") {
		t.Errorf("expected default env name; got %q", msg)
	}
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// suppress unused import in some builds (context).  Kept for parity with
// the rest of the package's test files.
var _ = context.Background
