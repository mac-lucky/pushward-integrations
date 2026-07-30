package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const (
	maxRetries          = 3
	maxRateLimitRetries = 3

	// maxRepoPages bounds repo discovery. Forgejo sends no Link header and no
	// X-Total-Count, so a short page is the only natural terminator; this stops
	// an instance that ignores `page` from spinning the loop forever.
	maxRepoPages = 20

	// repoPageSize is the documented maximum. Asking for more is not an error but
	// buys nothing.
	repoPageSize = 50

	// activeRunsPageSize bounds the idle probe. Only the newest run is kept, so
	// this exists to survive a burst of simultaneous runs, not to enumerate them.
	activeRunsPageSize = 5

	// bodyLimit caps a response read. The runs endpoint embeds a full repository
	// object plus a multi-KB event_payload per run, so even a filtered page is
	// not small.
	bodyLimit = 8 << 20
)

// Options tune the client's behavior. The zero value is usable except for
// Timeout, which NewClient defaults.
type Options struct {
	// Timeout bounds a single API call. Self-hosted instances live on the far
	// side of a home LAN rather than a CDN, so this is configurable where the
	// github bridge hardcodes it.
	Timeout time.Duration

	// LiveTimings enables the per-poll tasks lookup that stamps a running job's
	// start. Without it the ladder still renders, just without an animated ETA.
	LiveTimings bool

	// HistoryTimings enables the prior-run tasks walk that measures each step
	// group's duration, which sizes the pills.
	HistoryTimings bool
}

// Client talks to a Forgejo instance's Actions API.
//
// Unlike GitHub, Forgejo sends no rate-limit headers at all, so there is no
// budget to track and no proactive wait - the reactive 429 path below is the
// whole story, and it exists because a reverse proxy in front of the instance
// can rate-limit even when Forgejo itself would not.
type Client struct {
	httpClient *http.Client
	token      string
	apiBase    string // <instance>/api/v1
	webBase    string // <instance>
	opts       Options

	mu    sync.Mutex
	login string // cached login of the token's user (lazy)
}

// NewClient builds a client for the instance at baseURL, which may be the
// instance root or an API base - normalizeBase accepts either.
func NewClient(baseURL, token string, opts Options) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	web := normalizeBase(baseURL)
	return &Client{
		// The per-request context carries the real deadline; this is a backstop
		// for a connection that never produces one.
		httpClient: &http.Client{Timeout: opts.Timeout + 5*time.Second},
		token:      token,
		apiBase:    web + "/api/v1",
		webBase:    web,
		opts:       opts,
	}
}

// normalizeBase trims a trailing slash and a trailing /api/v1 so both the
// instance root and a full API base are accepted, and returns the web base. The
// client appends /api/v1 itself; the poller uses the web base to synthesise a
// repository link when a run does not embed one.
func normalizeBase(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	s = strings.TrimSuffix(s, "/api/v1")
	return strings.TrimRight(s, "/")
}

// WebBase returns the instance root, for building repository links.
func (c *Client) WebBase() string { return c.webBase }

func (c *Client) setHeaders(req *http.Request) {
	// "token" is Forgejo's native scheme and is also what older Gitea accepts;
	// Bearer works on current releases but buys nothing. There is no API-version
	// header to send.
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
}

func (c *Client) doWithRetry(ctx context.Context, endpoint, operation string) ([]byte, error) {
	var lastErr error
	rateLimitRetries := 0

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoff := min(time.Second<<(attempt-1), 30*time.Second)
			slog.Warn("retrying Forgejo API call", "operation", operation, "attempt", attempt+1, "backoff", backoff)
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}

		// context.WithTimeout clamps to the parent deadline if it is earlier, so
		// retries cannot collectively exceed the caller's budget.
		reqCtx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
		body, err := c.doRequest(reqCtx, endpoint)
		cancel()
		if err != nil {
			var rle *rateLimitError
			if errors.As(err, &rle) {
				rateLimitRetries++
				if rateLimitRetries > maxRateLimitRetries {
					return nil, fmt.Errorf("%s: rate limit retries exceeded: %w", operation, err)
				}
				slog.Warn("rate limited, waiting", "operation", operation, "retry_after", rle.retryAfter)
				timer := time.NewTimer(rle.retryAfter)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx.Err()
				case <-timer.C:
				}
				attempt-- // a rate limit does not consume a normal retry slot
				continue
			}

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

// doRequest executes a single GET and returns the response body. 4xx responses
// come back as clientError (not retried); 429 as rateLimitError.
func (c *Client) doRequest(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}
	// Only 429 is retryable. GitHub also treats a 403 carrying rate-limit headers
	// as a limit, but Forgejo sends no such headers and answers an unauthenticated
	// or under-scoped request with 403 - retrying that would just hammer.
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &rateLimitError{retryAfter: retryAfter(resp), url: endpoint}
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return nil, &clientError{status: resp.StatusCode, url: endpoint, message: apiMessage(body)}
	}
	return nil, fmt.Errorf("unexpected status %d for %s", resp.StatusCode, endpoint)
}

// retryAfter derives how long to wait before retrying a rate-limited response.
// Retry-After is the only signal available (delta-seconds or HTTP-date per RFC
// 7231); there is no X-RateLimit-Reset to fall back on.
func retryAfter(resp *http.Response) time.Duration {
	const (
		defaultWait = 60 * time.Second
		maxWait     = 15 * time.Minute
	)
	// A non-positive parsed value is authoritative, not garbage: "Retry-After: 0"
	// and a date already in the past both mean retry now. The default applies only
	// when nothing parses.
	clamp := func(d time.Duration) time.Duration {
		if d < 0 {
			return 0
		}
		if d > maxWait {
			return maxWait
		}
		return d
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return clamp(time.Duration(secs) * time.Second)
		}
		if t, err := http.ParseTime(v); err == nil {
			return clamp(time.Until(t))
		}
	}
	return defaultWait
}

type clientError struct {
	status  int
	url     string
	message string // the API's own error text, when it sent one
}

func (e *clientError) Error() string {
	if e.message != "" {
		return fmt.Sprintf("client error %d for %s: %s", e.status, e.url, e.message)
	}
	return fmt.Sprintf("client error %d for %s", e.status, e.url)
}

// apiMessage pulls the "message" field out of a Forgejo error body. It is where
// a permission failure names the scope it wanted, e.g.
// "token does not have at least one of required scope(s): [read:repository]" -
// without it a 403 says nothing about how to fix it.
func apiMessage(body []byte) string {
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return text.Truncate(strings.TrimSpace(payload.Message), 200)
}

// Status exposes the HTTP status so callers can distinguish an absent resource
// from a permission problem.
func (e *clientError) Status() int { return e.status }

type rateLimitError struct {
	retryAfter time.Duration
	url        string
}

func (e *rateLimitError) Error() string {
	return fmt.Sprintf("rate limited for %s (retry after %s)", e.url, e.retryAfter)
}

// splitRepo parses an "owner/repo" string into its two halves.
func splitRepo(repo string) (owner, name string, err error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo format %q, expected owner/repo", repo)
	}
	return parts[0], parts[1], nil
}

// fullRef converts a run's bare prettyref into the fully-qualified ref the runs
// `ref` filter requires. Filtering on the bare name matches nothing at all -
// silently, with an HTTP 200 and an empty list.
func fullRef(branch string) string {
	if branch == "" || strings.HasPrefix(branch, "refs/") {
		return branch
	}
	return "refs/heads/" + branch
}

func (c *Client) runsURL(owner, name string, q url.Values) string {
	return fmt.Sprintf("%s/repos/%s/%s/actions/runs?%s",
		c.apiBase, url.PathEscape(owner), url.PathEscape(name), q.Encode())
}

// GetActiveRuns lists the repo's runs that are running or waiting to start.
//
// This is the idle probe, and it is the cheapest call the bridge makes: with
// nothing running the whole response is a few dozen bytes. Runs held for
// approval ("blocked") are excluded - see activeStatuses.
func (c *Client) GetActiveRuns(ctx context.Context, repo string) ([]Run, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("page", "1")
	q.Set("limit", strconv.Itoa(activeRunsPageSize))
	for _, s := range activeStatuses {
		q.Add("status", s)
	}
	return c.fetchRuns(ctx, c.runsURL(owner, name, q), "get active runs")
}

// GetRun fetches one run by its API id. Note the id here is Run.ID, never
// Run.IndexInRepo.
func (c *Client) GetRun(ctx context.Context, repo string, runID int64) (*Run, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d",
		c.apiBase, url.PathEscape(owner), url.PathEscape(name), runID)

	body, err := c.doWithRetry(ctx, endpoint, "get run")
	if err != nil {
		return nil, err
	}
	var w wireRun
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("decode run: %w", err)
	}
	run := toRun(w)
	return &run, nil
}

// GetLatestFinishedRun returns the most recent terminal run of the same workflow
// on the same branch, used to seed a stable step total.
//
// Forgejo has no "completed" umbrella status the way GitHub does, so this runs
// two passes: a fully-successful run first (it executed the whole job DAG), then
// the remaining terminal statuses in one repeatable-parameter query. Returns nil
// with no error when there is nothing to seed from.
func (c *Client) GetLatestFinishedRun(ctx context.Context, repo, workflowID, branch string) (*Run, error) {
	if workflowID == "" || branch == "" {
		return nil, nil
	}
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}

	for _, statuses := range finishedStatusSets {
		q := url.Values{}
		q.Set("page", "1")
		q.Set("limit", "1")
		q.Set("workflow_id", workflowID)
		q.Set("ref", fullRef(branch))
		for _, s := range statuses {
			q.Add("status", s)
		}
		runs, err := c.fetchRuns(ctx, c.runsURL(owner, name, q), "get latest finished run")
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 {
			return &runs[0], nil
		}
	}
	return nil, nil
}

func (c *Client) fetchRuns(ctx context.Context, endpoint, operation string) ([]Run, error) {
	body, err := c.doWithRetry(ctx, endpoint, operation)
	if err != nil {
		return nil, err
	}
	var resp wireRunsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode runs: %w", err)
	}
	runs := make([]Run, 0, len(resp.WorkflowRuns))
	for _, w := range resp.WorkflowRuns {
		runs = append(runs, toRun(w))
	}
	return runs, nil
}

// GetJobs lists a run's jobs.
//
// The endpoint returns a bare array with no envelope and no pagination
// parameters, so this is one request and there is no page loop. The jobs carry
// no timestamps; stampLiveTimings / stampHistoricTimings fill those in.
func (c *Client) GetJobs(ctx context.Context, repo string, runID int64) ([]Job, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs",
		c.apiBase, url.PathEscape(owner), url.PathEscape(name), runID)

	body, err := c.doWithRetry(ctx, endpoint, "get jobs")
	if err != nil {
		return nil, err
	}
	var wires []wireJob
	if err := json.Unmarshal(body, &wires); err != nil {
		return nil, fmt.Errorf("decode jobs: %w", err)
	}
	jobs := make([]Job, 0, len(wires))
	for _, w := range wires {
		jobs = append(jobs, toJob(w))
	}
	return jobs, nil
}

// GetLiveJobs lists a run's jobs and stamps the running ones with their start
// time, which is what the live-progress anchor is measured from.
func (c *Client) GetLiveJobs(ctx context.Context, repo string, runID int64) ([]Job, error) {
	jobs, err := c.GetJobs(ctx, repo, runID)
	if err != nil {
		return nil, err
	}
	return c.stampLiveTimings(ctx, repo, jobs), nil
}

// GetFinishedJobs lists a finished run's jobs and stamps them with the durations
// that size the step pills.
func (c *Client) GetFinishedJobs(ctx context.Context, repo string, runID, indexInRepo int64) ([]Job, error) {
	jobs, err := c.GetJobs(ctx, repo, runID)
	if err != nil {
		return nil, err
	}
	return c.stampHistoricTimings(ctx, repo, jobs, indexInRepo), nil
}

// authenticatedLogin returns the token owner's login, cached after the first
// lookup.
func (c *Client) authenticatedLogin(ctx context.Context) (string, error) {
	c.mu.Lock()
	cached := c.login
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	body, err := c.doWithRetry(ctx, c.apiBase+"/user", "get authenticated user")
	if err != nil {
		return "", err
	}
	var u wireUser
	if err := json.Unmarshal(body, &u); err != nil {
		return "", fmt.Errorf("decode user: %w", err)
	}

	c.mu.Lock()
	c.login = u.Login
	c.mu.Unlock()
	return u.Login, nil
}

// GetVersion reports the instance version, logged once at startup so a support
// question about behavior has the release to hand.
func (c *Client) GetVersion(ctx context.Context) (string, error) {
	body, err := c.doWithRetry(ctx, c.apiBase+"/version", "get version")
	if err != nil {
		return "", err
	}
	var v wireVersion
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("decode version: %w", err)
	}
	return v.Version, nil
}

// ListRepos discovers the repos to watch for an owner, skipping archived and
// empty ones.
//
// /user/repos is tried first and unconditionally. It lists every repo the token
// can reach - including repos owned by someone else, which is the normal shape
// of a self-hosted instance where one account owns the repos and another holds
// the token - and it needs no scope beyond the token's own. The owner-scoped
// endpoints do: /orgs/{owner}/repos wants read:organization, and even
// /users/{owner}/repos is refused outright by a narrowly-scoped token. Reaching
// for those first is what made a restricted token fail discovery entirely and
// crashloop the bridge.
//
// When owner is the token's own login, everything reachable counts. Otherwise
// the result is narrowed to that owner, and only if that leaves nothing do the
// owner-scoped endpoints get a turn.
func (c *Client) ListRepos(ctx context.Context, owner string) ([]string, error) {
	reachable, reachErr := c.listReposPaged(ctx, c.apiBase+"/user/repos")
	if reachErr == nil {
		// A failed login lookup is not fatal here: it only decides whether to
		// narrow, and an unnarrowed list is still a usable answer.
		if login, err := c.authenticatedLogin(ctx); err == nil && login != "" && login == owner {
			return reachable, nil
		}
		if scoped := filterByOwner(reachable, owner); len(scoped) > 0 {
			return scoped, nil
		}
		slog.Debug("owner has no repos among those the token can reach, trying the owner endpoints",
			"owner", owner, "reachable", len(reachable))
	} else {
		slog.Warn("could not list the token's own repos, trying the owner endpoints",
			"error", reachErr)
	}

	repos, err := c.listReposPaged(ctx, fmt.Sprintf("%s/orgs/%s/repos", c.apiBase, url.PathEscape(owner)))
	if err == nil {
		return repos, nil
	}
	var ce *clientError
	if errors.As(err, &ce) && (ce.status == http.StatusNotFound || ce.status == http.StatusForbidden) {
		slog.Debug("org repo listing unavailable, falling back to user repos",
			"owner", owner, "status", ce.status)
		repos, err = c.listReposPaged(ctx, fmt.Sprintf("%s/users/%s/repos", c.apiBase, url.PathEscape(owner)))
		if err == nil {
			return repos, nil
		}
	}
	// Every route failed. Surface the /user/repos error when there was one -
	// it is the one whose cause (token scope) the operator can act on.
	if reachErr != nil {
		return nil, fmt.Errorf("listing repos for %q failed on every endpoint; the token may lack the scope to read repositories: %w", owner, reachErr)
	}
	return nil, err
}

// filterByOwner narrows a reachable-repo list to one owner.
func filterByOwner(repos []string, owner string) []string {
	if owner == "" {
		return nil
	}
	prefix := owner + "/"
	var out []string
	for _, r := range repos {
		if strings.HasPrefix(r, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func (c *Client) listReposPaged(ctx context.Context, endpoint string) ([]string, error) {
	var out []string
	for page := 1; page <= maxRepoPages; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("limit", strconv.Itoa(repoPageSize))

		body, err := c.doWithRetry(ctx, endpoint+"?"+q.Encode(), "list repos")
		if err != nil {
			return nil, err
		}
		var repos []wireRepository
		if err := json.Unmarshal(body, &repos); err != nil {
			return nil, fmt.Errorf("decode repos: %w", err)
		}
		for _, r := range repos {
			if r.Archived || r.Empty || r.FullName == "" {
				continue
			}
			out = append(out, r.FullName)
		}
		// No Link header and no X-Total-Count, so a short page is the terminator.
		if len(repos) < repoPageSize {
			return out, nil
		}
	}
	slog.Warn("repo discovery hit the page cap", "pages", maxRepoPages, "found", len(out))
	return out, nil
}
