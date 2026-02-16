package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxRetries = 3

type Client struct {
	httpClient *http.Client
	token      string
}

func NewClient(token string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		token:      token,
	}
}

func (c *Client) doWithRetry(ctx context.Context, url, operation string) (*http.Response, error) {
	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			backoff := time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(attempt-1)), float64(30*time.Second)))
			slog.Warn("retrying GitHub API call", "operation", operation, "attempt", attempt+1, "backoff", backoff)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		c.setHeaders(req)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", operation, err)
			continue
		}

		c.checkRateLimit(resp)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		resp.Body.Close()
		lastErr = fmt.Errorf("unexpected status %d for %s", resp.StatusCode, url)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, lastErr
		}
	}
	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}

func (c *Client) GetInProgressRuns(ctx context.Context, repo string) ([]WorkflowRun, error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid repo format %q, expected owner/repo", repo)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs?status=in_progress&per_page=5", parts[0], parts[1])

	resp, err := c.doWithRetry(ctx, url, "requesting workflow runs")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result WorkflowRunsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
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

	resp, err := c.doWithRetry(ctx, url, "requesting jobs")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result JobsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding jobs: %w", err)
	}
	return result.Jobs, nil
}

func (c *Client) ListRepos(ctx context.Context, owner string) ([]string, error) {
	var all []string
	page := 1

	for {
		url := fmt.Sprintf("https://api.github.com/users/%s/repos?per_page=100&page=%d&type=owner", owner, page)

		resp, err := c.doWithRetry(ctx, url, "listing repos")
		if err != nil {
			return nil, err
		}

		var repos []Repository
		if err := json.NewDecoder(resp.Body).Decode(&repos); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("decoding repos: %w", err)
		}
		resp.Body.Close()

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

func (c *Client) checkRateLimit(resp *http.Response) {
	remaining := resp.Header.Get("X-RateLimit-Remaining")
	if remaining == "" {
		return
	}
	n, err := strconv.Atoi(remaining)
	if err != nil {
		return
	}
	if n < 100 {
		slog.Warn("GitHub API rate limit low", "remaining", n)
	}
}
