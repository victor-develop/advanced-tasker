package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	slackpkg "github.com/victor-develop/advanced-tasker/internal/slack"
)

// stubClient implements slackpkg.APIClient for doctor unit tests. Each
// method returns the canned value (or canned error) on the respective field.
type stubClient struct {
	authInfo slackpkg.AuthInfo
	authErr  error

	channels map[string]slackpkg.ChannelInfo
	chanErrs map[string]error

	histErr    error
	histResult slackpkg.HistoryPage
}

func (s *stubClient) AuthTest(ctx context.Context) (slackpkg.AuthInfo, error) {
	return s.authInfo, s.authErr
}

func (s *stubClient) ConversationInfo(ctx context.Context, ch string) (slackpkg.ChannelInfo, error) {
	if err, ok := s.chanErrs[ch]; ok {
		return slackpkg.ChannelInfo{}, err
	}
	info, ok := s.channels[ch]
	if !ok {
		return slackpkg.ChannelInfo{}, errors.New("channel_not_found")
	}
	return info, nil
}

func (s *stubClient) History(ctx context.Context, p slackpkg.HistoryParams) (slackpkg.HistoryPage, error) {
	if s.histErr != nil {
		return slackpkg.HistoryPage{}, s.histErr
	}
	return s.histResult, nil
}

func (s *stubClient) Replies(ctx context.Context, p slackpkg.RepliesParams) (slackpkg.RepliesPage, error) {
	return slackpkg.RepliesPage{}, nil
}

func (s *stubClient) Permalink(ctx context.Context, ch, ts string) (string, error) {
	return "", nil
}

// seedDoctorConfig writes a minimal config with one watched channel.
func seedDoctorConfig(t *testing.T, stateDir, channel string) {
	t.Helper()
	dir := filepath.Join(stateDir, "sources", "slack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `auth:
  token_env: TEST_SLACK_TOKEN
watch:
  channels:
    - id: ` + channel + `
poll_interval: 30s
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runDoctorCmd runs the doctor cobra subcommand directly with a stub client
// injected via the package-level test seam. Returns
// (stdout, stderr-or-err-msg, exitCode).
func runDoctorCmd(t *testing.T, stateDir string, stub slackpkg.APIClient,
	extraArgs ...string) (string, string, int) {

	t.Helper()
	testDoctorBuildClient = func(cfg *slackpkg.Config, apiURL string) (slackpkg.APIClient, error) {
		return stub, nil
	}
	t.Cleanup(func() { testDoctorBuildClient = nil })

	root := NewRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	args := append([]string{"--state-dir", stateDir, "doctor"}, extraArgs...)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	code := 0
	if err != nil {
		code = ExtractExitCode(err)
		stderr.WriteString(err.Error())
		stderr.WriteString("\n")
	}
	return stdout.String(), stderr.String(), code
}

func TestDoctor_HappyPath(t *testing.T) {
	stateDir := t.TempDir()
	seedDoctorConfig(t, stateDir, "C_OK")
	t.Setenv("TEST_SLACK_TOKEN", "xoxb-doctor")

	stub := &stubClient{
		authInfo: slackpkg.AuthInfo{User: "tasker-bot", Team: "Acme"},
		channels: map[string]slackpkg.ChannelInfo{
			"C_OK": {ID: "C_OK", Name: "data-alerts", IsMember: true},
		},
		histResult: slackpkg.HistoryPage{Messages: []slackpkg.Message{{
			TS:   "1700000000.000000",
			User: "U_A",
			Text: "hello",
		}}},
	}
	stdout, stderr, code := runDoctorCmd(t, stateDir, stub)
	if code != 0 {
		t.Errorf("exit = %d, want 0; stdout=%s; stderr=%s", code, stdout, stderr)
	}
	for _, want := range []string{
		"[ok]   token from env $TEST_SLACK_TOKEN",
		"[ok]   authenticated as tasker-bot in workspace Acme",
		"[ok]   channel C_OK (data-alerts) -- bot has access",
		"summary: all checks passed (exit 0)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\n--full--\n%s", want, stdout)
		}
	}
}

func TestDoctor_TokenMissing(t *testing.T) {
	stateDir := t.TempDir()
	seedDoctorConfig(t, stateDir, "C_X")
	// Note: TEST_SLACK_TOKEN unset.

	stub := &stubClient{}
	stdout, _, code := runDoctorCmd(t, stateDir, stub)
	if code != 1 {
		t.Errorf("exit = %d, want 1; stdout=%s", code, stdout)
	}
	wantMsg := "token not found: set $TEST_SLACK_TOKEN or auth.token in state/sources/slack/config.yaml"
	if !strings.Contains(stdout, wantMsg) {
		t.Errorf("stdout missing %q\n--full--\n%s", wantMsg, stdout)
	}
}

func TestDoctor_AuthFail_InvalidToken(t *testing.T) {
	stateDir := t.TempDir()
	seedDoctorConfig(t, stateDir, "C_X")
	t.Setenv("TEST_SLACK_TOKEN", "xoxb-bad")

	stub := &stubClient{
		authErr: errors.New("invalid_auth"),
	}
	stdout, _, code := runDoctorCmd(t, stateDir, stub)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	for _, want := range []string{
		"[ok]   token from env $TEST_SLACK_TOKEN",
		"[fail] auth.test failed",
		"invalid_auth",
		"token invalid (check SLACK_BOT_TOKEN, bot must be in channel)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\n--full--\n%s", want, stdout)
		}
	}
}

func TestDoctor_ChannelNotFound(t *testing.T) {
	stateDir := t.TempDir()
	seedDoctorConfig(t, stateDir, "C_BAD")
	t.Setenv("TEST_SLACK_TOKEN", "xoxb-x")

	stub := &stubClient{
		authInfo: slackpkg.AuthInfo{User: "tasker-bot", Team: "Acme"},
		chanErrs: map[string]error{
			"C_BAD": errors.New("channel_not_found"),
		},
	}
	stdout, _, code := runDoctorCmd(t, stateDir, stub)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, "channel_not_found") {
		t.Errorf("stdout missing slack code:\n%s", stdout)
	}
	if !strings.Contains(stdout, "channel C_BAD inaccessible") {
		t.Errorf("stdout missing summary:\n%s", stdout)
	}
}

func TestDoctor_NotInChannelHint(t *testing.T) {
	stateDir := t.TempDir()
	seedDoctorConfig(t, stateDir, "C_BUSY")
	t.Setenv("TEST_SLACK_TOKEN", "xoxb-x")

	stub := &stubClient{
		authInfo: slackpkg.AuthInfo{User: "tasker-bot", Team: "Acme"},
		channels: map[string]slackpkg.ChannelInfo{
			"C_BUSY": {ID: "C_BUSY", Name: "alerts", IsMember: false},
		},
	}
	stdout, _, code := runDoctorCmd(t, stateDir, stub)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, "hint: invite the bot via `/invite @tasker-bot` in #alerts") {
		t.Errorf("stdout missing /invite hint:\n%s", stdout)
	}
}

func TestDoctor_EmptyHistorySoftExit(t *testing.T) {
	stateDir := t.TempDir()
	seedDoctorConfig(t, stateDir, "C_QUIET")
	t.Setenv("TEST_SLACK_TOKEN", "xoxb-x")

	stub := &stubClient{
		authInfo: slackpkg.AuthInfo{User: "tasker-bot", Team: "Acme"},
		channels: map[string]slackpkg.ChannelInfo{
			"C_QUIET": {ID: "C_QUIET", Name: "quiet-room", IsMember: true},
		},
		histResult: slackpkg.HistoryPage{Messages: nil},
	}
	stdout, _, code := runDoctorCmd(t, stateDir, stub)
	if code != 2 {
		t.Errorf("exit = %d, want 2 (soft signal); stdout=%s", code, stdout)
	}
	if !strings.Contains(stdout, "history is empty") {
		t.Errorf("stdout missing 'history is empty':\n%s", stdout)
	}
}

func TestDoctor_NoChannelsConfigured_SoftExit(t *testing.T) {
	stateDir := t.TempDir()
	// Seed a config with empty watch.channels.
	dir := filepath.Join(stateDir, "sources", "slack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `auth:
  token_env: TEST_SLACK_TOKEN
watch:
  channels: []
poll_interval: 30s
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SLACK_TOKEN", "xoxb-x")
	stub := &stubClient{
		authInfo: slackpkg.AuthInfo{User: "bot", Team: "Acme"},
	}
	stdout, _, code := runDoctorCmd(t, stateDir, stub)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stdout, "no channels configured") {
		t.Errorf("stdout missing message:\n%s", stdout)
	}
}

func TestDoctor_JSONOutput(t *testing.T) {
	stateDir := t.TempDir()
	seedDoctorConfig(t, stateDir, "C_OK")
	t.Setenv("TEST_SLACK_TOKEN", "xoxb-doctor")

	stub := &stubClient{
		authInfo: slackpkg.AuthInfo{User: "bot", Team: "Acme"},
		channels: map[string]slackpkg.ChannelInfo{
			"C_OK": {ID: "C_OK", Name: "ok", IsMember: true},
		},
		histResult: slackpkg.HistoryPage{Messages: []slackpkg.Message{{TS: "1.1"}}},
	}
	stdout, _, code := runDoctorCmd(t, stateDir, stub, "--json")
	if code != 0 {
		t.Errorf("exit = %d", code)
	}
	var report DoctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("parse JSON: %v\n%s", err, stdout)
	}
	if !report.Token.OK || !report.Auth.OK {
		t.Errorf("token.ok=%v auth.ok=%v", report.Token.OK, report.Auth.OK)
	}
	if len(report.Channels) != 1 || !report.Channels[0].OK {
		t.Errorf("channels not ok: %+v", report.Channels)
	}
	if report.ExitCode != 0 {
		t.Errorf("exit_code = %d", report.ExitCode)
	}
}

func TestDoctor_MissingScope(t *testing.T) {
	stateDir := t.TempDir()
	seedDoctorConfig(t, stateDir, "C_X")
	t.Setenv("TEST_SLACK_TOKEN", "xoxb-x")

	stub := &stubClient{
		// slack-go formats missing_scope errors with "needed: <scope>".
		authErr: errors.New("missing_scope, needed: channels:history, provided: chat:write"),
	}
	stdout, _, code := runDoctorCmd(t, stateDir, stub)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, "missing scope: channels:history") {
		t.Errorf("stdout missing scope name:\n%s", stdout)
	}
}
