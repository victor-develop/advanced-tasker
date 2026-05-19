// Package gitops wraps the small set of git plumbing/porcelain commands
// the harness CLI invokes against the state/ repository. We deliberately
// shell out to the `git` binary (per design/03 §Implementation notes) so
// state's git history remains readable by standard tools.
package gitops

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Repo represents a state git repository rooted at Dir.
type Repo struct {
	Dir string
}

// Init runs `git init -q` inside r.Dir. Idempotent: if .git already
// exists, the command is a no-op for our purposes.
func (r Repo) Init() error {
	out, err := r.run("init", "-q", "--initial-branch=main")
	if err != nil {
		// Older git versions may not support --initial-branch; retry without.
		out2, err2 := r.run("init", "-q")
		if err2 != nil {
			return fmt.Errorf("git init: %w (out: %s / %s)", err, out, out2)
		}
	}
	// Make sure user.name / user.email exist locally so commit works even
	// when global git config is absent (e.g. in CI / fresh containers).
	if _, err := r.run("config", "user.name", "harness"); err != nil {
		return fmt.Errorf("git config user.name: %w", err)
	}
	if _, err := r.run("config", "user.email", "harness@local"); err != nil {
		return fmt.Errorf("git config user.email: %w", err)
	}
	return nil
}

// Add stages the given paths (relative to r.Dir) for the next commit.
// Paths may include "." or "-A" for "stage everything".
func (r Repo) Add(paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"add", "--"}, paths...)
	if _, err := r.run(args...); err != nil {
		return fmt.Errorf("git add %v: %w", paths, err)
	}
	return nil
}

// AddAll stages all changes in the working tree.
func (r Repo) AddAll() error {
	if _, err := r.run("add", "-A"); err != nil {
		return fmt.Errorf("git add -A: %w", err)
	}
	return nil
}

// Commit creates a commit with the given message. Returns the commit SHA.
// If there is nothing to commit, returns ErrNothingToCommit.
func (r Repo) Commit(message string) (string, error) {
	// Check for staged changes before committing so we can give a precise
	// error instead of relying on git's exit code semantics.
	if clean, err := r.IndexClean(); err != nil {
		return "", err
	} else if clean {
		return "", ErrNothingToCommit
	}
	if _, err := r.run("commit", "-q", "-m", message); err != nil {
		return "", fmt.Errorf("git commit: %w", err)
	}
	sha, err := r.run("rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(sha), nil
}

// IndexClean reports whether the index has no staged changes vs HEAD.
// Before the very first commit, HEAD does not resolve; we treat the
// presence of any staged path as "not clean" in that case.
func (r Repo) IndexClean() (bool, error) {
	// Use `git diff --cached --quiet`. Exit 0 = clean, 1 = changes.
	cmd := exec.Command("git", "diff", "--cached", "--quiet")
	cmd.Dir = r.Dir
	err := cmd.Run()
	if err == nil {
		// Still need to handle the no-HEAD case: if HEAD is absent, the
		// diff --cached above returns 0 even when there are staged files
		// (because there's no baseline). Detect this by listing the
		// cached tree.
		if _, headErr := r.run("rev-parse", "--verify", "HEAD"); headErr != nil {
			ls, err := r.run("ls-files", "--cached")
			if err != nil {
				return false, fmt.Errorf("git ls-files --cached: %w", err)
			}
			return strings.TrimSpace(ls) == "", nil
		}
		return true, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git diff --cached: %w", err)
}

// HeadSHA returns the current HEAD commit SHA, or empty string if no
// commits exist yet.
func (r Repo) HeadSHA() (string, error) {
	out, err := r.run("rev-parse", "HEAD")
	if err != nil {
		// Likely no commits yet.
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

// run invokes git with the given args inside r.Dir and returns combined
// stdout/stderr as a string.
func (r Repo) run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), buf.String(), err)
	}
	return buf.String(), nil
}

// ErrNothingToCommit is returned by Commit when the index is clean.
type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

const ErrNothingToCommit = sentinelErr("nothing to commit")
