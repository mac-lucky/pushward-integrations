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

// ResultInfo is passed to the onResult callback after each API call completes.
type ResultInfo struct {
	Operation string // "create" or "update"
	Attempts  int    // 1 = no retries
	Err       error
	Duration  time.Duration
}

// Client is the PushWard API client used by all integrations.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	onResult   func(context.Context, ResultInfo)
	breaker    *CircuitBreaker
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithHTTPClient sets a custom HTTP client (e.g. with an instrumented transport).
func WithHTTPClient(c *http.Client) ClientOption {
	return func(cl *Client) { cl.httpClient = c }
}

// WithOnResult registers a callback invoked after each API call completes.
func WithOnResult(fn func(context.Context, ResultInfo)) ClientOption {
	return func(cl *Client) { cl.onResult = fn }
}

// WithCircuitBreaker attaches a circuit breaker to the client.
func WithCircuitBreaker(cb *CircuitBreaker) ClientOption {
	return func(cl *Client) { cl.breaker = cb }
}

// NewClient creates a new PushWard API client.
func NewClient(baseURL, apiKey string, opts ...ClientOption) *Client {
	c := &Client{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    baseURL,
		apiKey:     apiKey,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// doWithRetry executes an HTTP request with exponential backoff and jitter.
// It handles 429 rate limiting (Retry-After header), retries on 5xx and network
// errors, and returns immediately on 2xx or non-retryable 4xx errors.
// The handleConflict callback, if non-nil, is invoked on 409 responses; it
// receives the response body and returns (done bool, err error). If done is
// true, doWithRetry returns err immediately.
func (c *Client) doWithRetry(ctx context.Context, operation, method, url string, body interface{}, handleConflict func([]byte) (bool, error)) error {
	if c.breaker != nil && !c.breaker.Allow() {
		err := ErrCircuitOpen
		if c.onResult != nil {
			c.onResult(ctx, ResultInfo{Operation: operation, Attempts: 0, Err: err})
		}
		return err
	}

	start := time.Now()

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
	attempts := 0
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			backoff := retryAfterOverride
			if backoff == 0 {
				// Integer-based exponential backoff capped at 30s with equal jitter.
				base := min(time.Second<<(attempt-1), 30*time.Second)
				backoff = base/2 + rand.N(base/2) // #nosec G404 -- jitter for retry backoff, not security-sensitive
			}
			retryAfterOverride = 0
			slog.Warn("retrying PushWard request", "method", method, "url", url, "attempt", attempt+1, "backoff", backoff)
			retryTimer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				retryTimer.Stop()
				return ctx.Err()
			case <-retryTimer.C:
			}
		}

		attempts++

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
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if done, cerr := handleConflict(respBody); done {
				c.recordResult(ctx, operation, attempts, start, cerr, false)
				return cerr
			}
			// If handleConflict says not done, fall through to default handling
			lastErr = fmt.Errorf("conflict (409)")
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			c.recordResult(ctx, operation, attempts, start, nil, false)
			return nil
		}

		// Read body for error diagnostics. Problem bodies routinely exceed
		// a few hundred bytes (type URL + detail + errors array), so cap at
		// 64 KiB — enough for any realistic Problem, still bounded.
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()

		problem := parseProblem(respBody)

		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfterOverride = parseRetryAfter(resp.Header.Get("Retry-After"))
			if retryAfterOverride == 0 && problem.RetryAfterMs > 0 {
				retryAfterOverride = time.Duration(problem.RetryAfterMs) * time.Millisecond
			}
			slog.Warn("rate limited by PushWard", "url", url, "retry_after", retryAfterOverride, "code", problem.Code)
			lastErr = fmt.Errorf("rate limited (429)")
			continue
		}
		lastErr = newHTTPError(resp.StatusCode, problem)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			slog.Warn("PushWard client error",
				"status", resp.StatusCode,
				"url", url,
				"code", problem.Code,
				"detail", problem.Detail,
			)
			// 4xx client errors are not retryable and don't trip the breaker.
			c.recordResult(ctx, operation, attempts, start, lastErr, false)
			return lastErr
		}
	}
	err := fmt.Errorf("max retries exceeded: %w", lastErr)
	c.recordResult(ctx, operation, attempts, start, err, true)
	return err
}

// Stable programmatic error codes emitted by pushward-server on the Problem
// body. Callers branch on HTTPError.Code instead of the human-readable Detail.
const (
	ErrCodeActivityLimitExceeded = "activity.limit_exceeded"
)

// problem is the parsed RFC 9457 error body. It is an internal parsing
// helper; callers inspect errors via *HTTPError, which carries the same
// fields.
type problem struct {
	Type         string `json:"type,omitempty"`
	Title        string `json:"title,omitempty"`
	Status       int    `json:"status,omitempty"`
	Detail       string `json:"detail,omitempty"`
	Code         string `json:"code,omitempty"`
	RetryAfterMs int64  `json:"retry_after_ms,omitempty"`
}

// parseProblem decodes an RFC 9457 Problem Details body, tolerating legacy
// {"error":"..."} shapes by promoting `error` into Detail. Returns a zero
// problem if the body is empty or not JSON. The legacy fallback is a
// rollout-safety net; remove it once all deployed servers emit Problem
// bodies.
func parseProblem(body []byte) problem {
	var p problem
	if len(body) == 0 {
		return p
	}
	_ = json.Unmarshal(body, &p)
	if p.Detail == "" && p.Title == "" && p.Code == "" {
		var legacy struct {
			Error        string `json:"error"`
			RetryAfterMs int64  `json:"retry_after_ms"`
		}
		if err := json.Unmarshal(body, &legacy); err == nil && legacy.Error != "" {
			p.Detail = legacy.Error
			if p.RetryAfterMs == 0 {
				p.RetryAfterMs = legacy.RetryAfterMs
			}
		}
	}
	return p
}

// HTTPError is returned when PushWard responds with a non-2xx, non-retryable
// status that is not handled by handleConflict. Callers can use errors.As to
// inspect the status code and Problem Details fields (e.g. to surface 401/403
// upstream instead of masking as a 502, or to branch on the stable `Code`
// identifier).
type HTTPError struct {
	StatusCode   int
	Type         string // Problem.type (typically "about:blank")
	Title        string // Problem.title
	Detail       string // Problem.detail
	Code         string // Problem.code — stable programmatic identifier
	RetryAfterMs int64  // Problem.retry_after_ms (populated on 409/429)
}

func newHTTPError(status int, p problem) *HTTPError {
	return &HTTPError{
		StatusCode:   status,
		Type:         p.Type,
		Title:        p.Title,
		Detail:       p.Detail,
		Code:         p.Code,
		RetryAfterMs: p.RetryAfterMs,
	}
}

func (e *HTTPError) Error() string {
	switch {
	case e.Detail != "":
		return fmt.Sprintf("status %d: %s", e.StatusCode, e.Detail)
	case e.Title != "":
		return fmt.Sprintf("status %d: %s", e.StatusCode, e.Title)
	default:
		return fmt.Sprintf("unexpected status %d", e.StatusCode)
	}
}

var _ error = (*HTTPError)(nil)

// recordResult records the breaker outcome and fires the onResult callback.
// Only retryable failures (5xx/network exhaustion) trip the breaker; 4xx client
// errors, conflict resolutions, and circuit-open short-circuits do not.
func (c *Client) recordResult(ctx context.Context, operation string, attempts int, start time.Time, err error, retryable bool) {
	if c.breaker != nil {
		if err == nil {
			c.breaker.RecordSuccess()
		} else if retryable {
			c.breaker.RecordFailure()
		}
	}
	if c.onResult != nil {
		c.onResult(ctx, ResultInfo{
			Operation: operation,
			Attempts:  attempts,
			Err:       err,
			Duration:  time.Since(start),
		})
	}
}

// CreateActivity creates (or refreshes) an activity via POST /activities.
// The server upserts and always returns 201 with an X-Resource-Action header
// distinguishing created vs. updated, so duplicate slugs are no longer a 409.
// A 409 now signals only activity.limit_exceeded — surfaced as a typed error.
func (c *Client) CreateActivity(ctx context.Context, slug, name string, priority, endedTTL, staleTTL int) error {
	return c.doWithRetry(ctx, "create", http.MethodPost, fmt.Sprintf("%s/activities", c.baseURL),
		CreateActivityRequest{
			Slug:     slug,
			Name:     name,
			Priority: priority,
			EndedTTL: endedTTL,
			StaleTTL: staleTTL,
		},
		func(body []byte) (bool, error) {
			p := parseProblem(body)
			// bytes.Contains is a rollout-safety net for servers that have
			// not yet migrated to RFC 9457 Problem bodies; remove once the
			// Code path is universally available.
			if p.Code == ErrCodeActivityLimitExceeded || bytes.Contains(body, []byte("limit")) {
				return true, fmt.Errorf("activity limit reached")
			}
			return true, newHTTPError(http.StatusConflict, p)
		},
	)
}

// UpdateActivity sends a typed UpdateRequest via PATCH /activities/{slug}. Use
// it for the seed (establishes template/icon/accent) and the final ENDED
// frame; use PatchActivity for mid-sequence ticks.
func (c *Client) UpdateActivity(ctx context.Context, slug string, req UpdateRequest) error {
	return c.doWithRetry(ctx, "update", http.MethodPatch, fmt.Sprintf("%s/activities/%s", c.baseURL, slug), req, nil)
}

// PatchActivity sends a typed RFC 7396 merge-patch body to
// PATCH /activities/{slug}. Unset ContentPatch pointer fields are omitted and
// preserved server-side; present fields overwrite. To arm the AlarmKit alarm,
// set ContentPatch.Alarm to BoolPtr(true); the server clears alarm on any
// transition to ENDED.
func (c *Client) PatchActivity(ctx context.Context, slug string, req PatchRequest) error {
	return c.doWithRetry(ctx, "update", http.MethodPatch, fmt.Sprintf("%s/activities/%s", c.baseURL, slug), req, nil)
}

// SendNotification creates a notification record and optionally pushes an APNs alert.
func (c *Client) SendNotification(ctx context.Context, req SendNotificationRequest) error {
	req.FillSourceDisplayName()
	return c.doWithRetry(ctx, "notify", http.MethodPost,
		fmt.Sprintf("%s/notifications", c.baseURL), req, nil)
}
