package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxRetries = 3

const maxRateLimitRetries = 3

const requestTimeout = 10 * time.Second

// rateLimitBuffer is the threshold below which we proactively wait for the
// rate limit window to reset before making further requests.
const rateLimitBuffer = 50

type Client struct {
	httpClient *http.Client
	token      string
	baseURL    string // defaults to https://api.github.com

	mu        sync.Mutex
	remaining int
	resetAt   time.Time
	login     string // cached login of the token's user (lazy)
}

func NewClient(token string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		token:      token,
		baseURL:    "https://api.github.com",
		remaining:  -1, // unknown until first response
	}
}

// SetBaseURL overrides the GitHub API base URL (for testing).
func (c *Client) SetBaseURL(url string) {
	c.baseURL = url
}

// waitForRateLimit blocks until the rate limit window resets if remaining
// requests are below the safety buffer.
func (c *Client) waitForRateLimit(ctx context.Context) error {
	c.mu.Lock()
	remaining := c.remaining
	resetAt := c.resetAt
	c.mu.Unlock()

	if remaining >= 0 && remaining <= rateLimitBuffer && time.Now().Before(resetAt) {
		wait := time.Until(resetAt)
		slog.Warn("rate limit low, waiting for reset", "remaining", remaining, "wait", wait.Round(time.Second))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func (c *Client) doWithRetry(ctx context.Context, url, operation string) ([]byte, error) {
	var lastErr error
	rateLimitRetries := 0

	for attempt := 0; attempt < maxRetries; attempt++ {
		if err := c.waitForRateLimit(ctx); err != nil {
			return nil, err
		}

		if attempt > 0 {
			backoff := min(time.Second<<(attempt-1), 30*time.Second)
			slog.Warn("retrying GitHub API call", "operation", operation, "attempt", attempt+1, "backoff", backoff)
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}

		// context.WithTimeout clamps to the parent deadline if it is earlier,
		// so retries cannot collectively exceed the caller's budget.
		reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		body, err := c.doRequest(reqCtx, url)
		cancel()
		if err != nil {
			// Handle rate limit (429) — wait and retry without consuming a normal retry slot.
			var rle *rateLimitError
			if errors.As(err, &rle) {
				rateLimitRetries++
				if rateLimitRetries > maxRateLimitRetries {
					return nil, fmt.Errorf("%s: rate limit retries exceeded: %w", operation, err)
				}
				slog.Warn("rate limited by GitHub, waiting", "operation", operation, "retry_after", rle.retryAfter)
				rateLimitTimer := time.NewTimer(rle.retryAfter)
				select {
				case <-ctx.Done():
					rateLimitTimer.Stop()
					return nil, ctx.Err()
				case <-rateLimitTimer.C:
				}
				attempt-- // don't consume a normal retry slot
				continue
			}

			// Don't retry client errors (4xx).
			var ce *clientError
			if errors.As(err, &ce) {
				return nil, fmt.Errorf("%s: %w", operation, err)
			}
			lastErr = fmt.Errorf("%s: %w", operation, err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		return body, nil
	}
	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

// doRequest executes a single HTTP request and returns the response body.
// Non-retryable client errors (4xx) are returned wrapped in clientError.
// Rate limit errors (429) are returned wrapped in rateLimitError.
func (c *Client) doRequest(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	c.recordRateLimit(resp)

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}
	// GitHub signals rate limiting as 429 OR as 403 carrying rate-limit headers
	// (primary limit: X-RateLimit-Remaining: 0; secondary/abuse limit:
	// Retry-After). Treat both as retryable so the poller backs off instead of
	// hammering and re-tripping the limit.
	if resp.StatusCode == 429 ||
		(resp.StatusCode == 403 && (resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0")) {
		return nil, &rateLimitError{retryAfter: rateLimitRetryAfter(resp), url: url}
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return nil, &clientError{status: resp.StatusCode, url: url}
	}
	return nil, fmt.Errorf("unexpected status %d for %s", resp.StatusCode, url)
}

// rateLimitRetryAfter derives how long to wait before retrying a rate-limited
// GitHub response. It prefers Retry-After (delta-seconds or HTTP-date per RFC
// 7231), falls back to the X-RateLimit-Reset epoch, and finally a 60s default.
// The wait is clamped so a hostile or skewed value can't park the poller.
func rateLimitRetryAfter(resp *http.Response) time.Duration {
	const (
		defaultWait = 60 * time.Second
		maxWait     = 15 * time.Minute
	)
	// clamp bounds a *successfully parsed* signal to [0, maxWait]. A non-positive
	// value is authoritative, not garbage: "Retry-After: 0" means retry now, and
	// an X-RateLimit-Reset / HTTP-date already in the past (clock skew or the
	// window already reset) means the limit is open again — both map to an
	// immediate retry, NOT the 60s default. defaultWait applies only when no
	// header parses at all.
	clamp := func(d time.Duration) time.Duration {
		if d < 0 {
			return 0
		}
		if d > maxWait {
			return maxWait
		}
		return d
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return clamp(time.Duration(secs) * time.Second)
		}
		if t, err := http.ParseTime(v); err == nil {
			return clamp(time.Until(t))
		}
	}
	if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
		if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil {
			return clamp(time.Until(time.Unix(epoch, 0)))
		}
	}
	return defaultWait
}

type clientError struct {
	status int
	url    string
}

func (e *clientError) Error() string {
	return fmt.Sprintf("client error %d for %s", e.status, e.url)
}

type rateLimitError struct {
	retryAfter time.Duration
	url        string
}

func (e *rateLimitError) Error() string {
	return fmt.Sprintf("rate limited for %s (retry after %s)", e.url, e.retryAfter)
}

// splitRepo parses an "owner/repo" string into its two halves, returning an
// error for any other shape. Centralizes the validation shared by the run/job
// endpoints.
func splitRepo(repo string) (owner, name string, err error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid repo format %q, expected owner/repo", repo)
	}
	return parts[0], parts[1], nil
}

func (c *Client) GetInProgressRuns(ctx context.Context, repo string) ([]WorkflowRun, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}

	// per_page=50 caps memory while covering concurrent workflows on busy repos.
	// The poller selects only the most recent run, so ordering is stable.
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs?status=in_progress&per_page=50", c.baseURL, owner, name)

	body, err := c.doWithRetry(ctx, url, "requesting workflow runs")
	if err != nil {
		return nil, err
	}

	var result WorkflowRunsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decoding workflow runs: %w", err)
	}
	return result.WorkflowRuns, nil
}

// GetRun fetches a single workflow run so callers can consult the run's own
// authoritative Status/Conclusion rather than inferring completion from the
// (lazily-created) job list.
func (c *Client) GetRun(ctx context.Context, repo string, runID int64) (*WorkflowRun, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d", c.baseURL, owner, name, runID)
	body, err := c.doWithRetry(ctx, url, "requesting workflow run")
	if err != nil {
		return nil, err
	}

	var run WorkflowRun
	if err := json.Unmarshal(body, &run); err != nil {
		return nil, fmt.Errorf("decoding workflow run: %w", err)
	}
	return &run, nil
}

func (c *Client) GetJobs(ctx context.Context, repo string, runID int64) ([]Job, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}

	var all []Job
	page := 1

	for {
		url := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs?per_page=100&page=%d", c.baseURL, owner, name, runID, page)

		body, err := c.doWithRetry(ctx, url, "requesting jobs")
		if err != nil {
			return nil, err
		}

		var result JobsResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("decoding jobs: %w", err)
		}

		all = append(all, result.Jobs...)

		if len(all) >= result.TotalCount || len(result.Jobs) < 100 {
			break
		}
		page++
	}

	return all, nil
}

// authenticatedLogin returns the login of the token's user, cached after the
// first successful lookup.
func (c *Client) authenticatedLogin(ctx context.Context) (string, error) {
	c.mu.Lock()
	login := c.login
	c.mu.Unlock()
	if login != "" {
		return login, nil
	}

	body, err := c.doWithRetry(ctx, c.baseURL+"/user", "getting authenticated user")
	if err != nil {
		return "", err
	}
	var u User
	if err := json.Unmarshal(body, &u); err != nil {
		return "", fmt.Errorf("decoding authenticated user: %w", err)
	}
	c.mu.Lock()
	c.login = u.Login
	c.mu.Unlock()
	return u.Login, nil
}

// ListRepos discovers repositories for owner, honoring it correctly:
//   - the token's own account → GET /user/repos?affiliation=owner (includes
//     private repos the user owns);
//   - any other owner → GET /orgs/{owner}/repos (org repos the token can see),
//     falling back to GET /users/{owner}/repos for personal accounts (public).
//
// Archived and disabled repos are filtered out.
func (c *Client) ListRepos(ctx context.Context, owner string) ([]string, error) {
	login, err := c.authenticatedLogin(ctx)
	if err != nil {
		return nil, err
	}

	if owner == "" || strings.EqualFold(owner, login) {
		return c.listReposPaged(ctx, c.baseURL+"/user/repos?affiliation=owner&per_page=100")
	}

	orgURL := fmt.Sprintf("%s/orgs/%s/repos?per_page=100", c.baseURL, url.PathEscape(owner))
	repos, err := c.listReposPaged(ctx, orgURL)
	if err != nil {
		var ce *clientError
		if errors.As(err, &ce) && ce.status == http.StatusNotFound {
			// Not an org — treat owner as a personal account (public repos).
			userURL := fmt.Sprintf("%s/users/%s/repos?per_page=100", c.baseURL, url.PathEscape(owner))
			return c.listReposPaged(ctx, userURL)
		}
		return nil, err
	}
	return repos, nil
}

// listReposPaged pages through a repos endpoint. baseURL must already carry a
// query string (so "&page=N" is appended).
func (c *Client) listReposPaged(ctx context.Context, baseURL string) ([]string, error) {
	var all []string
	page := 1

	for {
		url := fmt.Sprintf("%s&page=%d", baseURL, page)

		body, err := c.doWithRetry(ctx, url, "listing repos")
		if err != nil {
			return nil, err
		}

		var repos []Repository
		if err := json.Unmarshal(body, &repos); err != nil {
			return nil, fmt.Errorf("decoding repos: %w", err)
		}

		if len(repos) == 0 {
			break
		}

		for _, r := range repos {
			if !r.Archived && !r.Disabled {
				all = append(all, r.FullName)
			}
		}

		if len(repos) < 100 {
			break
		}
		page++
	}

	return all, nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

// recordRateLimit stores the rate limit state from response headers for
// proactive backoff in subsequent requests.
func (c *Client) recordRateLimit(resp *http.Response) {
	remaining := resp.Header.Get("X-RateLimit-Remaining")
	if remaining == "" {
		return
	}
	n, err := strconv.Atoi(remaining)
	if err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.remaining = n

	if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
		if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil {
			c.resetAt = time.Unix(epoch, 0)
		}
	}
}
