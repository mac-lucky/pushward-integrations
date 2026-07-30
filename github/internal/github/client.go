package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxRetries = 3

const maxRateLimitRetries = 3

const requestTimeout = 10 * time.Second

// HourlyRateLimit is GitHub's documented primary rate limit for an authenticated
// personal access token. Exported because the shared poller paces detection
// against it and reports the configured rate against it at startup - it is
// GitHub's number, so it lives here rather than in the forge-neutral loop.
const HourlyRateLimit = 5000

// noWorkflowsTTL bounds how long a repo stays written off as having no
// workflows. A workflow can be added at any time and the bridge should notice
// without a restart. Deliberately the same half hour the forgejo client gives its
// Actions-disabled answers, so the two caches read alike.
const noWorkflowsTTL = 30 * time.Minute

// cacheRetention drops a repo's cached answers once nothing has asked about it for
// this long. Both caches are keyed by repo name and would otherwise keep an entry
// for every repo ever seen, including ones since renamed, archived, or dropped from
// the owner. Comfortably longer than the widest gap detection can be paced to,
// which is one rate-limit window.
const cacheRetention = 2 * time.Hour

// runsProbe is the cached answer to one repo's in-progress-runs probe. GitHub
// does not count a conditional request that answers 304 against the primary rate
// limit, so caching the decoded runs alongside the ETag turns the poll every idle
// repo pays every pass into something free.
type runsProbe struct {
	etag string
	runs []WorkflowRun
	// usedAt is when this entry was last read or written, for cacheRetention.
	usedAt time.Time
}

// workflowPresence is what we know about whether a repo has any workflows.
// Repos with none still answer the runs probe with an empty 200, indistinguishable
// from "nothing running right now", so it takes a separate lookup to tell them
// apart - and on a real account they are a large share of everything discovery
// finds.
type workflowPresence struct {
	// has is false for a repo with no workflow files at all.
	has bool
	// etag makes the re-check below conditional, so a repo written off as having no
	// workflows costs one request ever rather than one per TTL. Without it the
	// filter would spend billed requests to avoid probes that conditional requests
	// have already made free.
	etag string
	// checkedAt stamps a negative answer so it expires. Positives never expire: a
	// repo that loses its workflows costs one wasted probe per pass until restart,
	// which is exactly what the old behavior was for every repo.
	checkedAt time.Time
	// usedAt is when this entry was last read or written, for cacheRetention.
	usedAt time.Time
}

type Client struct {
	httpClient *http.Client
	token      string
	baseURL    string // defaults to https://api.github.com

	mu        sync.Mutex
	remaining int
	resetAt   time.Time
	login     string // cached login of the token's user (lazy)
	// runsCache and workflows are keyed by "owner/repo".
	runsCache map[string]runsProbe
	workflows map[string]workflowPresence
}

func NewClient(token string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		token:      token,
		baseURL:    "https://api.github.com",
		remaining:  -1, // unknown until first response
		runsCache:  make(map[string]runsProbe),
		workflows:  make(map[string]workflowPresence),
	}
}

// SetBaseURL overrides the GitHub API base URL (for testing).
func (c *Client) SetBaseURL(url string) {
	c.baseURL = url
}

// Budget reports the requests left in the current window and when it refills,
// implementing cipoll.BudgetReporter. ok is false until a response has been seen,
// when there is nothing to pace against yet.
func (c *Client) Budget() (remaining int, resetAt time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.remaining < 0 {
		return 0, time.Time{}, false
	}
	return c.remaining, c.resetAt, true
}

// BudgetError refuses a request the current window cannot cover.
//
// It replaces sleeping until the reset. GitHub documents that continuing to make
// requests while rate limited can get an integration banned, so this still
// declines - but it declines immediately, leaving the caller to decide what to
// drop. Blocking here instead held the poller's only goroutine for up to an hour
// and froze every card it was tracking, which is a worse outcome than detecting a
// new run late.
type BudgetError struct {
	ResetAt time.Time
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("github request budget exhausted, resets in %s", time.Until(e.ResetAt).Round(time.Second))
}

// checkBudget declines when the window is provably spent. The threshold is zero,
// not a safety buffer: GitHub answers 403 at that point anyway, and holding back a
// buffer is the poller's job - it reserves one so the runs it already tracks stay
// affordable while detection paces itself down.
func (c *Client) checkBudget() error {
	c.mu.Lock()
	remaining := c.remaining
	resetAt := c.resetAt
	c.mu.Unlock()

	if remaining == 0 && time.Now().Before(resetAt) {
		return &BudgetError{ResetAt: resetAt}
	}
	return nil
}

// apiResponse is one successful GitHub read. notModified says the server answered
// 304 and body is empty, so the caller must use whatever it cached under etag.
type apiResponse struct {
	body        []byte
	etag        string
	notModified bool
}

func (c *Client) doWithRetry(ctx context.Context, url, operation string) ([]byte, error) {
	resp, err := c.doConditional(ctx, url, operation, "")
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}

// doConditional is doWithRetry with an If-None-Match. An empty etag makes it an
// ordinary unconditional request; a still-current one answers 304, which GitHub
// documents as not counting against the primary rate limit and is the whole reason
// the per-repo idle probe is affordable at all.
func (c *Client) doConditional(ctx context.Context, url, operation, etag string) (apiResponse, error) {
	var lastErr error
	rateLimitRetries := 0

	for attempt := 0; attempt < maxRetries; attempt++ {
		if err := c.checkBudget(); err != nil {
			return apiResponse{}, fmt.Errorf("%s: %w", operation, err)
		}

		if attempt > 0 {
			backoff := min(time.Second<<(attempt-1), 30*time.Second)
			slog.Warn("retrying GitHub API call", "operation", operation, "attempt", attempt+1, "backoff", backoff)
			if err := sleepCtx(ctx, backoff); err != nil {
				return apiResponse{}, err
			}
		}

		// context.WithTimeout clamps to the parent deadline if it is earlier,
		// so retries cannot collectively exceed the caller's budget.
		reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		resp, err := c.doRequest(reqCtx, url, etag)
		cancel()
		if err != nil {
			// Handle rate limit (429) - wait and retry without consuming a normal retry slot.
			var rle *rateLimitError
			if errors.As(err, &rle) {
				rateLimitRetries++
				if rateLimitRetries > maxRateLimitRetries {
					return apiResponse{}, fmt.Errorf("%s: rate limit retries exceeded: %w", operation, err)
				}
				wait := rle.wait(rateLimitRetries)
				slog.Warn("rate limited by GitHub, waiting", "operation", operation,
					"wait", wait.Round(time.Second), "attempt", rateLimitRetries, "from_header", rle.fromHeader)
				if err := sleepCtx(ctx, wait); err != nil {
					return apiResponse{}, err
				}
				attempt-- // don't consume a normal retry slot
				continue
			}

			// Don't retry client errors (4xx).
			var ce *clientError
			if errors.As(err, &ce) {
				return apiResponse{}, fmt.Errorf("%s: %w", operation, err)
			}
			lastErr = fmt.Errorf("%s: %w", operation, err)
			if ctx.Err() != nil {
				return apiResponse{}, ctx.Err()
			}
			continue
		}
		return resp, nil
	}
	return apiResponse{}, fmt.Errorf("max retries exceeded: %w", lastErr)
}

// doRequest executes a single HTTP request. A non-empty ifNoneMatch makes it
// conditional, in which case a 304 comes back as apiResponse{notModified: true}
// rather than an error - it is a successful answer meaning "what you have is
// current". Non-retryable client errors (4xx) are wrapped in clientError, rate
// limit errors (429, or 403 carrying rate-limit headers) in rateLimitError.
func (c *Client) doRequest(ctx context.Context, url, ifNoneMatch string) (apiResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return apiResponse{}, err
	}
	c.setHeaders(req)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return apiResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	// Before the status checks, and deliberately also on a 304: the headers are
	// there either way, and this is what keeps the budget view current in the
	// steady state where almost every response is a 304.
	c.recordRateLimit(resp)

	if resp.StatusCode == http.StatusNotModified {
		// No etag: the caller already holds the one it sent, and the body it cached
		// under it.
		return apiResponse{notModified: true}, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return apiResponse{}, err
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return apiResponse{body: body, etag: resp.Header.Get("ETag")}, nil
	}
	// GitHub signals rate limiting as 429 OR as 403 carrying rate-limit headers
	// (primary limit: X-RateLimit-Remaining: 0; secondary/abuse limit:
	// Retry-After). Treat both as retryable so the poller backs off instead of
	// hammering and re-tripping the limit.
	if resp.StatusCode == 429 ||
		(resp.StatusCode == 403 && (resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0")) {
		wait, fromHeader := rateLimitRetryAfter(resp)
		return apiResponse{}, &rateLimitError{retryAfter: wait, fromHeader: fromHeader, url: url}
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return apiResponse{}, &clientError{status: resp.StatusCode, url: url}
	}
	return apiResponse{}, fmt.Errorf("unexpected status %d for %s", resp.StatusCode, url)
}

const (
	// defaultRateLimitWait is GitHub's own floor for a rate-limit response that
	// carries no usable timing: "wait for at least one minute before retrying".
	defaultRateLimitWait = 60 * time.Second
	// maxRateLimitWait caps any single wait so a hostile or skewed value cannot
	// park the poller.
	maxRateLimitWait = 15 * time.Minute
	// resetBuffer is added to a wait derived from X-RateLimit-Reset so the retry
	// lands just inside the new window rather than exactly on its boundary.
	resetBuffer = 2 * time.Second
)

// rateLimitRetryAfter reads how long GitHub asked us to wait. It prefers
// Retry-After (delta-seconds or HTTP-date per RFC 7231) and falls back to the
// X-RateLimit-Reset epoch. ok is false when neither parses, in which case the
// duration is meaningless and rateLimitError.wait supplies its own - that is the
// one case where the caller escalates instead of trusting a number.
func rateLimitRetryAfter(resp *http.Response) (wait time.Duration, ok bool) {
	// clamp bounds a *successfully parsed* signal to [0, maxRateLimitWait]. A
	// non-positive value is authoritative, not garbage: "Retry-After: 0" means
	// retry now, and an X-RateLimit-Reset / HTTP-date already in the past (clock
	// skew or the window already reset) means the limit is open again - both map to
	// an immediate retry, NOT the default.
	clamp := func(d time.Duration) time.Duration {
		if d < 0 {
			return 0
		}
		return min(d, maxRateLimitWait)
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return clamp(time.Duration(secs) * time.Second), true
		}
		if t, err := http.ParseTime(v); err == nil {
			return clamp(time.Until(t)), true
		}
	}
	if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
		if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil {
			return clamp(time.Until(time.Unix(epoch, 0)) + resetBuffer), true
		}
	}
	return 0, false
}

// sleepCtx waits for d, or returns early if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// jittered spreads a wait by up to a quarter of itself. GitHub hands every caller
// the same reset timestamp, so backing off to it exactly means anything sharing
// this token resumes in the same instant and re-trips the limit together.
func jittered(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	spread := int64(d / 4)
	if spread <= 0 {
		return d
	}
	return d + time.Duration(rand.Int64N(spread)) // #nosec G404 -- spreading a retry, not security-sensitive
}

type clientError struct {
	status int
	url    string
}

func (e *clientError) Error() string {
	return fmt.Sprintf("client error %d for %s", e.status, e.url)
}

type rateLimitError struct {
	// retryAfter is what GitHub asked for, meaningful only when fromHeader.
	retryAfter time.Duration
	// fromHeader records whether GitHub said when to resume at all.
	fromHeader bool
	url        string
}

func (e *rateLimitError) Error() string {
	if e.fromHeader {
		return fmt.Sprintf("rate limited for %s (retry after %s)", e.url, e.retryAfter)
	}
	// No number to quote: wait() picks one, and it grows per attempt.
	return fmt.Sprintf("rate limited for %s (no retry timing given)", e.url)
}

// wait is how long to hold off before rate-limit retry n (1-based).
//
// A parsed header is authoritative and is used as-is: GitHub said exactly when to
// resume, so waiting longer would only cost freshness. Without one we are most
// likely looking at a secondary limit, which GitHub documents as "wait for at
// least one minute ... then an exponentially increasing amount of time" - so that
// is the one path that grows.
func (e *rateLimitError) wait(n int) time.Duration {
	if e.fromHeader {
		return jittered(e.retryAfter)
	}
	// Shift bounded before it is applied: maxRateLimitWait/defaultRateLimitWait is
	// reached within a handful of attempts, and an unbounded shift would overflow
	// into a negative duration - i.e. no wait at all - for a large n.
	const maxShift = 8
	return jittered(min(defaultRateLimitWait<<min(max(n-1, 0), maxShift), maxRateLimitWait))
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

// fetchWorkflowRuns issues a GET against a workflow-runs list endpoint and
// decodes the run list. The repo-level (in-progress) and workflow-level (latest)
// list endpoints share this response shape and error context.
func (c *Client) fetchWorkflowRuns(ctx context.Context, url, operation string) ([]WorkflowRun, error) {
	body, err := c.doWithRetry(ctx, url, operation)
	if err != nil {
		return nil, err
	}
	return decodeWorkflowRuns(body)
}

// decodeWorkflowRuns reads a workflow-runs list response. Shared with the
// conditional path in GetInProgressRuns, which needs the ETag off the response and
// so cannot go through fetchWorkflowRuns.
func decodeWorkflowRuns(body []byte) ([]WorkflowRun, error) {
	var result WorkflowRunsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decoding workflow runs: %w", err)
	}
	return result.WorkflowRuns, nil
}

// GetInProgressRuns returns the repo's queued-or-running workflow runs.
//
// This is the request the bridge makes most by a wide margin - once per watched
// repo per detection pass, whether or not anything is running - so it is the one
// place worth spending code to keep cheap. Two things do that: repos with no
// workflows are not polled at all, and everything else goes out as a conditional
// request, which GitHub does not bill when the answer is unchanged.
func (c *Client) GetInProgressRuns(ctx context.Context, repo string) ([]WorkflowRun, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}

	has, err := c.hasWorkflows(ctx, repo)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, nil
	}

	c.mu.Lock()
	cached := c.runsCache[repo]
	c.mu.Unlock()

	// per_page=50 caps memory while covering concurrent workflows on busy repos.
	// The poller selects only the most recent run, so ordering is stable. Do not add
	// a sort parameter: GitHub reorders such a list whenever any item in it changes,
	// which would stop the ETag from ever matching and put this request back on the
	// bill every pass.
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs?status=in_progress&per_page=50", c.baseURL, owner, name)
	resp, err := c.doConditional(ctx, url, "requesting workflow runs", cached.etag)
	if err != nil {
		return nil, err
	}

	if resp.notModified {
		// Nothing changed since the cached answer, and this cost no rate limit. Stored
		// again only to restamp usedAt for the cache sweep.
		c.storeRunsProbe(repo, cached.etag, cached.runs)
		return slices.Clone(cached.runs), nil
	}

	runs, err := decodeWorkflowRuns(resp.body)
	if err != nil {
		return nil, err
	}
	c.storeRunsProbe(repo, resp.etag, runs)
	return runs, nil
}

// storeRunsProbe replaces the cached answer for a repo. An empty etag is stored as
// such: a response without one simply means the next request is unconditional.
//
// The runs are cloned rather than aliased, so a caller that reorders what it was
// handed cannot corrupt every later answer for that repo.
func (c *Client) storeRunsProbe(repo, etag string, runs []WorkflowRun) {
	c.mu.Lock()
	c.runsCache[repo] = runsProbe{etag: etag, runs: slices.Clone(runs), usedAt: time.Now()}
	c.mu.Unlock()
}

// hasWorkflows reports whether the repo has any workflow definitions at all,
// caching the answer.
//
// It takes a separate lookup because GitHub answers the runs probe for a repo with
// no workflows the same way it answers one that simply has nothing running - an
// empty 200 - so the runs probe cannot tell them apart. (Forgejo 404s that case,
// which is why its client can write the repo off from the runs call itself.) What
// it buys is a round trip per workflow-less repo on every pass, and on a real
// account those are a large share of what owner discovery returns.
//
// A positive answer is kept for the process lifetime - a repo that loses its
// workflows just costs what it always used to. A negative one is re-checked after
// noWorkflowsTTL so a workflow added later is picked up without a restart, and that
// re-check is conditional: otherwise the filter would spend billed requests to save
// round trips the ETag above has already made free.
func (c *Client) hasWorkflows(ctx context.Context, repo string) (bool, error) {
	c.mu.Lock()
	known, ok := c.workflows[repo]
	if ok {
		known.usedAt = time.Now()
		c.workflows[repo] = known
	}
	c.mu.Unlock()
	if ok && (known.has || time.Since(known.checkedAt) < noWorkflowsTTL) {
		return known.has, nil
	}

	owner, name, err := splitRepo(repo)
	if err != nil {
		return false, err
	}

	// per_page=1: only the count is read.
	url := fmt.Sprintf("%s/repos/%s/%s/actions/workflows?per_page=1", c.baseURL, owner, name)
	resp, err := c.doConditional(ctx, url, "listing workflows", known.etag)
	if err != nil {
		// A 404 means Actions is disabled for the repo, or the repo went away between
		// discovery and now. Either way there is nothing to poll for, so it gets the
		// same answer as having no workflow files - and caching it stops both the
		// probe and its error line repeating on every pass.
		var ce *clientError
		if errors.As(err, &ce) && ce.status == http.StatusNotFound {
			c.markWorkflows(repo, false, "")
			slog.Info("repo has no Actions, skipping it until the next re-check",
				"repo", repo, "recheck_in", noWorkflowsTTL)
			return false, nil
		}
		return false, err
	}

	if resp.notModified {
		// The workflow list is unchanged, so the previous answer still holds. Restamps
		// the TTL, which is the point: this cost nothing.
		c.markWorkflows(repo, known.has, known.etag)
		return known.has, nil
	}

	var result WorkflowsResponse
	if err := json.Unmarshal(resp.body, &result); err != nil {
		return false, fmt.Errorf("decoding workflows: %w", err)
	}
	has := result.TotalCount > 0
	c.markWorkflows(repo, has, resp.etag)
	if !has {
		slog.Info("repo has no workflows, skipping it until the next re-check",
			"repo", repo, "recheck_in", noWorkflowsTTL)
	}
	return has, nil
}

func (c *Client) markWorkflows(repo string, has bool, etag string) {
	now := time.Now()
	c.mu.Lock()
	c.workflows[repo] = workflowPresence{has: has, etag: etag, checkedAt: now, usedAt: now}
	c.mu.Unlock()
}

// pruneCaches drops entries for repos nothing has asked about in cacheRetention.
// Called from discovery rather than on a timer: it is the one place that already
// runs periodically without being per-repo, and a pruned entry costs only the one
// request that re-establishes its ETag.
func (c *Client) pruneCaches() {
	cutoff := time.Now().Add(-cacheRetention)
	c.mu.Lock()
	defer c.mu.Unlock()
	for repo, e := range c.runsCache {
		if e.usedAt.Before(cutoff) {
			delete(c.runsCache, repo)
		}
	}
	for repo, e := range c.workflows {
		if e.usedAt.Before(cutoff) {
			delete(c.workflows, repo)
		}
	}
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

// GetLatestWorkflowRun returns the most recent run of the given workflow on the
// given branch matching status (a GitHub run status/conclusion filter value such
// as "success" or "completed"), or nil if none exists. Used to learn a run's
// full step shape up front: GitHub creates jobs lazily within a run, so a fresh
// scan only sees the first wave, but a finished prior run has revealed its entire
// job DAG. The per-workflow runs endpoint sorts created-descending, so per_page=1
// is the latest match.
func (c *Client) GetLatestWorkflowRun(ctx context.Context, repo string, workflowID int64, branch, status string) (*WorkflowRun, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}

	q := url.Values{"per_page": {"1"}}
	if status != "" {
		q.Set("status", status)
	}
	if branch != "" {
		q.Set("branch", branch)
	}
	reqURL := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%d/runs?%s", c.baseURL, owner, name, workflowID, q.Encode())

	runs, err := c.fetchWorkflowRuns(ctx, reqURL, "requesting latest workflow run")
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return &runs[0], nil
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
//   - the token's own account -> GET /user/repos?affiliation=owner (includes
//     private repos the user owns);
//   - any other owner -> GET /orgs/{owner}/repos (org repos the token can see),
//     falling back to GET /users/{owner}/repos for personal accounts (public).
//
// Archived and disabled repos are filtered out.
func (c *Client) ListRepos(ctx context.Context, owner string) ([]string, error) {
	// Discovery is the periodic non-per-repo call, so it is where the per-repo
	// caches get swept.
	c.pruneCaches()

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
			// Not an org - treat owner as a personal account (public repos).
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
