package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
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

	mu        sync.Mutex
	remaining int
	resetAt   time.Time
}

func NewClient(token string) *Client {
	return &Client{
		httpClient: &http.Client{},
		token:      token,
		remaining:  -1, // unknown until first response
	}
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
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
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
			backoff := time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(attempt-1)), float64(30*time.Second)))
			slog.Warn("retrying GitHub API call", "operation", operation, "attempt", attempt+1, "backoff", backoff)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

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
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(rle.retryAfter):
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
	defer resp.Body.Close()

	c.recordRateLimit(resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}
	if resp.StatusCode == 429 {
		retryAfter := 60 * time.Second // default
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil {
				retryAfter = time.Duration(secs) * time.Second
			}
		}
		return nil, &rateLimitError{retryAfter: retryAfter, url: url}
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return nil, &clientError{status: resp.StatusCode, url: url}
	}
	return nil, fmt.Errorf("unexpected status %d for %s", resp.StatusCode, url)
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

func (c *Client) GetInProgressRuns(ctx context.Context, repo string) ([]WorkflowRun, error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid repo format %q, expected owner/repo", repo)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs?status=in_progress&per_page=5", parts[0], parts[1])

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

func (c *Client) GetJobs(ctx context.Context, repo string, runID int64) ([]Job, error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid repo format %q, expected owner/repo", repo)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs/%d/jobs", parts[0], parts[1], runID)

	body, err := c.doWithRetry(ctx, url, "requesting jobs")
	if err != nil {
		return nil, err
	}

	var result JobsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decoding jobs: %w", err)
	}
	return result.Jobs, nil
}

func (c *Client) ListRepos(ctx context.Context, owner string) ([]string, error) {
	var all []string
	page := 1

	for {
		url := fmt.Sprintf("https://api.github.com/user/repos?per_page=100&page=%d&affiliation=owner", page)

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
