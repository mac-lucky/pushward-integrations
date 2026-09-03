package poller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/github/internal/config"
	ghclient "github.com/mac-lucky/pushward-integrations/github/internal/github"
	"github.com/mac-lucky/pushward-integrations/shared/ci"
	"github.com/mac-lucky/pushward-integrations/shared/cipoll"
	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const testRepo = "owner/repo"

func testConfig() *config.Config {
	cfg := &config.Config{
		GitHub: config.GitHubConfig{Repos: []string{testRepo}},
		PushWard: sharedconfig.PushWardConfig{
			Priority:       1,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
		Polling: sharedconfig.DefaultPollingConfig(),
		// Mirrors the production default so tests exercise the shipped behavior.
		Render: sharedconfig.DefaultRenderConfig(),
	}
	// As Load leaves it: a resolved active tier, never the zero one the constructor
	// alone returns.
	cfg.Polling.ApplyActiveDefault()
	return cfg
}

func mockGitHubClient(t *testing.T, handler http.Handler) *ghclient.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := ghclient.NewClient("test-token")
	c.SetBaseURL(srv.URL)
	return c
}

func testForge(t *testing.T, handler http.Handler) *forge {
	t.Helper()
	return &forge{gh: mockGitHubClient(t, handler)}
}

// hasWorkflowsRoute answers the workflow-presence lookup the client makes before it
// will poll a repo for runs at all. Repos with no workflows are skipped, so a mux
// without this route describes a repo the bridge would ignore.
func hasWorkflowsRoute(mux *http.ServeMux) {
	mux.HandleFunc("/repos/"+testRepo+"/actions/workflows", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowsResponse{TotalCount: 1})
	})
}

// --- wire conversion ---

func TestToRun(t *testing.T) {
	created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	got := toRun(testRepo, ghclient.WorkflowRun{
		ID:         42,
		Name:       "CI",
		Status:     ci.StatusInProgress,
		Conclusion: "",
		CreatedAt:  created,
		HeadBranch: "main",
		WorkflowID: 99,
		HTMLURL:    "https://github.com/owner/repo/actions/runs/42",
	})

	want := cipoll.Run{
		ID:          42,
		Name:        "CI",
		WorkflowKey: "99",
		Status:      ci.StatusInProgress,
		RawStatus:   ci.StatusInProgress,
		HeadBranch:  "main",
		CreatedAt:   created,
		HTMLURL:     "https://github.com/owner/repo/actions/runs/42",
		RepoURL:     "https://github.com/owner/repo",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("toRun mismatch:\ngot  %+v\nwant %+v", got, want)
	}
}

// A run with no workflow id cannot target a workflow. The key has to come back
// blank so the shared loop short-circuits, rather than "0" which would send a
// lookup for a workflow that does not exist.
func TestWorkflowKey(t *testing.T) {
	if got := workflowKey(0); got != "" {
		t.Errorf("workflowKey(0) = %q, want blank so the seed short-circuits", got)
	}
	if got := workflowKey(99); got != "99" {
		t.Errorf("workflowKey(99) = %q, want %q", got, "99")
	}
}

// Terminal reads the normalized status, so it has to agree with what GitHub puts
// on the wire for a run that has stopped.
func TestToRunTerminal(t *testing.T) {
	done := toRun(testRepo, ghclient.WorkflowRun{ID: 1, Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess})
	if !done.Terminal() {
		t.Error("a completed run must read as terminal")
	}
	live := toRun(testRepo, ghclient.WorkflowRun{ID: 1, Status: ci.StatusInProgress})
	if live.Terminal() {
		t.Error("an in-progress run must not read as terminal")
	}
}

// --- Outcome ---

func TestOutcome(t *testing.T) {
	tests := []struct {
		name       string
		conclusion string
		anyFailed  bool
		wantState  string
		wantColor  string
	}{
		{name: "success", conclusion: ci.ConclusionSuccess, wantState: cipoll.OutcomeSuccess, wantColor: pushward.ColorGreen},
		{name: "failure", conclusion: ci.ConclusionFailure, wantState: cipoll.OutcomeFailed, wantColor: pushward.ColorRed},
		{name: "cancelled counts as failed", conclusion: ci.ConclusionCancelled, wantState: cipoll.OutcomeFailed, wantColor: pushward.ColorRed},
		{name: "timed out counts as failed", conclusion: ci.ConclusionTimedOut, wantState: cipoll.OutcomeFailed, wantColor: pushward.ColorRed},
		// Skipped is deliberately not a failure.
		{name: "skipped", conclusion: ci.ConclusionSkipped, wantState: cipoll.OutcomeSuccess, wantColor: pushward.ColorGreen},
		// With no conclusion the ladder's view of the jobs is the only signal.
		{name: "blank falls back to the jobs", conclusion: "", anyFailed: true, wantState: cipoll.OutcomeFailed, wantColor: pushward.ColorRed},
		{name: "blank with healthy jobs", conclusion: "", anyFailed: false, wantState: cipoll.OutcomeSuccess, wantColor: pushward.ColorGreen},
	}
	f := &forge{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state, color := f.Outcome(cipoll.Run{Conclusion: tc.conclusion}, tc.anyFailed)
			if state != tc.wantState || color != tc.wantColor {
				t.Errorf("Outcome = %q/%q, want %q/%q", state, color, tc.wantState, tc.wantColor)
			}
		})
	}
}

// --- BaselineJobs ---

func jobsResponse(jobs ...ghclient.Job) ghclient.JobsResponse {
	return ghclient.JobsResponse{TotalCount: len(jobs), Jobs: jobs}
}

func runsResponse(runs ...ghclient.WorkflowRun) ghclient.WorkflowRunsResponse {
	return ghclient.WorkflowRunsResponse{TotalCount: len(runs), WorkflowRuns: runs}
}

// The successful run is preferred: it ran the whole DAG, so its group count is
// the most accurate seed.
func TestBaselineJobs_PrefersTheSuccessfulRun(t *testing.T) {
	var statuses []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, r *http.Request) {
		statuses = append(statuses, r.URL.Query().Get("status"))
		_ = json.NewEncoder(w).Encode(runsResponse(ghclient.WorkflowRun{ID: 41, WorkflowID: 99, HeadBranch: "main"}))
	})
	mux.HandleFunc("/repos/owner/repo/actions/runs/41/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jobsResponse(
			ghclient.Job{ID: 1, Name: "Lint", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess},
			ghclient.Job{ID: 2, Name: "Build", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess},
		))
	})

	base, err := testForge(t, mux).BaselineJobs(
		context.Background(), testRepo,
		cipoll.Run{WorkflowKey: "99", HeadBranch: "main"}, "main", true)
	if err != nil {
		t.Fatalf("expected a usable seed: %v", err)
	}
	if base.RunID != 41 {
		t.Errorf("RunID = %d, want 41", base.RunID)
	}
	if len(base.Jobs) != 2 {
		t.Errorf("expected 2 jobs, got %d", len(base.Jobs))
	}
	if want := []string{ci.ConclusionSuccess}; !reflect.DeepEqual(statuses, want) {
		t.Errorf("lookups = %v, want only %v: a hit must not trigger the fallback", statuses, want)
	}
}

// With no successful run on the branch, the last completed run (a failure that
// still ran the full DAG) seeds the shape.
func TestBaselineJobs_FallsBackToAnyCompletedRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("status") {
		case ci.ConclusionSuccess:
			_ = json.NewEncoder(w).Encode(runsResponse())
		case ci.StatusCompleted:
			_ = json.NewEncoder(w).Encode(runsResponse(ghclient.WorkflowRun{ID: 40, WorkflowID: 99, HeadBranch: "feature"}))
		default:
			t.Errorf("unexpected status filter %q", r.URL.Query().Get("status"))
		}
	})
	mux.HandleFunc("/repos/owner/repo/actions/runs/40/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jobsResponse(
			ghclient.Job{ID: 1, Name: "Build", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess},
			ghclient.Job{ID: 2, Name: "Test", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess},
			ghclient.Job{ID: 3, Name: "Scan", Status: ci.StatusCompleted, Conclusion: ci.ConclusionFailure},
			ghclient.Job{ID: 4, Name: "Publish", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSkipped},
		))
	})

	base, err := testForge(t, mux).BaselineJobs(
		context.Background(), testRepo,
		cipoll.Run{WorkflowKey: "99", HeadBranch: "feature"}, "feature", true)
	if err != nil {
		t.Fatalf("expected the completed run to seed the shape: %v", err)
	}
	if base.RunID != 40 {
		t.Errorf("RunID = %d, want 40", base.RunID)
	}
	if len(base.Jobs) != 4 {
		t.Errorf("expected 4 jobs, got %d", len(base.Jobs))
	}
}

// The success lookup failing says nothing about whether a successful run exists,
// so the seed must abort outright rather than accept a possibly-truncated
// completed run in its place.
func TestBaselineJobs_AbortsOnTheFirstLookupError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") == ci.StatusCompleted {
			t.Error("the completed lookup must not run after the success lookup errored")
		}
		w.WriteHeader(http.StatusNotFound)
	})

	if _, err := testForge(t, mux).BaselineJobs(
		context.Background(), testRepo,
		cipoll.Run{WorkflowKey: "99", HeadBranch: "main"}, "main", true); err == nil {
		t.Error("expected the seed to abort with an error")
	}
}

func TestBaselineJobs_NoPriorRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(runsResponse())
	})

	base, err := testForge(t, mux).BaselineJobs(
		context.Background(), testRepo,
		cipoll.Run{WorkflowKey: "99", HeadBranch: "main"}, "main", true)
	// No prior run is not an error - the caller just keeps its live scan.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(base.Jobs) != 0 {
		t.Errorf("expected an empty baseline, got %d jobs", len(base.Jobs))
	}
}

// A malformed key must not become a lookup for workflow 0. The shared loop
// short-circuits a blank key before reaching here, so this guards the adapter
// against a caller that does not.
func TestBaselineJobs_RejectsAMalformedWorkflowKey(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub call: %s", r.URL.Path)
	})
	f := testForge(t, mux)

	for _, key := range []string{"", "not-a-number"} {
		if _, err := f.BaselineJobs(context.Background(), testRepo,
			cipoll.Run{WorkflowKey: key, HeadBranch: "main"}, "main", true); err == nil {
			t.Errorf("expected an error for WorkflowKey %q", key)
		}
	}
}

// --- ActiveRuns / GetRun / LiveJobs passthrough ---

func TestActiveRunsAndLiveJobs(t *testing.T) {
	mux := http.NewServeMux()
	hasWorkflowsRoute(mux)
	mux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(runsResponse(ghclient.WorkflowRun{
			ID: 42, Name: "CI", Status: ci.StatusInProgress, HeadBranch: "main", WorkflowID: 99,
			HTMLURL: "https://github.com/owner/repo/actions/runs/42",
		}))
	})
	mux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jobsResponse(
			ghclient.Job{
				ID: 1, Name: "Build", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess,
				StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:00:30Z",
			},
		))
	})
	mux.HandleFunc("/repos/owner/repo/actions/runs/42", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRun{
			ID: 42, Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess,
		})
	})
	f := testForge(t, mux)
	ctx := context.Background()

	runs, err := f.ActiveRuns(ctx, testRepo)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != 42 || runs[0].WorkflowKey != "99" {
		t.Fatalf("unexpected active runs: %+v", runs)
	}
	if runs[0].RepoURL != "https://github.com/owner/repo" {
		t.Errorf("RepoURL = %q, want the composed repo link", runs[0].RepoURL)
	}

	jobs, err := f.LiveJobs(ctx, testRepo, 42)
	if err != nil {
		t.Fatal(err)
	}
	// The timestamps have to survive the conversion: they are what sizes the pills
	// and anchors the live window.
	if len(jobs) != 1 || jobs[0].StartedAt.IsZero() || jobs[0].CompletedAt.IsZero() {
		t.Fatalf("unexpected live jobs: %+v", jobs)
	}

	run, err := f.GetRun(ctx, testRepo, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Terminal() || run.Conclusion != ci.ConclusionSuccess {
		t.Errorf("unexpected run re-read: %+v", run)
	}
}

// The adapter must forward the client's rate-limit view, not shadow it. When this
// lived on an optional interface the adapter did not satisfy, every pacing decision
// in the shared loop silently became a no-op with nothing to show for it.
func TestBudgetReachesTheClient(t *testing.T) {
	mux := http.NewServeMux()
	hasWorkflowsRoute(mux)
	mux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4242")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		_ = json.NewEncoder(w).Encode(runsResponse())
	})
	f := testForge(t, mux)

	if _, _, ok := f.Budget(); ok {
		t.Error("Budget reported a figure before any response was seen")
	}
	if _, err := f.ActiveRuns(context.Background(), testRepo); err != nil {
		t.Fatal(err)
	}
	remaining, resetAt, ok := f.Budget()
	if !ok {
		t.Fatal("Budget still unknown after a response carrying the headers")
	}
	if remaining != 4242 {
		t.Errorf("remaining = %d, want 4242", remaining)
	}
	if resetAt.IsZero() {
		t.Error("resetAt was not carried through")
	}
}

func TestListRepos(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.User{Login: "testowner"})
	})
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]ghclient.Repository{
			{FullName: "testowner/one"},
			{FullName: "testowner/two"},
		})
	})

	repos, err := testForge(t, mux).ListRepos(context.Background(), "testowner")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Errorf("expected 2 repos, got %d: %v", len(repos), repos)
	}
}

// --- end-to-end ---

// TestNewEndToEnd is the seam test: a real config, a real GitHub client and a
// real shared poller, driven through one full poll. The orchestration is covered
// in shared/cipoll and the wire conversion above, so what this proves is that the
// wiring between them is right - options reach the poller, the adapter reaches
// the API, and the seed frame carries the shape the prior run implies.
func TestNewEndToEnd(t *testing.T) {
	mux := http.NewServeMux()
	hasWorkflowsRoute(mux)
	// The live run has revealed only its first wave.
	mux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(runsResponse(ghclient.WorkflowRun{
			ID: 42, Name: "CI", Status: ci.StatusInProgress, HeadBranch: "main", WorkflowID: 99,
			HTMLURL: "https://github.com/owner/repo/actions/runs/42",
		}))
	})
	// Build started 10s ago against a 300s estimate, so its window is still well
	// open and the active tick can anchor it.
	buildStart := time.Now().Add(-10 * time.Second).UTC().Truncate(time.Second)
	mux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jobsResponse(
			ghclient.Job{
				ID: 1, Name: "Lint", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess,
			},
			ghclient.Job{
				ID: 2, Name: "Build", Status: ci.StatusInProgress,
				StartedAt: buildStart.Format(time.RFC3339),
			},
		))
	})
	// The prior successful run revealed three groups, one of them a matrix.
	mux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(runsResponse(ghclient.WorkflowRun{ID: 41, WorkflowID: 99, HeadBranch: "main"}))
	})
	mux.HandleFunc("/repos/owner/repo/actions/runs/41/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jobsResponse(
			ghclient.Job{
				ID: 1, Name: "Lint", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess,
				StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:00:05Z",
			},
			ghclient.Job{
				ID: 2, Name: "Build (ubuntu)", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess,
				StartedAt: "2026-01-01T00:00:05Z", CompletedAt: "2026-01-01T00:05:05Z",
			},
			ghclient.Job{
				ID: 3, Name: "Build (macos)", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess,
				StartedAt: "2026-01-01T00:00:05Z", CompletedAt: "2026-01-01T00:02:05Z",
			},
			ghclient.Job{
				ID: 4, Name: "Test", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess,
				StartedAt: "2026-01-01T00:05:05Z", CompletedAt: "2026-01-01T00:05:45Z",
			},
		))
	})

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.Render.StepColors = true
	cfg.Render.StepWeights = true

	p := New(cfg, mockGitHubClient(t, mux), pushward.NewClient(pwSrv.URL, "hlk_test"))
	if err := p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}

	// One Poll is both phases: the idle probe creates and seeds the activity, then
	// the active phase advances the run it just picked up.
	got := testutil.GetCalls(calls, mu)
	if len(got) != 3 {
		t.Fatalf("expected 3 PushWard calls (create + seed + tick), got %d", len(got))
	}

	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, got[0].Body, &create)
	wantSlug := text.SlugHash(slugPrefix, testRepo, cipoll.SlugHashLen)
	if create.Slug != wantSlug {
		t.Errorf("slug = %q, want %q", create.Slug, wantSlug)
	}
	if create.Name != "GitHub: repo" {
		t.Errorf("name = %q, want %q", create.Name, "GitHub: repo")
	}

	var seed pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &seed)
	if seed.Content.Template != pushward.TemplateSteps {
		t.Errorf("template = %q, want steps", seed.Content.Template)
	}
	if seed.Content.Subtitle != "repo / CI" {
		t.Errorf("subtitle = %q, want %q", seed.Content.Subtitle, "repo / CI")
	}
	if seed.Content.URL != "https://github.com/owner/repo/actions/runs/42" {
		t.Errorf("url = %q", seed.Content.URL)
	}
	if seed.Content.SecondaryURL != "https://github.com/owner/repo" {
		t.Errorf("secondary_url = %q", seed.Content.SecondaryURL)
	}
	// The prior run's shape is adopted wholesale, matrix fan-out and all.
	if got := seed.Content.TotalSteps; got == nil || *got != 3 {
		t.Errorf("total_steps = %v, want the prior run's 3", got)
	}
	if want := []int{1, 2, 1}; !reflect.DeepEqual(seed.Content.StepRows, want) {
		t.Errorf("step_rows = %v, want %v", seed.Content.StepRows, want)
	}
	if want := []string{"Lint", "Build", "Test"}; !reflect.DeepEqual(seed.Content.StepLabels, want) {
		t.Errorf("step_labels = %v, want %v", seed.Content.StepLabels, want)
	}
	if want := []float64{5, 300, 40}; !reflect.DeepEqual(seed.Content.StepWeights, want) {
		t.Errorf("step_weights = %v, want the prior run's durations %v", seed.Content.StepWeights, want)
	}

	// The active tick reports the running group and omits the seed-only fields,
	// which merge-patch preserves server-side.
	var tick pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[2].Body, &tick)
	if tick.Content.State != "Build" {
		t.Errorf("tick state = %q, want the running group Build", tick.Content.State)
	}
	if tick.Content.Template != "" || tick.Content.Subtitle != "" || tick.Content.URL != "" {
		t.Errorf("the tick must omit the seed-only fields, got %+v", tick.Content)
	}
	// The live scan sees two groups where the seed has three, so the clamp holds
	// the denominator and moves current_step onto Build's seeded position.
	if got := tick.Content.TotalSteps; got == nil || *got != 3 {
		t.Errorf("total_steps = %v, want the seeded 3 to hold", got)
	}
	if got := tick.Content.CurrentStep; got == nil || *got != 2 {
		t.Errorf("current_step = %v, want Build's position 2", got)
	}
	// Build ran 300s in the prior run, so the tick anchors a window for it.
	if tick.Content.LiveProgress == nil || !*tick.Content.LiveProgress {
		t.Errorf("expected the tick to anchor the live window, got %v", tick.Content.LiveProgress)
	}
	if tick.Content.StartDate == nil || tick.Content.EndDate == nil {
		t.Fatal("an anchored window needs both stamps")
	}
	if *tick.Content.StartDate != buildStart.Unix() {
		t.Errorf("start_date = %d, want the job's started_at %d", *tick.Content.StartDate, buildStart.Unix())
	}
	if got := *tick.Content.EndDate - *tick.Content.StartDate; got != 300 {
		t.Errorf("window = %ds, want Build's prior 300s", got)
	}
}

// The adapter's error returns are load-bearing: they are what make the loop defer
// the end, skip the tick and skip the repo rather than acting on nothing.
func TestForgeErrorsPropagate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	// The repo has workflows, so a 404 from anything else is a real failure. Without
	// this the presence lookup itself 404s, which means "Actions is off here" - a
	// legitimately empty answer rather than the error this test is about.
	hasWorkflowsRoute(mux)
	f := testForge(t, mux)
	ctx := context.Background()

	if _, err := f.ActiveRuns(ctx, testRepo); err == nil {
		t.Error("expected an error so the loop skips the repo")
	}
	if _, err := f.GetRun(ctx, testRepo, 42); err == nil {
		t.Error("expected an error so the loop defers the end")
	}
	if _, err := f.LiveJobs(ctx, testRepo, 42); err == nil {
		t.Error("expected an error so the loop skips the tick")
	}
	if _, err := f.ListRepos(ctx, "owner"); err == nil {
		t.Error("expected an error so discovery reports it")
	}
}

// The jobs lookup failing AFTER a prior run was found is a distinct path from the
// run lookup failing, and it is the one that leaves a run identified but unusable.
func TestBaselineJobs_JobsLookupFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(runsResponse(ghclient.WorkflowRun{ID: 41, WorkflowID: 99, HeadBranch: "main"}))
	})
	mux.HandleFunc("/repos/owner/repo/actions/runs/41/jobs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	_, err := testForge(t, mux).BaselineJobs(context.Background(), testRepo,
		cipoll.Run{WorkflowKey: "99", HeadBranch: "main"}, "main", true)
	if err == nil {
		t.Fatal("expected the jobs lookup failure to surface")
	}
	// The loop logs this, so it has to say which run it could not read.
	if !strings.Contains(err.Error(), "41") {
		t.Errorf("error should name the prior run, got %v", err)
	}
}

// The github bridge treats a failed initial discovery as fatal - it points at a
// single well-known host, so a failure there is a credential problem rather than a
// transient one. Flipping this would silently leave the bridge polling a partial
// repo list.
func TestNewMakesDiscoveryFatal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	cfg := testConfig()
	cfg.GitHub.Owner = "owner"
	cfg.GitHub.Repos = []string{testRepo}

	pwSrv, _, _ := testutil.MockPushWardServer(t)
	p := New(cfg, mockGitHubClient(t, mux), pushward.NewClient(pwSrv.URL, "hlk_test"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Run(ctx); err == nil {
		t.Error("expected Run to fail when discovery fails, even with repos configured")
	}
}

// TestBaselineJobs_BlankRefSendsNoBranchFilter is the loop's any-ref rung: no
// branch parameter at all, and the run found there seeds with its wall clock.
func TestBaselineJobs_BlankRefSendsNoBranchFilter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, r *http.Request) {
		if _, filtered := r.URL.Query()["branch"]; filtered {
			t.Errorf("sent branch=%q, want no branch filter", r.URL.Query().Get("branch"))
		}
		_ = json.NewEncoder(w).Encode(runsResponse(ghclient.WorkflowRun{
			ID: 39, WorkflowID: 99, HeadBranch: "main",
			CreatedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			RunStartedAt: time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC),
			UpdatedAt:    time.Date(2026, 1, 1, 0, 6, 0, 0, time.UTC),
		}))
	})
	mux.HandleFunc("/repos/owner/repo/actions/runs/39/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jobsResponse(
			ghclient.Job{ID: 1, Name: "Build", Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess},
		))
	})

	base, err := testForge(t, mux).BaselineJobs(
		context.Background(), testRepo,
		cipoll.Run{WorkflowKey: "99", HeadBranch: "v1.2.3"}, "", true)
	if err != nil {
		t.Fatalf("expected the any-branch run to seed: %v", err)
	}
	if base.RunID != 39 {
		t.Errorf("RunID = %d, want 39", base.RunID)
	}
	// From the first pickup, not from creation: the queue wait is not the run.
	if base.Duration != 5*time.Minute {
		t.Errorf("Duration = %v, want the run's 5m", base.Duration)
	}
}
