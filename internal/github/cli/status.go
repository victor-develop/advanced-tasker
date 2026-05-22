package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	ghp "github.com/victor-develop/advanced-tasker/internal/github"
)

// StatusReport is the JSON shape of `github-poller status --json`.
type StatusReport struct {
	StateRoot string         `json:"state_root"`
	Repos     []RepoStatus   `json:"repos"`
	OrphanPRs []TrackedPR    `json:"orphan_prs,omitempty"`
}

// RepoStatus is one watched repo + the PRs that have thread dirs.
type RepoStatus struct {
	Repo              string       `json:"repo"`
	LastPRDiscoveryAt time.Time    `json:"last_pr_discovery_at"`
	HasPullsETag      bool         `json:"has_pulls_etag"`
	Tracked           []TrackedPR  `json:"tracked"`
}

// TrackedPR is the cursor view of one tracked PR.
type TrackedPR struct {
	Repo                string    `json:"repo"`
	Number              int       `json:"number"`
	ThreadID            string    `json:"thread_id"`
	LastPolledAt        time.Time `json:"last_polled_at"`
	PRUpdatedAt         time.Time `json:"pr_updated_at"`
	IssueCommentsSince  time.Time `json:"issue_comments_since"`
	ReviewCommentsSince time.Time `json:"review_comments_since"`
	ReviewsSeenCount    int       `json:"reviews_seen_count"`
	ETags               struct {
		IssueComments  bool `json:"issue_comments"`
		ReviewComments bool `json:"review_comments"`
		Pull           bool `json:"pull"`
		Reviews        bool `json:"reviews"`
	} `json:"etags"`
}

// NewStatusCmd implements `github-poller status [--json]`.
func NewStatusCmd(stateRoot *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print tracked repos + tracked PRs with cursors",
		RunE: func(_ *cobra.Command, _ []string) error {
			rep, err := BuildStatus(*stateRoot)
			if err != nil {
				return err
			}
			if asJSON {
				data, err := json.MarshalIndent(rep, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}
			return printStatusText(rep)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON instead of human-readable output")
	return cmd
}

// BuildStatus reads cursors + config from `stateRoot` and assembles a
// StatusReport.  Exposed for unit tests.
func BuildStatus(stateRoot string) (*StatusReport, error) {
	rep := &StatusReport{StateRoot: stateRoot}

	cfgPath := ghp.DefaultConfigPath(stateRoot)
	var watched []string
	if root, err := ghp.LoadConfigRaw(cfgPath); err == nil {
		watched = ghp.WatchRepos(root)
	} else if !errors.Is(err, ghp.ErrConfigMissing) {
		return nil, err
	}

	cursors := ghp.NewCursorStore(stateRoot)
	repoStates := map[string]*RepoStatus{}
	for _, w := range watched {
		r, err := ghp.ParseRepo(w)
		if err != nil {
			continue
		}
		repoCursor, err := cursors.LoadRepo(r)
		if err != nil {
			return nil, err
		}
		rs := &RepoStatus{
			Repo:              r.String(),
			LastPRDiscoveryAt: repoCursor.LastPRDiscoveryAt,
			HasPullsETag:      repoCursor.PullsETag != "",
		}
		repoStates[r.String()] = rs
		rep.Repos = append(rep.Repos, *rs)
	}

	// Sniff all PR cursor files and bucket under their repo.
	prsDir := filepath.Join(stateRoot, "sources", "github", "cursors", "prs")
	entries, err := os.ReadDir(prsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// We need to mutate Repos in-place; rebuild after collecting.
	repoMap := map[string]*RepoStatus{}
	for i := range rep.Repos {
		rs := rep.Repos[i]
		repoMap[rs.Repo] = &rep.Repos[i]
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		stem := strings.TrimSuffix(name, ".json")
		// stem = "<owner>-<repo>-<n>"; last dash is between repo and n.
		dash := strings.LastIndexByte(stem, '-')
		if dash < 1 {
			continue
		}
		numStr := stem[dash+1:]
		num := 0
		for _, c := range numStr {
			if c < '0' || c > '9' {
				num = 0
				break
			}
			num = num*10 + int(c-'0')
		}
		if num == 0 {
			continue
		}
		// Owner-repo = stem[:dash]; we don't know the split between
		// owner and repo (both may contain dashes), so we look up
		// against the watched set.
		ownerRepoDashed := stem[:dash]
		var rRef ghp.RepoRef
		matched := false
		for _, w := range watched {
			r, err := ghp.ParseRepo(w)
			if err != nil {
				continue
			}
			joined := r.Owner + "-" + r.Repo
			if joined == ownerRepoDashed {
				rRef = r
				matched = true
				break
			}
		}
		cur, err := readPRCursorFile(filepath.Join(prsDir, name))
		if err != nil {
			return nil, err
		}
		tpr := TrackedPR{
			Repo:                ownerRepoDashed,
			Number:              num,
			LastPolledAt:        cur.LastPolledAt,
			PRUpdatedAt:         cur.Endpoints.PRUpdatedAt,
			IssueCommentsSince:  cur.Endpoints.IssueCommentsSince,
			ReviewCommentsSince: cur.Endpoints.ReviewCommentsSince,
			ReviewsSeenCount:    len(cur.Endpoints.ReviewsSeenIDs),
		}
		tpr.ETags.IssueComments = cur.Endpoints.IssueCommentsETag != ""
		tpr.ETags.ReviewComments = cur.Endpoints.ReviewCommentsETag != ""
		tpr.ETags.Pull = cur.Endpoints.PullETag != ""
		tpr.ETags.Reviews = cur.Endpoints.ReviewsETag != ""
		if matched {
			tpr.Repo = rRef.String()
			tpr.ThreadID = ghp.ThreadID(rRef, num)
			rs := repoMap[rRef.String()]
			if rs == nil {
				// No watched-set match: bucket as orphan.
				rep.OrphanPRs = append(rep.OrphanPRs, tpr)
			} else {
				rs.Tracked = append(rs.Tracked, tpr)
			}
		} else {
			rep.OrphanPRs = append(rep.OrphanPRs, tpr)
		}
	}
	// Stable order.
	sort.Slice(rep.Repos, func(i, j int) bool { return rep.Repos[i].Repo < rep.Repos[j].Repo })
	for i := range rep.Repos {
		sort.Slice(rep.Repos[i].Tracked, func(a, b int) bool {
			return rep.Repos[i].Tracked[a].Number < rep.Repos[i].Tracked[b].Number
		})
	}
	sort.Slice(rep.OrphanPRs, func(i, j int) bool {
		if rep.OrphanPRs[i].Repo != rep.OrphanPRs[j].Repo {
			return rep.OrphanPRs[i].Repo < rep.OrphanPRs[j].Repo
		}
		return rep.OrphanPRs[i].Number < rep.OrphanPRs[j].Number
	})
	return rep, nil
}

func readPRCursorFile(path string) (*ghp.PRCursor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c ghp.PRCursor
	if len(data) == 0 {
		return &c, nil
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

func printStatusText(rep *StatusReport) error {
	fmt.Printf("state_root: %s\n", rep.StateRoot)
	if len(rep.Repos) == 0 {
		fmt.Println("(no repos watched)")
	}
	for _, r := range rep.Repos {
		fmt.Printf("\nrepo: %s\n", r.Repo)
		if r.LastPRDiscoveryAt.IsZero() {
			fmt.Println("  last_pr_discovery_at: (never)")
		} else {
			fmt.Printf("  last_pr_discovery_at: %s\n", r.LastPRDiscoveryAt.Format(time.RFC3339))
		}
		fmt.Printf("  has_pulls_etag: %v\n", r.HasPullsETag)
		fmt.Printf("  tracked PRs: %d\n", len(r.Tracked))
		for _, t := range r.Tracked {
			fmt.Printf("    #%d (%s): last_polled=%s pr_updated=%s reviews_seen=%d etags=[ic=%v rc=%v pull=%v rev=%v]\n",
				t.Number, t.ThreadID,
				fmtTime(t.LastPolledAt), fmtTime(t.PRUpdatedAt),
				t.ReviewsSeenCount,
				t.ETags.IssueComments, t.ETags.ReviewComments,
				t.ETags.Pull, t.ETags.Reviews)
		}
	}
	if len(rep.OrphanPRs) > 0 {
		fmt.Println("\norphan PR cursors (repo not in watch.repos):")
		for _, t := range rep.OrphanPRs {
			fmt.Printf("  %s#%d last_polled=%s\n", t.Repo, t.Number, fmtTime(t.LastPolledAt))
		}
	}
	return nil
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "(zero)"
	}
	return t.Format(time.RFC3339)
}
