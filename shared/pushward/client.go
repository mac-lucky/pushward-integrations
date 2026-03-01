package pushward

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// parseRetryAfter parses a Retry-After header value as either seconds or HTTP-date.
// Returns 0 if the header is empty or unparseable.
func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// Client is the PushWard API client used by all integrations.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
}

// NewClient creates a new PushWard API client.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    baseURL,
		apiKey:     apiKey,
	}
}

// doWithRetry executes an HTTP request with exponential backoff and jitter.
// It handles 429 rate limiting (Retry-After header), retries on 5xx and network
// errors, and returns immediately on 2xx or non-retryable 4xx errors.
// The handleConflict callback, if non-nil, is invoked on 409 responses; it
// receives the response body and returns (done bool, err error). If done is
// true, doWithRetry returns err immediately.
func (c *Client) doWithRetry(ctx context.Context, method, url string, body interface{}, handleConflict func([]byte) (bool, error)) error {
	var reqBody []byte
	if body != nil {
		var err error
		reqBody, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshaling request body: %w", err)
		}
	}

	var lastErr error
	var retryAfterOverride time.Duration
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			backoff := retryAfterOverride
			if backoff == 0 {
				// Integer-based exponential backoff capped at 30s with equal jitter.
				base := min(time.Second<<(attempt-1), 30*time.Second)
				backoff = base/2 + rand.N(base/2)
			}
			retryAfterOverride = 0
			slog.Warn("retrying PushWard request", "method", method, "url", url, "attempt", attempt+1, "backoff", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		var bodyReader io.Reader
		if reqBody != nil {
			bodyReader = bytes.NewReader(reqBody)
		}

		httpReq, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
		if err != nil {
			return fmt.Errorf("creating request: %w", err)
		}
		if reqBody != nil {
			httpReq.Header.Set("Content-Type", "application/json")
		}
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("sending request: %w", err)
			continue
		}

		if resp.StatusCode == http.StatusConflict && handleConflict != nil {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if done, cerr := handleConflict(respBody); done {
				return cerr
			}
			// If handleConflict says not done, fall through to default handling
			lastErr = fmt.Errorf("conflict (409)")
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfterOverride = parseRetryAfter(resp.Header.Get("Retry-After"))
			slog.Warn("rate limited by PushWard", "url", url, "retry_after", retryAfterOverride)
			lastErr = fmt.Errorf("rate limited (429)")
			continue
		}
		lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return lastErr // Don't retry client errors
		}
	}
	return fmt.Errorf("max retries exceeded: %w", lastErr)
}

// CreateActivity creates a new activity via POST /activities.
// Returns nil on 2xx or 409 "already exists". Returns error on 409 "limit".
func (c *Client) CreateActivity(ctx context.Context, slug, name string, priority, endedTTL, staleTTL int) error {
	return c.doWithRetry(ctx, http.MethodPost, fmt.Sprintf("%s/activities", c.baseURL),
		CreateActivityRequest{
			Slug:     slug,
			Name:     name,
			Priority: priority,
			EndedTTL: endedTTL,
			StaleTTL: staleTTL,
		},
		func(body []byte) (bool, error) {
			if bytes.Contains(body, []byte("limit")) {
				return true, fmt.Errorf("activity limit reached")
			}
			return true, nil // Already exists, OK
		},
	)
}

// UpdateActivity updates an activity via PATCH /activity/{slug}.
func (c *Client) UpdateActivity(ctx context.Context, slug string, req UpdateRequest) error {
	return c.doWithRetry(ctx, http.MethodPatch, fmt.Sprintf("%s/activity/%s", c.baseURL, slug), req, nil)
}
