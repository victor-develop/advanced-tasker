package github

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPollerSourceIsReadOnly enforces the round-3 D3 invariant that the
// github-poller binary only issues GET requests against the GitHub API.
// We grep the source tree for the standard write verbs (POST / PUT /
// PATCH / DELETE) and for known go-github mutating method names.
//
// Why this matters: round-3 brief specifies that the poller is read-only
// by design and the lead-driven REAL acceptance is safe to run against
// the user's account.  If a future change introduces a mutating call,
// this test fails before it can land.
//
// Whitelist: only `*_test.go` and `vcr_*.go` (cassette replay infra) are
// allowed to contain these strings.  Everything under cmd/github-poller
// and internal/github (excluding the whitelist) is in scope.
func TestPollerSourceIsReadOnly(t *testing.T) {
	roots, err := repoSourceRoots()
	if err != nil {
		t.Fatal(err)
	}

	// HTTP verb usage patterns: cover both quoted string args to
	// `NewRequest(..., "POST", ...)` and method-field literal
	// `Method: "POST"` on cobra-style request builders.
	verbPattern := regexp.MustCompile(
		`NewRequest\([^,]*,\s*"(POST|PUT|PATCH|DELETE)"|Method:\s*"(POST|PUT|PATCH|DELETE)"`,
	)

	// go-github mutator method names on the resources this binary
	// might touch.  We check for the prefix on the receiver
	// (PullRequests / Issues / Repositories) so unrelated `Create()` /
	// `Delete()` calls (e.g., cursor file delete) don't false-positive.
	ghMutatorPattern := regexp.MustCompile(
		`(PullRequests|Issues|Repositories)\.(Create|Edit|Delete|Merge|UpdateBranch|RequestReviewers|RemoveReviewers|CreateReview|SubmitReview|DismissReview|UpdateReview)\(`,
	)

	var hits []string
	for _, root := range roots {
		walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			name := info.Name()
			if !strings.HasSuffix(name, ".go") {
				return nil
			}
			// Whitelist.
			if strings.HasSuffix(name, "_test.go") {
				return nil
			}
			if strings.HasPrefix(name, "vcr_") {
				return nil
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			for _, line := range strings.Split(string(data), "\n") {
				if verbPattern.MatchString(line) {
					hits = append(hits, path+": "+strings.TrimSpace(line))
				}
				if ghMutatorPattern.MatchString(line) {
					hits = append(hits, path+": "+strings.TrimSpace(line))
				}
			}
			return nil
		})
		if walkErr != nil {
			t.Fatal(walkErr)
		}
	}

	if len(hits) > 0 {
		t.Errorf("github-poller must be read-only; found forbidden write usages:\n  %s",
			strings.Join(hits, "\n  "))
	}
}

// repoSourceRoots returns the source directories the read-only guard
// applies to.  We resolve them relative to this test's working
// directory, walking up to find the module root (a go.mod file).
func repoSourceRoots() ([]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	root := cwd
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			// Fall back to cwd if we couldn't find module root.
			root = cwd
			break
		}
		root = parent
	}
	return []string{
		filepath.Join(root, "cmd", "github-poller"),
		filepath.Join(root, "internal", "github"),
	}, nil
}
