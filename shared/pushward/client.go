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
	"net/url"
	"strconv"
	"time"
)

// maxRetryAfter caps how long a server-supplied Retry-After (header or
// problem.retry_after_ms) may park the calling goroutine. Legitimate throttle
// windows are seconds; this bounds a hostile or buggy value while still
// honoring realistic backoff requests.
const maxRetryAfter = 2 * time.Minute

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

// throttleDelay picks how long to wait before retrying a throttled request:
// the Retry-After header first, else the Problem body's retry_after_ms. The
// result is clamped to maxRetryAfter so a misbehaving or compromised server
// cannot park the calling goroutine for hours. Zero means "no server hint",
// leaving the caller on its exponential backoff.
func throttleDelay(retryAfterHeader string, problemRetryAfterMs int64) time.Duration {
	d := parseRetryAfter(retryAfterHeader)
	if d == 0 && problemRetryAfterMs > 0 {
		d = time.Duration(problemRetryAfterMs) * time.Millisecond
	}
	return min(d, maxRetryAfter)
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
// If contentType is empty, "application/json" is used. Widget PATCH requests
// must pass "application/merge-patch+json" to satisfy RFC 7396 content
// negotiation enforced by pushward-server.
func (c *Client) doWithRetry(ctx context.Context, operation, method, url, contentType string, body interface{}, handleConflict func([]byte) (bool, error)) error {
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
			// The breaker may have admitted this as a half-open probe; the
			// request never reached the backend, so re-arm rather than wedge.
			if c.breaker != nil {
				c.breaker.Abort()
			}
			return fmt.Errorf("marshaling request body: %w", err)
		}
	}

	var lastErr error
	var lastThrottled bool
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
				// Probe abandoned before reaching the backend; re-arm the
				// breaker so a half-open probe is not left dangling.
				if c.breaker != nil {
					c.breaker.Abort()
				}
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
			if c.breaker != nil {
				c.breaker.Abort()
			}
			return fmt.Errorf("creating request: %w", err)
		}
		if reqBody != nil {
			ct := contentType
			if ct == "" {
				ct = "application/json"
			}
			httpReq.Header.Set("Content-Type", ct)
		}
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("sending request: %w", err)
			lastThrottled = false
			continue
		}

		if resp.StatusCode == http.StatusConflict && handleConflict != nil {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if done, cerr := handleConflict(respBody); done {
				// Backend answered (409) — it is reachable but this is not a
				// success, so it must not zero the closed-state failure streak.
				c.recordResult(ctx, operation, attempts, start, cerr, breakerReachable)
				return cerr
			}
			// If handleConflict says not done, fall through to default handling
			lastErr = fmt.Errorf("conflict (409)")
			lastThrottled = false
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			c.recordResult(ctx, operation, attempts, start, nil, breakerHealthy)
			return nil
		}

		// Read body for error diagnostics. Problem bodies routinely exceed
		// a few hundred bytes (type URL + detail + errors array), so cap at
		// 64 KiB — enough for any realistic Problem, still bounded.
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()

		problem := parseProblem(respBody)

		if resp.StatusCode == http.StatusTooManyRequests {
			// quota.exceeded is a sticky monthly cap, not transient
			// backpressure: Retry-After can be weeks out and no retry inside
			// this call can ever succeed. Fail fast instead of parking the
			// goroutine for maxRetryAfter on every remaining attempt. The
			// backend answered, so this is breakerReachable, not a fault:
			// same classification as the 4xx branch below.
			if problem.Code == ErrCodeQuotaExceeded {
				lastErr = newHTTPError(http.StatusTooManyRequests, problem)
				slog.Warn("PushWard monthly quota exhausted",
					"url", url,
					"code", problem.Code,
					"kind", problem.Kind,
					"used", problem.Used,
					"limit", problem.Limit,
					"reset_at", problem.ResetAt,
				)
				c.recordResult(ctx, operation, attempts, start, lastErr, breakerReachable)
				return lastErr
			}

			retryAfterOverride = throttleDelay(resp.Header.Get("Retry-After"), problem.RetryAfterMs)
			slog.Warn("rate limited by PushWard", "url", url, "retry_after", retryAfterOverride, "code", problem.Code)
			// Typed error so callers can errors.As the 429 (status + Retry-After).
			lastErr = newHTTPError(http.StatusTooManyRequests, problem)
			lastThrottled = true
			continue
		}
		lastErr = newHTTPError(resp.StatusCode, problem)
		lastThrottled = false
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			slog.Warn("PushWard client error",
				"status", resp.StatusCode,
				"url", url,
				"code", problem.Code,
				"detail", problem.Detail,
			)
			// 4xx client errors are not retryable, and the backend is clearly
			// reachable, so this must not trip the breaker — but it is not a
			// success either, so it must not zero the closed-state streak.
			c.recordResult(ctx, operation, attempts, start, lastErr, breakerReachable)
			return lastErr
		}
	}
	err := fmt.Errorf("max retries exceeded: %w", lastErr)
	// Sustained 429 throttling is backpressure from a reachable backend, not a
	// health fault — it must not open the (relay-wide shared) breaker. Only
	// 5xx/network exhaustion counts as a fault. 429 is reachable-not-success, so
	// it un-wedges a half-open probe without zeroing the closed-state streak.
	signal := breakerFault
	if lastThrottled {
		signal = breakerReachable
	}
	c.recordResult(ctx, operation, attempts, start, err, signal)
	return err
}

// Stable programmatic error codes emitted by pushward-server on the Problem
// body. Callers branch on HTTPError.Code instead of the human-readable Detail.
//
// The two 429 codes mean very different things. ErrCodeRateLimitExceeded is
// transient IP backpressure and is retried automatically. ErrCodeQuotaExceeded
// is the free-tier monthly cap: it stays exhausted until HTTPError.ResetAt or
// until the user upgrades, so the client fails fast on it rather than retrying.
const (
	ErrCodeActivityLimitExceeded        = "activity.limit_exceeded"
	ErrCodeWidgetLimitExceeded          = "widget.limit_exceeded"
	ErrCodeQuotaExceeded                = "quota.exceeded"
	ErrCodeRateLimitExceeded            = "rate_limit.exceeded"
	ErrCodeActivityNotFound             = "activity.not_found"
	ErrCodeActivityContentInvalid       = "activity.content_invalid"
	ErrCodeActivityUpsertForbidden      = "activity.upsert_forbidden"
	ErrCodeNotificationActivityNotFound = "notification.activity_not_found"
	ErrCodeSubscriptionRequired         = "subscription.required"
	ErrCodeSubscriptionOwnerInactive    = "subscription.owner_inactive"
	ErrCodeWidgetTokenNotPermitted      = "widget.token_not_permitted"
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

	// Quota extension members, present only on a quota.exceeded 429.
	Kind    string `json:"kind,omitempty"`
	Used    int    `json:"used,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	ResetAt string `json:"reset_at,omitempty"`
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

	// Quota detail, populated only when Code == ErrCodeQuotaExceeded.
	// Kind is one of "notifications", "live_activity_updates",
	// "widget_updates", "emails". ResetAt is RFC 3339 UTC and marks the start
	// of the next calendar month, when the quota refills.
	Kind    string
	Used    int
	Limit   int
	ResetAt string
}

func newHTTPError(status int, p problem) *HTTPError {
	return &HTTPError{
		StatusCode:   status,
		Type:         p.Type,
		Title:        p.Title,
		Detail:       p.Detail,
		Code:         p.Code,
		RetryAfterMs: p.RetryAfterMs,
		Kind:         p.Kind,
		Used:         p.Used,
		Limit:        p.Limit,
		ResetAt:      p.ResetAt,
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

// breakerSignal classifies a request outcome for the circuit breaker. The
// breaker tracks backend *health*, which is distinct from request success: a
// 4xx/409/429 proves the backend is reachable and serving (healthy), even
// though the request itself failed.
type breakerSignal int

const (
	// breakerHealthy: a genuine 2xx. Resets the failure count and closes a
	// half-open probe.
	breakerHealthy breakerSignal = iota
	// breakerReachable: a response came back proving the backend is up but the
	// request itself failed — a non-retryable 4xx, a resolved 409 conflict, or
	// sustained 429 throttling. Closes a half-open probe (the backend recovered)
	// but leaves the closed-state failure streak intact, so it neither counts as
	// a fault nor zeroes a climbing streak of real 5xx/network faults.
	breakerReachable
	// breakerFault: the backend is unreachable or unhealthy — 5xx or network
	// errors that survived all retries. Counts toward the open threshold and
	// re-opens a half-open probe.
	breakerFault
)

// recordResult records the breaker outcome and fires the onResult callback.
// Only genuine backend faults (5xx/network exhaustion) trip the breaker; a 2xx
// is breakerHealthy, while 4xx client errors, 429 throttling, and conflict
// resolutions are breakerReachable (un-wedge a half-open probe without zeroing
// the closed-state streak). The circuit-open short-circuit returns before
// reaching recordResult (it calls c.onResult directly at the Allow() guard), and
// a probe admitted by Allow() that never reaches the backend must call
// breaker.Abort() directly, not recordResult, so it does not register a fake
// success — see the early-return paths in doWithRetry.
func (c *Client) recordResult(ctx context.Context, operation string, attempts int, start time.Time, err error, signal breakerSignal) {
	if c.breaker != nil {
		switch signal {
		case breakerHealthy:
			c.breaker.RecordSuccess()
		case breakerReachable:
			c.breaker.RecordReachable()
		case breakerFault:
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
	return c.doWithRetry(ctx, "create", http.MethodPost, fmt.Sprintf("%s/activities", c.baseURL), "",
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
	return c.doWithRetry(ctx, "update", http.MethodPatch, c.activityURL(slug), "", req, nil)
}

// activityURL escapes the slug into the path. net/http sends the request path
// close to verbatim, so an unescaped slug carrying "?" or "/" would let a
// caller graft a query parameter (e.g. upsert=true) or a path segment onto the
// request. Every slug in this repo is already restricted to [a-zA-Z0-9_-], so
// this is a no-op today and a guard against a future caller that is not.
func (c *Client) activityURL(slug string) string {
	return fmt.Sprintf("%s/activities/%s", c.baseURL, url.PathEscape(slug))
}

func (c *Client) widgetURL(slug string) string {
	return fmt.Sprintf("%s/widgets/%s", c.baseURL, url.PathEscape(slug))
}

// PatchOption tunes a single PatchActivity call.
type PatchOption func(*patchOptions)

type patchOptions struct {
	upsert bool
}

// WithUpsert makes PatchActivity create the activity when the slug does not
// exist, instead of failing with 404. The server names the new activity after
// the slug and gives it no TTLs, so prefer CreateActivity whenever a
// human-readable name, ended_ttl or stale_ttl matters, which is every bridge
// in this repo today. Requires an activity:manage or full-access key;
// activity:update keys get 403 activity.upsert_forbidden.
func WithUpsert() PatchOption {
	return func(o *patchOptions) { o.upsert = true }
}

// PatchActivity sends a typed RFC 7396 merge-patch body to
// PATCH /activities/{slug}. Unset ContentPatch pointer fields are omitted and
// preserved server-side; present fields overwrite. To arm the AlarmKit alarm,
// set ContentPatch.Alarm to BoolPtr(true); the server clears alarm on any
// transition to ENDED.
func (c *Client) PatchActivity(ctx context.Context, slug string, req PatchRequest, opts ...PatchOption) error {
	var o patchOptions
	for _, opt := range opts {
		opt(&o)
	}
	endpoint := c.activityURL(slug)
	if o.upsert {
		endpoint += "?upsert=true"
	}
	return c.doWithRetry(ctx, "update", http.MethodPatch, endpoint, "", req, nil)
}

// SendNotification creates a notification record and optionally pushes an APNs alert.
func (c *Client) SendNotification(ctx context.Context, req SendNotificationRequest) error {
	req.FillSourceDisplayName()
	return c.doWithRetry(ctx, "notify", http.MethodPost,
		fmt.Sprintf("%s/notifications", c.baseURL), "", req, nil)
}

// CreateWidget creates (or upserts) a widget via POST /widgets. The server
// upserts on (user, slug) so idempotent calls at startup are safe. A 409
// signals widget.limit_exceeded — surfaced as a typed *HTTPError with that
// stable Code.
func (c *Client) CreateWidget(ctx context.Context, req CreateWidgetRequest) error {
	return c.doWithRetry(ctx, "widget.create", http.MethodPost,
		fmt.Sprintf("%s/widgets", c.baseURL), "", req,
		func(body []byte) (bool, error) {
			return true, newHTTPError(http.StatusConflict, parseProblem(body))
		},
	)
}

// UpdateWidget sends a merge-patch body to PATCH /widgets/{slug}. The widget
// API requires Content-Type "application/merge-patch+json" (RFC 7396); absent
// fields are preserved, present fields overwrite, null clears.
func (c *Client) UpdateWidget(ctx context.Context, slug string, req UpdateWidgetRequest) error {
	return c.doWithRetry(ctx, "widget.update", http.MethodPatch,
		c.widgetURL(slug),
		"application/merge-patch+json", req, nil)
}

// DeleteWidget removes a widget via DELETE /widgets/{slug}.
func (c *Client) DeleteWidget(ctx context.Context, slug string) error {
	return c.doWithRetry(ctx, "widget.delete", http.MethodDelete,
		c.widgetURL(slug), "", nil, nil)
}
