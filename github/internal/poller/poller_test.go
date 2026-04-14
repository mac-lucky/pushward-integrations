package poller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/github/internal/config"
	ghclient "github.com/mac-lucky/pushward-integrations/github/internal/github"
	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func testConfig() *config.Config {
	return &config.Config{
		PushWard: sharedconfig.PushWardConfig{
			Priority:       1,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
		Polling: config.PollingConfig{
			IdleInterval: 60 * time.Second,
		},
	}
}

func TestComputeSteps_AllQueued(t *testing.T) {
	jobs := []ghclient.Job{
		{Name: "Lint", Status: "queued"},
		{Name: "Build", Status: "queued"},
		{Name: "Test", Status: "queued"},
	}
	info := computeSteps(jobs)
	if info.TotalSteps != 3 {
		t.Errorf("expected TotalSteps=3, got %d", info.TotalSteps)
	}
	if info.CurrentStepName != "Queued" {
		t.Errorf("expected CurrentStepName=Queued, got %s", info.CurrentStepName)
	}
	if info.CurrentStep != 1 {
		t.Errorf("expected CurrentStep=1, got %d", info.CurrentStep)
	}
	if info.AllCompleted {
		t.Error("expected AllCompleted=false")
	}
	if info.Progress != 0.0 {
		t.Errorf("expected Progress=0.0, got %f", info.Progress)
	}
	if len(info.StepRows) != 3 || info.StepRows[0] != 1 || info.StepRows[1] != 1 || info.StepRows[2] != 1 {
		t.Errorf("expected StepRows=[1,1,1], got %v", info.StepRows)
	}
}

func TestComputeSteps_MatrixJobs(t *testing.T) {
	jobs := []ghclient.Job{
		{Name: "Build (ubuntu, node-16)", Status: "completed", Conclusion: "success"},
		{Name: "Build (ubuntu, node-18)", Status: "completed", Conclusion: "success"},
		{Name: "Build (ubuntu, node-20)", Status: "in_progress"},
		{Name: "Test", Status: "queued"},
	}
	info := computeSteps(jobs)
	if info.TotalSteps != 2 {
		t.Errorf("expected TotalSteps=2, got %d", info.TotalSteps)
	}
	if info.CurrentStepName != "Build" {
		t.Errorf("expected CurrentStepName=Build, got %s", info.CurrentStepName)
	}
	if info.CurrentStep != 1 {
		t.Errorf("expected CurrentStep=1, got %d", info.CurrentStep)
	}
	if len(info.StepRows) != 2 || info.StepRows[0] != 3 || info.StepRows[1] != 1 {
		t.Errorf("expected StepRows=[3,1], got %v", info.StepRows)
	}
	if info.Progress != 0.5 {
		t.Errorf("expected Progress=0.5, got %f", info.Progress)
	}
}

func TestComputeSteps_AllCompletedSuccess(t *testing.T) {
	jobs := []ghclient.Job{
		{Name: "Lint", Status: "completed", Conclusion: "success"},
		{Name: "Build", Status: "completed", Conclusion: "success"},
	}
	info := computeSteps(jobs)
	if !info.AllCompleted {
		t.Error("expected AllCompleted=true")
	}
	if info.AnyFailed {
		t.Error("expected AnyFailed=false")
	}
	if info.TotalSteps != 2 {
		t.Errorf("expected TotalSteps=2, got %d", info.TotalSteps)
	}
	if info.Progress != 1.0 {
		t.Errorf("expected Progress=1.0, got %f", info.Progress)
	}
}

func TestComputeSteps_WithFailure(t *testing.T) {
	jobs := []ghclient.Job{
		{Name: "Lint", Status: "completed", Conclusion: "success"},
		{Name: "Build", Status: "completed", Conclusion: "failure"},
	}
	info := computeSteps(jobs)
	if !info.AllCompleted {
		t.Error("expected AllCompleted=true")
	}
	if !info.AnyFailed {
		t.Error("expected AnyFailed=true")
	}
}

func TestScheduleEnd_TwoPhaseSuccess(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")

	p := &Poller{
		cfg:     cfg,
		pw:      client,
		tracked: make(map[string]*trackedRun),
	}

	p.tracked["owner/repo"] = &trackedRun{
		Repo:  "owner/repo",
		RunID: 100,
		Slug:  "gh-repo",
		Name:  "CI",
	}

	content := pushward.Content{
		Template:     "steps",
		Progress:     1.0,
		State:        "Success",
		Icon:         "arrow.triangle.branch",
		Subtitle:     "repo / CI",
		AccentColor:  "green",
		CurrentStep:  pushward.IntPtr(2),
		TotalSteps:   pushward.IntPtr(2),
		URL:          "https://github.com/owner/repo/actions/runs/100",
		SecondaryURL: "https://github.com/owner/repo",
	}

	p.scheduleEnd("owner/repo", content)

	// Wait for both phases to complete
	time.Sleep(100 * time.Millisecond)

	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 API calls, got %d", len(got))
	}

	// Phase 1: ONGOING
	if got[0].Method != "PATCH" {
		t.Errorf("phase 1: expected PATCH, got %s", got[0].Method)
	}
	if got[0].Path != "/activity/gh-repo" {
		t.Errorf("phase 1: expected /activity/gh-repo, got %s", got[0].Path)
	}
	var req1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req1)
	if req1.State != pushward.StateOngoing {
		t.Errorf("phase 1: expected ONGOING, got %s", req1.State)
	}
	if req1.Content.State != "Success" {
		t.Errorf("phase 1: expected content state Success, got %s", req1.Content.State)
	}

	// Phase 2: ENDED
	if got[1].Method != "PATCH" {
		t.Errorf("phase 2: expected PATCH, got %s", got[1].Method)
	}
	var req2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req2)
	if req2.State != pushward.StateEnded {
		t.Errorf("phase 2: expected ENDED, got %s", req2.State)
	}
	if req2.Content.State != "Success" {
		t.Errorf("phase 2: expected content state Success, got %s", req2.Content.State)
	}

	// Repo should be removed from tracked
	p.mu.Lock()
	if _, ok := p.tracked["owner/repo"]; ok {
		t.Error("expected repo to be removed from tracked after two-phase end")
	}
	p.mu.Unlock()
}

func TestScheduleEnd_TwoPhaseFailed(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")

	p := &Poller{
		cfg:     cfg,
		pw:      client,
		tracked: make(map[string]*trackedRun),
	}

	p.tracked["owner/repo"] = &trackedRun{
		Repo:  "owner/repo",
		RunID: 200,
		Slug:  "gh-repo",
		Name:  "CI",
	}

	content := pushward.Content{
		Template:     "steps",
		Progress:     1.0,
		State:        "Failed",
		Icon:         "arrow.triangle.branch",
		Subtitle:     "repo / CI",
		AccentColor:  "red",
		CurrentStep:  pushward.IntPtr(1),
		TotalSteps:   pushward.IntPtr(3),
		URL:          "https://github.com/owner/repo/actions/runs/200",
		SecondaryURL: "https://github.com/owner/repo",
	}

	p.scheduleEnd("owner/repo", content)
	time.Sleep(100 * time.Millisecond)

	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 API calls, got %d", len(got))
	}

	var req1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req1)
	if req1.State != pushward.StateOngoing {
		t.Errorf("phase 1: expected ONGOING, got %s", req1.State)
	}
	if req1.Content.AccentColor != "red" {
		t.Errorf("phase 1: expected accent_color red, got %s", req1.Content.AccentColor)
	}

	var req2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req2)
	if req2.State != pushward.StateEnded {
		t.Errorf("phase 2: expected ENDED, got %s", req2.State)
	}
	if req2.Content.State != "Failed" {
		t.Errorf("phase 2: expected content state Failed, got %s", req2.Content.State)
	}
}

func TestScheduleEnd_CancelledByNewRun(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	// Use longer delays so we can cancel before they fire
	cfg.PushWard.EndDelay = 500 * time.Millisecond
	cfg.PushWard.EndDisplayTime = 500 * time.Millisecond
	client := pushward.NewClient(srv.URL, "hlk_test")

	p := &Poller{
		cfg:     cfg,
		pw:      client,
		tracked: make(map[string]*trackedRun),
	}

	p.tracked["owner/repo"] = &trackedRun{
		Repo:  "owner/repo",
		RunID: 300,
		Slug:  "gh-repo",
		Name:  "CI",
	}

	content := pushward.Content{
		Template: "steps",
		State:    "Success",
	}

	p.scheduleEnd("owner/repo", content)

	// Simulate new run taking over: cancel the timer and replace the entry
	time.Sleep(10 * time.Millisecond)
	p.mu.Lock()
	if t2, ok := p.tracked["owner/repo"]; ok && t2.endTimers != nil {
		t2.endTimers.phase1.Stop()
		if t2.endTimers.phase2 != nil {
			t2.endTimers.phase2.Stop()
		}
		delete(p.tracked, "owner/repo")
	}
	p.tracked["owner/repo"] = &trackedRun{
		Repo:  "owner/repo",
		RunID: 301,
		Slug:  "gh-repo",
		Name:  "CI v2",
	}
	p.mu.Unlock()

	// Wait long enough for the original timer to have fired if not cancelled
	time.Sleep(200 * time.Millisecond)

	got := testutil.GetCalls(calls, mu)
	if len(got) != 0 {
		t.Fatalf("expected 0 API calls after cancellation, got %d", len(got))
	}

	// New run should still be tracked
	p.mu.Lock()
	entry, ok := p.tracked["owner/repo"]
	p.mu.Unlock()
	if !ok {
		t.Fatal("expected new run to be tracked")
	}
	if entry.RunID != 301 {
		t.Errorf("expected RunID 301, got %d", entry.RunID)
	}
}

func TestComputeSteps_ReusableWorkflowMatrix(t *testing.T) {
	// Simulates a real reusable workflow where jobs appear with "ci-cd / " prefix
	// and matrix parameters. All jobs visible from the start.
	jobs := []ghclient.Job{
		{Name: "ci-cd / Check Code Changes", Status: "completed", Conclusion: "success"},
		{Name: "ci-cd / Setup Build Environment", Status: "completed", Conclusion: "success"},
		{Name: "ci-cd / Code Analysis (go-vet)", Status: "in_progress"},
		{Name: "ci-cd / Code Analysis (staticcheck)", Status: "queued"},
		{Name: "ci-cd / Code Analysis (grype)", Status: "queued"},
		{Name: "ci-cd / Go Tests", Status: "in_progress"},
		{Name: "ci-cd / Build Container Image", Status: "queued"},
		{Name: "ci-cd / Container Integration Test", Status: "completed", Conclusion: "skipped"},
		{Name: "ci-cd / Kubernetes Integration Test", Status: "completed", Conclusion: "skipped"},
		{Name: "ci-cd / Build (linux/amd64)", Status: "queued"},
		{Name: "ci-cd / Build (linux/arm64)", Status: "queued"},
		{Name: "ci-cd / Create Multi-arch Manifest", Status: "queued"},
		{Name: "ci-cd / Post-deployment Verification", Status: "queued"},
	}
	info := computeSteps(jobs)

	// 10 unique steps after matrix grouping:
	// Check Code Changes, Setup Build Environment, Code Analysis (x3),
	// Go Tests, Build Container Image, Container Integration Test,
	// Kubernetes Integration Test, Build (x2), Create Multi-arch Manifest,
	// Post-deployment Verification
	if info.TotalSteps != 10 {
		t.Errorf("expected TotalSteps=10, got %d", info.TotalSteps)
	}
	if info.CurrentStepName != "Code Analysis" {
		t.Errorf("expected CurrentStepName='Code Analysis', got %q", info.CurrentStepName)
	}
	if info.CurrentStep != 3 {
		t.Errorf("expected CurrentStep=3, got %d", info.CurrentStep)
	}
	// StepRows: [1,1,3,1,1,1,1,2,1,1]
	wantRows := []int{1, 1, 3, 1, 1, 1, 1, 2, 1, 1}
	if len(info.StepRows) != len(wantRows) {
		t.Fatalf("expected StepRows len=%d, got %d: %v", len(wantRows), len(info.StepRows), info.StepRows)
	}
	for i, v := range wantRows {
		if info.StepRows[i] != v {
			t.Errorf("StepRows[%d]: expected %d, got %d (full: %v)", i, v, info.StepRows[i], info.StepRows)
		}
	}
	// Progress: 4 completed (check, setup, container-test, k8s-test) out of 13 jobs
	expectedProgress := 4.0 / 13.0
	if info.Progress != expectedProgress {
		t.Errorf("expected Progress=%f, got %f", expectedProgress, info.Progress)
	}
}

func TestComputeSteps_LazyJobCreation(t *testing.T) {
	// Simulates the progressive job discovery issue: initially only 7 jobs
	// visible (5 steps), then more appear.

	// Poll 1: Only first few jobs exist
	jobsPoll1 := []ghclient.Job{
		{Name: "ci-cd / Check Code Changes", Status: "completed", Conclusion: "success"},
		{Name: "ci-cd / Setup Build Environment", Status: "completed", Conclusion: "success"},
		{Name: "ci-cd / Code Analysis (go-vet)", Status: "in_progress"},
		{Name: "ci-cd / Code Analysis (staticcheck)", Status: "queued"},
		{Name: "ci-cd / Code Analysis (grype)", Status: "queued"},
		{Name: "ci-cd / Go Tests", Status: "in_progress"},
		{Name: "ci-cd / Build Container Image", Status: "queued"},
	}
	info1 := computeSteps(jobsPoll1)
	if info1.TotalSteps != 5 {
		t.Errorf("poll 1: expected TotalSteps=5, got %d", info1.TotalSteps)
	}

	// Poll 2: More jobs appeared (after code-analysis completed)
	jobsPoll2 := []ghclient.Job{
		{Name: "ci-cd / Check Code Changes", Status: "completed", Conclusion: "success"},
		{Name: "ci-cd / Setup Build Environment", Status: "completed", Conclusion: "success"},
		{Name: "ci-cd / Code Analysis (go-vet)", Status: "completed", Conclusion: "success"},
		{Name: "ci-cd / Code Analysis (staticcheck)", Status: "completed", Conclusion: "success"},
		{Name: "ci-cd / Code Analysis (grype)", Status: "completed", Conclusion: "success"},
		{Name: "ci-cd / Go Tests", Status: "completed", Conclusion: "success"},
		{Name: "ci-cd / Build Container Image", Status: "in_progress"},
		{Name: "ci-cd / Container Integration Test", Status: "completed", Conclusion: "skipped"},
		{Name: "ci-cd / Kubernetes Integration Test", Status: "completed", Conclusion: "skipped"},
		{Name: "ci-cd / Build (linux/amd64)", Status: "queued"},
		{Name: "ci-cd / Build (linux/arm64)", Status: "queued"},
		{Name: "ci-cd / Create Multi-arch Manifest", Status: "queued"},
		{Name: "ci-cd / Post-deployment Verification", Status: "queued"},
	}
	info2 := computeSteps(jobsPoll2)
	if info2.TotalSteps != 10 {
		t.Errorf("poll 2: expected TotalSteps=10, got %d", info2.TotalSteps)
	}

	// Verify that max clamping would work: total should go from 5 to 10
	if info2.TotalSteps <= info1.TotalSteps {
		t.Errorf("expected poll 2 total (%d) > poll 1 total (%d)", info2.TotalSteps, info1.TotalSteps)
	}
}

func TestScheduleEnd_ContentPreserved(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")

	p := &Poller{
		cfg:     cfg,
		pw:      client,
		tracked: make(map[string]*trackedRun),
	}

	p.tracked["owner/repo"] = &trackedRun{
		Repo:  "owner/repo",
		RunID: 400,
		Slug:  "gh-repo",
		Name:  "Deploy",
	}

	content := pushward.Content{
		Template:     "steps",
		Progress:     1.0,
		State:        "Success",
		Icon:         "arrow.triangle.branch",
		Subtitle:     "repo / Deploy",
		AccentColor:  "green",
		CurrentStep:  pushward.IntPtr(4),
		TotalSteps:   pushward.IntPtr(4),
		URL:          "https://github.com/owner/repo/actions/runs/400",
		SecondaryURL: "https://github.com/owner/repo",
	}

	p.scheduleEnd("owner/repo", content)
	time.Sleep(100 * time.Millisecond)

	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 API calls, got %d", len(got))
	}

	var req1, req2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req1)
	testutil.UnmarshalBody(t, got[1].Body, &req2)

	// Verify content fields are identical between Phase 1 and Phase 2
	if req1.Content.Template != req2.Content.Template {
		t.Errorf("template mismatch: %s vs %s", req1.Content.Template, req2.Content.Template)
	}
	if req1.Content.Progress != req2.Content.Progress {
		t.Errorf("progress mismatch: %f vs %f", req1.Content.Progress, req2.Content.Progress)
	}
	if req1.Content.State != req2.Content.State {
		t.Errorf("state mismatch: %s vs %s", req1.Content.State, req2.Content.State)
	}
	if req1.Content.Icon != req2.Content.Icon {
		t.Errorf("icon mismatch: %s vs %s", req1.Content.Icon, req2.Content.Icon)
	}
	if req1.Content.Subtitle != req2.Content.Subtitle {
		t.Errorf("subtitle mismatch: %s vs %s", req1.Content.Subtitle, req2.Content.Subtitle)
	}
	if req1.Content.AccentColor != req2.Content.AccentColor {
		t.Errorf("accent_color mismatch: %s vs %s", req1.Content.AccentColor, req2.Content.AccentColor)
	}
	if *req1.Content.CurrentStep != *req2.Content.CurrentStep {
		t.Errorf("current_step mismatch: %d vs %d", *req1.Content.CurrentStep, *req2.Content.CurrentStep)
	}
	if *req1.Content.TotalSteps != *req2.Content.TotalSteps {
		t.Errorf("total_steps mismatch: %d vs %d", *req1.Content.TotalSteps, *req2.Content.TotalSteps)
	}
	if req1.Content.URL != req2.Content.URL {
		t.Errorf("url mismatch: %s vs %s", req1.Content.URL, req2.Content.URL)
	}
	if req1.Content.SecondaryURL != req2.Content.SecondaryURL {
		t.Errorf("secondary_url mismatch: %s vs %s", req1.Content.SecondaryURL, req2.Content.SecondaryURL)
	}
}

// --- Utility function tests ---

func TestRepoName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"owner/repo", "repo"},
		{"mac-lucky/pushward-server", "pushward-server"},
		{"noslash", "noslash"},
		{"a/b/c", "b/c"},
	}
	for _, tt := range tests {
		got := repoName(tt.input)
		if got != tt.want {
			t.Errorf("repoName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBaseJobName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Build (ubuntu, node-16)", "Build"},
		{"Test", "Test"},
		{"ci-cd / Code Analysis (go-vet)", "Code Analysis"},
		{"ci-cd / Setup Build Environment", "Setup Build Environment"},
		{"ci-cd / Go Tests", "Go Tests"},
		{"Deploy (prod)", "Deploy"},
		{"NoParens", "NoParens"},
		{"Has (Parens) Mid", "Has (Parens) Mid"}, // no trailing )
	}
	for _, tt := range tests {
		got := baseJobName(tt.input)
		if got != tt.want {
			t.Errorf("baseJobName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestComputeSteps_Empty(t *testing.T) {
	info := computeSteps(nil)
	if info.TotalSteps != 0 {
		t.Errorf("expected TotalSteps=0, got %d", info.TotalSteps)
	}
	if info.Progress != 0 {
		t.Errorf("expected Progress=0, got %f", info.Progress)
	}
	if !info.AllCompleted {
		// All zero jobs means all completed
		t.Error("expected AllCompleted=true for empty jobs")
	}
}

func TestComputeSteps_WithCancelled(t *testing.T) {
	jobs := []ghclient.Job{
		{Name: "Lint", Status: "completed", Conclusion: "success"},
		{Name: "Build", Status: "completed", Conclusion: "cancelled"},
	}
	info := computeSteps(jobs)
	if !info.AnyFailed {
		t.Error("expected AnyFailed=true for cancelled job")
	}
}

func TestNew(t *testing.T) {
	cfg := testConfig()
	cfg.GitHub.Repos = []string{"owner/repo1", "owner/repo2"}
	gh := ghclient.NewClient("token")
	pw := pushward.NewClient("http://localhost", "key")

	p := New(cfg, gh, pw)
	if p.cfg != cfg {
		t.Error("expected cfg to be set")
	}
	if p.gh != gh {
		t.Error("expected gh to be set")
	}
	if p.pw != pw {
		t.Error("expected pw to be set")
	}
	if len(p.repos) != 2 {
		t.Errorf("expected 2 repos, got %d", len(p.repos))
	}
	if len(p.tracked) != 0 {
		t.Errorf("expected empty tracked map, got %d", len(p.tracked))
	}
}

// --- Mock GitHub server helper ---

func mockGitHubClient(t *testing.T, handler http.Handler) *ghclient.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := ghclient.NewClient("test-token")
	c.SetBaseURL(srv.URL)
	return c
}

// --- pollIdle tests ---

func TestPollIdle_DiscoversAndTracksWorkflow(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{
			TotalCount: 1,
			WorkflowRuns: []ghclient.WorkflowRun{
				{
					ID: 42, Name: "CI", Status: "in_progress", HeadBranch: "main",
					HTMLURL: "https://github.com/owner/repo/actions/runs/42",
				},
			},
		})
	})
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 2,
			Jobs: []ghclient.Job{
				{ID: 1, Name: "Build", Status: "in_progress"},
				{ID: 2, Name: "Test", Status: "queued"},
			},
		})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	cfg := testConfig()
	p := &Poller{
		cfg:     cfg,
		gh:      gh,
		pw:      pw,
		tracked: make(map[string]*trackedRun),
		repos:   []string{"owner/repo"},
	}

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 API calls (create + update), got %d", len(got))
	}

	// Verify create activity (POST)
	if got[0].Method != "POST" {
		t.Errorf("expected POST for create, got %s", got[0].Method)
	}
	if got[0].Path != "/activities" {
		t.Errorf("expected /activities path, got %s", got[0].Path)
	}

	// Verify initial update (PATCH)
	if got[1].Method != "PATCH" {
		t.Errorf("expected PATCH for update, got %s", got[1].Method)
	}
	if got[1].Path != "/activity/gh-repo" {
		t.Errorf("expected /activity/gh-repo, got %s", got[1].Path)
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req)
	if req.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING state, got %s", req.State)
	}
	if req.Content.Template != "steps" {
		t.Errorf("expected steps template, got %s", req.Content.Template)
	}
	if req.Content.URL != "https://github.com/owner/repo/actions/runs/42" {
		t.Errorf("unexpected URL: %s", req.Content.URL)
	}
	if req.Content.SecondaryURL != "https://github.com/owner/repo" {
		t.Errorf("unexpected SecondaryURL: %s", req.Content.SecondaryURL)
	}

	// Verify tracked state
	p.mu.Lock()
	tracked, ok := p.tracked["owner/repo"]
	p.mu.Unlock()
	if !ok {
		t.Fatal("expected repo to be tracked")
	}
	if tracked.RunID != 42 {
		t.Errorf("expected RunID 42, got %d", tracked.RunID)
	}
	if tracked.Slug != "gh-repo" {
		t.Errorf("expected slug gh-repo, got %s", tracked.Slug)
	}
	if tracked.maxTotalSteps != 2 {
		t.Errorf("expected maxTotalSteps 2, got %d", tracked.maxTotalSteps)
	}
}

func TestPollIdle_SkipsAlreadyTrackedRepo(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Error("GitHub API should not be called for tracked repo")
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, _, _ := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	cfg := testConfig()
	p := &Poller{
		cfg:     cfg,
		gh:      gh,
		pw:      pw,
		tracked: make(map[string]*trackedRun),
		repos:   []string{"owner/repo"},
	}
	// Pre-track the repo (no pending end timer, so it's actively tracked)
	p.tracked["owner/repo"] = &trackedRun{
		Repo:  "owner/repo",
		RunID: 100,
		Slug:  "gh-repo",
	}

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPollIdle_NoRunsFound(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{TotalCount: 0})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	cfg := testConfig()
	p := &Poller{
		cfg:     cfg,
		gh:      gh,
		pw:      pw,
		tracked: make(map[string]*trackedRun),
		repos:   []string{"owner/repo"},
	}

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := testutil.GetCalls(calls, mu)
	if len(got) != 0 {
		t.Errorf("expected 0 API calls when no runs found, got %d", len(got))
	}
	if len(p.tracked) != 0 {
		t.Errorf("expected nothing tracked, got %d", len(p.tracked))
	}
}

func TestPollIdle_PicksMostRecentRun(t *testing.T) {
	now := time.Now()
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{
			TotalCount: 2,
			WorkflowRuns: []ghclient.WorkflowRun{
				{ID: 10, Name: "Old", CreatedAt: now.Add(-time.Hour)},
				{ID: 20, Name: "New", CreatedAt: now},
			},
		})
	})
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/20/jobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{TotalCount: 0})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, _, _ := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	cfg := testConfig()
	p := &Poller{
		cfg:     cfg,
		gh:      gh,
		pw:      pw,
		tracked: make(map[string]*trackedRun),
		repos:   []string{"owner/repo"},
	}

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	tracked := p.tracked["owner/repo"]
	p.mu.Unlock()
	if tracked.RunID != 20 {
		t.Errorf("expected most recent run (ID=20), got %d", tracked.RunID)
	}
}

// --- pollActive tests ---

func TestPollActive_UpdatesOngoingWorkflow(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 3,
			Jobs: []ghclient.Job{
				{ID: 1, Name: "Lint", Status: "completed", Conclusion: "success"},
				{ID: 2, Name: "Build", Status: "in_progress"},
				{ID: 3, Name: "Test", Status: "queued"},
			},
		})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	cfg := testConfig()
	p := &Poller{
		cfg:     cfg,
		gh:      gh,
		pw:      pw,
		tracked: make(map[string]*trackedRun),
		repos:   []string{"owner/repo"},
	}
	p.tracked["owner/repo"] = &trackedRun{
		Repo:          "owner/repo",
		RunID:         42,
		Slug:          "gh-repo",
		Name:          "CI",
		HTMLURL:       "https://github.com/owner/repo/actions/runs/42",
		maxTotalSteps: 3,
		maxStepRows:   []int{1, 1, 1},
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 PATCH call, got %d", len(got))
	}
	if got[0].Method != "PATCH" {
		t.Errorf("expected PATCH, got %s", got[0].Method)
	}

	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)
	if req.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", req.State)
	}
	if req.Content.State != "Build" {
		t.Errorf("expected state Build, got %s", req.Content.State)
	}
	if req.Content.AccentColor != "green" {
		t.Errorf("expected green accent, got %s", req.Content.AccentColor)
	}
}

func TestPollActive_CompletesSuccessfulWorkflow(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 2,
			Jobs: []ghclient.Job{
				{ID: 1, Name: "Build", Status: "completed", Conclusion: "success"},
				{ID: 2, Name: "Test", Status: "completed", Conclusion: "success"},
			},
		})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	cfg := testConfig()
	p := &Poller{
		cfg:     cfg,
		gh:      gh,
		pw:      pw,
		tracked: make(map[string]*trackedRun),
	}
	p.tracked["owner/repo"] = &trackedRun{
		Repo:          "owner/repo",
		RunID:         42,
		Slug:          "gh-repo",
		Name:          "CI",
		HTMLURL:       "https://github.com/owner/repo/actions/runs/42",
		maxTotalSteps: 2,
		maxStepRows:   []int{1, 1},
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Wait for two-phase end to fire
	time.Sleep(100 * time.Millisecond)

	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 API calls (two-phase end), got %d", len(got))
	}

	var req1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req1)
	if req1.State != pushward.StateOngoing {
		t.Errorf("phase 1: expected ONGOING, got %s", req1.State)
	}
	if req1.Content.State != "Success" {
		t.Errorf("phase 1: expected Success, got %s", req1.Content.State)
	}
	if req1.Content.AccentColor != "green" {
		t.Errorf("phase 1: expected green, got %s", req1.Content.AccentColor)
	}

	var req2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req2)
	if req2.State != pushward.StateEnded {
		t.Errorf("phase 2: expected ENDED, got %s", req2.State)
	}
}

func TestPollActive_CompletesFailedWorkflow(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 2,
			Jobs: []ghclient.Job{
				{ID: 1, Name: "Build", Status: "completed", Conclusion: "failure"},
				{ID: 2, Name: "Test", Status: "completed", Conclusion: "success"},
			},
		})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	cfg := testConfig()
	p := &Poller{
		cfg:     cfg,
		gh:      gh,
		pw:      pw,
		tracked: make(map[string]*trackedRun),
	}
	p.tracked["owner/repo"] = &trackedRun{
		Repo:          "owner/repo",
		RunID:         42,
		Slug:          "gh-repo",
		Name:          "CI",
		HTMLURL:       "https://github.com/owner/repo/actions/runs/42",
		maxTotalSteps: 2,
		maxStepRows:   []int{1, 1},
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 API calls, got %d", len(got))
	}

	var req1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req1)
	if req1.Content.State != "Failed" {
		t.Errorf("expected Failed, got %s", req1.Content.State)
	}
	if req1.Content.AccentColor != "red" {
		t.Errorf("expected red, got %s", req1.Content.AccentColor)
	}
}

func TestPollActive_SkipsRepoWithPendingEnd(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Error("GitHub API should not be called for repo with pending end")
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, _, _ := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	cfg := testConfig()
	p := &Poller{
		cfg:     cfg,
		gh:      gh,
		pw:      pw,
		tracked: make(map[string]*trackedRun),
	}
	timer := time.AfterFunc(time.Hour, func() {}) // won't fire
	defer timer.Stop()
	p.tracked["owner/repo"] = &trackedRun{
		Repo:      "owner/repo",
		RunID:     42,
		Slug:      "gh-repo",
		endTimers: &timerPair{phase1: timer},
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPollActive_NoJobs(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{TotalCount: 0})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	cfg := testConfig()
	p := &Poller{
		cfg:     cfg,
		gh:      gh,
		pw:      pw,
		tracked: make(map[string]*trackedRun),
	}
	p.tracked["owner/repo"] = &trackedRun{
		Repo:  "owner/repo",
		RunID: 42,
		Slug:  "gh-repo",
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := testutil.GetCalls(calls, mu)
	if len(got) != 0 {
		t.Errorf("expected 0 PushWard calls for no jobs, got %d", len(got))
	}
}

func TestPollActive_MaxStepsClamping(t *testing.T) {
	// Simulate lazy job creation: initially 3 steps tracked, poll returns only 2
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 2,
			Jobs: []ghclient.Job{
				{ID: 1, Name: "Build", Status: "in_progress"},
				{ID: 2, Name: "Test", Status: "queued"},
			},
		})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	cfg := testConfig()
	p := &Poller{
		cfg:     cfg,
		gh:      gh,
		pw:      pw,
		tracked: make(map[string]*trackedRun),
	}
	p.tracked["owner/repo"] = &trackedRun{
		Repo:          "owner/repo",
		RunID:         42,
		Slug:          "gh-repo",
		Name:          "CI",
		HTMLURL:       "https://github.com/owner/repo/actions/runs/42",
		maxTotalSteps: 5, // Higher than current 2 steps
		maxStepRows:   []int{1, 1, 1, 1, 1},
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}

	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)
	// TotalSteps should be clamped to maxTotalSteps (5), not the current 2
	if *req.Content.TotalSteps != 5 {
		t.Errorf("expected TotalSteps clamped to 5, got %d", *req.Content.TotalSteps)
	}
}

// --- refreshRepos tests ---

func TestRefreshRepos_NoOwner(t *testing.T) {
	cfg := testConfig()
	// No owner set
	p := &Poller{
		cfg:   cfg,
		repos: []string{"owner/repo"},
	}

	if err := p.refreshRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	// repos should be unchanged
	if len(p.repos) != 1 {
		t.Errorf("expected repos unchanged, got %d", len(p.repos))
	}
}

func TestRefreshRepos_MergesDiscoveredAndConfigured(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]ghclient.Repository{
			{FullName: "testowner/discovered1"},
			{FullName: "testowner/discovered2"},
		})
	})
	gh := mockGitHubClient(t, ghMux)

	cfg := testConfig()
	cfg.GitHub.Owner = "testowner"
	cfg.GitHub.Repos = []string{"other/configured"}

	p := &Poller{
		cfg:     cfg,
		gh:      gh,
		tracked: make(map[string]*trackedRun),
		repos:   []string{"other/configured"},
	}

	if err := p.refreshRepos(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(p.repos) != 3 {
		t.Fatalf("expected 3 repos (2 discovered + 1 configured), got %d: %v", len(p.repos), p.repos)
	}
}

func TestRefreshRepos_SkipsCooldown(t *testing.T) {
	callCount := 0
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		_ = json.NewEncoder(w).Encode([]ghclient.Repository{})
	})
	gh := mockGitHubClient(t, ghMux)

	cfg := testConfig()
	cfg.GitHub.Owner = "owner"

	p := &Poller{
		cfg:     cfg,
		gh:      gh,
		tracked: make(map[string]*trackedRun),
	}

	// First call should hit the API
	if err := p.refreshRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 API call on first refresh, got %d", callCount)
	}

	// Second call within cooldown should skip
	if err := p.refreshRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Errorf("expected no additional API calls during cooldown, got %d", callCount)
	}
}

func TestRefreshRepos_DeduplicatesRepos(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]ghclient.Repository{
			{FullName: "owner/repo1"},
			{FullName: "owner/repo2"},
		})
	})
	gh := mockGitHubClient(t, ghMux)

	cfg := testConfig()
	cfg.GitHub.Owner = "owner"
	cfg.GitHub.Repos = []string{"owner/repo1"} // duplicate of discovered

	p := &Poller{
		cfg:     cfg,
		gh:      gh,
		tracked: make(map[string]*trackedRun),
		repos:   []string{"owner/repo1"},
	}

	if err := p.refreshRepos(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(p.repos) != 2 {
		t.Errorf("expected 2 repos (deduped), got %d: %v", len(p.repos), p.repos)
	}
}

// --- poll tests ---

func TestPoll_CallsBothPhases(t *testing.T) {
	// poll() calls pollIdle then pollActive. Test that both run.
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{TotalCount: 0})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, _, _ := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	cfg := testConfig()
	p := &Poller{
		cfg:     cfg,
		gh:      gh,
		pw:      pw,
		tracked: make(map[string]*trackedRun),
		repos:   []string{"owner/repo"},
	}

	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// --- scheduleEnd edge case ---

func TestScheduleEnd_UnknownRepo(t *testing.T) {
	cfg := testConfig()
	p := &Poller{
		cfg:     cfg,
		tracked: make(map[string]*trackedRun),
	}

	// Should not panic when repo isn't tracked
	p.scheduleEnd("nonexistent", pushward.Content{})
}

// --- Run tests ---

func TestRun_ShutdownImmediately(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{TotalCount: 0})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, _, _ := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	cfg := testConfig()
	cfg.Polling.IdleInterval = 100 * time.Millisecond

	p := New(cfg, gh, pw)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- p.Run(ctx)
	}()

	// Let it run one poll cycle then cancel
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error on graceful shutdown, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within timeout")
	}
}

func TestRun_CleansUpTimersOnShutdown(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{TotalCount: 0})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, _, _ := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	cfg := testConfig()
	cfg.Polling.IdleInterval = time.Hour // won't tick

	p := New(cfg, gh, pw)

	// Add a tracked entry with a pending end timer
	timer := time.AfterFunc(time.Hour, func() {})
	p.tracked["owner/repo"] = &trackedRun{
		Repo:      "owner/repo",
		RunID:     42,
		Slug:      "gh-repo",
		endTimers: &timerPair{phase1: timer},
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- p.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within timeout")
	}
}
