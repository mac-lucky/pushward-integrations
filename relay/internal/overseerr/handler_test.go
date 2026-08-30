package overseerr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/poster"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

func testConfig() *config.OverseerrConfig {
	return &config.OverseerrConfig{
		BaseProviderConfig: config.BaseProviderConfig{
			Enabled:        true,
			Priority:       1,
			CleanupDelay:   1 * time.Hour,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
			DismissalDelay: testDismissalDelay(),
		},
	}
}

// testDismissalDelay mirrors the shipped default so the handler tests prove the
// configured value actually reaches the create body.
func testDismissalDelay() *time.Duration {
	d := 2 * time.Minute
	return &d
}

func newHandler(t *testing.T, cfg *config.OverseerrConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL, nil)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, store, pool, cfg, poster.Static(testutil.Thumbhash))

	return mux, calls, mu
}

// newHandlerRejecting builds a handler whose upstream answers status to every
// notification and activity create, standing in for a webhook configured with a
// key the server refuses, or a tenant over quota.
func newHandlerRejecting(t *testing.T, cfg *config.OverseerrConfig, status int) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServerRejecting(t, status, status)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, state.NewMemoryStore(), client.NewPool(srv.URL, nil), cfg, poster.Static(testutil.Thumbhash))
	return mux, calls, mu
}

// decodeBody decodes into the real response type, so a field rename on the
// envelope breaks these tests rather than silently passing against a stale copy.
func decodeBody(t *testing.T, w *httptest.ResponseRecorder) humautil.WebhookResponse {
	t.Helper()
	var r humautil.WebhookResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r.Body); err != nil {
		t.Fatalf("decoding response body %q: %v", w.Body.String(), err)
	}
	return r
}

func send(t *testing.T, h http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/overseerr", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
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
	// notification + create + ONGOING = 3 (no two-phase end for non-terminal)
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}

	// Verify notification
	if recorded[0].Method != "POST" || recorded[0].Path != "/notifications" {
		t.Errorf("expected POST /notifications, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &notif)
	if notif.Title != "Overseerr" {
		t.Errorf("expected title 'Overseerr', got %s", notif.Title)
	}
	if notif.Subtitle != "Inception (2010)" {
		t.Errorf("expected subtitle 'Inception (2010)', got %s", notif.Subtitle)
	}
	if notif.Body != "Requested" {
		t.Errorf("expected body 'Requested', got %s", notif.Body)
	}
	if notif.Metadata["media_title"] != "Inception (2010)" {
		t.Errorf("expected media_title 'Inception (2010)', got %s", notif.Metadata["media_title"])
	}
	if notif.ThreadID != "media-movie-27205" {
		t.Errorf("expected thread_id 'media-movie-27205', got %s", notif.ThreadID)
	}
	if notif.Source != "overseerr" {
		t.Errorf("expected source 'overseerr', got %s", notif.Source)
	}
	if notif.Media == nil || notif.Media.URL != "https://image.tmdb.org/t/p/w600_and_h900_bestv2/inception.jpg" || notif.Media.Type != "image" {
		t.Errorf("expected image media attachment, got %+v", notif.Media)
	}
	if notif.Metadata["media_type"] != "movie" {
		t.Errorf("expected media_type 'movie', got %s", notif.Metadata["media_type"])
	}
	if notif.Metadata["tmdb_id"] != "27205" {
		t.Errorf("expected tmdb_id '27205', got %s", notif.Metadata["tmdb_id"])
	}
	if notif.Metadata["requested_by"] != "admin" {
		t.Errorf("expected requested_by 'admin', got %s", notif.Metadata["requested_by"])
	}

	// Verify create
	if recorded[1].Method != "POST" || recorded[1].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[1].Method, recorded[1].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &createReq)
	if createReq.Slug != "overseerr-movie-27205" {
		t.Errorf("expected slug overseerr-movie-27205, got %s", createReq.Slug)
	}
	if createReq.Name != "Inception (2010)" {
		t.Errorf("expected name 'Inception (2010)', got %s", createReq.Name)
	}
	if createReq.Priority != 1 {
		t.Errorf("expected priority 1, got %d", createReq.Priority)
	}
	// A completion confirmation should not sit on the Lock Screen for the full
	// cleanup_delay; the configured dismissal_delay has to reach the create body.
	if createReq.DismissalTTL == nil || *createReq.DismissalTTL != 120 {
		t.Errorf("expected dismissal_ttl 120, got %v", createReq.DismissalTTL)
	}

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.State != "Requested" {
		t.Errorf("expected state 'Requested', got %s", update.Content.State)
	}
	if update.Content.Icon != "hourglass" {
		t.Errorf("expected icon hourglass, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != pushward.ColorOrange {
		t.Errorf("expected orange color, got %s", update.Content.AccentColor)
	}
	if update.Content.Template != "steps" {
		t.Errorf("expected template steps, got %s", update.Content.Template)
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
	// notification + create + ONGOING = 3 (no two-phase end for non-terminal)
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}

	// Verify notification
	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &notif)
	if notif.Body != "Approved" {
		t.Errorf("expected body 'Approved', got %s", notif.Body)
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.State != "Approved" {
		t.Errorf("expected state 'Approved', got %s", update.Content.State)
	}
	if update.Content.Icon != "checkmark.circle" {
		t.Errorf("expected icon checkmark.circle, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != pushward.ColorBlue {
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
	// notification + create + ONGOING + phase1(ONGOING) + phase2(ENDED) = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update)
	if update.Content.State != "Available" {
		t.Errorf("expected state 'Available', got %s", update.Content.State)
	}
	if update.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green color, got %s", update.Content.AccentColor)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 4 {
		t.Errorf("expected current_step 4, got %v", update.Content.CurrentStep)
	}

	// Phase 1: ONGOING
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Available" {
		t.Errorf("expected state 'Available', got %s", phase1.Content.State)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &phase2)
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
	// notification + create + ONGOING + phase1(ONGOING) + phase2(ENDED) = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update)
	if update.Content.State != "Declined" {
		t.Errorf("expected state 'Declined', got %s", update.Content.State)
	}
	if update.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red color, got %s", update.Content.AccentColor)
	}
	if update.Content.Icon != "xmark.circle.fill" {
		t.Errorf("expected icon xmark.circle.fill, got %s", update.Content.Icon)
	}

	// Declined starts at step 0
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 0 {
		t.Errorf("expected current_step 0 for declined, got %v", update.Content.CurrentStep)
	}

	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &phase2)
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
	// notification + create + ONGOING + phase1(ONGOING) + phase2(ENDED) = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update)
	if update.Content.State != "Failed" {
		t.Errorf("expected state 'Failed', got %s", update.Content.State)
	}
	if update.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red color, got %s", update.Content.AccentColor)
	}
	if update.Content.Icon != "xmark.circle.fill" {
		t.Errorf("expected icon xmark.circle.fill, got %s", update.Content.Icon)
	}

	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &phase2)
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
	// PENDING: notification + create + ONGOING = 3
	// APPROVED: notification + create + ONGOING = 3
	// AVAILABLE: notification + create + ONGOING + phase1(ONGOING) + phase2(ENDED) = 5
	// Total = 11
	if len(recorded) != 11 {
		t.Fatalf("expected 11 calls, got %d", len(recorded))
	}

	// Verify progression: step 1 -> step 2 -> step 4
	// PENDING: [0]=notif, [1]=create, [2]=update
	var pending pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &pending)
	if *pending.Content.CurrentStep != 1 {
		t.Errorf("expected step 1, got %d", *pending.Content.CurrentStep)
	}

	// APPROVED: [3]=notif, [4]=create, [5]=update
	var approved pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[5].Body, &approved)
	if *approved.Content.CurrentStep != 2 {
		t.Errorf("expected step 2, got %d", *approved.Content.CurrentStep)
	}

	// AVAILABLE: [6]=notif, [7]=create, [8]=update, [9]=phase1, [10]=phase2
	var available pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[8].Body, &available)
	if *available.Content.CurrentStep != 4 {
		t.Errorf("expected step 4, got %d", *available.Content.CurrentStep)
	}

	// Final ENDED
	var ended pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[10].Body, &ended)
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

// An "{{media}}" key holding an empty object in the webhook JSON payload is sent
// through verbatim, so the relay receives "media": {} and has no TMDB id to key
// the activity on. The push still has to go out, and the response has to say why
// there is no card - answering 200 {"status":"ok"} is what made a misconfigured
// template look like a working one.
func TestEmptyMediaNotifiesWithoutActivity(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"notification_type": "MEDIA_AVAILABLE",
		"subject": "Big Buck Bunny",
		"message": "Description",
		"image": "https://image.tmdb.org/t/p/w1280/bunny.jpg",
		"media": {},
		"request": {},
		"extra": []
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	body := decodeBody(t, w).Body
	if body.Status != humautil.StatusIgnoredActivity {
		t.Errorf("status = %q, want ignored_activity", body.Status)
	}
	if !strings.Contains(body.Detail, "media_type") {
		t.Errorf("detail %q does not name the missing field", body.Detail)
	}

	time.Sleep(50 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (the notification), got %d: %+v", len(recorded), recorded)
	}
	if recorded[0].Method != "POST" || recorded[0].Path != "/notifications" {
		t.Fatalf("expected POST /notifications, got %s %s", recorded[0].Method, recorded[0].Path)
	}

	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &notif)
	if notif.Subtitle != "Big Buck Bunny" {
		t.Errorf("subtitle = %q, want Big Buck Bunny", notif.Subtitle)
	}
	if notif.Body != "Available" {
		t.Errorf("body = %q, want Available", notif.Body)
	}
	// No usable ids, so no cross-provider thread and no id metadata rather than
	// empty-string keys.
	if notif.ThreadID != "" {
		t.Errorf("thread_id = %q, want empty", notif.ThreadID)
	}
	if _, ok := notif.Metadata["tmdb_id"]; ok {
		t.Errorf("tmdb_id metadata should be absent, got %+v", notif.Metadata)
	}
	if notif.Metadata["media_title"] != "Big Buck Bunny" {
		t.Errorf("media_title = %q, want Big Buck Bunny", notif.Metadata["media_title"])
	}
}

// Overseerr and Seerr send "media": null on events with no media object. Fields
// are optional by default on the relay API, so null decodes to the zero value
// instead of failing validation.
func TestNullMediaIsNotRejected(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"notification_type": "MEDIA_PENDING",
		"subject": "Big Buck Bunny",
		"media": null,
		"request": null,
		"issue": null,
		"comment": null,
		"extra": []
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if got := decodeBody(t, w).Body.Status; got != humautil.StatusIgnoredActivity {
		t.Errorf("status = %q, want ignored_activity", got)
	}

	time.Sleep(50 * time.Millisecond)
	if recorded := testutil.GetCalls(calls, mu); len(recorded) != 1 {
		t.Fatalf("expected 1 call (the notification), got %d", len(recorded))
	}
}

// Without a TMDB id the collapse key falls back to a digest of the subject. The
// contract has two halves and both matter: the same event for the same title
// must collapse (so a redelivery replaces rather than duplicates), and anything
// else must not (or each new event silently replaces the previous push).
func TestEmptyMediaCollapseIDs(t *testing.T) {
	collapseIDs := func(t *testing.T, events ...[2]string) []string {
		t.Helper()
		h, calls, mu := newHandler(t, testConfig())
		for _, e := range events {
			send(t, h, `{"notification_type":"`+e[0]+`","subject":"`+e[1]+`","media":{},"request":{}}`)
		}
		time.Sleep(50 * time.Millisecond)

		recorded := testutil.GetCalls(calls, mu)
		if len(recorded) != len(events) {
			t.Fatalf("expected %d notifications, got %d", len(events), len(recorded))
		}
		ids := make([]string, 0, len(recorded))
		for _, c := range recorded {
			var notif pushward.SendNotificationRequest
			testutil.UnmarshalBody(t, c.Body, &notif)
			if notif.CollapseID == "" {
				t.Fatal("collapse_id is empty")
			}
			ids = append(ids, notif.CollapseID)
		}
		return ids
	}

	t.Run("same event and title collapse", func(t *testing.T) {
		ids := collapseIDs(t, [2]string{"MEDIA_AVAILABLE", "Big Buck Bunny"}, [2]string{"MEDIA_AVAILABLE", "Big Buck Bunny"})
		if ids[0] != ids[1] {
			t.Errorf("a redelivery got a new collapse_id: %q then %q", ids[0], ids[1])
		}
	})

	t.Run("different titles do not collapse", func(t *testing.T) {
		ids := collapseIDs(t, [2]string{"MEDIA_AVAILABLE", "Big Buck Bunny"}, [2]string{"MEDIA_AVAILABLE", "Sintel"})
		if ids[0] == ids[1] {
			t.Errorf("two titles share collapse_id %q", ids[0])
		}
	})

	t.Run("different events do not collapse", func(t *testing.T) {
		ids := collapseIDs(t, [2]string{"MEDIA_AVAILABLE", "Big Buck Bunny"}, [2]string{"MEDIA_FAILED", "Big Buck Bunny"})
		if ids[0] == ids[1] {
			t.Errorf("two lifecycle events share collapse_id %q", ids[0])
		}
	})
}

// docPayload renders the webhook JSON payload exactly as relay/README.md hands
// it out, so these tests exercise the bytes users actually paste rather than a
// copy that can drift from them. Overseerr does plain placeholder substitution,
// replacing the {{media}}/{{request}}/{{issue}}/{{comment}}/{{extra}} *keys*
// with the bare name and each {{value}} with the event's field.
func docPayload(t *testing.T, fields map[string]string) string {
	t.Helper()
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("reading relay README: %v", err)
	}
	// Other providers document their own JSON payloads, so scope the search to
	// the Overseerr section before looking for the fenced block.
	section := regexp.MustCompile(`(?s)\n### Overseerr[^\n]*\n(.*?)\n### `).FindStringSubmatch(string(b))
	if section == nil {
		t.Fatal("relay/README.md has no ### Overseerr section")
	}
	m := regexp.MustCompile("(?s)```json\n(\\{.*?)\n```").FindStringSubmatch(section[1])
	if m == nil {
		t.Fatal("the Overseerr section has no fenced JSON payload block")
	}

	subs := []string{
		"{{media}}", "media", "{{request}}", "request",
		"{{issue}}", "issue", "{{comment}}", "comment", "{{extra}}", "extra",
	}
	for k, v := range fields {
		subs = append(subs, k, v)
	}
	rendered := strings.NewReplacer(subs...).Replace(m[1])

	// Anything the caller did not supply is a variable Overseerr would render to
	// the empty string; leaving it as a literal would test the wrong bytes.
	rendered = regexp.MustCompile(`\{\{[a-zA-Z_]+\}\}`).ReplaceAllString(rendered, "")
	if !json.Valid([]byte(rendered)) {
		t.Fatalf("the documented payload does not render to valid JSON:\n%s", rendered)
	}
	return rendered
}

// The template the README hands out has to produce the full lifecycle card.
// Pinning it to the README is the point: re-emptying the "{{media}}" block, the
// literal regression this whole change fixes, has to fail a test rather than
// only a user's setup.
func TestDocumentedTemplateProducesActivity(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, docPayload(t, map[string]string{
		"{{notification_type}}":    "MEDIA_PENDING",
		"{{event}}":                "media.pending",
		"{{subject}}":              "Inception (2010)",
		"{{message}}":              "A new request has been submitted.",
		"{{media_type}}":           "movie",
		"{{media_tmdbid}}":         "27205",
		"{{media_status}}":         "PENDING",
		"{{request_id}}":           "1",
		"{{requestedBy_username}}": "admin",
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if got := decodeBody(t, w).Body.Status; got != humautil.StatusOK {
		t.Errorf("status = %q, want ok", got)
	}
	// detail is omitempty: a success body must not carry the field at all, or
	// every provider's response surface changes.
	if strings.Contains(w.Body.String(), "detail") {
		t.Errorf("success body carries a detail field: %s", w.Body.String())
	}

	time.Sleep(50 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if n := testutil.CountPath(recorded, "/activities"); n != 1 {
		t.Fatalf("the documented template produced no Live Activity (%d POST /activities)", n)
	}
	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &create)
	if create.Slug != "overseerr-movie-27205" {
		t.Errorf("slug = %q, want overseerr-movie-27205", create.Slug)
	}
}

// The documented template has to carry the fields the ISSUE_* handlers read, or
// those events arrive stripped of their text - the same failure the media block
// had.
func TestDocumentedTemplateCarriesIssueFields(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	send(t, h, docPayload(t, map[string]string{
		"{{notification_type}}":    "ISSUE_COMMENT",
		"{{subject}}":              "Inception (2010)",
		"{{message}}":              "Audio is out of sync",
		"{{media_type}}":           "movie",
		"{{media_tmdbid}}":         "27205",
		"{{comment_message}}":      "Still broken after the re-grab",
		"{{commentedBy_username}}": "admin",
		"{{issue_type}}":           "AUDIO",
		"{{reportedBy_username}}":  "admin",
	}))
	time.Sleep(50 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(recorded))
	}
	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &notif)
	// Deliberately different from p.Message: a handler reading Message instead of
	// comment_message would still look right without this.
	if want := "New comment" + text.SepDot + "Still broken after the re-grab"; notif.Body != want {
		t.Errorf("body = %q, want %q", notif.Body, want)
	}
	if notif.Metadata["issue_type"] != "AUDIO" {
		t.Errorf("issue_type metadata = %q, want AUDIO", notif.Metadata["issue_type"])
	}
}

// Seerr's issue events have no request lifecycle to track, so they are a push
// and nothing else - but they carry the media object, so they still thread onto
// that title's other notifications.
func TestIssueCreatedNotifiesOnly(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"notification_type": "ISSUE_CREATED",
		"event": "issue.created",
		"subject": "Inception (2010)",
		"message": "Audio is out of sync",
		"image": "",
		"media": {"media_type": "movie", "tmdbId": "27205", "tvdbId": "", "status": "AVAILABLE", "status4k": "UNKNOWN"},
		"request": null,
		"issue": {"issue_id": "7", "issue_type": "AUDIO", "issue_status": "OPEN", "reportedBy_username": "admin"},
		"comment": null
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	time.Sleep(50 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (the notification), got %d", len(recorded))
	}
	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &notif)
	if notif.Body != "Issue reported"+text.SepDot+"Audio is out of sync" {
		t.Errorf("body = %q", notif.Body)
	}
	if notif.ThreadID != "media-movie-27205" {
		t.Errorf("thread_id = %q, want media-movie-27205", notif.ThreadID)
	}
}

func TestUnknownNotificationTypeIsReported(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{"notification_type": "SOMETHING_NEW", "subject": "x"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := decodeBody(t, w).Body.Status; got != humautil.StatusIgnored {
		t.Errorf("status = %q, want ignored", got)
	}
	time.Sleep(50 * time.Millisecond)
	if len(testutil.GetCalls(calls, mu)) != 0 {
		t.Error("an unsupported type should not reach the API")
	}
}

// An upstream refusal of the caller's own key has to reach the webhook caller
// unchanged, not as a generic 502: the caller is the one holding the key. With
// the push as the only delivery path there is no activity call to fail instead,
// so swallowing it would answer 200 to a webhook that reached nobody.
// 429 is deliberately absent: the client retries it five times with backoff, so
// driving it through the handler costs ~11s of sleeping to prove what
// humautil's own TestUpstreamError already covers.
func TestUpstreamRefusalSurfacesUnchanged(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h, calls, mu := newHandlerRejecting(t, testConfig(), status)

			w := send(t, h, `{
				"notification_type": "MEDIA_AVAILABLE",
				"subject": "Big Buck Bunny",
				"media": {},
				"request": {}
			}`)
			if w.Code != status {
				t.Fatalf("expected %d, got %d (%s)", status, w.Code, w.Body.String())
			}
			// Assert what was attempted, or this passes for a handler that sent
			// nothing and failed for an unrelated reason.
			recorded := testutil.GetCalls(calls, mu)
			if len(recorded) != 1 || recorded[0].Path != "/notifications" {
				t.Fatalf("expected exactly one POST /notifications, got %+v", recorded)
			}
			if status == http.StatusUnauthorized && w.Header().Get("WWW-Authenticate") == "" {
				t.Error("RFC 9110 section 15.5.2 requires a challenge on a 401")
			}
		})
	}
}

// The activity calls are the other side of the same rule: a create that the
// upstream refuses must not collapse into 502 either.
func TestUpstreamRefusalOnActivitySurfacesUnchanged(t *testing.T) {
	h, _, _ := newHandlerRejecting(t, testConfig(), http.StatusForbidden)

	w := send(t, h, availableBody)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d (%s)", w.Code, w.Body.String())
	}
}

// A payload the upstream already refused on the push will be refused on the
// activity too, so the handler must not spend two more round-trips learning it.
func TestAuthFailureSkipsTheActivityCalls(t *testing.T) {
	h, calls, mu := newHandlerRejecting(t, testConfig(), http.StatusUnauthorized)

	if w := send(t, h, availableBody); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)

	for _, c := range testutil.GetCalls(calls, mu) {
		if strings.HasPrefix(c.Path, "/activities") {
			t.Errorf("activity call attempted after an upstream auth failure: %s %s", c.Method, c.Path)
		}
	}
}

// Every reason has to reach the caller distinctly - a caller that cannot tell
// "no media_type" from "tmdbId is not a number" is back to guessing.
func TestSkipReasonsReachTheCaller(t *testing.T) {
	tests := []struct {
		name, media string
		want        skipReason
	}{
		{"empty media block", `{}`, reasonNoMediaType},
		{"unsupported type", `{"media_type":"music","tmdbId":"1"}`, reasonBadMediaType},
		{"missing tmdb id", `{"media_type":"movie","tmdbId":""}`, reasonNoTmdbID},
		{"non numeric tmdb id", `{"media_type":"movie","tmdbId":"tt1375666"}`, reasonBadTmdbID},
		// strconv.Atoi would accept these; the slug they build is rejected by the
		// server, so the gate has to catch them here with a named reason.
		{"signed tmdb id", `{"media_type":"movie","tmdbId":"+5"}`, reasonBadTmdbID},
		{"negative tmdb id", `{"media_type":"movie","tmdbId":"-5"}`, reasonBadTmdbID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, _ := newHandler(t, testConfig())
			w := send(t, h, `{"notification_type":"MEDIA_AVAILABLE","subject":"x","media":`+tt.media+`}`)

			body := decodeBody(t, w).Body
			if body.Status != humautil.StatusIgnoredActivity {
				t.Errorf("status = %q, want ignored_activity", body.Status)
			}
			if body.Detail != string(tt.want) {
				t.Errorf("detail = %q, want %q", body.Detail, tt.want)
			}
			if strings.Contains(body.Detail, "music") {
				t.Errorf("detail echoes the caller-supplied value: %q", body.Detail)
			}
		})
	}
}

// Each notify-only type carries its own label, and two of them read a different
// payload field than the rest.
func TestNotifyOnlyEventBodies(t *testing.T) {
	tests := []struct {
		name, typ, want string
	}{
		{"auto requested", "MEDIA_AUTO_REQUESTED", "Auto-requested" + text.SepDot + "A new request"},
		{"issue created", "ISSUE_CREATED", "Issue reported" + text.SepDot + "A new request"},
		{"comment", "ISSUE_COMMENT", "New comment" + text.SepDot + "Still broken"},
		// message is present and deliberately not appended: these two say all
		// there is to say, and the issue description is stale by then.
		{"resolved", "ISSUE_RESOLVED", "Issue resolved"},
		{"reopened", "ISSUE_REOPENED", "Issue reopened"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, calls, mu := newHandler(t, testConfig())
			send(t, h, `{
				"notification_type": "`+tt.typ+`",
				"subject": "Inception (2010)",
				"message": "A new request",
				"media": {"media_type":"movie","tmdbId":"27205"},
				"comment": {"comment_message":"Still broken"}
			}`)
			time.Sleep(50 * time.Millisecond)

			recorded := testutil.GetCalls(calls, mu)
			if len(recorded) != 1 || recorded[0].Path != "/notifications" {
				t.Fatalf("expected exactly one POST /notifications, got %+v", recorded)
			}
			var notif pushward.SendNotificationRequest
			testutil.UnmarshalBody(t, recorded[0].Body, &notif)
			if notif.Body != tt.want {
				t.Errorf("body = %q, want %q", notif.Body, tt.want)
			}
			if notif.ThreadID != "media-movie-27205" {
				t.Errorf("thread_id = %q, want media-movie-27205", notif.ThreadID)
			}
		})
	}
}

// A tv payload whose template lists tvdbId but not tmdbId can still group with
// Sonarr's and Jellyfin's pushes about the same show. Gating the thread on the
// activity key would throw that away.
func TestTVWithoutTmdbIDStillThreads(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"notification_type": "MEDIA_AVAILABLE",
		"subject": "Stranger Things (2016)",
		"media": {"media_type":"tv","tmdbId":"","tvdbId":"305288"}
	}`)
	if got := decodeBody(t, w).Body.Status; got != humautil.StatusIgnoredActivity {
		t.Errorf("status = %q, want ignored_activity", got)
	}
	time.Sleep(50 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(recorded))
	}
	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &notif)
	if notif.ThreadID != "media-tv-305288" {
		t.Errorf("thread_id = %q, want media-tv-305288", notif.ThreadID)
	}
	if notif.Metadata["media_type"] != "tv" {
		t.Errorf("media_type metadata = %q, want tv", notif.Metadata["media_type"])
	}
}

// An unrendered template variable must not become part of the thread key.
func TestUnrenderedTvdbIDIsNotThreaded(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	send(t, h, `{
		"notification_type": "MEDIA_AVAILABLE",
		"subject": "Stranger Things (2016)",
		"media": {"media_type":"tv","tmdbId":"","tvdbId":"{{media_tvdbid}}"}
	}`)
	time.Sleep(50 * time.Millisecond)

	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, testutil.GetCalls(calls, mu)[0].Body, &notif)
	if notif.ThreadID != "" {
		t.Errorf("thread_id = %q, want empty", notif.ThreadID)
	}
}

// An issue event with the notification surface suppressed reaches nobody, so it
// must not answer "ok" - that is the failure this whole change is about.
func TestNotifyOnlySuppressedIsReported(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendQuery(t, h, "channels=activity", `{
		"notification_type": "ISSUE_CREATED",
		"subject": "Inception (2010)",
		"media": {"media_type":"movie","tmdbId":"27205"}
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if got := decodeBody(t, w).Body.Status; got != humautil.StatusIgnored {
		t.Errorf("status = %q, want ignored", got)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(testutil.GetCalls(calls, mu)); n != 0 {
		t.Errorf("a suppressed notification still reached the API (%d calls)", n)
	}
}

// A caller that asked for notification-only wanted no card, so the missing card
// is not reported as skipped - but the push failing still fails the request.
func TestSuppressedActivityDoesNotReportSkipped(t *testing.T) {
	h, _, _ := newHandler(t, testConfig())

	w := sendQuery(t, h, "channels=notification", availableBody)
	if got := decodeBody(t, w).Body.Status; got != humautil.StatusOK {
		t.Errorf("status = %q, want ok", got)
	}

	failing, _, _ := newHandlerRejecting(t, testConfig(), http.StatusUnauthorized)
	if w := sendQuery(t, failing, "channels=notification", availableBody); w.Code != http.StatusUnauthorized {
		t.Errorf("a failed push on the only delivery path answered %d, want 401", w.Code)
	}
}

// sendQuery posts with query-parameter overrides on the webhook URL.
func sendQuery(t *testing.T, h http.Handler, query, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/overseerr?"+query, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

const availableBody = `{
	"notification_type": "MEDIA_AVAILABLE",
	"event": "media.available",
	"subject": "Inception (2010)",
	"message": "",
	"image": "",
	"media": {"media_type": "movie", "tmdbId": "27205", "tvdbId": "", "status": "AVAILABLE", "status4k": "UNKNOWN"},
	"request": {"request_id": "1", "requestedBy_username": "admin"}
}`

// TestSuppressedTerminalEndsPriorActivity: Overseerr can be configured with more
// than one webhook URL, so the request that opens the activity and the one that
// completes it need not carry the same channels override. A terminal event with
// the activity surface suppressed must still close an activity an earlier event
// opened, or it hangs on the lock screen until the stale TTL.
func TestSuppressedTerminalEndsPriorActivity(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	send(t, h, `{
		"notification_type": "MEDIA_PENDING",
		"event": "media.requested",
		"subject": "Inception (2010)",
		"message": "",
		"image": "",
		"media": {"media_type": "movie", "tmdbId": "27205", "tvdbId": "", "status": "PENDING", "status4k": "UNKNOWN"},
		"request": {"request_id": "1", "requestedBy_username": "admin"}
	}`)

	if w := sendQuery(t, h, "channels=notification", availableBody); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	// notification + create + ONGOING for PENDING, then notification + the
	// ender's two phases for AVAILABLE.
	recorded := testutil.WaitForCalls(t, calls, mu, 6, 2*time.Second)
	var ended bool
	for _, c := range recorded {
		if !strings.HasPrefix(c.Path, "/activities/") {
			continue
		}
		var req pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &req)
		if req.State == pushward.StateEnded {
			ended = true
		}
	}
	if !ended {
		t.Error("the activity opened by MEDIA_PENDING was never ended")
	}
}

// A non-terminal event must not end anything: the activity is meant to stay open
// for the rest of the request's lifecycle.
func TestSuppressedNonTerminalEndsNothing(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	send(t, h, `{
		"notification_type": "MEDIA_PENDING",
		"event": "media.requested",
		"subject": "Inception (2010)",
		"message": "",
		"image": "",
		"media": {"media_type": "movie", "tmdbId": "27205", "tvdbId": "", "status": "PENDING", "status4k": "UNKNOWN"},
		"request": {"request_id": "1", "requestedBy_username": "admin"}
	}`)
	before := len(testutil.GetCalls(calls, mu))

	sendQuery(t, h, "channels=notification", `{
		"notification_type": "MEDIA_APPROVED",
		"event": "media.approved",
		"subject": "Inception (2010)",
		"message": "",
		"image": "",
		"media": {"media_type": "movie", "tmdbId": "27205", "tvdbId": "", "status": "APPROVED", "status4k": "UNKNOWN"},
		"request": {"request_id": "1", "requestedBy_username": "admin"}
	}`)
	time.Sleep(100 * time.Millisecond)

	for _, c := range testutil.GetCalls(calls, mu)[before:] {
		if strings.HasPrefix(c.Path, "/activities") {
			t.Errorf("a suppressed non-terminal event touched the activity: %s %s", c.Method, c.Path)
		}
	}
}
