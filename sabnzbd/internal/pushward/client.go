package pushward

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"time"
)

type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    baseURL,
		apiKey:     apiKey,
	}
}

type CreateActivityRequest struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Priority int    `json:"priority"`
}

type UpdateRequest struct {
	State   string  `json:"state"`
	Content Content `json:"content"`
}

type Content struct {
	Template      string  `json:"template"`
	Progress      float64 `json:"progress"`
	State         string  `json:"state,omitempty"`
	Icon          string  `json:"icon,omitempty"`
	Subtitle      string  `json:"subtitle,omitempty"`
	AccentColor   string  `json:"accent_color,omitempty"`
	RemainingTime *int    `json:"remaining_time,omitempty"`
	CurrentStep   *int    `json:"current_step,omitempty"`
	TotalSteps    *int    `json:"total_steps,omitempty"`
}

func (c *Client) UpdateActivity(ctx context.Context, slug string, req UpdateRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling update: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(attempt-1)), float64(30*time.Second)))
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
		lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return lastErr // Don't retry client errors
		}
	}
	return fmt.Errorf("max retries exceeded: %w", lastErr)
}

func (c *Client) CreateActivity(ctx context.Context, slug, name string, priority int) error {
	body, err := json.Marshal(CreateActivityRequest{
		Slug:     slug,
		Name:     name,
		Priority: priority,
	})
	if err != nil {
		return fmt.Errorf("marshaling create activity: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(attempt-1)), float64(30*time.Second)))
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
		resp.Body.Close()

		if resp.StatusCode == http.StatusConflict {
			return nil // Already exists, OK
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return lastErr
		}
	}
	return fmt.Errorf("max retries exceeded: %w", lastErr)
}

func (c *Client) DeleteActivity(ctx context.Context, slug string) error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(attempt-1)), float64(30*time.Second)))
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
		lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return lastErr
		}
	}
	return fmt.Errorf("max retries exceeded: %w", lastErr)
}
