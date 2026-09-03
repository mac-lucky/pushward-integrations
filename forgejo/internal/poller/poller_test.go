package poller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/forgejo/internal/config"
	fjclient "github.com/mac-lucky/pushward-integrations/forgejo/internal/forgejo"
	"github.com/mac-lucky/pushward-integrations/shared/ci"
	"github.com/mac-lucky/pushward-integrations/shared/cipoll"
	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const testRepo = "acme/app"

func testConfig() *config.Config {
	cfg := &config.Config{
		Forgejo: config.ForgejoConfig{Repos: []string{testRepo}},
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

// mockForgejoClient points a client at a stub instance.
func mockForgejoClient(t *testing.T, handler http.Handler) *fjclient.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return fjclient.NewClient(srv.URL, "test-token", fjclient.Options{
		Timeout:        2 * time.Second,
		LiveTimings:    true,
		HistoryTimings: true,
	})
}

func testForge(t *testing.T, handler http.Handler) *forge {
	t.Helper()
	return &forge{fj: mockForgejoClient(t, handler)}
}

// --- wire builders, shaped like the real API ---

// runJSON builds a run. Note id and indexInRepo differ, and html_url is built
// from indexInRepo, exactly as the instance does it.
func runJSON(id, indexInRepo int64, status, workflow, branch string) string {
	return fmt.Sprintf(`{
		"id": %d, "index_in_repo": %d,
		"title": "some commit subject",
		"workflow_id": %q, "prettyref": %q,
		"commit_sha": "deadbeef", "event": "push", "trigger_event": "push",
		"status": %q,
		"created": "2026-07-30T10:00:00Z", "started": "2026-07-30T10:00:00Z",
		"stopped": "2026-07-30T10:05:00Z", "updated": "2026-07-30T10:05:00Z",
		"duration": 300000000000,
		"html_url": "https://forgejo.example.com/acme/app/actions/runs/%d",
		"repository": {"full_name": "acme/app", "html_url": "https://forgejo.example.com/acme/app"}
	}`, id, indexInRepo, workflow, branch, status, indexInRepo)
}

func runsJSON(runs ...string) string {
	return fmt.Sprintf(`{"total_count": %d, "workflow_runs": [%s]}`, len(runs), strings.Join(runs, ","))
}

func jobJSON(id, taskID int64, name, status string) string {
	return fmt.Sprintf(`{"id": %d, "run_id": 1, "task_id": %d, "name": %q, "status": %q, "needs": null, "runs_on": ["x"]}`,
		id, taskID, name, status)
}

func jobsJSON(jobs ...string) string {
	return "[" + strings.Join(jobs, ",") + "]"
}

func taskJSON(id int64, name, status, started, updated string) string {
	return fmt.Sprintf(`{"id": %d, "name": %q, "status": %q, "run_number": 33,
		"workflow_id": "ci.yml", "head_branch": "main",
		"created_at": %q, "run_started_at": %q, "updated_at": %q}`,
		id, name, status, started, started, updated)
}

func tasksJSON(tasks ...string) string {
	return fmt.Sprintf(`{"total_count": %d, "workflow_runs": [%s]}`, len(tasks), strings.Join(tasks, ","))
}

// --- wire conversion ---

func TestToCIJobs(t *testing.T) {
	start := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Second)
	got := toCIJobs([]fjclient.Job{
		{Name: "build", Status: "completed", Conclusion: "success", StartedAt: start, CompletedAt: end},
		{Name: "test", Status: "in_progress"},
	})
	if len(got) != 2 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Name != "build" || !got[0].StartedAt.Equal(start) || !got[0].CompletedAt.Equal(end) {
		t.Errorf("job 0 = %+v", got[0])
	}
	if !got[1].StartedAt.IsZero() {
		t.Error("an unstamped job must stay zero")
	}
	if toCIJobs(nil) == nil {
		t.Error("toCIJobs(nil) must return an empty slice, not nil")
	}
}

func TestToRun(t *testing.T) {
	created := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	f := &forge{fj: fjclient.NewClient("https://forgejo.example.com", "t", fjclient.Options{})}

	got := f.toRun(testRepo, fjclient.Run{
		ID:           39,
		IndexInRepo:  33,
		Name:         "tofu",
		WorkflowID:   "tofu.yml",
		Status:       ci.StatusInProgress,
		RawStatus:    "running",
		HeadBranch:   "master",
		Event:        "push",
		CreatedAt:    created,
		HTMLURL:      "https://forgejo.example.com/acme/app/actions/runs/33",
		RepoHTMLURL:  "https://forgejo.example.com/acme/app",
		RepoFullName: testRepo,
	})

	want := cipoll.Run{
		ID:          39,
		Number:      33,
		Name:        "tofu",
		WorkflowKey: "tofu.yml",
		Status:      ci.StatusInProgress,
		RawStatus:   "running",
		HeadBranch:  "master",
		Event:       "push",
		CreatedAt:   created,
		HTMLURL:     "https://forgejo.example.com/acme/app/actions/runs/33",
		RepoURL:     "https://forgejo.example.com/acme/app",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("toRun mismatch:\ngot  %+v\nwant %+v", got, want)
	}
	// The link must be the API's own, built from index_in_repo. Anything composed
	// from the id points at a different run.
	if strings.HasSuffix(got.HTMLURL, "/runs/39") {
		t.Error("the URL was built from the API id, which points at a different run")
	}
}

// Forgejo embeds the repository in a run, but the field is optional across
// versions, so an absent one falls back to the configured instance root rather
// than emitting an empty link.
func TestRepoURL(t *testing.T) {
	f := &forge{fj: fjclient.NewClient("https://forgejo.example.com", "t", fjclient.Options{})}

	embedded := f.repoURL(testRepo, fjclient.Run{RepoHTMLURL: "https://elsewhere.example.com/acme/app"})
	if embedded != "https://elsewhere.example.com/acme/app" {
		t.Errorf("an embedded repository link must win, got %q", embedded)
	}

	composed := f.repoURL(testRepo, fjclient.Run{})
	if composed != "https://forgejo.example.com/acme/app" {
		t.Errorf("composed link = %q", composed)
	}
}

// cipoll.Run.Terminal reads the normalized status where fjclient.Run.Terminal
// reads Forgejo's raw one. The client guarantees the two agree (see
// TestIsTerminalMatchesNormalizeStatus); what this pins is that toRun carries the
// normalized status across so the loop's check has something to read.
func TestToRunCarriesTheNormalizedStatus(t *testing.T) {
	f := &forge{fj: fjclient.NewClient("https://forgejo.example.com", "t", fjclient.Options{})}

	done := f.toRun(testRepo, fjclient.Run{
		Status: ci.StatusCompleted, RawStatus: fjclient.StatusSuccess, Conclusion: ci.ConclusionSuccess,
	})
	if !done.Terminal() {
		t.Error("a completed run must read as terminal")
	}
	live := f.toRun(testRepo, fjclient.Run{Status: ci.StatusInProgress, RawStatus: fjclient.StatusRunning})
	if live.Terminal() {
		t.Error("a running run must not read as terminal")
	}
	// Anything the client could not classify is queued, never completed: ending a
	// card early is worse than deferring it by one poll.
	unknown := f.toRun(testRepo, fjclient.Run{Status: ci.StatusQueued, RawStatus: "a-later-release-adds-this"})
	if unknown.Terminal() {
		t.Error("an unrecognised status must not read as terminal")
	}
}

// --- Outcome ---

func TestOutcome(t *testing.T) {
	tests := []struct {
		conclusion string
		anyFailed  bool
		wantState  string
		wantColor  string
	}{
		{ci.ConclusionSuccess, false, cipoll.OutcomeSuccess, pushward.ColorGreen},
		{ci.ConclusionFailure, false, cipoll.OutcomeFailed, pushward.ColorRed},
		{ci.ConclusionCancelled, false, cipoll.OutcomeCancelled, pushward.ColorOrange},
		{ci.ConclusionSkipped, false, cipoll.OutcomeSkipped, pushward.ColorBlue},
		// An unrecognised terminal status with a failed job under it still reads as
		// a failure rather than a success.
		{"", true, cipoll.OutcomeFailed, pushward.ColorRed},
		{"", false, cipoll.OutcomeComplete, pushward.ColorGreen},
	}
	f := &forge{}
	for _, tc := range tests {
		state, color := f.Outcome(cipoll.Run{Conclusion: tc.conclusion}, tc.anyFailed)
		if state != tc.wantState || color != tc.wantColor {
			t.Errorf("Outcome(%q, %v) = (%q, %q), want (%q, %q)",
				tc.conclusion, tc.anyFailed, state, color, tc.wantState, tc.wantColor)
		}
	}
}

// --- ActiveRuns ---

// A run awaiting approval may never execute, so it is filtered out server-side
// and the poller never sees it - a tracked one would strand a card that only the
// 12-hour lifetime guard could reclaim.
func TestActiveRuns_NeverAsksForBlockedRuns(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		asked := r.URL.Query()["status"]
		for _, s := range asked {
			if s == fjclient.StatusBlocked {
				t.Error("the poller asked for blocked runs")
			}
		}
		if len(asked) == 0 {
			t.Error("the idle probe must filter by status")
		}
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	})

	runs, err := testForge(t, mux).ActiveRuns(context.Background(), testRepo)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("expected no runs, got %d", len(runs))
	}
}

// GetRun is the re-read that gates the two-phase end: all visible jobs can be
// done while the run is still revealing another wave.
func TestGetRunAndLiveJobs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(runJSON(1, 33, "success", "ci.yml", "main")))
	})
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsJSON(jobJSON(10, 100, "build", "running"))))
	})
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	})
	f := testForge(t, mux)
	ctx := context.Background()

	run, err := f.GetRun(ctx, testRepo, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Terminal() || run.Conclusion != ci.ConclusionSuccess {
		t.Errorf("unexpected run re-read: %+v", run)
	}
	if run.RawStatus != fjclient.StatusSuccess {
		t.Errorf("RawStatus = %q, want Forgejo's own value for the log line", run.RawStatus)
	}

	jobs, err := f.LiveJobs(ctx, testRepo, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Name != "build" {
		t.Errorf("unexpected live jobs: %+v", jobs)
	}
}

func TestGetRunError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	f := testForge(t, mux)

	if _, err := f.GetRun(context.Background(), testRepo, 1); err == nil {
		t.Error("expected an error so the loop defers the end instead of guessing")
	}
	if _, err := f.LiveJobs(context.Background(), testRepo, 1); err == nil {
		t.Error("expected an error so the loop skips the tick")
	}
	// ActiveRuns is deliberately absent here: a 404 from the runs endpoint means
	// the repo has Actions switched off, not that a lookup failed. See
	// TestActiveRunsTreats404AsNoActions.
}

// TestActiveRunsTreats404AsNoActions: the runs endpoint does not exist when a
// repo has Actions disabled, and owner discovery hands the poller every repo the
// token can see. Surfacing that as an error cost a request and an error line per
// repo per tick - on a real instance most repos have no workflows, which is
// enough noise to bury anything worth reading.
func TestActiveRunsTreats404AsNoActions(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	f := testForge(t, mux)

	runs, err := f.ActiveRuns(context.Background(), testRepo)
	if err != nil {
		t.Fatalf("a repo without Actions must not error: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("runs = %v, want none", runs)
	}
}

func TestListRepos(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login": "acme"}`))
	})
	mux.HandleFunc("/api/v1/user/repos", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"full_name": "acme/one", "html_url": "https://forgejo.example.com/acme/one"},
			{"full_name": "acme/two", "html_url": "https://forgejo.example.com/acme/two"}
		]`))
	})

	repos, err := testForge(t, mux).ListRepos(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Errorf("expected 2 repos, got %d: %v", len(repos), repos)
	}
}

// --- BaselineJobs ---

// baselineMux serves a prior successful run (id 7) with three jobs, plus the
// tasks rows that are the only source of per-job timing.
func baselineMux(t *testing.T, onTasks func()) *http.ServeMux {
	t.Helper()
	return seedMux(t, mainRuns(t), priorTasks(onTasks,
		taskJSON(201, "lint", "success", "2026-07-30T10:00:00Z", "2026-07-30T10:00:05Z"),
		taskJSON(202, "build", "success", "2026-07-30T10:00:05Z", "2026-07-30T10:05:05Z"),
		taskJSON(203, "deploy", "success", "2026-07-30T10:05:05Z", "2026-07-30T10:05:45Z"),
	))
}

// seedMux serves the prior-run lookup and the tasks page as scripted, and run
// 7's three jobs as always.
func seedMux(t *testing.T, runs, tasks http.HandlerFunc) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs", runs)
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/7/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsJSON(
			jobJSON(1, 201, "lint", "success"),
			jobJSON(2, 202, "build", "success"),
			jobJSON(3, 203, "deploy", "success"),
		)))
	})
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", tasks)
	return mux
}

// mainRuns finds run 7 on main's success pass and nothing anywhere else.
func mainRuns(t *testing.T) http.HandlerFunc {
	t.Helper()
	return runsHandler(t, map[string]string{"refs/heads/main": runJSON(7, 20, "success", "ci.yml", "main")}, new([]string))
}

// priorTasks serves one page of task rows, counting the requests.
func priorTasks(onTasks func(), rows ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if onTasks != nil {
			onTasks()
		}
		_, _ = w.Write([]byte(tasksJSON(rows...)))
	}
}

// With timings wanted, the tasks join runs and the jobs come back stamped.
func TestBaselineJobs_JoinsTimingsWhenWanted(t *testing.T) {
	joined := 0
	f := testForge(t, baselineMux(t, func() { joined++ }))

	base, err := f.BaselineJobs(context.Background(), testRepo,
		cipoll.Run{WorkflowKey: "ci.yml", HeadBranch: "main"}, "main", true)
	if err != nil {
		t.Fatalf("expected a usable seed: %v", err)
	}
	if base.RunID != 7 {
		t.Errorf("RunID = %d, want 7", base.RunID)
	}
	if base.Duration != 5*time.Minute {
		t.Errorf("Duration = %v, want the run's own 5m", base.Duration)
	}
	jobs := base.Jobs
	if joined == 0 {
		t.Error("expected the tasks join to run when timings are wanted")
	}
	if len(jobs) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(jobs))
	}
	// lint 5s, build 300s, deploy 40s - measured through the task join.
	want := map[string]float64{"lint": 5, "build": 300, "deploy": 40}
	got := ci.GroupWeights(jobs)
	for name, secs := range want {
		if got[name] != secs {
			t.Errorf("%s weighed %v, want %v (all: %v)", name, got[name], secs, got)
		}
	}
}

// Forgejo's job objects carry no timestamps, so the join is an extra paginated
// call. With nothing downstream reading the durations it must not be paid for.
func TestBaselineJobs_SkipsTheJoinWhenTimingsAreNotWanted(t *testing.T) {
	joined := 0
	f := testForge(t, baselineMux(t, func() { joined++ }))

	base, err := f.BaselineJobs(context.Background(), testRepo,
		cipoll.Run{WorkflowKey: "ci.yml", HeadBranch: "main"}, "main", false)
	if err != nil {
		t.Fatalf("expected a usable seed: %v", err)
	}
	jobs := base.Jobs
	if joined != 0 {
		t.Errorf("the tasks join ran %d times with timings switched off", joined)
	}
	if len(jobs) != 3 {
		t.Fatalf("expected the shape regardless, got %d jobs", len(jobs))
	}
	for _, j := range jobs {
		if !j.StartedAt.IsZero() || !j.CompletedAt.IsZero() {
			t.Errorf("job %q carries times the join was never asked for: %+v", j.Name, j)
		}
	}
}

func TestBaselineJobs_NoPriorRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	})

	base, err := testForge(t, mux).BaselineJobs(context.Background(), testRepo,
		cipoll.Run{WorkflowKey: "ci.yml", HeadBranch: "main"}, "main", true)
	// No prior run is not an error - the caller just keeps its live scan.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(base.Jobs) != 0 {
		t.Errorf("expected an empty baseline, got %d jobs", len(base.Jobs))
	}
}

func TestBaselineJobs_LookupErrorIsNotASeed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	if _, err := testForge(t, mux).BaselineJobs(context.Background(), testRepo,
		cipoll.Run{WorkflowKey: "ci.yml", HeadBranch: "main"}, "main", true); err == nil {
		t.Error("expected an error when the lookup failed")
	}
}

// --- New ---

// DiscoveryRequired is derived from the config: with an explicit repo list the
// bridge rides out a failed enumeration, without one it has nothing to poll.
func TestDiscoveryRequired(t *testing.T) {
	withRepos := testConfig()
	withRepos.Forgejo.Owner = "acme"
	withRepos.Forgejo.Repos = []string{testRepo}
	if discoveryRequired(withRepos) {
		t.Error("an explicit repo list must survive a discovery failure")
	}

	discoveryOnly := testConfig()
	discoveryOnly.Forgejo.Repos = nil
	discoveryOnly.Forgejo.Owner = "acme"
	if !discoveryRequired(discoveryOnly) {
		t.Error("with discovery as the only source of repos, a failure is worth exiting on")
	}
}

// --- end-to-end ---

// TestNewEndToEnd is the seam test: a real config, a real Forgejo client and a
// real shared poller, driven through one full poll. The orchestration is covered
// in shared/cipoll and the wire conversion above, so what this proves is that the
// wiring between them is right.
func TestNewEndToEnd(t *testing.T) {
	// Build started 10s ago against a 300s estimate, so the active tick can anchor
	// a window for it.
	buildStart := time.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// Only the prior-run lookup carries workflow_id; the idle probe does not.
		if q.Get("workflow_id") == "" {
			_, _ = w.Write([]byte(runsJSON(runJSON(1, 33, "running", "tofu.yml", "master"))))
			return
		}
		if len(q["status"]) == 1 && q["status"][0] == fjclient.StatusSuccess {
			_, _ = w.Write([]byte(runsJSON(runJSON(7, 20, "success", "tofu.yml", "master"))))
			return
		}
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	})
	// The live run reveals a folded matrix: checks, detect, tofu (x3).
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsJSON(
			jobJSON(10, 100, "checks", "success"),
			jobJSON(11, 101, "detect", "success"),
			jobJSON(12, 102, "tofu (tailscale)", "running"),
			jobJSON(13, 103, "tofu (grafana)", "waiting"),
			jobJSON(14, 104, "tofu (cloudflare)", "waiting"),
		)))
	})
	// The prior run shows the same three groups.
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/7/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsJSON(
			jobJSON(1, 201, "checks", "success"),
			jobJSON(2, 202, "detect", "success"),
			jobJSON(3, 203, "tofu (tailscale)", "success"),
		)))
	})
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, r *http.Request) {
		// Both joins page the repo-wide list unfiltered, newest first: the live
		// run's rows sit ahead of the prior run's on one page.
		if got := r.URL.Query()["status"]; len(got) != 0 {
			t.Errorf("tasks status filter = %v, want none", got)
		}
		_, _ = w.Write([]byte(tasksJSON(
			taskJSON(102, "tofu (tailscale)", "running", buildStart, buildStart),
			taskJSON(101, "detect", "success", "2026-07-30T10:10:05Z", "2026-07-30T10:10:15Z"),
			taskJSON(100, "checks", "success", "2026-07-30T10:10:00Z", "2026-07-30T10:10:05Z"),
			taskJSON(203, "tofu (tailscale)", "success", "2026-07-30T10:00:15Z", "2026-07-30T10:05:15Z"),
			taskJSON(202, "detect", "success", "2026-07-30T10:00:05Z", "2026-07-30T10:00:15Z"),
			taskJSON(201, "checks", "success", "2026-07-30T10:00:00Z", "2026-07-30T10:00:05Z"),
		)))
	})

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.Render.StepColors = true
	cfg.Render.StepWeights = true

	p := New(cfg, mockForgejoClient(t, mux), pushward.NewClient(pwSrv.URL, "hlk_test"))
	if err := p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}

	// One Poll is both phases: the idle probe creates and seeds the activity, then
	// the active phase advances the run it just picked up.
	got := testutil.GetCalls(calls, mu)
	if len(got) < 2 {
		t.Fatalf("expected at least a create and a seed, got %d calls", len(got))
	}

	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, got[0].Body, &create)
	if want := text.SlugHash(slugPrefix, testRepo, cipoll.SlugHashLen); create.Slug != want {
		t.Errorf("slug = %q, want %q", create.Slug, want)
	}
	// The prefix must stay distinct from the relay's "forgejo-" for these repos.
	if strings.HasPrefix(create.Slug, "forgejo-") {
		t.Error("the poller must not use the relay's slug prefix")
	}
	if create.Name != "Forgejo: app" {
		t.Errorf("name = %q, want %q", create.Name, "Forgejo: app")
	}

	var seed pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &seed)
	// Forgejo offers no workflow display name, so the filename stem is the
	// subtitle; the commit subject is far too long for it.
	if seed.Content.Subtitle != "app / tofu" {
		t.Errorf("subtitle = %q, want %q", seed.Content.Subtitle, "app / tofu")
	}
	if strings.Contains(seed.Content.Subtitle, "commit subject") {
		t.Error("the commit subject leaked into the subtitle")
	}
	if want := "https://forgejo.example.com/acme/app/actions/runs/33"; seed.Content.URL != want {
		t.Errorf("url = %q, want the API's own html_url %q", seed.Content.URL, want)
	}
	if want := "https://forgejo.example.com/acme/app"; seed.Content.SecondaryURL != want {
		t.Errorf("secondary_url = %q, want %q", seed.Content.SecondaryURL, want)
	}
	// The matrix folds to one group, and the fan-out reaches the wire.
	if got := seed.Content.TotalSteps; got == nil || *got != 3 {
		t.Fatalf("total_steps = %v, want 3 (checks, detect, tofu)", got)
	}
	if want := []int{1, 1, 3}; !reflect.DeepEqual(seed.Content.StepRows, want) {
		t.Errorf("step_rows = %v, want %v", seed.Content.StepRows, want)
	}
	if want := []string{"checks", "detect", "tofu"}; !reflect.DeepEqual(seed.Content.StepLabels, want) {
		t.Errorf("step_labels = %v, want %v", seed.Content.StepLabels, want)
	}
	if want := []float64{5, 10, 300}; !reflect.DeepEqual(seed.Content.StepWeights, want) {
		t.Errorf("step_weights = %v, want the prior run's durations %v", seed.Content.StepWeights, want)
	}
	if len(seed.Content.StepColors) != 3 {
		t.Errorf("step_colors = %v, want one per step", seed.Content.StepColors)
	}

	// The active tick anchors the running group's window off the live tasks join.
	if len(got) < 3 {
		t.Fatal("expected the active phase to advance the run it just picked up")
	}
	var tick pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[2].Body, &tick)
	if tick.Content.State != "tofu" {
		t.Errorf("tick state = %q, want the running group tofu", tick.Content.State)
	}
	if tick.Content.LiveProgress == nil || !*tick.Content.LiveProgress {
		t.Fatalf("expected the tick to anchor the live window, got %v", tick.Content.LiveProgress)
	}
	if tick.Content.StartDate == nil || tick.Content.EndDate == nil {
		t.Fatal("an anchored window needs both stamps")
	}
	if got := *tick.Content.EndDate - *tick.Content.StartDate; got != 300 {
		t.Errorf("window = %ds, want tofu's prior 300s", got)
	}
}

// The jobs lookup failing AFTER a prior run was found is a distinct path from the
// run lookup failing, and it is the one that leaves a run identified but unusable.
func TestBaselineJobs_JobsLookupFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if slices.Contains(r.URL.Query()["status"], fjclient.StatusSuccess) {
			_, _ = w.Write([]byte(runsJSON(runJSON(7, 20, "success", "ci.yml", "main"))))
			return
		}
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	})
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/7/jobs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	_, err := testForge(t, mux).BaselineJobs(context.Background(), testRepo,
		cipoll.Run{WorkflowKey: "ci.yml", HeadBranch: "main"}, "main", false)
	if err == nil {
		t.Fatal("expected the jobs lookup failure to surface")
	}
	if !strings.Contains(err.Error(), "7") {
		t.Errorf("error should name the prior run, got %v", err)
	}
}

// An empty tasks response is the graceful-degrade case: Forgejo's jobs endpoint
// carries no timestamps of its own, so a missed join must leave them zero rather
// than inventing one - the ladder reads zero as "unknown" and draws the static bar.
func TestLiveJobs_MissedTaskJoinLeavesTimesZero(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsJSON(jobJSON(10, 100, "build", "running"))))
	})
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	})

	jobs, err := testForge(t, mux).LiveJobs(context.Background(), testRepo, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if !jobs[0].StartedAt.IsZero() || !jobs[0].CompletedAt.IsZero() {
		t.Errorf("a missed join must leave the times zero, got (%v, %v)",
			jobs[0].StartedAt, jobs[0].CompletedAt)
	}
}

// runsHandler serves the prior-run lookups by ref: refs maps a ref filter (""
// for none) to the run JSON it finds on the success pass, and records every
// request's ref in order.
func runsHandler(t *testing.T, refs map[string]string, seen *[]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		*seen = append(*seen, q.Get("ref"))
		if run, ok := refs[q.Get("ref")]; ok && slices.Contains(q["status"], fjclient.StatusSuccess) {
			_, _ = w.Write([]byte(runsJSON(run)))
			return
		}
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	}
}

// TestBaselineJobs_BlankRefSendsNoFilter is the loop's any-ref rung: no ref
// parameter at all, and the run found there seeds.
func TestBaselineJobs_BlankRefSendsNoFilter(t *testing.T) {
	var seen []string
	mux := seedMux(t, runsHandler(t,
		map[string]string{"": runJSON(7, 20, "success", "ci.yml", "main")}, &seen), priorTasks(nil))

	base, err := testForge(t, mux).BaselineJobs(context.Background(), testRepo,
		cipoll.Run{WorkflowKey: "ci.yml", HeadBranch: "v1.2.3"}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if base.RunID != 7 || len(base.Jobs) != 3 {
		t.Fatalf("baseline = %+v, want run 7 with its 3 jobs", base)
	}
	if want := []string{""}; !reflect.DeepEqual(seen, want) {
		t.Errorf("ref filters sent = %q, want %q", seen, want)
	}
}

// TestBaselineJobs_QualifiesTheRef: the loop hands over the prettyref as the
// forge reported it, and the adapter qualifies it - a PR's "#17" is looked up
// as its head ref, where the PR's earlier runs live.
func TestBaselineJobs_QualifiesTheRef(t *testing.T) {
	var seen []string
	mux := seedMux(t, runsHandler(t,
		map[string]string{"refs/pull/17/head": runJSON(7, 20, "success", "ci.yml", "#17")}, &seen), priorTasks(nil))

	base, err := testForge(t, mux).BaselineJobs(context.Background(), testRepo,
		cipoll.Run{WorkflowKey: "ci.yml", HeadBranch: "#17"}, "#17", false)
	if err != nil {
		t.Fatal(err)
	}
	if base.RunID != 7 {
		t.Errorf("baseline = %+v, want run 7 found under refs/pull/17/head", base)
	}
	if len(seen) != 1 || seen[0] != "refs/pull/17/head" {
		t.Errorf("ref filters sent = %q, want one hit on the pull ref", seen)
	}
}

// TestBaselineJobs_PassesTheRunStopToTheJoin: a task row updated two days
// after the run stopped is the rewritten-row case, and it must reach the join
// as unknown rather than as a two-day group.
func TestBaselineJobs_PassesTheRunStopToTheJoin(t *testing.T) {
	mux := seedMux(t, mainRuns(t), priorTasks(nil,
		taskJSON(201, "lint", "success", "2026-07-30T10:00:00Z", "2026-07-30T10:00:05Z"),
		taskJSON(202, "build", "success", "2026-07-30T10:00:05Z", "2026-08-01T00:00:00Z"),
		taskJSON(203, "deploy", "success", "2026-07-30T10:05:05Z", "2026-07-30T10:05:45Z"),
	))

	base, err := testForge(t, mux).BaselineJobs(context.Background(), testRepo,
		cipoll.Run{WorkflowKey: "ci.yml", HeadBranch: "main"}, "main", true)
	if err != nil {
		t.Fatal(err)
	}
	got := ci.GroupWeights(base.Jobs)
	if got["build"] != ci.StepWeightFloor {
		t.Errorf("build = %v, want the floor for a completion after the run's stop", got["build"])
	}
	if got["lint"] != 5 || got["deploy"] != 40 {
		t.Errorf("weights = %v, want the intact rows measured", got)
	}
}
