package overseerr

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func testConfig() *config.OverseerrConfig {
	return &config.OverseerrConfig{
		Enabled:        true,
		Priority:       1,
		CleanupDelay:   1 * time.Hour,
		StaleTimeout:   30 * time.Minute,
		EndDelay:       10 * time.Millisecond,
		EndDisplayTime: 10 * time.Millisecond,
	}
}

func newHandler(t *testing.T, cfg *config.OverseerrConfig) (*Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL)
	h := NewHandler(store, pool, cfg)
	return h, calls, mu
}

func send(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/overseerr", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	auth.Middleware(h).ServeHTTP(w, req)
	return w
}

func TestMediaPending(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"notification_type": "MEDIA_PENDING",
		"event": "media.pending",
		"subject": "Inception (2010)",
		"message": "A new request for Inception (2010) has been submitted.",
		"image": "https://image.tmdb.org/t/p/w600_and_h900_bestv2/inception.jpg",
		"media": {
			"media_type": "movie",
			"tmdbId": "27205",
			"tvdbId": "",
			"status": "PENDING",
			"status4k": "UNKNOWN"
		},
		"request": {
			"request_id": "1",
			"requestedBy_username": "admin"
		}
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// No scheduleEnd for non-terminal events, just wait a bit for async
	time.Sleep(50 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING = 2 (no two-phase end for non-terminal)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}

	// Verify create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Slug != "overseerr-movie-27205" {
		t.Errorf("expected slug overseerr-movie-27205, got %s", createReq.Slug)
	}
	if createReq.Name != "Inception (2010)" {
		t.Errorf("expected name 'Inception (2010)', got %s", createReq.Name)
	}
	if createReq.Priority != 1 {
		t.Errorf("expected priority 1, got %d", createReq.Priority)
	}

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.State != "Requested" {
		t.Errorf("expected state 'Requested', got %s", update.Content.State)
	}
	if update.Content.Icon != "hourglass" {
		t.Errorf("expected icon hourglass, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != "#FF9500" {
		t.Errorf("expected orange color, got %s", update.Content.AccentColor)
	}
	if update.Content.Template != "pipeline" {
		t.Errorf("expected template pipeline, got %s", update.Content.Template)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 1 {
		t.Errorf("expected current_step 1, got %v", update.Content.CurrentStep)
	}
	if update.Content.TotalSteps == nil || *update.Content.TotalSteps != 4 {
		t.Errorf("expected total_steps 4, got %v", update.Content.TotalSteps)
	}
	expectedProgress := 1.0 / 4.0
	if update.Content.Progress != expectedProgress {
		t.Errorf("expected progress %f, got %f", expectedProgress, update.Content.Progress)
	}
	if update.Content.Subtitle != "Overseerr · Inception (2010)" {
		t.Errorf("expected subtitle 'Overseerr · Inception (2010)', got %q", update.Content.Subtitle)
	}
}

func TestMediaApproved(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"notification_type": "MEDIA_APPROVED",
		"event": "media.approved",
		"subject": "Inception (2010)",
		"message": "Your request for Inception (2010) has been approved.",
		"image": "",
		"media": {
			"media_type": "movie",
			"tmdbId": "27205",
			"tvdbId": "",
			"status": "PROCESSING",
			"status4k": "UNKNOWN"
		},
		"request": {
			"request_id": "1",
			"requestedBy_username": "admin"
		}
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(50 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING = 2 (no two-phase end for non-terminal)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.State != "Approved" {
		t.Errorf("expected state 'Approved', got %s", update.Content.State)
	}
	if update.Content.Icon != "checkmark.circle" {
		t.Errorf("expected icon checkmark.circle, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != "#007AFF" {
		t.Errorf("expected blue color, got %s", update.Content.AccentColor)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", update.Content.CurrentStep)
	}
}

func TestMediaAvailable(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"notification_type": "MEDIA_AVAILABLE",
		"event": "media.available",
		"subject": "Inception (2010)",
		"message": "Inception (2010) is now available!",
		"image": "",
		"media": {
			"media_type": "movie",
			"tmdbId": "27205",
			"tvdbId": "",
			"status": "AVAILABLE",
			"status4k": "UNKNOWN"
		},
		"request": {
			"request_id": "1",
			"requestedBy_username": "admin"
		}
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Available" {
		t.Errorf("expected state 'Available', got %s", update.Content.State)
	}
	if update.Content.AccentColor != "#34C759" {
		t.Errorf("expected green color, got %s", update.Content.AccentColor)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 4 {
		t.Errorf("expected current_step 4, got %v", update.Content.CurrentStep)
	}

	// Phase 1: ONGOING
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Available" {
		t.Errorf("expected state 'Available', got %s", phase1.Content.State)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Available" {
		t.Errorf("expected state 'Available', got %s", phase2.Content.State)
	}
}

func TestMediaDeclined(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"notification_type": "MEDIA_DECLINED",
		"event": "media.declined",
		"subject": "The Matrix (1999)",
		"message": "Your request for The Matrix (1999) has been declined.",
		"image": "",
		"media": {
			"media_type": "movie",
			"tmdbId": "603",
			"tvdbId": "",
			"status": "UNKNOWN",
			"status4k": "UNKNOWN"
		},
		"request": {
			"request_id": "2",
			"requestedBy_username": "user1"
		}
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Declined" {
		t.Errorf("expected state 'Declined', got %s", update.Content.State)
	}
	if update.Content.AccentColor != "#FF3B30" {
		t.Errorf("expected red color, got %s", update.Content.AccentColor)
	}
	if update.Content.Icon != "xmark.circle.fill" {
		t.Errorf("expected icon xmark.circle.fill, got %s", update.Content.Icon)
	}

	// Declined has no step (step=0)
	if update.Content.CurrentStep != nil {
		t.Errorf("expected nil current_step for declined, got %d", *update.Content.CurrentStep)
	}

	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestMediaFailed(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"notification_type": "MEDIA_FAILED",
		"event": "media.failed",
		"subject": "Interstellar (2014)",
		"message": "Failed to process request for Interstellar (2014).",
		"image": "",
		"media": {
			"media_type": "movie",
			"tmdbId": "157336",
			"tvdbId": "",
			"status": "UNKNOWN",
			"status4k": "UNKNOWN"
		},
		"request": {
			"request_id": "3",
			"requestedBy_username": "user2"
		}
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Failed" {
		t.Errorf("expected state 'Failed', got %s", update.Content.State)
	}
	if update.Content.AccentColor != "#FF3B30" {
		t.Errorf("expected red color, got %s", update.Content.AccentColor)
	}
	if update.Content.Icon != "xmark.circle.fill" {
		t.Errorf("expected icon xmark.circle.fill, got %s", update.Content.Icon)
	}

	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestFullLifecycle(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Step 1: MEDIA_PENDING
	send(t, h, `{
		"notification_type": "MEDIA_PENDING",
		"event": "media.pending",
		"subject": "Inception (2010)",
		"message": "",
		"image": "",
		"media": {"media_type": "movie", "tmdbId": "27205", "tvdbId": "", "status": "PENDING", "status4k": "UNKNOWN"},
		"request": {"request_id": "1", "requestedBy_username": "admin"}
	}`)

	// Step 2: MEDIA_APPROVED
	send(t, h, `{
		"notification_type": "MEDIA_APPROVED",
		"event": "media.approved",
		"subject": "Inception (2010)",
		"message": "",
		"image": "",
		"media": {"media_type": "movie", "tmdbId": "27205", "tvdbId": "", "status": "PROCESSING", "status4k": "UNKNOWN"},
		"request": {"request_id": "1", "requestedBy_username": "admin"}
	}`)

	// Step 3: MEDIA_AVAILABLE
	send(t, h, `{
		"notification_type": "MEDIA_AVAILABLE",
		"event": "media.available",
		"subject": "Inception (2010)",
		"message": "",
		"image": "",
		"media": {"media_type": "movie", "tmdbId": "27205", "tvdbId": "", "status": "AVAILABLE", "status4k": "UNKNOWN"},
		"request": {"request_id": "1", "requestedBy_username": "admin"}
	}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// PENDING: create + ONGOING = 2
	// APPROVED: create + ONGOING = 2
	// AVAILABLE: create + ONGOING + phase1(ONGOING) + phase2(ENDED) = 4
	// Total = 8
	if len(recorded) != 8 {
		t.Fatalf("expected 8 calls, got %d", len(recorded))
	}

	// Verify progression: step 1 -> step 2 -> step 4
	var pending pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &pending)
	if *pending.Content.CurrentStep != 1 {
		t.Errorf("expected step 1, got %d", *pending.Content.CurrentStep)
	}

	var approved pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &approved)
	if *approved.Content.CurrentStep != 2 {
		t.Errorf("expected step 2, got %d", *approved.Content.CurrentStep)
	}

	var available pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[5].Body, &available)
	if *available.Content.CurrentStep != 4 {
		t.Errorf("expected step 4, got %d", *available.Content.CurrentStep)
	}

	// Final ENDED
	var ended pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[7].Body, &ended)
	if ended.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", ended.State)
	}
}

func TestTestNotification(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"notification_type": "TEST_NOTIFICATION",
		"event": "test",
		"subject": "Test Notification",
		"message": "This is a test notification from Overseerr.",
		"image": "",
		"media": {"media_type": "", "tmdbId": "", "tvdbId": "", "status": "", "status4k": ""},
		"request": {"request_id": "", "requestedBy_username": ""}
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify dispatch to selftest (content details tested in selftest/provider_test.go)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls (create + update), got %d", len(recorded))
	}

	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if create.Slug != "relay-test-overseerr" {
		t.Errorf("expected slug relay-test-overseerr, got %s", create.Slug)
	}
}
