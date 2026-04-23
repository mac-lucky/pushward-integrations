package tracker

import (
	"context"
	"sync"
	"testing"
	"time"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
	"github.com/mac-lucky/pushward-integrations/unraid/internal/config"
	"github.com/mac-lucky/pushward-integrations/unraid/internal/graphql"
)

func testConfig() *config.Config {
	return &config.Config{
		Unraid: config.UnraidConfig{
			Host:       "tower.local",
			Port:       80,
			APIKey:     "test-key",
			ServerName: "Tower",
		},
		PushWard: sharedconfig.PushWardConfig{
			URL:            "http://localhost",
			APIKey:         "hlk_test",
			Priority:       2,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   24 * time.Hour,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
	}
}

// --- Parity check lifecycle tests ---

func TestParityCheck_StartProgressComplete(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.URL = srv.URL
	gql := graphql.NewClient("tower.local", 80, "test-key", false)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	tr := New(cfg, gql, pw)

	ctx := context.Background()

	// Step 1: Parity check starts
	tr.handleArrayStatus(ctx, graphql.ArrayStatus{
		State:       "STARTED",
		ParityCheck: graphql.ParityCheck{Status: graphql.ParityStatusRunning, Progress: 5.0},
	})

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("parity start: expected >= 2 calls (create + update), got %d", len(recorded))
	}

	// Verify create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("call 0: expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Slug != "unraid-parity" {
		t.Errorf("slug = %q, want unraid-parity", createReq.Slug)
	}
	if createReq.Priority != 2 {
		t.Errorf("priority = %d, want 2", createReq.Priority)
	}

	// Verify first update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("state = %q, want ONGOING", update.State)
	}
	if update.Content.State != "Checking · 5%" {
		t.Errorf("content state = %q, want Checking · 5%%", update.Content.State)
	}
	if update.Content.Progress != 0.05 {
		t.Errorf("progress = %f, want 0.05", update.Content.Progress)
	}
	if update.Content.Subtitle != "Unraid · Tower" {
		t.Errorf("subtitle = %q, want Unraid · Tower", update.Content.Subtitle)
	}

	// Step 2: Parity check completes (status flips to a non-running value)
	tr.handleArrayStatus(ctx, graphql.ArrayStatus{
		State:       "STARTED",
		ParityCheck: graphql.ParityCheck{Status: graphql.ParityStatusCompleted},
	})

	// Wait for two-phase end
	time.Sleep(80 * time.Millisecond)

	recorded = testutil.GetCalls(calls, mu)
	var foundOngoing, foundEnded bool
	for _, c := range recorded {
		if c.Method != "PATCH" {
			continue
		}
		var req pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &req)
		if req.Content.State == "Parity Valid" && req.Content.Icon == "checkmark.circle.fill" {
			if req.State == pushward.StateOngoing {
				foundOngoing = true
			}
			if req.State == pushward.StateEnded {
				foundEnded = true
			}
		}
	}
	if !foundOngoing {
		t.Error("two-phase end: missing ONGOING with Parity Valid content")
	}
	if !foundEnded {
		t.Error("two-phase end: missing ENDED")
	}
}

func TestParityCheck_Debounce(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.URL = srv.URL
	gql := graphql.NewClient("tower.local", 80, "test-key", false)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	tr := New(cfg, gql, pw)

	ctx := context.Background()

	// Start parity
	tr.handleArrayStatus(ctx, graphql.ArrayStatus{
		State:       "STARTED",
		ParityCheck: graphql.ParityCheck{Status: graphql.ParityStatusRunning, Progress: 5.0},
	})

	callsAfterStart := len(testutil.GetCalls(calls, mu))

	// Immediate update should be debounced (< 30s)
	tr.handleArrayStatus(ctx, graphql.ArrayStatus{
		State:       "STARTED",
		ParityCheck: graphql.ParityCheck{Status: graphql.ParityStatusRunning, Progress: 6.0},
	})

	callsAfterDebounce := len(testutil.GetCalls(calls, mu))
	if callsAfterDebounce != callsAfterStart {
		t.Errorf("debounced update generated %d extra calls, want 0", callsAfterDebounce-callsAfterStart)
	}
}

// --- Array state transition tests ---

func TestArrayState_StartingToStarted(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.URL = srv.URL
	gql := graphql.NewClient("tower.local", 80, "test-key", false)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	tr := New(cfg, gql, pw)

	ctx := context.Background()

	// First update sets initial state (no transition)
	tr.handleArrayStatus(ctx, graphql.ArrayStatus{State: "STOPPED"})
	initialCalls := len(testutil.GetCalls(calls, mu))
	if initialCalls != 0 {
		t.Errorf("first update should not generate calls, got %d", initialCalls)
	}

	// STOPPED -> STARTING
	tr.handleArrayStatus(ctx, graphql.ArrayStatus{State: "STARTING"})

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("STARTING: expected >= 2 calls, got %d", len(recorded))
	}

	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Slug != "unraid-array" {
		t.Errorf("slug = %q, want unraid-array", createReq.Slug)
	}

	var startingUpdate pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &startingUpdate)
	if startingUpdate.Content.State != "Starting..." {
		t.Errorf("state = %q, want Starting...", startingUpdate.Content.State)
	}
	if startingUpdate.Content.AccentColor != pushward.ColorBlue {
		t.Errorf("accent = %q, want #007AFF", startingUpdate.Content.AccentColor)
	}

	// STARTING -> STARTED
	tr.handleArrayStatus(ctx, graphql.ArrayStatus{State: "STARTED"})

	// Wait for two-phase end
	time.Sleep(80 * time.Millisecond)

	recorded = testutil.GetCalls(calls, mu)
	var foundOngoing, foundEnded bool
	for _, c := range recorded {
		if c.Method != "PATCH" {
			continue
		}
		var req pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &req)
		if req.Content.State == "Array Started" {
			if req.State == pushward.StateOngoing {
				foundOngoing = true
				if req.Content.AccentColor != pushward.ColorGreen {
					t.Errorf("started accent = %q, want #34C759", req.Content.AccentColor)
				}
			}
			if req.State == pushward.StateEnded {
				foundEnded = true
			}
		}
	}
	if !foundOngoing {
		t.Error("Array Started: missing ONGOING phase")
	}
	if !foundEnded {
		t.Error("Array Started: missing ENDED phase")
	}
}

func TestArrayState_StoppingToStopped(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.URL = srv.URL
	gql := graphql.NewClient("tower.local", 80, "test-key", false)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	tr := New(cfg, gql, pw)

	ctx := context.Background()

	// Set initial state
	tr.handleArrayStatus(ctx, graphql.ArrayStatus{State: "STARTED"})

	// STARTED -> STOPPING
	tr.handleArrayStatus(ctx, graphql.ArrayStatus{State: "STOPPING"})

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("STOPPING: expected >= 2 calls, got %d", len(recorded))
	}

	var stoppingUpdate pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[len(recorded)-1].Body, &stoppingUpdate)
	if stoppingUpdate.Content.State != "Stopping..." {
		t.Errorf("state = %q, want Stopping...", stoppingUpdate.Content.State)
	}
	if stoppingUpdate.Content.AccentColor != pushward.ColorOrange {
		t.Errorf("accent = %q, want #FF9500", stoppingUpdate.Content.AccentColor)
	}

	// STOPPING -> STOPPED
	tr.handleArrayStatus(ctx, graphql.ArrayStatus{State: "STOPPED"})

	// Wait for two-phase end
	time.Sleep(80 * time.Millisecond)

	recorded = testutil.GetCalls(calls, mu)
	var foundEnded bool
	for _, c := range recorded {
		if c.Method != "PATCH" {
			continue
		}
		var req pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &req)
		if req.Content.State == "Array Stopped" && req.State == pushward.StateEnded {
			foundEnded = true
		}
	}
	if !foundEnded {
		t.Error("Array Stopped: missing ENDED phase")
	}
}

func TestArrayState_NoTransitionIgnored(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.URL = srv.URL
	gql := graphql.NewClient("tower.local", 80, "test-key", false)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	tr := New(cfg, gql, pw)

	// Set initial state
	tr.handleArrayStatus(context.Background(), graphql.ArrayStatus{State: "STARTED"})

	// Same state again — should not generate calls
	tr.handleArrayStatus(context.Background(), graphql.ArrayStatus{State: "STARTED"})

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Errorf("same state should generate 0 calls, got %d", len(recorded))
	}
}

// --- Notification tests ---

// newTrackerForNotif is a convenience wrapper to reduce setup noise in the
// notification tests. Returns the tracker and handles to inspect recorded calls.
func newTrackerForNotif(t *testing.T) (*Tracker, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.URL = srv.URL
	gql := graphql.NewClient("tower.local", 80, "test-key", false)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	return New(cfg, gql, pw), calls, mu
}

func requireSingleNotification(t *testing.T, calls *[]testutil.APICall, mu *sync.Mutex) pushward.SendNotificationRequest {
	t.Helper()
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 API call, got %d", len(recorded))
	}
	if recorded[0].Method != "POST" || recorded[0].Path != "/notifications" {
		t.Fatalf("expected POST /notifications, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	return req
}

func TestNotification_DiskAlert(t *testing.T) {
	tr, calls, mu := newTrackerForNotif(t)

	tr.handleNotification(context.Background(), graphql.Notification{
		ID:          "1",
		Subject:     "SMART error on disk1",
		Description: "Reallocated sector count exceeded threshold",
		Importance:  "alert",
	})

	req := requireSingleNotification(t, calls, mu)
	if req.Title != "SMART error on disk1" {
		t.Errorf("title = %q, want SMART error on disk1", req.Title)
	}
	if req.Body != "Reallocated sector count exceeded threshold" {
		t.Errorf("body = %q, want Reallocated sector count exceeded threshold", req.Body)
	}
	if req.Level != pushward.LevelActive {
		t.Errorf("level = %q, want active", req.Level)
	}
	if req.Category != pushward.SeverityCritical {
		t.Errorf("category = %q, want critical", req.Category)
	}
	if !req.Push {
		t.Errorf("push = false, want true")
	}
	if req.ThreadID != "unraid" {
		t.Errorf("thread_id = %q, want unraid", req.ThreadID)
	}
	if req.Source != "unraid" {
		t.Errorf("source = %q, want unraid", req.Source)
	}
	if req.CollapseID == "" {
		t.Errorf("collapse_id is empty, want a stable hash")
	}
	if req.Subtitle != "Unraid · Tower" {
		t.Errorf("subtitle = %q, want Unraid · Tower", req.Subtitle)
	}
	if req.Metadata["importance"] != "alert" {
		t.Errorf("metadata[importance] = %q, want alert", req.Metadata["importance"])
	}
	if req.Metadata["unraid_id"] != "1" {
		t.Errorf("metadata[unraid_id] = %q, want 1", req.Metadata["unraid_id"])
	}
}

func TestNotification_UPSWarning(t *testing.T) {
	tr, calls, mu := newTrackerForNotif(t)

	tr.handleNotification(context.Background(), graphql.Notification{
		ID:          "2",
		Subject:     "UPS on battery power",
		Description: "Running on battery — shutdown imminent",
		Importance:  "warning",
	})

	req := requireSingleNotification(t, calls, mu)
	if req.Level != pushward.LevelActive {
		t.Errorf("level = %q, want active", req.Level)
	}
	if req.Category != pushward.SeverityWarning {
		t.Errorf("category = %q, want warning", req.Category)
	}
	if !req.Push {
		t.Errorf("push = false, want true")
	}
}

func TestNotification_UPSAlert(t *testing.T) {
	tr, calls, mu := newTrackerForNotif(t)

	tr.handleNotification(context.Background(), graphql.Notification{
		ID:          "3",
		Subject:     "UPS battery critically low",
		Description: "Battery at 5% — shutting down",
		Importance:  "alert",
	})

	req := requireSingleNotification(t, calls, mu)
	if req.Level != pushward.LevelActive {
		t.Errorf("level = %q, want active", req.Level)
	}
	if req.Category != pushward.SeverityCritical {
		t.Errorf("category = %q, want critical", req.Category)
	}
}

func TestNotification_GenericForwarded(t *testing.T) {
	tr, calls, mu := newTrackerForNotif(t)

	tr.handleNotification(context.Background(), graphql.Notification{
		ID:          "4",
		Subject:     "Docker container started",
		Description: "nginx started successfully",
		Importance:  "info",
	})

	req := requireSingleNotification(t, calls, mu)
	if req.Level != pushward.LevelPassive {
		t.Errorf("level = %q, want passive", req.Level)
	}
	if req.Category != pushward.SeverityInfo {
		t.Errorf("category = %q, want info", req.Category)
	}
	if !req.Push {
		t.Errorf("push = false, want true (passive still pushes, iOS handles quiet delivery)")
	}
}

func TestNotification_UnknownImportanceDefaultsToPassive(t *testing.T) {
	tr, calls, mu := newTrackerForNotif(t)

	tr.handleNotification(context.Background(), graphql.Notification{
		ID:          "5",
		Subject:     "Custom user script completed",
		Description: "ran successfully",
		Importance:  "",
	})

	req := requireSingleNotification(t, calls, mu)
	if req.Level != pushward.LevelPassive {
		t.Errorf("level = %q, want passive", req.Level)
	}
	if req.Category != pushward.SeverityInfo {
		t.Errorf("category = %q, want info", req.Category)
	}
	if _, ok := req.Metadata["importance"]; ok {
		t.Errorf("metadata[importance] should be absent when importance is empty, got %q", req.Metadata["importance"])
	}
}

func TestNotification_EmptySubjectSkipped(t *testing.T) {
	tr, calls, mu := newTrackerForNotif(t)

	tr.handleNotification(context.Background(), graphql.Notification{
		ID:          "6",
		Subject:     "",
		Title:       "",
		Description: "some description",
		Importance:  "info",
	})

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Errorf("empty subject+title: expected 0 calls, got %d", len(recorded))
	}
}

func TestNotification_CollapseIDStable(t *testing.T) {
	tr, calls, mu := newTrackerForNotif(t)

	tr.handleNotification(context.Background(), graphql.Notification{
		ID: "a", Subject: "SMART error on disk1", Title: "alert", Importance: "alert",
	})
	tr.handleNotification(context.Background(), graphql.Notification{
		ID: "b", Subject: "SMART error on disk1", Title: "alert", Importance: "alert",
	})
	tr.handleNotification(context.Background(), graphql.Notification{
		ID: "c", Subject: "SMART error on disk2", Title: "alert", Importance: "alert",
	})

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}
	var reqs [3]pushward.SendNotificationRequest
	for i, c := range recorded {
		testutil.UnmarshalBody(t, c.Body, &reqs[i])
	}
	if reqs[0].CollapseID != reqs[1].CollapseID {
		t.Errorf("same subject+title should yield same collapse_id, got %q and %q", reqs[0].CollapseID, reqs[1].CollapseID)
	}
	if reqs[0].CollapseID == reqs[2].CollapseID {
		t.Errorf("different subject should yield different collapse_id, got %q for both", reqs[0].CollapseID)
	}
}

func TestNotification_SubjectFallsBackToTitle(t *testing.T) {
	tr, calls, mu := newTrackerForNotif(t)

	tr.handleNotification(context.Background(), graphql.Notification{
		ID:          "7",
		Subject:     "",
		Title:       "Array started",
		Description: "array is started",
		Importance:  "info",
	})

	req := requireSingleNotification(t, calls, mu)
	if req.Title != "Array started" {
		t.Errorf("title = %q, want Array started (falling back to notif.Title)", req.Title)
	}
}
