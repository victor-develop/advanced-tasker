package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	gh "github.com/google/go-github/v75/github"
)

// Client is a thin wrapper around go-github that adds:
//   - ETag-based conditional requests via If-None-Match
//   - 304-aware result types (NotModified is set)
//   - Centralized rate-limit / 404 detection
//
// The wrapper intentionally keeps the go-github types in the public signatures
// so the writer layer can pass them straight through to JSON without further
// translation.
type Client struct {
	gh   *gh.Client
	http *http.Client
}

// NewClient builds a client bound to the given GitHub PAT.  If baseURL is
// non-empty the client is repointed at it (used by the test cassettes).
// httpClient is optional; if nil the default is used (and the auth token is
// applied via WithAuthToken).
func NewClient(token, baseURL string, httpClient *http.Client) (*Client, error) {
	hc := httpClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	c := gh.NewClient(hc)
	if token != "" {
		c = c.WithAuthToken(token)
	}
	if baseURL != "" {
		// Use a direct BaseURL override rather than WithEnterpriseURLs,
		// which silently appends "/api/v3/" for non-api.github.com hosts
		// and breaks httptest / VCR fixtures.
		if !strings.HasSuffix(baseURL, "/") {
			baseURL += "/"
		}
		u, err := url.Parse(baseURL)
		if err != nil {
			return nil, fmt.Errorf("parse base URL %q: %w", baseURL, err)
		}
		c.BaseURL = u
		c.UploadURL = u
	}
	return &Client{gh: c, http: hc}, nil
}

// PRListPage is one page of repo-level PR discovery.
type PRListPage struct {
	PRs         []*gh.PullRequest
	NextPage    int
	ETag        string
	NotModified bool
}

// ListOpenPRs returns one page of open PRs sorted by updated desc.  If
// ifNoneMatch is non-empty an If-None-Match header is sent and a 304 yields
// NotModified=true with PRs=nil.
func (c *Client) ListOpenPRs(ctx context.Context, r RepoRef, ifNoneMatch string, page int) (*PRListPage, error) {
	opts := &gh.PullRequestListOptions{
		State:     "open",
		Sort:      "updated",
		Direction: "desc",
		ListOptions: gh.ListOptions{
			Page:    page,
			PerPage: 50,
		},
	}
	// go-github does not directly expose If-None-Match on its high-level
	// methods, so we fall back to a raw request when the caller supplies an
	// ETag.  Otherwise use the typed method for ergonomic decoding.
	if ifNoneMatch == "" {
		prs, resp, err := c.gh.PullRequests.List(ctx, r.Owner, r.Repo, opts)
		if err != nil {
			return nil, classifyError(err)
		}
		return &PRListPage{
			PRs:      prs,
			NextPage: resp.NextPage,
			ETag:     resp.Header.Get("ETag"),
		}, nil
	}
	u := fmt.Sprintf("repos/%s/%s/pulls?state=open&sort=updated&direction=desc&per_page=50&page=%d",
		r.Owner, r.Repo, page)
	req, err := c.gh.NewRequest("GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("If-None-Match", ifNoneMatch)
	var prs []*gh.PullRequest
	resp, err := c.gh.Do(ctx, req, &prs)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotModified {
			return &PRListPage{NotModified: true, ETag: ifNoneMatch}, nil
		}
		return nil, classifyError(err)
	}
	if resp != nil && resp.StatusCode == http.StatusNotModified {
		return &PRListPage{NotModified: true, ETag: ifNoneMatch}, nil
	}
	return &PRListPage{
		PRs:      prs,
		NextPage: resp.NextPage,
		ETag:     resp.Header.Get("ETag"),
	}, nil
}

// IssueCommentsPage is one page of issue comments.
type IssueCommentsPage struct {
	Comments    []*gh.IssueComment
	NextPage    int
	ETag        string
	NotModified bool
}

// ListIssueComments fetches issue comments since the given time (UTC).
func (c *Client) ListIssueComments(ctx context.Context, r RepoRef, number int, since time.Time, ifNoneMatch string, page int) (*IssueCommentsPage, error) {
	q := fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=50&page=%d", r.Owner, r.Repo, number, page)
	if !since.IsZero() {
		q += "&since=" + since.UTC().Format(time.RFC3339)
	}
	req, err := c.gh.NewRequest("GET", q, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	var comments []*gh.IssueComment
	resp, err := c.gh.Do(ctx, req, &comments)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotModified {
			return &IssueCommentsPage{NotModified: true, ETag: ifNoneMatch}, nil
		}
		return nil, classifyError(err)
	}
	if resp != nil && resp.StatusCode == http.StatusNotModified {
		return &IssueCommentsPage{NotModified: true, ETag: ifNoneMatch}, nil
	}
	return &IssueCommentsPage{
		Comments: comments,
		NextPage: resp.NextPage,
		ETag:     resp.Header.Get("ETag"),
	}, nil
}

// ReviewCommentsPage is one page of PR review (inline) comments.
type ReviewCommentsPage struct {
	Comments    []*gh.PullRequestComment
	NextPage    int
	ETag        string
	NotModified bool
}

// ListReviewComments fetches inline review comments since the given time.
func (c *Client) ListReviewComments(ctx context.Context, r RepoRef, number int, since time.Time, ifNoneMatch string, page int) (*ReviewCommentsPage, error) {
	q := fmt.Sprintf("repos/%s/%s/pulls/%d/comments?per_page=50&page=%d", r.Owner, r.Repo, number, page)
	if !since.IsZero() {
		q += "&since=" + since.UTC().Format(time.RFC3339)
	}
	req, err := c.gh.NewRequest("GET", q, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	var comments []*gh.PullRequestComment
	resp, err := c.gh.Do(ctx, req, &comments)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotModified {
			return &ReviewCommentsPage{NotModified: true, ETag: ifNoneMatch}, nil
		}
		return nil, classifyError(err)
	}
	if resp != nil && resp.StatusCode == http.StatusNotModified {
		return &ReviewCommentsPage{NotModified: true, ETag: ifNoneMatch}, nil
	}
	return &ReviewCommentsPage{
		Comments: comments,
		NextPage: resp.NextPage,
		ETag:     resp.Header.Get("ETag"),
	}, nil
}

// PullPayload is one PR metadata response.
type PullPayload struct {
	Pull        *gh.PullRequest
	ETag        string
	NotModified bool
}

// GetPull returns the PR metadata using If-None-Match when available.
func (c *Client) GetPull(ctx context.Context, r RepoRef, number int, ifNoneMatch string) (*PullPayload, error) {
	q := fmt.Sprintf("repos/%s/%s/pulls/%d", r.Owner, r.Repo, number)
	req, err := c.gh.NewRequest("GET", q, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	var pull gh.PullRequest
	resp, err := c.gh.Do(ctx, req, &pull)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotModified {
			return &PullPayload{NotModified: true, ETag: ifNoneMatch}, nil
		}
		return nil, classifyError(err)
	}
	if resp != nil && resp.StatusCode == http.StatusNotModified {
		return &PullPayload{NotModified: true, ETag: ifNoneMatch}, nil
	}
	return &PullPayload{
		Pull: &pull,
		ETag: resp.Header.Get("ETag"),
	}, nil
}

// ReviewsPage is one page of reviews.
type ReviewsPage struct {
	Reviews     []*gh.PullRequestReview
	NextPage    int
	ETag        string
	NotModified bool
}

// ListReviews fetches reviews (no since support — dedup by ID upstream).
func (c *Client) ListReviews(ctx context.Context, r RepoRef, number int, ifNoneMatch string, page int) (*ReviewsPage, error) {
	q := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews?per_page=50&page=%d", r.Owner, r.Repo, number, page)
	req, err := c.gh.NewRequest("GET", q, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	var reviews []*gh.PullRequestReview
	resp, err := c.gh.Do(ctx, req, &reviews)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotModified {
			return &ReviewsPage{NotModified: true, ETag: ifNoneMatch}, nil
		}
		return nil, classifyError(err)
	}
	if resp != nil && resp.StatusCode == http.StatusNotModified {
		return &ReviewsPage{NotModified: true, ETag: ifNoneMatch}, nil
	}
	return &ReviewsPage{
		Reviews:  reviews,
		NextPage: resp.NextPage,
		ETag:     resp.Header.Get("ETag"),
	}, nil
}

// --- Error classification --------------------------------------------------

// IsNotFound reports whether err corresponds to a 404 response.
func IsNotFound(err error) bool {
	var er *gh.ErrorResponse
	if errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusNotFound {
		return true
	}
	return false
}

// IsRateLimit reports whether err corresponds to primary or secondary rate
// limiting.  Returns the suggested retry delay if available.
func IsRateLimit(err error) (bool, time.Duration) {
	var rle *gh.RateLimitError
	if errors.As(err, &rle) {
		if !rle.Rate.Reset.IsZero() {
			return true, time.Until(rle.Rate.Reset.Time)
		}
		return true, 0
	}
	var arle *gh.AbuseRateLimitError
	if errors.As(err, &arle) {
		return true, arle.GetRetryAfter()
	}
	return false, 0
}

// IsUnauthorized reports a 401.
func IsUnauthorized(err error) bool {
	var er *gh.ErrorResponse
	if errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusUnauthorized {
		return true
	}
	return false
}

// classifyError is a passthrough that exists so we can extend it later
// (e.g., wrap with our own sentinel types).  Currently it returns err as-is;
// callers use the Is* helpers above.
func classifyError(err error) error { return err }
