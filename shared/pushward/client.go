package pushward

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
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

// CreateActivity creates a new activity via POST /activities.
// Returns nil on 2xx or 409 "already exists". Returns error on 409 "limit".
func (c *Client) CreateActivity(ctx context.Context, slug, name string, priority, endedTTL, staleTTL int) error {
	body, err := json.Marshal(CreateActivityRequest{
		Slug:     slug,
		Name:     name,
		Priority: priority,
		EndedTTL: endedTTL,
		StaleTTL: staleTTL,
	})
	if err != nil {
		return fmt.Errorf("marshaling create activity: %w", err)
	}

	var lastErr error
	var retryAfterOverride time.Duration
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			backoff := retryAfterOverride
			if backoff == 0 {
				backoff = time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(attempt-1)), float64(30*time.Second)))
			}
			retryAfterOverride = 0
			slog.Warn("retrying PushWard create activity", "attempt", attempt+1, "backoff", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			fmt.Sprintf("%s/activities", c.baseURL), bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("creating request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("sending create activity: %w", err)
			continue
		}

		if resp.StatusCode == http.StatusConflict {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(body), "limit") {
				return fmt.Errorf("activity limit reached")
			}
			return nil // Already exists, OK
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfterOverride = parseRetryAfter(resp.Header.Get("Retry-After"))
			slog.Warn("rate limited by PushWard", "slug", slug, "retry_after", retryAfterOverride)
			lastErr = fmt.Errorf("rate limited (429)")
			continue
		}
		lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return lastErr
		}
	}
	return fmt.Errorf("max retries exceeded: %w", lastErr)
}

// UpdateActivity updates an activity via PATCH /activity/{slug}.
func (c *Client) UpdateActivity(ctx context.Context, slug string, req UpdateRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling update: %w", err)
	}

	var lastErr error
	var retryAfterOverride time.Duration
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			backoff := retryAfterOverride
			if backoff == 0 {
				backoff = time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(attempt-1)), float64(30*time.Second)))
			}
			retryAfterOverride = 0
			slog.Warn("retrying PushWard update", "attempt", attempt+1, "backoff", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPatch,
			fmt.Sprintf("%s/activity/%s", c.baseURL, slug), bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("creating request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("sending update: %w", err)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfterOverride = parseRetryAfter(resp.Header.Get("Retry-After"))
			slog.Warn("rate limited by PushWard", "slug", slug, "retry_after", retryAfterOverride)
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

// DeleteActivity deletes an activity via DELETE /activities/{slug}.
func (c *Client) DeleteActivity(ctx context.Context, slug string) error {
	var lastErr error
	var retryAfterOverride time.Duration
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			backoff := retryAfterOverride
			if backoff == 0 {
				backoff = time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(attempt-1)), float64(30*time.Second)))
			}
			retryAfterOverride = 0
			slog.Warn("retrying PushWard delete activity", "attempt", attempt+1, "backoff", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete,
			fmt.Sprintf("%s/activities/%s", c.baseURL, slug), nil)
		if err != nil {
			return fmt.Errorf("creating request: %w", err)
		}
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("sending delete activity: %w", err)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			return nil // Already gone, OK
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfterOverride = parseRetryAfter(resp.Header.Get("Retry-After"))
			slog.Warn("rate limited by PushWard", "slug", slug, "retry_after", retryAfterOverride)
			lastErr = fmt.Errorf("rate limited (429)")
			continue
		}
		lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return lastErr
		}
	}
	return fmt.Errorf("max retries exceeded: %w", lastErr)
}
