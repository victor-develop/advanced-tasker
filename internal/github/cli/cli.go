// Package cli implements the C6 tracking-lifecycle subcommands of the
// github-poller binary.  See design/09-github-poller.md §"Tracking
// lifecycle" for the verb-by-verb spec.
//
// The split between cmd/github-poller (cobra wiring) and this package
// (logic) keeps the verbs unit-testable without spawning the binary.
package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	ghp "github.com/victor-develop/advanced-tasker/internal/github"
)

// usageError lets us distinguish exit code 1 (usage / missing config)
// from exit code 2 (validation / IO).
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func newUsageError(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// ExitCodeFor maps an error to the conventional shell exit code.
//
// Exit code conventions for the binary (round-3 hardening, see brief D2):
//
//	0  success
//	1  missing config / usage error / doctor hard auth failure
//	2  other failures (IO, validation, doctor soft failure)
//	3  auth/scope rejection at runtime (401 from the poller, 403 SSO/scope)
//
// The 401/403 path runs through ErrAuthExit; doctor explicitly returns a
// doctorExit{} sentinel whose code wins.
func ExitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	// Doctor sentinel wins so we propagate its hard/soft distinction.
	if code := DoctorExitCode(err); code > 0 {
		return code
	}
	if errors.Is(err, ErrAuthExit) {
		return 3
	}
	if errors.Is(err, ghp.ErrConfigMissing) {
		return 1
	}
	var u *usageError
	if errors.As(err, &u) {
		return 1
	}
	return 2
}

// ErrAuthExit is a sentinel returned by run / force-poll when the poller
// hits an actionable 401 or 403 (non-rate-limit).  cmd/github-poller maps
// it to exit code 3.  Callers also print the operator-facing message
// directly to stderr before returning, so main.go shouldn't double-print.
var ErrAuthExit = errors.New("github-poller: authentication or authorization failure")

// requireConfig loads the YAML config tree or fails with the literal
// operator-facing message defined by ErrConfigMissing.  Callers may
// proceed with `--print-only` interactions even when the file is missing
// by checking errors.Is(err, ghp.ErrConfigMissing).
func requireConfig(stateRoot string) (string, error) {
	path := ghp.DefaultConfigPath(stateRoot)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return path, ghp.ErrConfigMissing
		}
		return path, fmt.Errorf("stat %s: %w", path, err)
	}
	return path, nil
}

// mkdirsForCursors is a tiny helper used by track-pr / watch: it makes
// sure the state/sources/github/cursors/{repos,prs} tree exists.  The
// directories themselves are normally created by `harness init` per
// design/10 §"What `harness init` does", but the C6 verbs operate
// stand-alone too, so we belt-and-suspender here.
func mkdirsForCursors(stateRoot string) error {
	for _, sub := range []string{
		filepath.Join("sources", "github", "cursors", "repos"),
		filepath.Join("sources", "github", "cursors", "prs"),
	} {
		if err := os.MkdirAll(filepath.Join(stateRoot, sub), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}
	return nil
}
