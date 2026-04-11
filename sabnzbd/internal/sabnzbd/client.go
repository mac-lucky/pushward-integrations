package sabnzbd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
}

// redactURL returns the URL string with the apikey query parameter value
// replaced with [REDACTED] to prevent leaking credentials in logs.
func redactURL(u *url.URL) string {
	q := u.Query()
	if q.Has("apikey") {
		q.Set("apikey", "[REDACTED]")
	}
	u2 := *u
	u2.RawQuery = q.Encode()
	return u2.String()
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 5 * time.Second},
		baseURL:    baseURL,
		apiKey:     apiKey,
	}
}

func (c *Client) GetQueue(ctx context.Context) (*Queue, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	// SABnzbd only supports API key auth via the "apikey" query parameter;
	// there is no header-based auth option.
	q := req.URL.Query()
	q.Set("apikey", c.apiKey)
	q.Set("output", "json")
	q.Set("mode", "queue")
	req.URL.RawQuery = q.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching queue from %s: %w", redactURL(req.URL), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var qr QueueResponse
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		return nil, fmt.Errorf("decoding queue: %w", err)
	}
	return &qr.Queue, nil
}

func (c *Client) GetHistory(ctx context.Context, limit int) (*History, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	q := req.URL.Query()
	q.Set("apikey", c.apiKey)
	q.Set("output", "json")
	q.Set("mode", "history")
	q.Set("limit", strconv.Itoa(limit))
	req.URL.RawQuery = q.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching history from %s: %w", redactURL(req.URL), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var hr HistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		return nil, fmt.Errorf("decoding history: %w", err)
	}
	return &hr.History, nil
}
