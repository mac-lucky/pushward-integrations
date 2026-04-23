package tracker

import (
	"context"
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

// --- sanitize tests ---

func TestSanitize_Simple(t *testing.T) {
	got := sanitize("Disk Error")
	if got != "disk-error" {
		t.Errorf("sanitize(Disk Error) = %q, want disk-error", got)
	}
}

func TestSanitize_LongInput(t *testing.T) {
	got := sanitize("Very Long Notification Subject That Exceeds Twenty Characters")
	if len(got) > 20 {
		t.Errorf("sanitize produced %d chars, want <= 20", len(got))
	}
}

func TestSanitize_SpecialChars(t *testing.T) {
	got := sanitize("SMART error: disk1")
	if got != "smart-error-disk1" {
		t.Errorf("sanitize(SMART error: disk1) = %q, want smart-error-disk1", got)
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

func TestNotification_DiskAlert(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.URL = srv.URL
	gql := graphql.NewClient("tower.local", 80, "test-key", false)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	tr := New(cfg, gql, pw)

	ctx := context.Background()
	tr.handleNotification(ctx, graphql.Notification{
		ID:          "1",
		Subject:     "SMART error on disk1",
		Description: "Reallocated sector count exceeded threshold",
		Importance:  "alert",
	})

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("disk alert: expected >= 2 calls, got %d", len(recorded))
	}

	// Verify create
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "SMART error on disk1" {
		t.Errorf("name = %q, want SMART error on disk1", createReq.Name)
	}

	// Verify update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.Severity != "error" {
		t.Errorf("severity = %q, want error", update.Content.Severity)
	}
	if update.Content.AccentColor != pushward.ColorRed {
		t.Errorf("accent = %q, want #FF3B30", update.Content.AccentColor)
	}
	if update.Content.Icon != "exclamationmark.octagon.fill" {
		t.Errorf("icon = %q, want exclamationmark.octagon.fill", update.Content.Icon)
	}
	if update.Content.State != "Reallocated sector count exceeded threshold" {
		t.Errorf("state = %q, want Reallocated sector count exceeded threshold", update.Content.State)
	}
}

func TestNotification_UPSWarning(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.URL = srv.URL
	gql := graphql.NewClient("tower.local", 80, "test-key", false)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	tr := New(cfg, gql, pw)

	ctx := context.Background()
	tr.handleNotification(ctx, graphql.Notification{
		ID:         "2",
		Subject:    "UPS on battery power",
		Importance: "warning",
	})

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("UPS warning: expected >= 2 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.AccentColor != pushward.ColorOrange {
		t.Errorf("accent = %q, want #FF9500 (warning)", update.Content.AccentColor)
	}
	if update.Content.Severity != "warning" {
		t.Errorf("severity = %q, want warning", update.Content.Severity)
	}
	if update.Content.Icon != "bolt.slash.fill" {
		t.Errorf("icon = %q, want bolt.slash.fill", update.Content.Icon)
	}
}

func TestNotification_UPSAlert(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.URL = srv.URL
	gql := graphql.NewClient("tower.local", 80, "test-key", false)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	tr := New(cfg, gql, pw)

	ctx := context.Background()
	tr.handleNotification(ctx, graphql.Notification{
		ID:         "3",
		Subject:    "UPS battery critically low",
		Importance: "alert",
	})

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("UPS alert: expected >= 2 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.AccentColor != pushward.ColorRed {
		t.Errorf("accent = %q, want #FF3B30 (alert)", update.Content.AccentColor)
	}
	if update.Content.Severity != "error" {
		t.Errorf("severity = %q, want error", update.Content.Severity)
	}
}

func TestNotification_IgnoredEvent(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.URL = srv.URL
	gql := graphql.NewClient("tower.local", 80, "test-key", false)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	tr := New(cfg, gql, pw)

	tr.handleNotification(context.Background(), graphql.Notification{
		ID:      "4",
		Subject: "Docker container started",
	})

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Errorf("ignored notification generated %d calls, want 0", len(recorded))
	}
}
