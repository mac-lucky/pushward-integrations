package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		json.NewEncoder(w).Encode(WorkflowRunsResponse{
			TotalCount: 1,
			WorkflowRuns: []WorkflowRun{
				{ID: 42, Name: "CI", Status: "in_progress", HeadBranch: "main"},
			},
		})
	})
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
		json.NewEncoder(w).Encode(WorkflowRunsResponse{TotalCount: 0})
	})
	c := testClient(t, mux)

	runs, err := c.GetInProgressRuns(context.Background(), "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected 0 runs, got %d", len(runs))
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
		json.NewEncoder(w).Encode(JobsResponse{
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
			json.NewEncoder(w).Encode(JobsResponse{TotalCount: 101, Jobs: jobs})
		} else {
			json.NewEncoder(w).Encode(JobsResponse{
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

func TestListRepos_FiltersArchivedAndDisabled(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Repository{
			{FullName: "owner/active1"},
			{FullName: "owner/archived", Archived: true},
			{FullName: "owner/disabled", Disabled: true},
			{FullName: "owner/active2"},
		})
	})
	c := testClient(t, mux)

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
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]Repository{})
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
		w.Write([]byte(`{}`))
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
		w.Write([]byte(`{}`))
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
		w.Write([]byte(`{}`))
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
	resetTime := time.Now().Add(5 * time.Minute).Unix()

	resp := &http.Response{
		Header: http.Header{
			"X-Ratelimit-Remaining": []string{"42"},
			"X-Ratelimit-Reset":     []string{time.Unix(resetTime, 0).Format("1136239445")},
		},
	}
	// Use the epoch format that strconv.ParseInt expects
	resp.Header.Set("X-RateLimit-Reset", "1893456000")

	c := NewClient("token")
	c.recordRateLimit(resp)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.remaining != 42 {
		t.Errorf("expected remaining 42, got %d", c.remaining)
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

func TestWaitForRateLimit_WaitsWhenLow(t *testing.T) {
	c := NewClient("token")
	c.mu.Lock()
	c.remaining = 10
	c.resetAt = time.Now().Add(100 * time.Millisecond)
	c.mu.Unlock()

	start := time.Now()
	err := c.waitForRateLimit(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if elapsed < 50*time.Millisecond {
		t.Errorf("expected to wait, but only waited %v", elapsed)
	}
}

func TestWaitForRateLimit_SkipsWhenPlenty(t *testing.T) {
	c := NewClient("token")
	c.mu.Lock()
	c.remaining = 1000
	c.resetAt = time.Now().Add(time.Hour)
	c.mu.Unlock()

	start := time.Now()
	err := c.waitForRateLimit(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 10*time.Millisecond {
		t.Errorf("expected no wait, but waited %v", elapsed)
	}
}

func TestWaitForRateLimit_SkipsWhenUnknown(t *testing.T) {
	c := NewClient("token")
	// remaining is -1 (unknown) by default

	start := time.Now()
	err := c.waitForRateLimit(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 10*time.Millisecond {
		t.Errorf("expected no wait, but waited %v", elapsed)
	}
}

func TestWaitForRateLimit_RespectsContextCancel(t *testing.T) {
	c := NewClient("token")
	c.mu.Lock()
	c.remaining = 10
	c.resetAt = time.Now().Add(time.Hour)
	c.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.waitForRateLimit(ctx)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
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
