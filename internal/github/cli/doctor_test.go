package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitHub builds a tiny httptest.Server that responds to the four
// endpoints the doctor uses: /user, /repos/{o}/{r}, and
// /repos/{o}/{r}/pulls.  Handlers are keyed by method+path.
type fakeGitHub struct {
	t        *testing.T
	mux      *http.ServeMux
	srv      *httptest.Server
	handlers map[string]http.HandlerFunc
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		t:        t,
		mux:      http.NewServeMux(),
		handlers: map[string]http.HandlerFunc{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		h, ok := f.handlers[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) on(method, path string, h http.HandlerFunc) {
	f.handlers[method+" "+path] = h
}

func (f *fakeGitHub) baseURL() string { return f.srv.URL + "/" }

func seedDoctorConfig(t *testing.T, dir string, repos ...string) {
	t.Helper()
	cfgDir := filepath.Join(dir, "sources", "github")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "auth:\n  token_env: TEST_DOCTOR_TOKEN\nwatch:\n  repos:\n"
	for _, r := range repos {
		body += "    - " + r + "\n"
	}
	if len(repos) == 0 {
		body = "auth:\n  token_env: TEST_DOCTOR_TOKEN\nwatch:\n  repos: []\n"
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func userOK(login string, id int64) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"login": login,
			"id":    id,
		})
	}
}

func repoOK(visibility, defaultBranch string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Some responses set "visibility", some set "private".  We send
		// both so go-github decodes the field set this client expects.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":           "x",
			"private":        visibility == "private",
			"visibility":     visibility,
			"default_branch": defaultBranch,
		})
	}
}

func httpStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"message":"not authorized"}`))
	}
}

func TestDoctor_AllOK(t *testing.T) {
	dir := t.TempDir()
	seedDoctorConfig(t, dir, "acme/api")

	t.Setenv("TEST_DOCTOR_TOKEN", "ghp_test_doctor_token")

	api := newFakeGitHub(t)
	api.on("GET", "/user", userOK("alice", 1234))
	api.on("GET", "/repos/acme/api", repoOK("private", "main"))
	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id": 1, "number": 1}]`))
	})

	rep := RunDoctor(context.Background(), dir, api.baseURL())
	if rep.ExitCode != DoctorExitOK {
		t.Fatalf("exit: got %d want %d (report=%+v)", rep.ExitCode, DoctorExitOK, rep)
	}
	if !rep.Token.OK || rep.Token.Length != len("ghp_test_doctor_token") {
		t.Errorf("token: %+v", rep.Token)
	}
	if !rep.Auth.OK || rep.Auth.Login != "alice" || rep.Auth.ID != 1234 {
		t.Errorf("auth: %+v", rep.Auth)
	}
	if len(rep.Repos) != 1 || !rep.Repos[0].OK || rep.Repos[0].Visibility != "private" || rep.Repos[0].DefaultBranch != "main" {
		t.Errorf("repos: %+v", rep.Repos)
	}
	if rep.PRsPing == nil || !rep.PRsPing.OK || rep.PRsPing.Returned != 1 {
		t.Errorf("prs ping: %+v", rep.PRsPing)
	}
}

func TestDoctor_NoToken(t *testing.T) {
	dir := t.TempDir()
	seedDoctorConfig(t, dir, "acme/api")
	// Deliberately do NOT set TEST_DOCTOR_TOKEN.
	t.Setenv("TEST_DOCTOR_TOKEN", "")

	rep := RunDoctor(context.Background(), dir, "")
	if rep.ExitCode != DoctorExitHard {
		t.Fatalf("expected hard failure; got %d (report=%+v)", rep.ExitCode, rep)
	}
	if !strings.Contains(rep.Token.Err, "token not found") {
		t.Errorf("token err: %q", rep.Token.Err)
	}
	if !strings.Contains(rep.Token.Err, "TEST_DOCTOR_TOKEN") {
		t.Errorf("token err should mention env var name; got %q", rep.Token.Err)
	}
}

func TestDoctor_Unauthorized(t *testing.T) {
	dir := t.TempDir()
	seedDoctorConfig(t, dir, "acme/api")
	t.Setenv("TEST_DOCTOR_TOKEN", "bad")

	api := newFakeGitHub(t)
	api.on("GET", "/user", httpStatus(http.StatusUnauthorized))

	rep := RunDoctor(context.Background(), dir, api.baseURL())
	if rep.ExitCode != DoctorExitHard {
		t.Fatalf("expected hard failure; got %d", rep.ExitCode)
	}
	if !strings.Contains(rep.Auth.Err, "token invalid") {
		t.Errorf("auth.err: %q", rep.Auth.Err)
	}
	if !strings.Contains(rep.Auth.Err, "TEST_DOCTOR_TOKEN") {
		t.Errorf("auth.err should mention env var; got %q", rep.Auth.Err)
	}
}

func TestDoctor_RepoNotFound_SoftFailure(t *testing.T) {
	dir := t.TempDir()
	seedDoctorConfig(t, dir, "acme/api", "acme/missing")
	t.Setenv("TEST_DOCTOR_TOKEN", "ok")

	api := newFakeGitHub(t)
	api.on("GET", "/user", userOK("alice", 1))
	api.on("GET", "/repos/acme/api", repoOK("public", "main"))
	api.on("GET", "/repos/acme/missing", httpStatus(http.StatusNotFound))
	api.on("GET", "/repos/acme/api/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})

	rep := RunDoctor(context.Background(), dir, api.baseURL())
	if rep.ExitCode != DoctorExitSoft {
		t.Fatalf("expected soft failure; got %d (report=%+v)", rep.ExitCode, rep)
	}
	// Verify per-repo statuses.
	var missing *DoctorRepo
	var ok *DoctorRepo
	for i := range rep.Repos {
		switch rep.Repos[i].Repo {
		case "acme/missing":
			missing = &rep.Repos[i]
		case "acme/api":
			ok = &rep.Repos[i]
		}
	}
	if ok == nil || !ok.OK {
		t.Errorf("acme/api should be ok; got %+v", ok)
	}
	if missing == nil || missing.OK {
		t.Fatalf("acme/missing should be failed; got %+v", missing)
	}
	if !strings.Contains(missing.Err, "404") {
		t.Errorf("missing.err should mention 404; got %q", missing.Err)
	}
	if !strings.Contains(missing.Err, "token may lack access") {
		t.Errorf("missing.err should mention scope; got %q", missing.Err)
	}
}

func TestDoctor_RepoForbidden_SoftFailure(t *testing.T) {
	dir := t.TempDir()
	seedDoctorConfig(t, dir, "acme/private")
	t.Setenv("TEST_DOCTOR_TOKEN", "ok")

	api := newFakeGitHub(t)
	api.on("GET", "/user", userOK("alice", 1))
	api.on("GET", "/repos/acme/private", httpStatus(http.StatusForbidden))
	// Ping may also 403; the doctor records the soft failure either way.
	api.on("GET", "/repos/acme/private/pulls", httpStatus(http.StatusForbidden))

	rep := RunDoctor(context.Background(), dir, api.baseURL())
	if rep.ExitCode != DoctorExitSoft {
		t.Fatalf("expected soft failure; got %d (report=%+v)", rep.ExitCode, rep)
	}
	if len(rep.Repos) != 1 {
		t.Fatalf("expected one repo; got %v", rep.Repos)
	}
	if !strings.Contains(rep.Repos[0].Err, "403") {
		t.Errorf("repo err should mention 403; got %q", rep.Repos[0].Err)
	}
	if !strings.Contains(rep.Repos[0].Err, "SSO") {
		t.Errorf("repo err should mention SSO; got %q", rep.Repos[0].Err)
	}
}

func TestDoctor_MissingConfigIsHard(t *testing.T) {
	dir := t.TempDir()
	// No config seeded.
	rep := RunDoctor(context.Background(), dir, "")
	if rep.ExitCode != DoctorExitHard {
		t.Fatalf("expected hard failure on missing config; got %d", rep.ExitCode)
	}
	if !strings.Contains(rep.ConfigErr, "harness config init github") {
		t.Errorf("config err should reference 'harness config init github'; got %q", rep.ConfigErr)
	}
}

func TestDoctorExitCode_Sentinel(t *testing.T) {
	if got := DoctorExitCode(nil); got != 0 {
		t.Errorf("nil err: got %d want 0", got)
	}
	if got := DoctorExitCode(&doctorExit{code: 1}); got != 1 {
		t.Errorf("hard err: got %d want 1", got)
	}
	if got := DoctorExitCode(&doctorExit{code: 2}); got != 2 {
		t.Errorf("soft err: got %d want 2", got)
	}
}

func TestExitCodeFor_AuthExitIs3(t *testing.T) {
	if got := ExitCodeFor(ErrAuthExit); got != 3 {
		t.Errorf("ErrAuthExit: got exit %d want 3", got)
	}
}
