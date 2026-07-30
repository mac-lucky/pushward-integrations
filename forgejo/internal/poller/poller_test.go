package poller

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/forgejo/internal/config"
	fjclient "github.com/mac-lucky/pushward-integrations/forgejo/internal/forgejo"
	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const testRepo = "acme/app"

func testConfig() *config.Config {
	return &config.Config{
		PushWard: sharedconfig.PushWardConfig{
			Priority:       1,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
		Polling: config.PollingConfig{IdleInterval: 60 * time.Second},
		// Mirrors the production default so tests exercise the shipped behavior.
		Render: config.RenderConfig{LiveProgress: true},
	}
}

func testConfigRender(colors, weights bool) *config.Config {
	cfg := testConfig()
	cfg.Render.StepColors = colors
	cfg.Render.StepWeights = weights
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

// activeMux serves an idle-probe response plus a jobs list, the minimum for a
// tracked run.
func activeMux(t *testing.T, runs, jobs string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(runs))
	})
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobs))
	})
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	})
	return mux
}

func newPoller(t *testing.T, cfg *config.Config, mux http.Handler) (*Poller, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	p := New(cfg, mockForgejoClient(t, mux), pushward.NewClient(srv.URL, "hlk_test"))
	p.repos = []string{testRepo}
	return p, calls, mu
}

func seedFrame(t *testing.T, calls *[]testutil.APICall, mu *sync.Mutex) pushward.UpdateRequest {
	t.Helper()
	got := testutil.GetCalls(calls, mu)
	for _, c := range got {
		if c.Method == http.MethodPatch {
			var req pushward.UpdateRequest
			testutil.UnmarshalBody(t, c.Body, &req)
			return req
		}
	}
	t.Fatal("no seed frame was sent")
	return pushward.UpdateRequest{}
}

// --- pollIdle ---

func TestPollIdle_DiscoversAndTracksRun(t *testing.T) {
	mux := activeMux(t,
		runsJSON(runJSON(1, 33, "running", "ci.yml", "main")),
		jobsJSON(jobJSON(10, 100, "build", "running")))
	p, calls, mu := newPoller(t, testConfig(), mux)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	tracked, ok := p.tracked[testRepo]
	if !ok {
		t.Fatal("the run was not tracked")
	}
	if tracked.RunID != 1 {
		t.Errorf("RunID = %d, want the API id", tracked.RunID)
	}

	// The slug MUST differ from the relay's Forgejo route, which owns
	// "forgejo-<hash8>" for these same repos. Sharing it would make the poller and
	// the webhook handler fight over one activity.
	wantSlug := text.SlugHash("fj", testRepo, 4)
	if tracked.Slug != wantSlug {
		t.Errorf("slug = %q, want %q", tracked.Slug, wantSlug)
	}
	if relaySlug := text.SlugHash("forgejo", testRepo, 4); tracked.Slug == relaySlug {
		t.Errorf("slug %q collides with the relay's Forgejo route", tracked.Slug)
	}

	created := false
	for _, c := range testutil.GetCalls(calls, mu) {
		if c.Method == http.MethodPost && c.Path == "/activities" {
			created = true
		}
	}
	if !created {
		t.Error("no activity was created")
	}
}

// TestPollIdle_UsesAPIHTMLURL locks in the single most dangerous field in the
// API: html_url is built from index_in_repo, not the id the bridge fetches by.
func TestPollIdle_UsesAPIHTMLURL(t *testing.T) {
	mux := activeMux(t,
		runsJSON(runJSON(39, 33, "running", "ci.yml", "main")),
		jobsJSON(jobJSON(10, 100, "build", "running")))
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/39/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsJSON(jobJSON(10, 100, "build", "running"))))
	})
	p, calls, mu := newPoller(t, testConfig(), mux)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	req := seedFrame(t, calls, mu)
	want := "https://forgejo.example.com/acme/app/actions/runs/33"
	if req.Content.URL != want {
		t.Errorf("URL = %q, want the API's own html_url %q", req.Content.URL, want)
	}
	if strings.HasSuffix(req.Content.URL, "/runs/39") {
		t.Error("the URL was built from the API id, which points at a different run")
	}
	if req.Content.SecondaryURL != "https://forgejo.example.com/acme/app" {
		t.Errorf("SecondaryURL = %q", req.Content.SecondaryURL)
	}
}

// TestPollIdle_MatrixGroupsFold uses the real job names from the instance.
func TestPollIdle_MatrixGroupsFold(t *testing.T) {
	mux := activeMux(t,
		runsJSON(runJSON(1, 33, "running", "tofu.yml", "master")),
		jobsJSON(
			jobJSON(10, 100, "checks", "success"),
			jobJSON(11, 101, "detect", "success"),
			jobJSON(12, 102, "tofu (tailscale)", "running"),
			jobJSON(13, 103, "tofu (grafana)", "waiting"),
			jobJSON(14, 104, "tofu (cloudflare)", "waiting"),
		))
	p, calls, mu := newPoller(t, testConfig(), mux)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	req := seedFrame(t, calls, mu)
	if req.Content.TotalSteps == nil || *req.Content.TotalSteps != 3 {
		t.Fatalf("total_steps = %v, want 3 (checks, detect, tofu)", req.Content.TotalSteps)
	}
	if want := []int{1, 1, 3}; !reflect.DeepEqual(req.Content.StepRows, want) {
		t.Errorf("step_rows = %v, want %v", req.Content.StepRows, want)
	}
	if want := []string{"checks", "detect", "tofu"}; !reflect.DeepEqual(req.Content.StepLabels, want) {
		t.Errorf("step_labels = %v, want %v", req.Content.StepLabels, want)
	}
}

// TestPollIdle_SubtitleUsesWorkflowFilename covers the missing display name:
// Forgejo only offers the filename, and the commit subject is far too long.
func TestPollIdle_SubtitleUsesWorkflowFilename(t *testing.T) {
	mux := activeMux(t,
		runsJSON(runJSON(1, 33, "running", "tofu.yml", "master")),
		jobsJSON(jobJSON(10, 100, "build", "running")))
	p, calls, mu := newPoller(t, testConfig(), mux)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	req := seedFrame(t, calls, mu)
	if req.Content.Subtitle != "app / tofu" {
		t.Errorf("subtitle = %q, want %q", req.Content.Subtitle, "app / tofu")
	}
	if strings.Contains(req.Content.Subtitle, "commit subject") {
		t.Error("the commit subject leaked into the subtitle")
	}
}

func TestPollIdle_SkipsAlreadyTrackedRepo(t *testing.T) {
	mux := activeMux(t,
		runsJSON(runJSON(1, 33, "running", "ci.yml", "main")),
		jobsJSON(jobJSON(10, 100, "build", "running")))
	p, calls, mu := newPoller(t, testConfig(), mux)
	p.tracked[testRepo] = &trackedRun{Repo: testRepo, RunID: 1, Slug: "fj-x"}

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(testutil.GetCalls(calls, mu)); n != 0 {
		t.Errorf("made %d calls for an already-tracked repo, want 0", n)
	}
}

func TestPollIdle_NoActiveRuns(t *testing.T) {
	mux := activeMux(t, `{"total_count":0,"workflow_runs":null}`, jobsJSON())
	p, calls, mu := newPoller(t, testConfig(), mux)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.tracked) != 0 {
		t.Error("nothing should be tracked")
	}
	if n := len(testutil.GetCalls(calls, mu)); n != 0 {
		t.Errorf("made %d calls, want 0", n)
	}
}

// TestPollIdle_BlockedRunIsNotTracked: a run awaiting approval is filtered out
// server-side, so the poller never sees it and never strands a card.
func TestPollIdle_BlockedRunIsNotTracked(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		// Behave like the instance: only return runs matching the status filter.
		want := map[string]bool{}
		for _, s := range r.URL.Query()["status"] {
			want[s] = true
		}
		if want["blocked"] {
			t.Error("the poller asked for blocked runs")
		}
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	})
	p, _, _ := newPoller(t, testConfig(), mux)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.tracked) != 0 {
		t.Error("a blocked run must not be tracked")
	}
}

func TestPollIdle_PicksMostRecentRun(t *testing.T) {
	older := strings.Replace(runJSON(1, 30, "running", "ci.yml", "main"),
		`"created": "2026-07-30T10:00:00Z"`, `"created": "2026-07-30T09:00:00Z"`, 1)
	newer := runJSON(2, 33, "running", "ci.yml", "main")

	mux := activeMux(t, runsJSON(older, newer), jobsJSON())
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/2/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsJSON(jobJSON(10, 100, "build", "running"))))
	})
	p, _, _ := newPoller(t, testConfig(), mux)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.tracked[testRepo].RunID; got != 2 {
		t.Errorf("tracked run %d, want the most recently created (2)", got)
	}
}

// TestPollIdle_SeedsShapeAndWeightsFromPriorRun exercises the whole seed path,
// including the tasks join that is the only source of per-job timing.
func TestPollIdle_SeedsShapeAndWeightsFromPriorRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// Only the prior-run lookup carries workflow_id; the idle probe does not.
		if q.Get("workflow_id") == "" {
			_, _ = w.Write([]byte(runsJSON(runJSON(1, 33, "running", "ci.yml", "main"))))
			return
		}
		if len(q["status"]) == 1 && q["status"][0] == "success" {
			_, _ = w.Write([]byte(runsJSON(runJSON(7, 20, "success", "ci.yml", "main"))))
			return
		}
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	})
	// The live run has only revealed its first job.
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsJSON(jobJSON(10, 100, "lint", "running"))))
	})
	// The prior run ran the full DAG.
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/7/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsJSON(
			jobJSON(1, 201, "lint", "success"),
			jobJSON(2, 202, "build", "success"),
			jobJSON(3, 203, "deploy", "success"),
		)))
	})
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(tasksJSON(
			taskJSON(201, "lint", "success", "2026-07-30T10:00:00Z", "2026-07-30T10:00:05Z"),
			taskJSON(202, "build", "success", "2026-07-30T10:00:05Z", "2026-07-30T10:05:05Z"),
			taskJSON(203, "deploy", "success", "2026-07-30T10:05:05Z", "2026-07-30T10:05:45Z"),
		)))
	})
	p, calls, mu := newPoller(t, testConfigRender(true, true), mux)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	req := seedFrame(t, calls, mu)
	if req.Content.TotalSteps == nil || *req.Content.TotalSteps != 3 {
		t.Fatalf("total_steps = %v, want the prior run's 3", req.Content.TotalSteps)
	}
	// lint 5s, build 300s, deploy 40s - measured through the task join.
	if want := []float64{5, 300, 40}; !reflect.DeepEqual(req.Content.StepWeights, want) {
		t.Errorf("step_weights = %v, want %v", req.Content.StepWeights, want)
	}
	if len(req.Content.StepColors) != 3 {
		t.Errorf("step_colors = %v, want one per step", req.Content.StepColors)
	}
}

func TestPollIdle_RenderFlags(t *testing.T) {
	tests := []struct {
		name           string
		colors         bool
		weights        bool
		wantColorsKey  bool
		wantWeightsKey bool
	}{
		{"both off", false, false, false, false},
		{"colors only", true, false, true, false},
		{"weights only", false, true, false, true},
		{"both on", true, true, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/repos/acme/app/actions/runs", func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Get("workflow_id") == "" {
					_, _ = w.Write([]byte(runsJSON(runJSON(1, 33, "running", "ci.yml", "main"))))
					return
				}
				if len(q["status"]) == 1 && q["status"][0] == "success" {
					_, _ = w.Write([]byte(runsJSON(runJSON(7, 20, "success", "ci.yml", "main"))))
					return
				}
				_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
			})
			mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(jobsJSON(jobJSON(10, 100, "build", "running"))))
			})
			mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/7/jobs", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(jobsJSON(jobJSON(1, 201, "build", "success"))))
			})
			mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tasksJSON(
					taskJSON(201, "build", "success", "2026-07-30T10:00:00Z", "2026-07-30T10:00:30Z"))))
			})
			p, calls, mu := newPoller(t, testConfigRender(tc.colors, tc.weights), mux)

			if err := p.pollIdle(context.Background()); err != nil {
				t.Fatal(err)
			}

			var body []byte
			for _, c := range testutil.GetCalls(calls, mu) {
				if c.Method == http.MethodPatch {
					body = c.Body
				}
			}
			if body == nil {
				t.Fatal("no seed frame")
			}
			if got := bytes.Contains(body, []byte(`"step_colors"`)); got != tc.wantColorsKey {
				t.Errorf("step_colors present = %v, want %v", got, tc.wantColorsKey)
			}
			if got := bytes.Contains(body, []byte(`"step_weights"`)); got != tc.wantWeightsKey {
				t.Errorf("step_weights present = %v, want %v", got, tc.wantWeightsKey)
			}
		})
	}
}

// TestPollIdle_SeedStopsCarriedLiveProgress: the slug is per-repo, so a run
// superseded before its end frames fired can leave an animation running.
func TestPollIdle_SeedStopsCarriedLiveProgress(t *testing.T) {
	mux := activeMux(t,
		runsJSON(runJSON(1, 33, "running", "ci.yml", "main")),
		jobsJSON(jobJSON(10, 100, "build", "running")))
	p, calls, mu := newPoller(t, testConfig(), mux)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	req := seedFrame(t, calls, mu)
	if req.Content.LiveProgress == nil || *req.Content.LiveProgress {
		t.Errorf("seed live_progress = %v, want an explicit false", req.Content.LiveProgress)
	}
}

// --- pollActive ---

// trackRun installs a tracked entry as pollIdle would have.
func trackRun(p *Poller, runID int64, totalSteps int, labels []string) {
	p.tracked[testRepo] = &trackedRun{
		Repo:          testRepo,
		RunID:         runID,
		Name:          "ci",
		Slug:          text.SlugHash("fj", testRepo, 4),
		HTMLURL:       "https://forgejo.example.com/acme/app/actions/runs/33",
		RepoURL:       "https://forgejo.example.com/acme/app",
		LastUpdate:    time.Now(),
		trackedAt:     time.Now(),
		maxTotalSteps: totalSteps,
		maxStepLabels: labels,
		shapeSent:     totalSteps,
	}
}

func TestPollActive_UpdatesOngoingRun(t *testing.T) {
	mux := activeMux(t, runsJSON(), jobsJSON(
		jobJSON(10, 100, "lint", "success"),
		jobJSON(11, 101, "build", "running"),
	))
	p, calls, mu := newPoller(t, testConfig(), mux)
	trackRun(p, 1, 2, []string{"lint", "build"})
	if err := p.pw.CreateActivity(context.Background(), p.tracked[testRepo].Slug, "Forgejo: app", 1, 900, 1800); err != nil {
		t.Fatal(err)
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	var patch pushward.PatchRequest
	found := false
	for _, c := range testutil.GetCalls(calls, mu) {
		if c.Method == http.MethodPatch {
			testutil.UnmarshalBody(t, c.Body, &patch)
			found = true
		}
	}
	if !found {
		t.Fatal("no patch was sent")
	}
	if patch.Content.State == nil || *patch.Content.State != "build" {
		t.Errorf("state = %v, want the running group", patch.Content.State)
	}
	if patch.Content.Progress == nil || *patch.Content.Progress != 0.5 {
		t.Errorf("progress = %v, want 0.5", patch.Content.Progress)
	}
}

func TestPollActive_CompletesRun(t *testing.T) {
	tests := []struct {
		status    string
		wantState string
		wantColor string
	}{
		{"success", "Success", pushward.ColorGreen},
		{"failure", "Failed", pushward.ColorRed},
		{"cancelled", "Cancelled", pushward.ColorOrange},
		{"skipped", "Skipped", pushward.ColorBlue},
	}
	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(jobsJSON(jobJSON(10, 100, "build", tc.status))))
			})
			mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/1", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(runJSON(1, 33, tc.status, "ci.yml", "main")))
			})
			mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
			})
			p, calls, mu := newPoller(t, testConfig(), mux)
			trackRun(p, 1, 1, []string{"build"})
			if err := p.pw.CreateActivity(context.Background(), p.tracked[testRepo].Slug, "Forgejo: app", 1, 900, 1800); err != nil {
				t.Fatal(err)
			}

			if err := p.pollActive(context.Background()); err != nil {
				t.Fatal(err)
			}

			// Two-phase end: a final ONGOING frame, then ENDED.
			got := testutil.WaitForCalls(t, calls, mu, 3, 2*time.Second)
			var last pushward.UpdateRequest
			testutil.UnmarshalBody(t, got[len(got)-1].Body, &last)
			if last.State != pushward.StateEnded {
				t.Errorf("final state = %q, want ended", last.State)
			}
			if last.Content.State != tc.wantState {
				t.Errorf("state = %q, want %q", last.Content.State, tc.wantState)
			}
			if last.Content.AccentColor != tc.wantColor {
				t.Errorf("color = %q, want %q", last.Content.AccentColor, tc.wantColor)
			}
			if last.Content.Progress != 1.0 {
				t.Errorf("progress = %v, want 1.0", last.Content.Progress)
			}
		})
	}
}

// TestPollActive_DefersEndWhileRunStillGoing: all visible jobs are done but the
// run has not finished, so a later wave is still coming.
func TestPollActive_DefersEndWhileRunStillGoing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsJSON(jobJSON(10, 100, "build", "success"))))
	})
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(runJSON(1, 33, "running", "ci.yml", "main")))
	})
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	})
	p, calls, mu := newPoller(t, testConfig(), mux)
	trackRun(p, 1, 1, []string{"build"})

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.tracked[testRepo].endTimers != nil {
		t.Error("an end was scheduled while the run was still going")
	}
	for _, c := range testutil.GetCalls(calls, mu) {
		var req pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &req)
		if req.State == pushward.StateEnded {
			t.Error("the activity was ended prematurely")
		}
	}
}

func TestPollActive_EvictsStaleRun(t *testing.T) {
	p, _, _ := newPoller(t, testConfig(), activeMux(t, runsJSON(), jobsJSON()))
	trackRun(p, 1, 1, []string{"build"})
	p.tracked[testRepo].LastUpdate = time.Now().Add(-2 * time.Hour)

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.tracked[testRepo]; ok {
		t.Error("the stale run was not evicted")
	}
}

func TestPollActive_EvictsRunExceedingMaxLifetime(t *testing.T) {
	p, _, _ := newPoller(t, testConfig(), activeMux(t, runsJSON(), jobsJSON()))
	trackRun(p, 1, 1, []string{"build"})
	p.tracked[testRepo].trackedAt = time.Now().Add(-maxRunLifetime - time.Minute)

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.tracked[testRepo]; ok {
		t.Error("the over-age run was not evicted")
	}
}

// TestPollActive_LiveProgressAnchor covers the end-to-end payoff of the tasks
// join: a measured prior run plus a stamped start yields an animated window.
func TestPollActive_LiveProgressAnchor(t *testing.T) {
	started := time.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsJSON(jobJSON(10, 100, "build", "running"))))
	})
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(tasksJSON(taskJSON(100, "build", "running", started, started))))
	})
	p, calls, mu := newPoller(t, testConfig(), mux)
	trackRun(p, 1, 1, []string{"build"})
	p.tracked[testRepo].stepWeightByName = map[string]float64{"build": 300}
	p.tracked[testRepo].liveSent = false
	if err := p.pw.CreateActivity(context.Background(), p.tracked[testRepo].Slug, "Forgejo: app", 1, 900, 1800); err != nil {
		t.Fatal(err)
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	var patch pushward.PatchRequest
	for _, c := range testutil.GetCalls(calls, mu) {
		if c.Method == http.MethodPatch {
			testutil.UnmarshalBody(t, c.Body, &patch)
		}
	}
	if patch.Content == nil || patch.Content.LiveProgress == nil || !*patch.Content.LiveProgress {
		t.Fatalf("live_progress = %v, want true", patch.Content)
	}
	if patch.Content.StartDate == nil || patch.Content.EndDate == nil {
		t.Fatal("an anchored window needs both dates")
	}
	if got := *patch.Content.EndDate - *patch.Content.StartDate; got != 300 {
		t.Errorf("window = %ds, want 300s from the prior run", got)
	}
}

// TestPollActive_NoAnchorWithoutTaskTimings is the graceful degrade: when the
// join misses there is no start to measure from, so the static bar is used.
func TestPollActive_NoAnchorWithoutTaskTimings(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/acme/app/actions/runs/1/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(jobsJSON(jobJSON(10, 100, "build", "running"))))
	})
	mux.HandleFunc("/api/v1/repos/acme/app/actions/tasks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	})
	p, calls, mu := newPoller(t, testConfig(), mux)
	trackRun(p, 1, 1, []string{"build"})
	p.tracked[testRepo].stepWeightByName = map[string]float64{"build": 300}
	p.tracked[testRepo].liveSent = false
	if err := p.pw.CreateActivity(context.Background(), p.tracked[testRepo].Slug, "Forgejo: app", 1, 900, 1800); err != nil {
		t.Fatal(err)
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, c := range testutil.GetCalls(calls, mu) {
		if c.Method != http.MethodPatch {
			continue
		}
		if bytes.Contains(c.Body, []byte(`"start_date"`)) || bytes.Contains(c.Body, []byte(`"end_date"`)) {
			t.Errorf("an unstamped step produced a live window: %s", c.Body)
		}
	}
}

func TestPollActive_SkipsRedundantTicks(t *testing.T) {
	mux := activeMux(t, runsJSON(), jobsJSON(
		jobJSON(10, 100, "lint", "success"),
		jobJSON(11, 101, "build", "running"),
	))
	p, calls, mu := newPoller(t, testConfig(), mux)
	trackRun(p, 1, 2, []string{"lint", "build"})
	if err := p.pw.CreateActivity(context.Background(), p.tracked[testRepo].Slug, "Forgejo: app", 1, 900, 1800); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if err := p.pollActive(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	patches := 0
	for _, c := range testutil.GetCalls(calls, mu) {
		if c.Method == http.MethodPatch {
			patches++
		}
	}
	if patches != 1 {
		t.Errorf("sent %d patches across 3 identical ticks, want 1", patches)
	}
}

// --- lifecycle ---

func TestScheduleEnd_TwoPhase(t *testing.T) {
	p, calls, mu := newPoller(t, testConfig(), activeMux(t, runsJSON(), jobsJSON()))
	trackRun(p, 1, 1, []string{"build"})
	slug := p.tracked[testRepo].Slug
	if err := p.pw.CreateActivity(context.Background(), slug, "Forgejo: app", 1, 900, 1800); err != nil {
		t.Fatal(err)
	}

	p.scheduleEnd(context.Background(), testRepo, pushward.Content{
		Template: pushward.TemplateSteps, Progress: 1.0, State: "Success",
		CurrentStep: pushward.IntPtr(1), TotalSteps: pushward.IntPtr(1),
	})

	got := testutil.WaitForCalls(t, calls, mu, 3, 2*time.Second)
	var phase1, phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &phase1)
	testutil.UnmarshalBody(t, got[2].Body, &phase2)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("phase 1 state = %q, want ongoing", phase1.State)
	}
	if phase2.State != pushward.StateEnded {
		t.Errorf("phase 2 state = %q, want ended", phase2.State)
	}
	// Phase 2 clears the tracked entry so the repo is eligible again.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		_, still := p.tracked[testRepo]
		p.mu.Unlock()
		if !still {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the tracked entry was not cleared after the end")
}

func TestScheduleEnd_UnknownRepoIsANoop(t *testing.T) {
	p, calls, mu := newPoller(t, testConfig(), activeMux(t, runsJSON(), jobsJSON()))
	p.scheduleEnd(context.Background(), "acme/nope", pushward.Content{
		Template:    pushward.TemplateSteps,
		CurrentStep: pushward.IntPtr(1), TotalSteps: pushward.IntPtr(1),
	})
	if n := len(testutil.GetCalls(calls, mu)); n != 0 {
		t.Errorf("made %d calls, want 0", n)
	}
}

// --- repo discovery ---

func TestRefreshRepos_NoOwnerIsANoop(t *testing.T) {
	p, _, _ := newPoller(t, testConfig(), http.NotFoundHandler())
	p.cfg.Forgejo.Owner = ""
	if err := p.refreshRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshRepos_MergesDiscoveredAndConfigured(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"acme"}`))
	})
	mux.HandleFunc("/api/v1/user/repos", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"full_name":"acme/app"},{"full_name":"acme/infra"}]`))
	})
	p, _, _ := newPoller(t, testConfig(), mux)
	p.cfg.Forgejo.Owner = "acme"
	p.cfg.Forgejo.Repos = []string{"other/thing", "acme/app"} // one overlap
	p.repos = nil

	if err := p.refreshRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"acme/app", "acme/infra", "other/thing"}
	if !reflect.DeepEqual(p.repos, want) {
		t.Errorf("repos = %v, want %v (deduped, discovered first)", p.repos, want)
	}
}

func TestRefreshRepos_RespectsCooldown(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"login":"acme"}`))
	})
	mux.HandleFunc("/api/v1/user/repos", func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`[{"full_name":"acme/app"}]`))
	})
	p, _, _ := newPoller(t, testConfig(), mux)
	p.cfg.Forgejo.Owner = "acme"

	for range 3 {
		if err := p.refreshRepos(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if hits != 1 {
		t.Errorf("discovered %d times inside the cooldown, want 1", hits)
	}
}

// --- Run lifecycle ---

func TestRun_ShutsDownOnContextCancel(t *testing.T) {
	p, _, _ := newPoller(t, testConfig(), activeMux(t, `{"total_count":0,"workflow_runs":[]}`, jobsJSON()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Run(ctx); err != nil {
		t.Errorf("Run returned %v, want nil on cancel", err)
	}
}

func TestRun_DrainsPendingEndTimers(t *testing.T) {
	p, _, _ := newPoller(t, testConfig(), activeMux(t, `{"total_count":0,"workflow_runs":[]}`, jobsJSON()))
	trackRun(p, 1, 1, []string{"build"})
	p.scheduleEnd(context.Background(), testRepo, pushward.Content{
		Template:    pushward.TemplateSteps,
		CurrentStep: pushward.IntPtr(1), TotalSteps: pushward.IntPtr(1),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		_ = p.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not drain its end timers")
	}
}

// --- helpers ---

func TestRepoName(t *testing.T) {
	cases := map[string]string{
		"acme/app": "app",
		"noslash":  "noslash",
		"a/b/c":    "b/c",
		"acme/":    "",
	}
	for in, want := range cases {
		if got := repoName(in); got != want {
			t.Errorf("repoName(%q) = %q, want %q", in, got, want)
		}
	}
}

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

func TestRunOutcome(t *testing.T) {
	mk := func(conclusion string) fjclient.Run { return fjclient.Run{Conclusion: conclusion} }
	tests := []struct {
		conclusion string
		anyFailed  bool
		wantState  string
		wantColor  string
	}{
		{"success", false, "Success", pushward.ColorGreen},
		{"failure", false, "Failed", pushward.ColorRed},
		{"cancelled", false, "Cancelled", pushward.ColorOrange},
		{"skipped", false, "Skipped", pushward.ColorBlue},
		// An unrecognised terminal status with a failed job under it still reads
		// as a failure rather than a success.
		{"", true, "Failed", pushward.ColorRed},
		{"", false, "Complete", pushward.ColorGreen},
	}
	for _, tc := range tests {
		state, color := runOutcome(mk(tc.conclusion), tc.anyFailed)
		if state != tc.wantState || color != tc.wantColor {
			t.Errorf("runOutcome(%q, %v) = (%q, %q), want (%q, %q)",
				tc.conclusion, tc.anyFailed, state, color, tc.wantState, tc.wantColor)
		}
	}
}

func TestNew(t *testing.T) {
	cfg := testConfig()
	cfg.Forgejo.Repos = []string{"a/b", "c/d"}
	p := New(cfg, fjclient.NewClient("https://x", "t", fjclient.Options{}), pushward.NewClient("http://y", "k"))
	if len(p.repos) != 2 {
		t.Errorf("repos = %v", p.repos)
	}
	if len(p.tracked) != 0 {
		t.Errorf("tracked = %v", p.tracked)
	}
}
