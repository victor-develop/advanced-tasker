package llm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Fake is a deterministic, file-backed driver used by tests and
// `--driver fake` acceptance runs. For each Invoke(role=R), it reads
// `<FixtureDir>/<R>/<call-index>.txt` and returns its content as the
// LLM output. The call index is per-role and starts at 0.
//
// If a scripted file is missing, Invoke returns a clearly-marked
// fallback string so loops still terminate. Tests that need exact
// outputs should pre-seed every expected fixture.
type Fake struct {
	FixtureDir string

	mu    sync.Mutex
	calls []FakeCall
	idx   map[Role]int
}

// FakeCall records one Invoke for later inspection.
type FakeCall struct {
	Role    Role
	Prompt  string
	Output  string
	Options InvokeOptions
}

// NewFake constructs a Fake whose scripted responses live under
// fixtureDir. Missing fixture files do not panic; see Invoke.
func NewFake(fixtureDir string) *Fake {
	return &Fake{
		FixtureDir: fixtureDir,
		idx:        map[Role]int{},
	}
}

// Name returns the driver identifier ("fake") per the Driver contract.
func (f *Fake) Name() string { return "fake" }

// Invoke returns the next scripted response for the given role.
func (f *Fake) Invoke(ctx context.Context, prompt string, opts InvokeOptions) (InvokeResult, error) {
	f.mu.Lock()
	role := opts.Role
	if role == "" {
		role = "default"
	}
	idx := f.idx[role]
	f.idx[role] = idx + 1
	f.mu.Unlock()

	start := time.Now()
	out, err := f.read(role, idx)
	if err != nil {
		out = fmt.Sprintf("[fake-driver: no fixture for role=%s index=%d; prompt=%dB]", role, idx, len(prompt))
	}

	res := InvokeResult{
		Output:     out,
		SessionID:  fmt.Sprintf("fake-%s-%d", role, idx),
		CostUSD:    0,
		DurationMS: time.Since(start).Milliseconds(),
		IsError:    false,
	}
	if opts.StreamJSONPath != "" {
		if perr := writeFakeJSONL(opts.StreamJSONPath, role, idx, out); perr == nil {
			res.RawArtifact = opts.StreamJSONPath
		}
	}

	f.mu.Lock()
	f.calls = append(f.calls, FakeCall{Role: role, Prompt: prompt, Output: out, Options: opts})
	f.mu.Unlock()
	return res, nil
}

// Calls returns a snapshot of every Invoke handled so far.
func (f *Fake) Calls() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// Reset clears the call log and call indices. Useful between subtests.
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
	f.idx = map[Role]int{}
}

func (f *Fake) read(role Role, idx int) (string, error) {
	if f.FixtureDir == "" {
		return "", fmt.Errorf("fixture dir not configured")
	}
	path := filepath.Join(f.FixtureDir, string(role), fmt.Sprintf("%d.txt", idx))
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// writeFakeJSONL synthesises a minimal stream-json transcript so tests
// that inspect telemetry files have something to look at.
func writeFakeJSONL(path string, role Role, idx int, out string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf(
		"{\"type\":\"system\",\"role\":%q,\"call\":%d}\n"+
			"{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n"+
			"{\"type\":\"result\",\"total_cost_usd\":0,\"duration_ms\":0,\"is_error\":false,\"session_id\":\"fake-%s-%d\"}\n",
		string(role), idx, out, role, idx,
	)
	return os.WriteFile(path, []byte(body), 0o644)
}
