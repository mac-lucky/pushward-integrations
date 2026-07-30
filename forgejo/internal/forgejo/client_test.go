package forgejo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// testClient points a client at a stub instance. Note there is no SetBaseURL
// seam: the instance URL is a real config key, so tests configure it the same
// way production does.
func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "test-token", Options{
		Timeout:        2 * time.Second,
		LiveTimings:    true,
		HistoryTimings: true,
	})
}

func serveFixture(t *testing.T, name string) http.HandlerFunc {
	t.Helper()
	body := fixture(t, name)
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

func TestNormalizeBase(t *testing.T) {
	want := "https://git.example.com"
	for _, in := range []string{
		"https://git.example.com",
		"https://git.example.com/",
		"https://git.example.com/api/v1",
		"https://git.example.com/api/v1/",
		"  https://git.example.com/  ",
	} {
		if got := normalizeBase(in); got != want {
			t.Errorf("normalizeBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewClientAppendsAPIBaseOnce(t *testing.T) {
	for _, in := range []string{"https://git.example.com", "https://git.example.com/api/v1"} {
		c := NewClient(in, "t", Options{})
		if c.apiBase != "https://git.example.com/api/v1" {
			t.Errorf("apiBase from %q = %q", in, c.apiBase)
		}
		if c.WebBase() != "https://git.example.com" {
			t.Errorf("webBase from %q = %q", in, c.WebBase())
		}
	}
}

func TestSetsTokenAuthHeader(t *testing.T) {
	var gotAuth, gotVersionHdr string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotVersionHdr = r.Header.Get("X-GitHub-Api-Version")
		_, _ = w.Write([]byte(`{"version":"16.0.1"}`))
	}))
	if _, err := c.GetVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "token test-token" {
		t.Errorf("Authorization = %q, want the Forgejo-native token scheme", gotAuth)
	}
	if gotVersionHdr != "" {
		t.Errorf("sent X-GitHub-Api-Version = %q; Forgejo has no such header", gotVersionHdr)
	}
}

// TestGetActiveRunsQuery pins the idle probe's filter, including that both
// statuses go out as repeated parameters and that blocked is not among them.
func TestGetActiveRunsQuery(t *testing.T) {
	var got url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write(fixture(t, "runs_active_empty.json"))
	})
	c := testClient(t, mux)

	runs, err := c.GetActiveRuns(context.Background(), "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("expected no runs, got %d", len(runs))
	}
	if got.Get("page") != "1" {
		t.Errorf("page = %q, want 1", got.Get("page"))
	}
	if got.Get("limit") == "" {
		t.Error("limit must be sent")
	}
	statuses := got["status"]
	if len(statuses) != 2 {
		t.Fatalf("status params = %v, want two repeated values", statuses)
	}
	for _, s := range statuses {
		if s == StatusBlocked {
			t.Error("the idle probe must not ask for blocked runs")
		}
	}
}

func TestGetActiveRunsInvalidRepo(t *testing.T) {
	c := testClient(t, http.NotFoundHandler())
	for _, repo := range []string{"noslash", "", "/app", "acme/"} {
		if _, err := c.GetActiveRuns(context.Background(), repo); err == nil {
			t.Errorf("expected an error for repo %q", repo)
		}
	}
}

func TestGetRunAndJobs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/39", serveFixture(t, "run_success.json"))
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/39/jobs", serveFixture(t, "jobs_matrix.json"))
	c := testClient(t, mux)

	run, err := c.GetRun(context.Background(), "acme/app", 39)
	if err != nil {
		t.Fatal(err)
	}
	if run.IndexInRepo != 33 {
		t.Errorf("index_in_repo = %d", run.IndexInRepo)
	}

	jobs, err := c.GetJobs(context.Background(), "acme/app", 39)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 4 {
		t.Errorf("got %d jobs, want 4", len(jobs))
	}
}

func TestGetJobsUnknownRunIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	if _, err := c.GetJobs(context.Background(), "acme/app", 999999); err == nil {
		t.Fatal("expected an error")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("made %d requests, want 1 - a 4xx must fail fast", n)
	}
}

// TestGetLatestFinishedRunSendsFullRef is the test that would have caught the
// bare-ref bug: filtering on "master" returns an empty list with a 200, so the
// seed would silently never find a prior run.
func TestGetLatestFinishedRunSendsFullRef(t *testing.T) {
	var refs []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("ref")
		refs = append(refs, ref)
		// A handler that only answers the fully-qualified form, exactly like the
		// real instance.
		if ref != "refs/heads/master" {
			_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"total_count":1,"workflow_runs":[` + string(fixture(t, "run_success.json")) + `]}`))
	})
	c := testClient(t, mux)

	run, err := c.GetLatestFinishedRun(context.Background(), "acme/app", "tofu.yml", "master")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil {
		t.Fatal("no run found; the ref filter was probably sent bare")
	}
	for _, ref := range refs {
		if ref != "refs/heads/master" {
			t.Errorf("sent ref %q, want the fully-qualified form", ref)
		}
	}
}

func TestFullRef(t *testing.T) {
	cases := map[string]string{
		"master":           "refs/heads/master",
		"main":             "refs/heads/main",
		"refs/heads/main":  "refs/heads/main",
		"refs/tags/v1.0.0": "refs/tags/v1.0.0",
		"feature/thing":    "refs/heads/feature/thing",
		"":                 "",
	}
	for in, want := range cases {
		if got := fullRef(in); got != want {
			t.Errorf("fullRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGetLatestFinishedRunFallsBackToOtherTerminalStatuses covers the missing
// "completed" umbrella: pass one asks for success, pass two enumerates the rest.
func TestGetLatestFinishedRunFallsBackToOtherTerminalStatuses(t *testing.T) {
	var passes [][]string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		statuses := r.URL.Query()["status"]
		passes = append(passes, statuses)
		if len(statuses) == 1 && statuses[0] == StatusSuccess {
			_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"total_count":1,"workflow_runs":[` + string(fixture(t, "run_failure.json")) + `]}`))
	})
	c := testClient(t, mux)

	run, err := c.GetLatestFinishedRun(context.Background(), "acme/app", "tofu.yml", "master")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil {
		t.Fatal("expected the fallback pass to find a run")
	}
	if len(passes) != 2 {
		t.Fatalf("made %d passes, want 2", len(passes))
	}
	if len(passes[1]) != 3 {
		t.Errorf("the fallback pass sent %v, want the three remaining terminal statuses", passes[1])
	}
}

func TestGetLatestFinishedRunShortCircuits(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	}))
	for _, tc := range []struct{ workflow, branch string }{
		{"", "master"}, {"tofu.yml", ""}, {"", ""},
	} {
		run, err := c.GetLatestFinishedRun(context.Background(), "acme/app", tc.workflow, tc.branch)
		if err != nil || run != nil {
			t.Errorf("workflow=%q branch=%q: got (%v, %v)", tc.workflow, tc.branch, run, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("made %d requests, want 0", n)
	}
}

// TestForbiddenIsNotRetried is a deliberate divergence from the github bridge,
// which treats a 403 carrying rate-limit headers as retryable. Forgejo sends no
// such headers and answers unauthenticated or under-scoped requests with 403, so
// retrying would just hammer.
func TestForbiddenIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	if _, err := c.GetVersion(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("made %d requests, want 1", n)
	}
}

func TestTooManyRequestsRetriesHonoringRetryAfter(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"version":"16.0.1"}`))
	}))
	v, err := c.GetVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "16.0.1" {
		t.Errorf("version = %q", v)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("made %d requests, want 2", n)
	}
}

func TestServerErrorRetries(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"version":"16.0.1"}`))
	}))
	if _, err := c.GetVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("made %d requests, want 3", n)
	}
}

func TestContextCancellation(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.GetVersion(ctx); err == nil {
		t.Fatal("expected an error")
	}
}

func TestRetryAfter(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"absent", "", 60 * time.Second},
		{"delta seconds", "30", 30 * time.Second},
		{"zero means now", "0", 0},
		{"negative means now", "-5", 0},
		{"absurd is clamped", "99999", 15 * time.Minute},
		{"unparseable falls back", "soon", 60 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			if tc.header != "" {
				resp.Header.Set("Retry-After", tc.header)
			}
			if got := retryAfter(resp); got != tc.want {
				t.Errorf("retryAfter(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}

	// An HTTP-date already in the past means the window is open again.
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	if got := retryAfter(resp); got != 0 {
		t.Errorf("a past date gave %v, want 0", got)
	}
}

// TestListReposToleratesOrgForbidden is the second divergence from github: a
// token without read:organization is refused with 403 rather than told the org
// does not exist, so both statuses must fall through to the user endpoint.
func TestListReposToleratesOrgForbidden(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var hitUserEndpoint bool
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/user", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"login":"someone-else"}`))
			})
			mux.HandleFunc("/api/v1/orgs/acme/repos", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write(fixture(t, "orgs_repos_403.json"))
			})
			mux.HandleFunc("/api/v1/users/acme/repos", func(w http.ResponseWriter, _ *http.Request) {
				hitUserEndpoint = true
				_, _ = w.Write(fixture(t, "repos_page.json"))
			})
			c := testClient(t, mux)

			repos, err := c.ListRepos(context.Background(), "acme")
			if err != nil {
				t.Fatal(err)
			}
			if !hitUserEndpoint {
				t.Error("expected the fallback to the user endpoint")
			}
			// The archived and empty repos are dropped.
			want := []string{"acme/app", "acme/infra"}
			if len(repos) != len(want) {
				t.Fatalf("repos = %v, want %v", repos, want)
			}
			for i, r := range repos {
				if r != want[i] {
					t.Errorf("repos[%d] = %q, want %q", i, r, want[i])
				}
			}
		})
	}
}

// TestListReposUsesUserEndpointForOwnLogin covers the homelab case: /user/repos
// returns everything the token can reach, including repos owned by others.
func TestListReposUsesUserEndpointForOwnLogin(t *testing.T) {
	var hitOwn bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"acme"}`))
	})
	mux.HandleFunc("/api/v1/user/repos", func(w http.ResponseWriter, _ *http.Request) {
		hitOwn = true
		_, _ = w.Write(fixture(t, "repos_page.json"))
	})
	mux.HandleFunc("/api/v1/orgs/acme/repos", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("must not query the org endpoint for the token's own login")
		w.WriteHeader(http.StatusForbidden)
	})
	c := testClient(t, mux)

	if _, err := c.ListRepos(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if !hitOwn {
		t.Error("expected /user/repos")
	}
}

// TestListReposPageCap stops a server that ignores `page` from spinning the loop
// forever - there is no Link header to terminate on.
func TestListReposPageCap(t *testing.T) {
	var calls atomic.Int32
	full := `[`
	for i := range repoPageSize {
		if i > 0 {
			full += ","
		}
		full += `{"full_name":"acme/r","html_url":"https://x/acme/r"}`
	}
	full += `]`

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"acme"}`))
	})
	mux.HandleFunc("/api/v1/user/repos", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(full)) // always a full page: never terminates naturally
	})
	c := testClient(t, mux)

	if _, err := c.ListRepos(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	if n := int(calls.Load()); n != maxRepoPages {
		t.Errorf("made %d page requests, want the cap of %d", n, maxRepoPages)
	}
}

func TestAuthenticatedLoginIsCached(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"login":"someone"}`))
	}))
	for range 3 {
		if _, err := c.authenticatedLogin(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("made %d requests, want 1", n)
	}
}

// TestListReposWorksWithARestrictedToken reproduces the failure a real
// deployment hit: a token that cannot call /user at all. Discovery used to gate
// /user/repos behind that lookup, so it fell through to the owner-scoped
// endpoints, got 403 from both, and crashlooped the bridge at startup.
func TestListReposWorksWithARestrictedToken(t *testing.T) {
	var triedOwnerEndpoints bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden) // no read:user scope
	})
	mux.HandleFunc("/api/v1/user/repos", serveFixture(t, "repos_page.json"))
	mux.HandleFunc("/api/v1/orgs/", func(w http.ResponseWriter, _ *http.Request) {
		triedOwnerEndpoints = true
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/api/v1/users/", func(w http.ResponseWriter, _ *http.Request) {
		triedOwnerEndpoints = true
		w.WriteHeader(http.StatusForbidden)
	})
	c := testClient(t, mux)

	repos, err := c.ListRepos(context.Background(), "acme")
	if err != nil {
		t.Fatalf("discovery failed for a token that can read its own repos: %v", err)
	}
	want := []string{"acme/app", "acme/infra"} // archived and empty dropped
	if len(repos) != len(want) {
		t.Fatalf("repos = %v, want %v", repos, want)
	}
	if triedOwnerEndpoints {
		t.Error("owner-scoped endpoints were queried even though /user/repos answered")
	}
}

// TestListReposNarrowsToTheRequestedOwner: /user/repos returns everything the
// token can reach, which on a self-hosted instance routinely includes other
// people's repos.
func TestListReposNarrowsToTheRequestedOwner(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"tokenowner"}`)) // not the requested owner
	})
	mux.HandleFunc("/api/v1/user/repos", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"full_name":"acme/app"},{"full_name":"other/thing"}]`))
	})
	c := testClient(t, mux)

	repos, err := c.ListRepos(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0] != "acme/app" {
		t.Errorf("repos = %v, want just acme/app", repos)
	}
}

// TestListReposOwnLoginKeepsEverything: when the owner IS the token's login,
// every reachable repo counts - including repos another account owns, which is
// exactly the homelab shape (one account owns the repos, another holds the
// token).
func TestListReposOwnLoginKeepsEverything(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"tokenowner"}`))
	})
	mux.HandleFunc("/api/v1/user/repos", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"full_name":"someoneelse/app"},{"full_name":"someoneelse/infra"}]`))
	})
	c := testClient(t, mux)

	repos, err := c.ListRepos(context.Background(), "tokenowner")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Errorf("repos = %v, want both repos the token can reach", repos)
	}
}

func TestFilterByOwner(t *testing.T) {
	in := []string{"acme/app", "acme/infra", "other/thing"}
	if got := filterByOwner(in, "acme"); len(got) != 2 {
		t.Errorf("filterByOwner(acme) = %v", got)
	}
	if got := filterByOwner(in, "nobody"); got != nil {
		t.Errorf("filterByOwner(nobody) = %v, want nil", got)
	}
	if got := filterByOwner(in, ""); got != nil {
		t.Errorf("filterByOwner(empty) = %v, want nil", got)
	}
	// A prefix match must not treat "acme2" as "acme".
	if got := filterByOwner([]string{"acme2/app"}, "acme"); got != nil {
		t.Errorf("filterByOwner matched a longer owner: %v", got)
	}
}
