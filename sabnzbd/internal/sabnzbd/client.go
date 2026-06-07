package sabnzbd

import (
	"context"
	"encoding/json"
	"errors"
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

// redactURLError rewrites the URL inside a wrapped *url.Error so the apikey
// query parameter does not survive into logs (the default *url.Error.Error()
// prints the full URL verbatim). Mutating in place is safe: http.Client.Do
// allocates a fresh *url.Error per request.
func redactURLError(err error, u *url.URL) {
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = redactURL(u)
	}
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
		// The wrapped *url.Error carries the full request URL (incl. the apikey
		// query param); redact it in place so it can't leak into logs.
		redactURLError(err, req.URL)
		return nil, fmt.Errorf("fetching queue: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var qr QueueResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&qr); err != nil {
		return nil, fmt.Errorf("decoding queue: %w", err)
	}
	// Drain so the keep-alive connection can be reused across the 5s polls.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return &qr.Queue, nil
}

// GetHistory fetches a page of history slots. start is the offset (0-based) and
// limit the page size; SABnzbd orders slots most-recently-completed first, so
// callers can paginate (start += limit) until a slot predates their window.
func (c *Client) GetHistory(ctx context.Context, start, limit int) (*History, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	q := req.URL.Query()
	q.Set("apikey", c.apiKey)
	q.Set("output", "json")
	q.Set("mode", "history")
	q.Set("start", strconv.Itoa(start))
	q.Set("limit", strconv.Itoa(limit))
	req.URL.RawQuery = q.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		redactURLError(err, req.URL)
		return nil, fmt.Errorf("fetching history: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var hr HistoryResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&hr); err != nil {
		return nil, fmt.Errorf("decoding history: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return &hr.History, nil
}
