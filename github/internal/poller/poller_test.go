package poller

import (
	"testing"
	"time"

	"github.com/mac-lucky/pushward-docker/github/internal/config"
	ghclient "github.com/mac-lucky/pushward-docker/github/internal/github"
	sharedconfig "github.com/mac-lucky/pushward-docker/shared/config"
	"github.com/mac-lucky/pushward-docker/shared/pushward"
	"github.com/mac-lucky/pushward-docker/shared/testutil"
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
	if info.CurrentStep != 0 {
		t.Errorf("expected CurrentStep=0, got %d", info.CurrentStep)
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
		Template:     "pipeline",
		Progress:     1.0,
		State:        "Success",
		Icon:         "arrow.triangle.branch",
		Subtitle:     "repo / CI",
		AccentColor:  "green",
		CurrentStep:  intPtr(2),
		TotalSteps:   intPtr(2),
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
	if req1.State != "ONGOING" {
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
	if req2.State != "ENDED" {
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
		Template:     "pipeline",
		Progress:     1.0,
		State:        "Failed",
		Icon:         "arrow.triangle.branch",
		Subtitle:     "repo / CI",
		AccentColor:  "red",
		CurrentStep:  intPtr(1),
		TotalSteps:   intPtr(3),
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
	if req1.State != "ONGOING" {
		t.Errorf("phase 1: expected ONGOING, got %s", req1.State)
	}
	if req1.Content.AccentColor != "red" {
		t.Errorf("phase 1: expected accent_color red, got %s", req1.Content.AccentColor)
	}

	var req2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req2)
	if req2.State != "ENDED" {
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
		Template: "pipeline",
		State:    "Success",
	}

	p.scheduleEnd("owner/repo", content)

	// Simulate new run taking over: cancel the timer and replace the entry
	time.Sleep(10 * time.Millisecond)
	p.mu.Lock()
	if t2, ok := p.tracked["owner/repo"]; ok && t2.endTimer != nil {
		t2.endTimer.Stop()
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
		{Name: "ci-cd / Code Analysis (trivy)", Status: "queued"},
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
	if info.CurrentStepName != "ci-cd / Code Analysis" {
		t.Errorf("expected CurrentStepName='ci-cd / Code Analysis', got %q", info.CurrentStepName)
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
		{Name: "ci-cd / Code Analysis (trivy)", Status: "queued"},
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
		{Name: "ci-cd / Code Analysis (trivy)", Status: "completed", Conclusion: "success"},
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
		Template:     "pipeline",
		Progress:     1.0,
		State:        "Success",
		Icon:         "arrow.triangle.branch",
		Subtitle:     "repo / Deploy",
		AccentColor:  "green",
		CurrentStep:  intPtr(4),
		TotalSteps:   intPtr(4),
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
