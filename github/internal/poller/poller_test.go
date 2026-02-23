package poller

import (
	"testing"
	"time"

	"github.com/mac-lucky/pushward-docker/github/internal/config"
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
