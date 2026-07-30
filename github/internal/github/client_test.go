package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient("test-token")
	c.SetBaseURL(srv.URL)
	return c
}

// workflowsRoute serves the presence lookup for a repo that has workflows. A test
// exercising the runs probe needs it, because a repo with no workflows is skipped
// before the runs endpoint is ever reached.
func workflowsRoute(mux *http.ServeMux, repo string) {
	mux.HandleFunc("/repos/"+repo+"/actions/workflows", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(WorkflowsResponse{TotalCount: 1})
	})
}

func TestNewClient(t *testing.T) {
	c := NewClient("ghp_abc123")
	if c.token != "ghp_abc123" {
		t.Errorf("expected token ghp_abc123, got %s", c.token)
	}
	if c.baseURL != "https://api.github.com" {
		t.Errorf("expected default baseURL, got %s", c.baseURL)
	}
	if c.remaining != -1 {
		t.Errorf("expected remaining -1, got %d", c.remaining)
	}
}

func TestSetBaseURL(t *testing.T) {
	c := NewClient("token")
	c.SetBaseURL("http://localhost:9999")
	if c.baseURL != "http://localhost:9999" {
		t.Errorf("expected custom baseURL, got %s", c.baseURL)
	}
}

func TestGetInProgressRuns_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "in_progress" {
			t.Errorf("expected status=in_progress query param")
		}
		_ = json.NewEncoder(w).Encode(WorkflowRunsResponse{
			TotalCount: 1,
			WorkflowRuns: []WorkflowRun{
				{ID: 42, Name: "CI", Status: "in_progress", HeadBranch: "main"},
			},
		})
	})
	workflowsRoute(mux, "owner/repo")
	c := testClient(t, mux)

	runs, err := c.GetInProgressRuns(context.Background(), "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	if runs[0].ID != 42 {
		t.Errorf("expected run ID 42, got %d", runs[0].ID)
	}
	if runs[0].Name != "CI" {
		t.Errorf("expected run name CI, got %s", runs[0].Name)
	}
}

func TestGetInProgressRuns_Empty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(WorkflowRunsResponse{TotalCount: 0})
	})
	workflowsRoute(mux, "owner/repo")
	c := testClient(t, mux)

	runs, err := c.GetInProgressRuns(context.Background(), "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected 0 runs, got %d", len(runs))
	}
}

// The idle probe is the request the bridge makes most, so it must go out
// conditionally and a 304 must answer from cache. GitHub does not bill a 304
// against the primary rate limit, which is what makes polling every repo every
// pass affordable at all.
func TestGetInProgressRuns_ConditionalRequestAnswersFromCache(t *testing.T) {
	const etag = `W/"abc123"`
	var calls atomic.Int32
	var sawIfNoneMatch atomic.Value

	mux := http.NewServeMux()
	workflowsRoute(mux, "owner/repo")
	mux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		sawIfNoneMatch.Store(r.Header.Get("If-None-Match"))
		w.Header().Set("ETag", etag)
		if n > 1 {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_ = json.NewEncoder(w).Encode(WorkflowRunsResponse{
			TotalCount:   1,
			WorkflowRuns: []WorkflowRun{{ID: 7, Name: "CI", Status: "in_progress"}},
		})
	})
	c := testClient(t, mux)

	first, err := c.GetInProgressRuns(context.Background(), "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].ID != 7 {
		t.Fatalf("first probe = %+v, want one run with ID 7", first)
	}
	if got := sawIfNoneMatch.Load(); got != "" {
		t.Errorf("first probe sent If-None-Match %q, want none", got)
	}

	second, err := c.GetInProgressRuns(context.Background(), "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if got := sawIfNoneMatch.Load(); got != etag {
		t.Errorf("second probe sent If-None-Match %q, want %q", got, etag)
	}
	if len(second) != 1 || second[0].ID != 7 {
		t.Fatalf("304 answered %+v, want the cached run", second)
	}

	// The cached slice must not be handed out directly, or a caller reordering it
	// would corrupt every later answer.
	second[0].ID = 999
	third, err := c.GetInProgressRuns(context.Background(), "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if third[0].ID != 7 {
		t.Errorf("cache was mutated through a returned slice: ID %d", third[0].ID)
	}
}

// A repo with no workflow files answers the runs probe with an empty 200, exactly
// like a repo that simply has nothing running - so it takes the presence lookup to
// tell them apart, and once told, the runs probe must stop entirely.
func TestGetInProgressRuns_SkipsRepoWithNoWorkflows(t *testing.T) {
	var runCalls, workflowCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows", func(w http.ResponseWriter, _ *http.Request) {
		workflowCalls.Add(1)
		_ = json.NewEncoder(w).Encode(WorkflowsResponse{TotalCount: 0})
	})
	mux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		runCalls.Add(1)
		_ = json.NewEncoder(w).Encode(WorkflowRunsResponse{TotalCount: 0})
	})
	c := testClient(t, mux)

	for range 3 {
		runs, err := c.GetInProgressRuns(context.Background(), "owner/repo")
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 0 {
			t.Fatalf("expected no runs, got %d", len(runs))
		}
	}

	if got := runCalls.Load(); got != 0 {
		t.Errorf("polled the runs endpoint %d times for a repo with no workflows", got)
	}
	// Cached, so the presence lookup itself does not repeat either.
	if got := workflowCalls.Load(); got != 1 {
		t.Errorf("workflow presence looked up %d times, want 1 (cached)", got)
	}
}

// Actions disabled on a repo 404s the workflows endpoint. Same conclusion as no
// workflow files, and it must be cached rather than re-erroring on every pass.
func TestGetInProgressRuns_WorkflowsNotFoundTreatedAsNoActions(t *testing.T) {
	var workflowCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows", func(w http.ResponseWriter, _ *http.Request) {
		workflowCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	c := testClient(t, mux)

	for range 2 {
		runs, err := c.GetInProgressRuns(context.Background(), "owner/repo")
		if err != nil {
			t.Fatalf("a 404 from the workflows endpoint must not surface as an error: %v", err)
		}
		if len(runs) != 0 {
			t.Fatalf("expected no runs, got %d", len(runs))
		}
	}
	if got := workflowCalls.Load(); got != 1 {
		t.Errorf("workflow presence looked up %d times, want 1 (the 404 is cached)", got)
	}
}

// A negative answer has to expire, or a workflow added later is never noticed
// until the bridge restarts.
func TestHasWorkflows_NegativeAnswerExpires(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows", func(w http.ResponseWriter, _ *http.Request) {
		total := 0
		if calls.Add(1) > 1 {
			total = 1 // a workflow was added since the first look
		}
		_ = json.NewEncoder(w).Encode(WorkflowsResponse{TotalCount: total})
	})
	c := testClient(t, mux)

	has, err := c.hasWorkflows(context.Background(), "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("expected no workflows on the first look")
	}

	// Age the cached negative past its TTL.
	c.mu.Lock()
	e := c.workflows["owner/repo"]
	e.checkedAt = time.Now().Add(-noWorkflowsTTL - time.Second)
	c.workflows["owner/repo"] = e
	c.mu.Unlock()

	has, err = c.hasWorkflows(context.Background(), "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("expected the re-check to notice the new workflow")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("looked up %d times, want 2", got)
	}
}

// A positive answer is kept for the process lifetime: a repo that loses its
// workflows costs only what every repo used to cost.
func TestHasWorkflows_PositiveAnswerIsNotRelookedUp(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(WorkflowsResponse{TotalCount: 2})
	})
	c := testClient(t, mux)

	for range 3 {
		has, err := c.hasWorkflows(context.Background(), "owner/repo")
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Fatal("expected workflows")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("looked up %d times, want 1", got)
	}
}

func TestGetInProgressRuns_InvalidRepo(t *testing.T) {
	c := NewClient("test-token")
	_, err := c.GetInProgressRuns(context.Background(), "noslash")
	if err == nil {
		t.Fatal("expected error for invalid repo format")
	}
}

func TestGetJobs_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(JobsResponse{
			TotalCount: 2,
			Jobs: []Job{
				{ID: 1, Name: "Build", Status: "completed", Conclusion: "success"},
				{ID: 2, Name: "Test", Status: "in_progress"},
			},
		})
	})
	c := testClient(t, mux)

	jobs, err := c.GetJobs(context.Background(), "owner/repo", 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}
	if jobs[0].Name != "Build" {
		t.Errorf("expected first job Build, got %s", jobs[0].Name)
	}
}

func TestGetJobs_Pagination(t *testing.T) {
	var pageCount int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		atomic.AddInt32(&pageCount, 1)
		if page == "" || page == "1" {
			jobs := make([]Job, 100)
			for i := range jobs {
				jobs[i] = Job{ID: int64(i + 1), Name: "Job", Status: "queued"}
			}
			_ = json.NewEncoder(w).Encode(JobsResponse{TotalCount: 101, Jobs: jobs})
		} else {
			_ = json.NewEncoder(w).Encode(JobsResponse{
				TotalCount: 101,
				Jobs:       []Job{{ID: 101, Name: "LastJob", Status: "queued"}},
			})
		}
	})
	c := testClient(t, mux)

	jobs, err := c.GetJobs(context.Background(), "owner/repo", 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 101 {
		t.Fatalf("expected 101 jobs, got %d", len(jobs))
	}
	if atomic.LoadInt32(&pageCount) != 2 {
		t.Errorf("expected 2 page requests, got %d", pageCount)
	}
}

func TestGetJobs_InvalidRepo(t *testing.T) {
	c := NewClient("test-token")
	_, err := c.GetJobs(context.Background(), "noslash", 1)
	if err == nil {
		t.Fatal("expected error for invalid repo format")
	}
}

func TestGetLatestWorkflowRun_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("status") != "success" {
			t.Errorf("expected status=success, got %q", q.Get("status"))
		}
		if q.Get("branch") != "main" {
			t.Errorf("expected branch=main, got %q", q.Get("branch"))
		}
		if q.Get("per_page") != "1" {
			t.Errorf("expected per_page=1, got %q", q.Get("per_page"))
		}
		_ = json.NewEncoder(w).Encode(WorkflowRunsResponse{
			TotalCount: 1,
			WorkflowRuns: []WorkflowRun{
				{ID: 41, Name: "CI", Status: "completed", Conclusion: "success", WorkflowID: 99, HeadBranch: "main"},
			},
		})
	})
	c := testClient(t, mux)

	run, err := c.GetLatestWorkflowRun(context.Background(), "owner/repo", 99, "main", "success")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil {
		t.Fatal("expected a run, got nil")
	}
	if run.ID != 41 {
		t.Errorf("expected run ID 41, got %d", run.ID)
	}
}

func TestGetLatestWorkflowRun_Empty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(WorkflowRunsResponse{TotalCount: 0})
	})
	c := testClient(t, mux)

	run, err := c.GetLatestWorkflowRun(context.Background(), "owner/repo", 99, "main", "success")
	if err != nil {
		t.Fatal(err)
	}
	if run != nil {
		t.Fatalf("expected nil run for empty list, got %+v", run)
	}
}

func TestGetLatestWorkflowRun_InvalidRepo(t *testing.T) {
	c := NewClient("test-token")
	_, err := c.GetLatestWorkflowRun(context.Background(), "noslash", 1, "main", "success")
	if err == nil {
		t.Fatal("expected error for invalid repo format")
	}
}

func TestGetLatestWorkflowRun_APIError(t *testing.T) {
	mux := http.NewServeMux()
	// 4xx is a non-retryable clientError, so the call returns immediately
	// (a 5xx would trigger the retry backoff and slow the test).
	mux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	c := testClient(t, mux)

	run, err := c.GetLatestWorkflowRun(context.Background(), "owner/repo", 99, "main", "success")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
	if run != nil {
		t.Errorf("expected nil run on error, got %+v", run)
	}
}

func TestListRepos_FiltersArchivedAndDisabled(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(User{Login: "owner"})
	})
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]Repository{
			{FullName: "owner/active1"},
			{FullName: "owner/archived", Archived: true},
			{FullName: "owner/disabled", Disabled: true},
			{FullName: "owner/active2"},
		})
	})
	c := testClient(t, mux)

	// owner == token login -> /user/repos (includes private repos).
	repos, err := c.ListRepos(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("expected 2 active repos, got %d: %v", len(repos), repos)
	}
	if repos[0] != "owner/active1" || repos[1] != "owner/active2" {
		t.Errorf("unexpected repos: %v", repos)
	}
}

func TestListRepos_Empty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(User{Login: "owner"})
	})
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]Repository{})
	})
	c := testClient(t, mux)

	repos, err := c.ListRepos(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Fatalf("expected 0 repos, got %d", len(repos))
	}
}

// A different owner must hit the org endpoint (honoring the argument), not the
// token user's own repos.
func TestListRepos_OrgOwner(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(User{Login: "tokenuser"})
	})
	mux.HandleFunc("/orgs/some-org/repos", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]Repository{{FullName: "some-org/repo1"}})
	})
	mux.HandleFunc("/user/repos", func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("must not call /user/repos for a non-token owner")
	})
	c := testClient(t, mux)

	repos, err := c.ListRepos(context.Background(), "some-org")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0] != "some-org/repo1" {
		t.Errorf("expected [some-org/repo1], got %v", repos)
	}
}

// When the org endpoint 404s, discovery falls back to the user-repos endpoint.
func TestListRepos_FallsBackToUserEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(User{Login: "tokenuser"})
	})
	mux.HandleFunc("/orgs/someone/repos", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/users/someone/repos", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]Repository{{FullName: "someone/pub"}})
	})
	c := testClient(t, mux)

	repos, err := c.ListRepos(context.Background(), "someone")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0] != "someone/pub" {
		t.Errorf("expected [someone/pub], got %v", repos)
	}
}

// A 403 carrying rate-limit headers must be retried like a 429, not treated as
// a non-retryable client error.
func TestDoRequest_403RateLimitRetried(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/test", func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	c := testClient(t, mux)

	if _, err := c.doWithRetry(context.Background(), c.baseURL+"/test", "test"); err != nil {
		t.Fatalf("expected 403 rate-limit to be retried to success, got %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 attempts (retry after 403 rate-limit), got %d", calls.Load())
	}
}

func TestDoRequest_SetsAuthHeaders(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %s", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("expected application/vnd.github+json, got %s", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("expected 2022-11-28, got %s", got)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	})
	c := testClient(t, mux)

	_, err := c.doWithRetry(context.Background(), c.baseURL+"/test", "test")
	if err != nil {
		t.Fatal(err)
	}
}

func TestDoRequest_RateLimitRetry(t *testing.T) {
	var attempts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	})
	c := testClient(t, mux)

	_, err := c.doWithRetry(context.Background(), c.baseURL+"/test", "test")
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
}

func TestDoRequest_ClientErrorNoRetry(t *testing.T) {
	var attempts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(404)
	})
	c := testClient(t, mux)

	_, err := c.doWithRetry(context.Background(), c.baseURL+"/test", "test")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if atomic.LoadInt32(&attempts) != 1 {
		t.Errorf("expected 1 attempt (no retry for 4xx), got %d", attempts)
	}
}

func TestDoRequest_403NoRateLimitHeadersNoRetry(t *testing.T) {
	// A 403 WITHOUT rate-limit headers (bad token / insufficient scope) must
	// fail fast as a client error, never be retried as a rate limit - otherwise
	// auth failures turn into retry storms.
	var attempts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/test", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusForbidden)
	})
	c := testClient(t, mux)

	if _, err := c.doWithRetry(context.Background(), c.baseURL+"/test", "test"); err == nil {
		t.Fatal("expected error for plain 403")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected 1 attempt (no retry for header-less 403), got %d", got)
	}
}

func TestDoRequest_ServerErrorRetries(t *testing.T) {
	var attempts int32
	mux := http.NewServeMux()
	mux.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n <= 2 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	})
	c := testClient(t, mux)

	_, err := c.doWithRetry(context.Background(), c.baseURL+"/test", "test")
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

func TestDoRequest_ContextCancellation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	})
	c := testClient(t, mux)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := c.doWithRetry(ctx, c.baseURL+"/test", "test")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestRecordRateLimit_ParsesHeaders(t *testing.T) {
	const resetEpoch int64 = 1893456000 // 2030-01-01T00:00:00Z

	resp := &http.Response{
		Header: http.Header{
			"X-Ratelimit-Remaining": []string{"42"},
			"X-Ratelimit-Reset":     []string{strconv.FormatInt(resetEpoch, 10)},
		},
	}

	c := NewClient("token")
	c.recordRateLimit(resp)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.remaining != 42 {
		t.Errorf("expected remaining 42, got %d", c.remaining)
	}
	if got := c.resetAt.Unix(); got != resetEpoch {
		t.Errorf("expected resetAt epoch %d, got %d", resetEpoch, got)
	}
}

func TestRecordRateLimit_MissingHeaders(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	c := NewClient("token")
	c.recordRateLimit(resp)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.remaining != -1 {
		t.Errorf("expected remaining unchanged at -1, got %d", c.remaining)
	}
}

// The budget check must refuse rather than sleep.
func TestCheckBudget_RefusesImmediatelyWhenSpent(t *testing.T) {
	c := NewClient("token")
	resetAt := time.Now().Add(time.Hour)
	c.mu.Lock()
	c.remaining = 0
	c.resetAt = resetAt
	c.mu.Unlock()

	start := time.Now()
	err := c.checkBudget()
	elapsed := time.Since(start)

	var be *BudgetError
	if !errors.As(err, &be) {
		t.Fatalf("expected a BudgetError, got %v", err)
	}
	if !be.ResetAt.Equal(resetAt) {
		t.Errorf("ResetAt = %v, want %v", be.ResetAt, resetAt)
	}
	if elapsed > 10*time.Millisecond {
		t.Errorf("checkBudget blocked for %v; it must refuse, not wait", elapsed)
	}
}

// The reserve that keeps already-tracked runs affordable belongs to the poller, not
// here. The client must not apply one of its own, or the cards on screen would
// freeze at exactly the moment the reserve was meant to keep them alive.
func TestCheckBudget_Allows(t *testing.T) {
	tests := []struct {
		name      string
		remaining int
		resetIn   time.Duration
	}{
		{name: "one request left is still a request", remaining: 1, resetIn: time.Hour},
		{name: "well inside what the poller reserves", remaining: 49, resetIn: time.Hour},
		{name: "plenty", remaining: 1000, resetIn: time.Hour},
		{name: "unknown before the first response", remaining: -1, resetIn: time.Hour},
		// A spent window that has already rolled over is open again; the next
		// response re-reads the real numbers.
		{name: "spent but the window has passed", remaining: 0, resetIn: -time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient("token")
			c.mu.Lock()
			c.remaining = tc.remaining
			c.resetAt = time.Now().Add(tc.resetIn)
			c.mu.Unlock()

			if err := c.checkBudget(); err != nil {
				t.Errorf("refused with remaining=%d: %v", tc.remaining, err)
			}
		})
	}
}

func TestBudget_UnknownUntilFirstResponse(t *testing.T) {
	c := NewClient("token")
	if _, _, ok := c.Budget(); ok {
		t.Error("Budget reported a figure before any response was seen")
	}

	const resetEpoch int64 = 1893456000
	c.recordRateLimit(&http.Response{Header: http.Header{
		"X-Ratelimit-Remaining": []string{"4321"},
		"X-Ratelimit-Reset":     []string{strconv.FormatInt(resetEpoch, 10)},
	}})

	remaining, resetAt, ok := c.Budget()
	if !ok {
		t.Fatal("Budget still unknown after a response carrying the headers")
	}
	if remaining != 4321 {
		t.Errorf("remaining = %d, want 4321", remaining)
	}
	if resetAt.Unix() != resetEpoch {
		t.Errorf("resetAt = %d, want %d", resetAt.Unix(), resetEpoch)
	}
}

// A 304 carries the rate-limit headers like any other response, and the budget
// view has to track them - in the steady state almost every response is a 304, so
// ignoring them would leave the poller pacing against a stale number.
func TestRecordRateLimit_TracksNotModifiedResponses(t *testing.T) {
	mux := http.NewServeMux()
	workflowsRoute(mux, "owner/repo")
	mux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"e1"`)
		w.Header().Set("X-RateLimit-Remaining", "4000")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		if r.Header.Get("If-None-Match") != "" {
			w.Header().Set("X-RateLimit-Remaining", "3999")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_ = json.NewEncoder(w).Encode(WorkflowRunsResponse{TotalCount: 0})
	})
	c := testClient(t, mux)

	if _, err := c.GetInProgressRuns(context.Background(), "owner/repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetInProgressRuns(context.Background(), "owner/repo"); err != nil {
		t.Fatal(err)
	}
	remaining, _, ok := c.Budget()
	if !ok || remaining != 3999 {
		t.Errorf("remaining = %d (ok=%v), want 3999 from the 304's headers", remaining, ok)
	}
}

// The presence re-check must be conditional too. Without it the workflow filter
// would spend a billed request every TTL per skipped repo, to avoid runs probes that
// the ETag above has already made free - paying real budget for no budget saving.
func TestHasWorkflows_RecheckIsConditional(t *testing.T) {
	const etag = `W/"wf1"`
	var conditional, unconditional atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			conditional.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		unconditional.Add(1)
		_ = json.NewEncoder(w).Encode(WorkflowsResponse{TotalCount: 0})
	})
	c := testClient(t, mux)

	has, err := c.hasWorkflows(context.Background(), "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("expected no workflows")
	}

	// Two TTL expiries in a row: both re-checks must ride the ETag.
	for range 2 {
		c.mu.Lock()
		e := c.workflows["owner/repo"]
		e.checkedAt = time.Now().Add(-noWorkflowsTTL - time.Second)
		c.workflows["owner/repo"] = e
		c.mu.Unlock()

		has, err = c.hasWorkflows(context.Background(), "owner/repo")
		if err != nil {
			t.Fatal(err)
		}
		if has {
			t.Error("a 304 must preserve the cached answer, not flip it")
		}
	}

	if got := unconditional.Load(); got != 1 {
		t.Errorf("made %d billed lookups, want 1: the re-checks must be conditional", got)
	}
	if got := conditional.Load(); got != 2 {
		t.Errorf("made %d conditional re-checks, want 2", got)
	}
}

// Both caches are keyed by repo name, so without a sweep they would hold an entry
// for every repo ever seen - including ones since renamed, archived, or dropped from
// the owner.
func TestPruneCaches_DropsReposNothingAsksAbout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(User{Login: "owner"})
	})
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]Repository{{FullName: "owner/live"}})
	})
	c := testClient(t, mux)

	stale := time.Now().Add(-cacheRetention - time.Minute)
	fresh := time.Now()
	c.mu.Lock()
	c.runsCache["owner/gone"] = runsProbe{etag: `"x"`, usedAt: stale}
	c.runsCache["owner/live"] = runsProbe{etag: `"y"`, usedAt: fresh}
	c.workflows["owner/gone"] = workflowPresence{has: true, usedAt: stale}
	c.workflows["owner/live"] = workflowPresence{has: true, usedAt: fresh}
	c.mu.Unlock()

	// Discovery is where the sweep runs.
	if _, err := c.ListRepos(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.runsCache["owner/gone"]; ok {
		t.Error("runsCache kept an entry nothing has asked about")
	}
	if _, ok := c.workflows["owner/gone"]; ok {
		t.Error("workflows kept an entry nothing has asked about")
	}
	if _, ok := c.runsCache["owner/live"]; !ok {
		t.Error("runsCache dropped a live repo")
	}
	if _, ok := c.workflows["owner/live"]; !ok {
		t.Error("workflows dropped a live repo")
	}
}

func TestRateLimitError_String(t *testing.T) {
	e := &rateLimitError{retryAfter: 60 * time.Second, url: "https://example.com"}
	if got := e.Error(); got == "" {
		t.Error("expected non-empty error string")
	}
}

func TestClientError_String(t *testing.T) {
	e := &clientError{status: 404, url: "https://example.com"}
	if got := e.Error(); got == "" {
		t.Error("expected non-empty error string")
	}
}

// rateLimitRetryAfter must honor each header form and clamp parsed signals to
// [0, 15m]. A non-positive parsed value means "retry now" (NOT the 60s default,
// which applies only when no header parses) - this pins the clamp fix.
func TestRateLimitRetryAfter(t *testing.T) {
	const maxWait = 15 * time.Minute

	tests := []struct {
		name           string
		key            string
		value          string
		minWant        time.Duration
		maxWant        time.Duration
		wantFromHeader bool
	}{
		{
			name:           "retry-after numeric seconds",
			key:            "Retry-After",
			value:          "30",
			minWant:        30 * time.Second,
			maxWant:        30 * time.Second,
			wantFromHeader: true,
		},
		{
			// "Retry-After: 0" is authoritative: retry immediately, not 60s.
			name:           "retry-after zero retries now",
			key:            "Retry-After",
			value:          "0",
			minWant:        0,
			maxWant:        0,
			wantFromHeader: true,
		},
		{
			name:           "retry-after http-date near future",
			key:            "Retry-After",
			value:          time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat),
			minWant:        2*time.Minute - 10*time.Second,
			maxWant:        2 * time.Minute,
			wantFromHeader: true,
		},
		{
			// An HTTP-date already in the past -> the window is open -> retry now.
			name:           "retry-after http-date in the past",
			key:            "Retry-After",
			value:          time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat),
			minWant:        0,
			maxWant:        0,
			wantFromHeader: true,
		},
		{
			name:           "x-ratelimit-reset epoch in the past",
			key:            "X-RateLimit-Reset",
			value:          strconv.FormatInt(time.Now().Add(-5*time.Minute).Unix(), 10),
			minWant:        0,
			maxWant:        0,
			wantFromHeader: true,
		},
		{
			// A reset far beyond maxWait must be clamped, never parked.
			name:           "x-ratelimit-reset far future clamped to maxWait",
			key:            "X-RateLimit-Reset",
			value:          strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10),
			minWant:        maxWait,
			maxWant:        maxWait,
			wantFromHeader: true,
		},
		{
			// No usable header is the one case GitHub prescribes exponential backoff
			// for, so the caller has to be able to tell it apart. The duration is
			// meaningless here - rateLimitError.wait supplies its own.
			name:           "no headers reports nothing to trust",
			key:            "",
			value:          "",
			minWant:        0,
			maxWant:        0,
			wantFromHeader: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.key != "" {
				h.Set(tt.key, tt.value)
			}
			got, fromHeader := rateLimitRetryAfter(&http.Response{Header: h})
			if got < tt.minWant || got > tt.maxWant {
				t.Errorf("rateLimitRetryAfter() = %v, want in [%v, %v]", got, tt.minWant, tt.maxWant)
			}
			if fromHeader != tt.wantFromHeader {
				t.Errorf("fromHeader = %v, want %v", fromHeader, tt.wantFromHeader)
			}
		})
	}
}

// GitHub reserves exponential backoff for the ambiguous case: a rate-limit
// response with no usable timing, which is most likely a secondary limit. When it
// did say when to resume, waiting longer than that only costs freshness.
func TestRateLimitErrorWait(t *testing.T) {
	t.Run("a parsed header is taken at its word", func(t *testing.T) {
		e := &rateLimitError{retryAfter: 20 * time.Second, fromHeader: true}
		for n := 1; n <= 3; n++ {
			got := e.wait(n)
			// Jitter adds up to a quarter on top; it must not grow with the attempt.
			if got < 20*time.Second || got > 25*time.Second {
				t.Errorf("wait(%d) = %v, want ~20s regardless of attempt", n, got)
			}
		}
	})

	t.Run("no header grows from a minute and caps", func(t *testing.T) {
		e := &rateLimitError{} // no header, so retryAfter carries nothing
		for _, tc := range []struct {
			n      int
			lo, hi time.Duration
		}{
			{1, 60 * time.Second, 75 * time.Second},
			{2, 120 * time.Second, 150 * time.Second},
			{3, 240 * time.Second, 300 * time.Second},
			// However many attempts, never longer than the ceiling - and never
			// wrapping into a negative, which would mean no wait at all.
			{30, maxRateLimitWait, maxRateLimitWait + maxRateLimitWait/4},
		} {
			if got := e.wait(tc.n); got < tc.lo || got > tc.hi {
				t.Errorf("wait(%d) = %v, want in [%v, %v]", tc.n, got, tc.lo, tc.hi)
			}
		}
	})
}

func TestDoRequest_RateLimitDefault(t *testing.T) {
	// 429 without Retry-After header should use 60s default
	mux := http.NewServeMux()
	mux.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	})
	c := testClient(t, mux)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := c.doWithRetry(ctx, c.baseURL+"/test", "test")
	if err == nil {
		t.Fatal("expected error (context should timeout before 60s default retry)")
	}
}
