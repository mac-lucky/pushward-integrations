package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/github/internal/config"
	ghclient "github.com/mac-lucky/pushward-integrations/github/internal/github"
	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/syncx"
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

// testConfigRender returns a config with the opt-in pill fields switched on.
func testConfigRender(colors, weights bool) *config.Config {
	cfg := testConfig()
	cfg.Render = config.RenderConfig{StepColors: colors, StepWeights: weights}
	return cfg
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

func TestStepColor(t *testing.T) {
	cases := map[string]string{
		"Test":           "yellow",
		"unit-tests":     "yellow",
		"pytest":         "yellow",
		"Lint":           "purple",
		"golangci-lint":  "purple",
		"Build":          "blue",
		"Build (ubuntu)": "blue",
		"Docker Build":   "blue", // build-family keyword wins over docker by switch order
		"Push image":     "cyan",
		"Deploy":         "green",
		"release":        "green",
		"CodeQL":         "orange",
		"security-scan":  "orange",
		"Something Else": "", // unmatched falls back to the accent color
	}
	for name, want := range cases {
		if got := stepColor(name); got != want {
			t.Errorf("stepColor(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestComputeSteps_StepColors(t *testing.T) {
	jobs := []ghclient.Job{
		{Name: "Test", Status: "in_progress"},
		{Name: "Build", Status: "queued"},
		{Name: "Deploy", Status: "queued"},
	}
	info := computeSteps(jobs)
	// step_colors must be one-per-step so the server's length check passes.
	if len(info.StepColors) != info.TotalSteps {
		t.Fatalf("expected StepColors length %d, got %d (%v)", info.TotalSteps, len(info.StepColors), info.StepColors)
	}
	want := []string{"yellow", "blue", "green"}
	for i, w := range want {
		if info.StepColors[i] != w {
			t.Errorf("StepColors[%d] = %q, want %q", i, info.StepColors[i], w)
		}
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

func TestGroupWeights(t *testing.T) {
	// Lint 5s; Build matrix runs in parallel (120/300/60s) so the group weighs the
	// longest = 300s; Deploy 40s. Keyed by group name, not position.
	jobs := []ghclient.Job{
		{
			Name: "Lint", Status: "completed", Conclusion: "success",
			StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:00:05Z",
		},
		{
			Name: "Build (ubuntu)", Status: "completed", Conclusion: "success",
			StartedAt: "2026-01-01T00:00:05Z", CompletedAt: "2026-01-01T00:02:05Z",
		},
		{
			Name: "Build (macos)", Status: "completed", Conclusion: "success",
			StartedAt: "2026-01-01T00:00:05Z", CompletedAt: "2026-01-01T00:05:05Z",
		},
		{
			Name: "Build (windows)", Status: "completed", Conclusion: "success",
			StartedAt: "2026-01-01T00:00:05Z", CompletedAt: "2026-01-01T00:01:05Z",
		},
		{
			Name: "Deploy", Status: "completed", Conclusion: "success",
			StartedAt: "2026-01-01T00:05:05Z", CompletedAt: "2026-01-01T00:05:45Z",
		},
	}
	got := groupWeights(jobs)
	want := map[string]float64{"Lint": 5, "Build": 300, "Deploy": 40}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groupWeights = %v, want %v", got, want)
	}
	// One entry per group computeSteps produces, so projectWeights can size every
	// label.
	if n := computeSteps(jobs).TotalSteps; len(got) != n {
		t.Errorf("len(weights)=%d, want groups=%d", len(got), n)
	}
}

func TestGroupWeights_NoDurations(t *testing.T) {
	// Queued/in-progress jobs have no completed_at, and a completed job missing a
	// start is unmeasurable — with nothing to measure, return nil so callers omit
	// step_weights and pills render equal-width.
	jobs := []ghclient.Job{
		{Name: "Lint", Status: "queued"},
		{Name: "Build", Status: "in_progress", StartedAt: "2026-01-01T00:00:05Z"},
		{
			Name: "Deploy", Status: "completed", Conclusion: "success",
			CompletedAt: "2026-01-01T00:05:45Z",
		},
	}
	if got := groupWeights(jobs); got != nil {
		t.Errorf("groupWeights = %v, want nil", got)
	}
}

func TestGroupWeights_Floor(t *testing.T) {
	// A present-but-unmeasurable group sits alongside a measured one: it keeps the
	// floor (a thin pill) rather than collapsing, and the measured group wins its
	// real duration.
	jobs := []ghclient.Job{
		{
			Name: "Lint", Status: "completed", Conclusion: "success",
			CompletedAt: "2026-01-01T00:00:05Z",
		}, // no StartedAt -> unmeasurable
		{
			Name: "Build", Status: "completed", Conclusion: "success",
			StartedAt: "2026-01-01T00:00:05Z", CompletedAt: "2026-01-01T00:00:10Z",
		},
	}
	got := groupWeights(jobs)
	want := map[string]float64{"Lint": stepWeightFloor, "Build": 5}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groupWeights = %v, want %v", got, want)
	}
}

func TestProjectWeights(t *testing.T) {
	byName := map[string]float64{"Lint": 5, "Build": 300, "Deploy": 40}

	// The core fix: weights follow their label by NAME, so a live scan that
	// surfaces the groups in a different order than the prior run still sizes each
	// pill correctly (positional alignment would have shifted them).
	got := projectWeights([]string{"Deploy", "Lint", "Build"}, byName)
	if want := []float64{40, 5, 300}; !reflect.DeepEqual(got, want) {
		t.Errorf("reordered projection = %v, want %v", got, want)
	}

	// A label GitHub added since the prior run has no history -> mean (115) of the
	// known weights; the length still equals len(labels).
	got = projectWeights([]string{"Lint", "Format", "Build", "Deploy"}, byName)
	if want := []float64{5, 115, 300, 40}; !reflect.DeepEqual(got, want) {
		t.Errorf("unknown-label projection = %v, want %v", got, want)
	}

	// No history -> nil so the send omits step_weights (equal-width pills).
	if got := projectWeights([]string{"Lint", "Build"}, nil); got != nil {
		t.Errorf("projectWeights(nil map) = %v, want nil", got)
	}

	// A sub-floor mean is clamped up so a padded pill stays visible.
	if got := projectWeights([]string{"Lint", "New"}, map[string]float64{"Lint": 0.5}); !reflect.DeepEqual(got, []float64{0.5, stepWeightFloor}) {
		t.Errorf("clamp = %v, want [0.5 %v]", got, stepWeightFloor)
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
		AccentColor:  pushward.ColorGreen,
		CurrentStep:  pushward.IntPtr(2),
		TotalSteps:   pushward.IntPtr(2),
		URL:          "https://github.com/owner/repo/actions/runs/100",
		SecondaryURL: "https://github.com/owner/repo",
	}

	p.scheduleEnd(context.Background(), "owner/repo", content)

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
	if got[0].Path != "/activities/gh-repo" {
		t.Errorf("phase 1: expected /activities/gh-repo, got %s", got[0].Path)
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
		AccentColor:  pushward.ColorRed,
		CurrentStep:  pushward.IntPtr(1),
		TotalSteps:   pushward.IntPtr(3),
		URL:          "https://github.com/owner/repo/actions/runs/200",
		SecondaryURL: "https://github.com/owner/repo",
	}

	p.scheduleEnd(context.Background(), "owner/repo", content)
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
	if req1.Content.AccentColor != pushward.ColorRed {
		t.Errorf("phase 1: expected accent_color %q, got %s", pushward.ColorRed, req1.Content.AccentColor)
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

	p.scheduleEnd(context.Background(), "owner/repo", content)

	// Simulate new run taking over: cancel the timer and replace the entry
	time.Sleep(10 * time.Millisecond)
	p.mu.Lock()
	if t2, ok := p.tracked["owner/repo"]; ok && t2.endTimers != nil {
		t2.endTimers.Stop()
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
		AccentColor:  pushward.ColorGreen,
		CurrentStep:  pushward.IntPtr(4),
		TotalSteps:   pushward.IntPtr(4),
		URL:          "https://github.com/owner/repo/actions/runs/400",
		SecondaryURL: "https://github.com/owner/repo",
	}

	p.scheduleEnd(context.Background(), "owner/repo", content)
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
	if got[1].Path != "/activities/gh-65e817ee" {
		t.Errorf("expected /activities/gh-65e817ee, got %s", got[1].Path)
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
	if tracked.Slug != "gh-65e817ee" {
		t.Errorf("expected slug gh-65e817ee, got %s", tracked.Slug)
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

func TestPollIdle_SeedsStepsFromPreviousRun(t *testing.T) {
	ghMux := http.NewServeMux()
	// In-progress run carries a workflow_id so the prior-run lookup can target it.
	ghMux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{
			TotalCount: 1,
			WorkflowRuns: []ghclient.WorkflowRun{
				{
					ID: 42, Name: "CI", Status: "in_progress", HeadBranch: "main", WorkflowID: 99,
					HTMLURL: "https://github.com/owner/repo/actions/runs/42",
				},
			},
		})
	})
	// Current run has only revealed its first wave (1 job) — GitHub creates jobs lazily.
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 1,
			Jobs:       []ghclient.Job{{ID: 1, Name: "Lint", Status: "in_progress"}},
		})
	})
	// Last successful run of the same workflow+branch revealed its full 6-step DAG.
	ghMux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "success" {
			t.Errorf("expected status=success on first lookup, got %q", r.URL.Query().Get("status"))
		}
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{
			TotalCount:   1,
			WorkflowRuns: []ghclient.WorkflowRun{{ID: 41, WorkflowID: 99, HeadBranch: "main"}},
		})
	})
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/41/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 6,
			Jobs: []ghclient.Job{
				{ID: 1, Name: "Lint", Status: "completed", Conclusion: "success"},
				{ID: 2, Name: "Build", Status: "completed", Conclusion: "success"},
				{ID: 3, Name: "Test", Status: "completed", Conclusion: "success"},
				{ID: 4, Name: "Scan", Status: "completed", Conclusion: "success"},
				{ID: 5, Name: "Publish", Status: "completed", Conclusion: "success"},
				{ID: 6, Name: "Notify", Status: "completed", Conclusion: "success"},
			},
		})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	p := &Poller{
		cfg:     testConfig(),
		gh:      gh,
		pw:      pw,
		tracked: make(map[string]*trackedRun),
		repos:   []string{"owner/repo"},
	}

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The seed update (PATCH) must carry the prior run's stable 6-step shape, not
	// the current scan's single revealed job.
	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 PushWard calls (create + update), got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req)
	if req.Content.TotalSteps == nil || *req.Content.TotalSteps != 6 {
		t.Errorf("expected seeded TotalSteps=6, got %v", req.Content.TotalSteps)
	}
	wantLabels := []string{"Lint", "Build", "Test", "Scan", "Publish", "Notify"}
	if !reflect.DeepEqual(req.Content.StepLabels, wantLabels) {
		t.Errorf("expected labels adopted wholesale from prior run %v, got %v", wantLabels, req.Content.StepLabels)
	}
	if len(req.Content.StepRows) != 6 {
		t.Errorf("expected 6 seeded StepRows, got %v", req.Content.StepRows)
	}

	p.mu.Lock()
	tracked := p.tracked["owner/repo"]
	p.mu.Unlock()
	if tracked == nil || tracked.maxTotalSteps != 6 {
		t.Errorf("expected tracked.maxTotalSteps=6, got %v", tracked)
	}
}

// priorRunMux serves a repo whose in-progress run 42 has revealed only its first
// wave of jobs, so the pill shape (and weights) must come from the prior finished
// run 41: Lint 5s, a parallel Build matrix (300s / 120s -> the group weighs the
// 300s longest), Test 40s.
func priorRunMux() *http.ServeMux {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{
			TotalCount: 1,
			WorkflowRuns: []ghclient.WorkflowRun{
				{ID: 42, Name: "CI", Status: "in_progress", HeadBranch: "main", WorkflowID: 99},
			},
		})
	})
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 1,
			Jobs:       []ghclient.Job{{ID: 1, Name: "Lint", Status: "in_progress"}},
		})
	})
	ghMux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{
			TotalCount:   1,
			WorkflowRuns: []ghclient.WorkflowRun{{ID: 41, WorkflowID: 99, HeadBranch: "main"}},
		})
	})
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/41/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 4,
			Jobs: []ghclient.Job{
				{
					ID: 1, Name: "Lint", Status: "completed", Conclusion: "success",
					StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:00:05Z",
				},
				{
					ID: 2, Name: "Build (ubuntu)", Status: "completed", Conclusion: "success",
					StartedAt: "2026-01-01T00:00:05Z", CompletedAt: "2026-01-01T00:05:05Z",
				},
				{
					ID: 3, Name: "Build (macos)", Status: "completed", Conclusion: "success",
					StartedAt: "2026-01-01T00:00:05Z", CompletedAt: "2026-01-01T00:02:05Z",
				},
				{
					ID: 4, Name: "Test", Status: "completed", Conclusion: "success",
					StartedAt: "2026-01-01T00:05:05Z", CompletedAt: "2026-01-01T00:05:45Z",
				},
			},
		})
	})
	return ghMux
}

// seedContent runs one pollIdle against priorRunMux with cfg and returns the
// seed frame (the second PushWard call: create, then the seeding update),
// alongside the raw request body so callers can assert on the JSON keys that
// actually went over the wire.
func seedContent(t *testing.T, cfg *config.Config) (pushward.Content, json.RawMessage) {
	t.Helper()
	gh := mockGitHubClient(t, priorRunMux())
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

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
		t.Fatalf("expected 2 PushWard calls (create + update), got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req)
	return req.Content, got[1].Body
}

// TestPollIdle_RenderFlags pins the opt-in contract: step_colors and step_weights
// are sent only when their flag is on, and each toggles independently. step_rows
// and step_labels are not gated and must survive every combination.
func TestPollIdle_RenderFlags(t *testing.T) {
	tests := []struct {
		name        string
		colors      bool
		weights     bool
		wantColors  []string
		wantWeights []float64
	}{
		{name: "both off"},
		{name: "colors only", colors: true, wantColors: []string{"purple", "blue", "yellow"}},
		{name: "weights only", weights: true, wantWeights: []float64{5, 300, 40}},
		{
			name: "both on", colors: true, weights: true,
			wantColors:  []string{"purple", "blue", "yellow"},
			wantWeights: []float64{5, 300, 40},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content, body := seedContent(t, testConfigRender(tc.colors, tc.weights))

			if !reflect.DeepEqual(content.StepColors, tc.wantColors) {
				t.Errorf("step_colors = %v, want %v", content.StepColors, tc.wantColors)
			}
			if !reflect.DeepEqual(content.StepWeights, tc.wantWeights) {
				t.Errorf("step_weights = %v, want %v", content.StepWeights, tc.wantWeights)
			}
			// A disabled field must be absent from the JSON, not present as null:
			// that is what makes the opt-out byte-identical to the payload the
			// bridge sent before the field existed.
			if got := bytes.Contains(body, []byte(`"step_colors"`)); got != tc.colors {
				t.Errorf("step_colors key present = %v, want %v; body: %s", got, tc.colors, body)
			}
			if got := bytes.Contains(body, []byte(`"step_weights"`)); got != tc.weights {
				t.Errorf("step_weights key present = %v, want %v; body: %s", got, tc.weights, body)
			}
			// Never gated: the fan-out layout and labels ship in every combination.
			if wantRows := []int{1, 2, 1}; !reflect.DeepEqual(content.StepRows, wantRows) {
				t.Errorf("step_rows = %v, want %v", content.StepRows, wantRows)
			}
			if content.TotalSteps == nil {
				t.Fatal("seed must set total_steps")
			}
			if len(content.StepLabels) != *content.TotalSteps {
				t.Errorf("step_labels length %d must equal total_steps %d",
					len(content.StepLabels), *content.TotalSteps)
			}
		})
	}
}

func TestShape_StepColorsDisabled(t *testing.T) {
	jobs := []ghclient.Job{
		{Name: "Lint", Status: "completed", Conclusion: "success"},
		{Name: "Build", Status: "in_progress"},
	}

	off := (&Poller{cfg: testConfigRender(false, false)}).shape(jobs)
	if off.StepColors != nil {
		t.Errorf("step_colors must be nil when disabled, got %v", off.StepColors)
	}
	if len(off.StepRows) != off.TotalSteps || len(off.StepLabels) != off.TotalSteps {
		t.Errorf("rows/labels must survive the flag: rows=%v labels=%v", off.StepRows, off.StepLabels)
	}

	on := (&Poller{cfg: testConfigRender(true, false)}).shape(jobs)
	if want := []string{"purple", "blue"}; !reflect.DeepEqual(on.StepColors, want) {
		t.Errorf("step_colors = %v, want %v", on.StepColors, want)
	}
}

func TestPollIdle_SeedsWeightsFromPriorRun(t *testing.T) {
	content, _ := seedContent(t, testConfigRender(true, true))
	req := pushward.UpdateRequest{Content: content}
	// The seed frame must carry pill weights sized by the prior run's durations,
	// one per step, matching total_steps so the server's length check passes.
	want := []float64{5, 300, 40}
	if !reflect.DeepEqual(req.Content.StepWeights, want) {
		t.Errorf("seed step_weights = %v, want %v", req.Content.StepWeights, want)
	}
	if req.Content.TotalSteps == nil {
		t.Fatal("seed must set total_steps")
	}
	total := *req.Content.TotalSteps
	if len(req.Content.StepWeights) != total {
		t.Errorf("step_weights length %d must equal total_steps %d", len(req.Content.StepWeights), total)
	}
	// Regression guard: the seed must carry step_rows (fan-out) ALONGSIDE
	// step_weights (widths), not one instead of the other. The server accepts
	// both together (weighted-matrix layout); older clients ignore weights and
	// render the fan-out from step_rows. Re-splitting them here would silently
	// drop the fan-out. Every per-step slice must match total_steps.
	if wantRows := []int{1, 2, 1}; !reflect.DeepEqual(req.Content.StepRows, wantRows) {
		t.Errorf("seed step_rows = %v, want %v (Build matrix fans out to 2)", req.Content.StepRows, wantRows)
	}
	if len(req.Content.StepRows) != total || len(req.Content.StepLabels) != total || len(req.Content.StepColors) != total {
		t.Errorf("per-step slice lengths must all equal total_steps (%d): rows=%d labels=%d colors=%d",
			total, len(req.Content.StepRows), len(req.Content.StepLabels), len(req.Content.StepColors))
	}
}

func TestPollIdle_FallsBackWhenNoPriorRun(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{
			TotalCount: 1,
			WorkflowRuns: []ghclient.WorkflowRun{
				{ID: 42, Name: "CI", Status: "in_progress", HeadBranch: "feature", WorkflowID: 99},
			},
		})
	})
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 2,
			Jobs: []ghclient.Job{
				{ID: 1, Name: "Build", Status: "in_progress"},
				{ID: 2, Name: "Test", Status: "queued"},
			},
		})
	})
	// No prior success or completed run exists for this workflow+branch.
	ghMux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{TotalCount: 0})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	p := &Poller{
		cfg:     testConfig(),
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
		t.Fatalf("expected 2 PushWard calls (create + update), got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req)
	if req.Content.TotalSteps == nil || *req.Content.TotalSteps != 2 {
		t.Errorf("expected fallback TotalSteps=2 from current scan, got %v", req.Content.TotalSteps)
	}

	p.mu.Lock()
	tracked := p.tracked["owner/repo"]
	p.mu.Unlock()
	if tracked == nil || tracked.maxTotalSteps != 2 {
		t.Errorf("expected tracked.maxTotalSteps=2, got %v", tracked)
	}
}

func TestPollIdle_SeedsFromCompletedWhenNoSuccess(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{
			TotalCount: 1,
			WorkflowRuns: []ghclient.WorkflowRun{
				{ID: 42, Name: "CI", Status: "in_progress", HeadBranch: "feature", WorkflowID: 99},
			},
		})
	})
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 1,
			Jobs:       []ghclient.Job{{ID: 1, Name: "Build", Status: "in_progress"}},
		})
	})
	// No successful run on this branch, but the last *completed* run (a failure
	// that still ran the full DAG) seeds the shape — the success→completed fallback.
	ghMux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("status") {
		case "success":
			_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{TotalCount: 0})
		case "completed":
			_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{
				TotalCount:   1,
				WorkflowRuns: []ghclient.WorkflowRun{{ID: 40, WorkflowID: 99, HeadBranch: "feature"}},
			})
		default:
			t.Errorf("unexpected status filter %q", r.URL.Query().Get("status"))
		}
	})
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/40/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 4,
			Jobs: []ghclient.Job{
				{ID: 1, Name: "Build", Status: "completed", Conclusion: "success"},
				{ID: 2, Name: "Test", Status: "completed", Conclusion: "success"},
				{ID: 3, Name: "Scan", Status: "completed", Conclusion: "failure"},
				{ID: 4, Name: "Publish", Status: "completed", Conclusion: "skipped"},
			},
		})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	p := &Poller{
		cfg:     testConfig(),
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
		t.Fatalf("expected 2 PushWard calls (create + update), got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req)
	if req.Content.TotalSteps == nil || *req.Content.TotalSteps != 4 {
		t.Errorf("expected TotalSteps=4 seeded from the completed run, got %v", req.Content.TotalSteps)
	}
}

func TestPollIdle_KeepsCurrentScanWhenPriorRunSmaller(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{
			TotalCount: 1,
			WorkflowRuns: []ghclient.WorkflowRun{
				{ID: 42, Name: "CI", Status: "in_progress", HeadBranch: "main", WorkflowID: 99},
			},
		})
	})
	// Current run already reveals 3 groups...
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 3,
			Jobs: []ghclient.Job{
				{ID: 1, Name: "Lint", Status: "in_progress"},
				{ID: 2, Name: "Build", Status: "queued"},
				{ID: 3, Name: "Test", Status: "queued"},
			},
		})
	})
	// ...while the prior successful run only had 2 — the seed must NOT shrink the total.
	ghMux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{
			TotalCount:   1,
			WorkflowRuns: []ghclient.WorkflowRun{{ID: 41, WorkflowID: 99, HeadBranch: "main"}},
		})
	})
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/41/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 2,
			Jobs: []ghclient.Job{
				{ID: 1, Name: "Lint", Status: "completed", Conclusion: "success"},
				{ID: 2, Name: "Build", Status: "completed", Conclusion: "success"},
			},
		})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	p := &Poller{
		cfg:     testConfig(),
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
		t.Fatalf("expected 2 PushWard calls (create + update), got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req)
	if req.Content.TotalSteps == nil || *req.Content.TotalSteps != 3 {
		t.Errorf("expected current scan's TotalSteps=3 to be kept over smaller prior shape, got %v", req.Content.TotalSteps)
	}
	wantLabels := []string{"Lint", "Build", "Test"}
	if !reflect.DeepEqual(req.Content.StepLabels, wantLabels) {
		t.Errorf("expected current scan labels %v, got %v", wantLabels, req.Content.StepLabels)
	}
}

func TestPollIdle_SeedAbortsOnSuccessLookupError(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRunsResponse{
			TotalCount: 1,
			WorkflowRuns: []ghclient.WorkflowRun{
				{ID: 42, Name: "CI", Status: "in_progress", HeadBranch: "main", WorkflowID: 99},
			},
		})
	})
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 2,
			Jobs: []ghclient.Job{
				{ID: 1, Name: "Build", Status: "in_progress"},
				{ID: 2, Name: "Test", Status: "queued"},
			},
		})
	})
	// The success lookup fails (4xx, non-retryable). The seed must abort outright —
	// NOT silently fall through to a completed lookup — so the live scan is kept.
	ghMux.HandleFunc("/repos/owner/repo/actions/workflows/99/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") == "completed" {
			t.Error("completed lookup must not run after the success lookup errored")
		}
		w.WriteHeader(http.StatusNotFound)
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	p := &Poller{
		cfg:     testConfig(),
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
		t.Fatalf("expected 2 PushWard calls (create + update), got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req)
	if req.Content.TotalSteps == nil || *req.Content.TotalSteps != 2 {
		t.Errorf("expected live-scan TotalSteps=2 after seed aborted, got %v", req.Content.TotalSteps)
	}
}

func TestBaselineShape_ShortCircuits(t *testing.T) {
	// Both guards (workflowID==0 and blank branch) must skip the lookup entirely —
	// the catch-all handler fails the test if any GitHub call is made.
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/", func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub call: %s", r.URL.Path)
	})
	gh := mockGitHubClient(t, ghMux)
	p := &Poller{cfg: testConfig(), gh: gh, tracked: make(map[string]*trackedRun)}

	if _, ok := p.baselineShape(context.Background(), "owner/repo", 0, "main"); ok {
		t.Error("expected ok=false when workflowID==0")
	}
	if _, ok := p.baselineShape(context.Background(), "owner/repo", 99, ""); ok {
		t.Error("expected ok=false when branch is blank")
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
	// Tick PATCH must omit seed-only fields so they're preserved server-side.
	if req.Content.AccentColor != "" {
		t.Errorf("expected tick to omit accent_color, got %q", req.Content.AccentColor)
	}
	if req.Content.Icon != "" {
		t.Errorf("expected tick to omit icon, got %q", req.Content.Icon)
	}
	if req.Content.Template != "" {
		t.Errorf("expected tick to omit template, got %q", req.Content.Template)
	}
	if req.Content.Subtitle != "" {
		t.Errorf("expected tick to omit subtitle, got %q", req.Content.Subtitle)
	}
	if req.Content.URL != "" {
		t.Errorf("expected tick to omit url, got %q", req.Content.URL)
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
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRun{ID: 42, Status: "completed", Conclusion: "success"})
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
	if req1.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("phase 1: expected %q, got %s", pushward.ColorGreen, req1.Content.AccentColor)
	}

	var req2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req2)
	if req2.State != pushward.StateEnded {
		t.Errorf("phase 2: expected ENDED, got %s", req2.State)
	}
}

// All visible jobs are complete, but the run itself is still in_progress
// (GitHub creating the next lazy job wave). pollActive must NOT end the
// activity prematurely — regression for github-premature-end-jobs-only.
func TestPollActive_DefersEndWhenRunNotCompleted(t *testing.T) {
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.JobsResponse{
			TotalCount: 1,
			Jobs:       []ghclient.Job{{ID: 1, Name: "Build", Status: "completed", Conclusion: "success"}},
		})
	})
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRun{ID: 42, Status: "in_progress"})
	})
	gh := mockGitHubClient(t, ghMux)

	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	cfg := testConfig()
	p := &Poller{cfg: cfg, gh: gh, pw: pw, tracked: make(map[string]*trackedRun)}
	p.tracked["owner/repo"] = &trackedRun{
		Repo: "owner/repo", RunID: 42, Slug: "gh-repo", Name: "CI",
		maxTotalSteps: 1, maxStepRows: []int{1},
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// No two-phase end should have been scheduled; the entry stays active.
	p.mu.Lock()
	tr, ok := p.tracked["owner/repo"]
	hasPendingEnd := ok && tr.endTimers != nil
	p.mu.Unlock()
	if hasPendingEnd {
		t.Error("scheduled an end while the run was still in_progress")
	}
	// The only call should be the ongoing tick, never an ENDED frame.
	for _, c := range testutil.GetCalls(calls, mu) {
		var req pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &req)
		if req.State == pushward.StateEnded {
			t.Error("sent an ENDED frame while the run was still in_progress")
		}
	}
}

// A run wedged in_progress past maxRunLifetime (12h) must be reclaimed by
// eviction guard 2 (absolute age), even though guard 1 (stale LastUpdate) never
// fires because the jobs endpoint still returns data. Eviction happens before
// any job fetch, so no end is scheduled and no PushWard call is made.
func TestPollActive_EvictsRunExceedingMaxLifetime(t *testing.T) {
	ghMux := http.NewServeMux()
	// If guard 2 were removed, the recent LastUpdate keeps guard 1 from firing,
	// so the poll would proceed to fetch these in_progress jobs and PATCH a tick.
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42/jobs", func(w http.ResponseWriter, _ *http.Request) {
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
		LastUpdate:    time.Now(),                      // recent: guard 1 (stale TTL) must NOT fire
		trackedAt:     time.Now().Add(-13 * time.Hour), // > maxRunLifetime (12h): guard 2 fires
		maxTotalSteps: 2,
		maxStepRows:   []int{1, 1},
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Give any (erroneously) scheduled end a chance to fire before asserting.
	time.Sleep(50 * time.Millisecond)

	p.mu.Lock()
	_, stillTracked := p.tracked["owner/repo"]
	p.mu.Unlock()
	if stillTracked {
		t.Error("expected run exceeding max lifetime to be evicted from tracked")
	}

	// Eviction is silent: no ongoing tick, no two-phase end.
	if got := testutil.GetCalls(calls, mu); len(got) != 0 {
		t.Errorf("expected 0 PushWard calls on lifetime eviction, got %d", len(got))
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
	ghMux.HandleFunc("/repos/owner/repo/actions/runs/42", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.WorkflowRun{ID: 42, Status: "completed", Conclusion: "failure"})
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
	if req1.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected %q, got %s", pushward.ColorRed, req1.Content.AccentColor)
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
	// A non-nil endTimers marks a pending end; pollActive must skip the repo.
	tg := &syncx.TimerGroup{}
	tg.Reset(time.Hour, func() {}) // won't fire
	defer tg.Close()
	p.tracked["owner/repo"] = &trackedRun{
		Repo:      "owner/repo",
		RunID:     42,
		Slug:      "gh-repo",
		endTimers: tg,
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
	ghMux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.User{Login: "testowner"})
	})
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
	ghMux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.User{Login: "owner"})
	})
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
	ghMux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghclient.User{Login: "owner"})
	})
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
	p.scheduleEnd(context.Background(), "nonexistent", pushward.Content{})
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
	tg := &syncx.TimerGroup{}
	tg.Reset(time.Hour, func() {})
	p.tracked["owner/repo"] = &trackedRun{
		Repo:      "owner/repo",
		RunID:     42,
		Slug:      "gh-repo",
		endTimers: tg,
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
