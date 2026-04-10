package gatus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func testConfig() *config.GatusConfig {
	return &config.GatusConfig{
		BaseProviderConfig: config.BaseProviderConfig{
			Enabled:        true,
			Priority:       2,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
			CleanupDelay:   1 * time.Hour,
			StaleTimeout:   1 * time.Hour,
		},
	}
}

func newHandler(t *testing.T, cfg *config.GatusConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL, nil)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, store, pool, cfg)

	return mux, calls, mu
}

func send(t *testing.T, h http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/gatus", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestTriggered(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"endpoint_name": "My API",
		"endpoint_group": "",
		"endpoint_url": "https://api.example.com/health",
		"alert_description": "Health check failed",
		"status": "TRIGGERED",
		"result_errors": "[STATUS] == 200 (expected 200, got 503)"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING + notification = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}

	// Verify create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "My API" {
		t.Errorf("expected name 'My API', got %s", createReq.Name)
	}
	if createReq.Priority != 2 {
		t.Errorf("expected priority 2, got %d", createReq.Priority)
	}

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "alert" {
		t.Errorf("expected template alert, got %s", update.Content.Template)
	}
	if update.Content.State != "[STATUS] == 200 (expected 200, got 503)" {
		t.Errorf("expected state '[STATUS] == 200 (expected 200, got 503)', got %s", update.Content.State)
	}
	if update.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red color, got %s", update.Content.AccentColor)
	}
	if update.Content.Icon != "exclamationmark.triangle.fill" {
		t.Errorf("expected icon exclamationmark.triangle.fill, got %s", update.Content.Icon)
	}
	if update.Content.Severity != "critical" {
		t.Errorf("expected severity critical, got %s", update.Content.Severity)
	}
	if update.Content.Subtitle != "Gatus \u00b7 My API" {
		t.Errorf("expected subtitle 'Gatus \u00b7 My API', got %q", update.Content.Subtitle)
	}
	if update.Content.URL != "https://api.example.com/health" {
		t.Errorf("expected URL https://api.example.com/health, got %s", update.Content.URL)
	}

	// Verify notification
	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &notif)
	if notif.Title != "My API" {
		t.Errorf("expected notification title 'My API', got %s", notif.Title)
	}
	if notif.Category != "critical" {
		t.Errorf("expected category critical, got %s", notif.Category)
	}
	if notif.Source != "gatus" {
		t.Errorf("expected source gatus, got %s", notif.Source)
	}
	if notif.Metadata["alert_name"] != "My API" {
		t.Errorf("expected alert_name 'My API', got %s", notif.Metadata["alert_name"])
	}
	if notif.Metadata["fingerprint"] == "" {
		t.Error("expected non-empty fingerprint in metadata")
	}
}

func TestTriggeredThenResolved(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send TRIGGERED
	w := send(t, h, `{
		"endpoint_name": "My API",
		"endpoint_group": "",
		"endpoint_url": "https://api.example.com/health",
		"alert_description": "Health check failed",
		"status": "TRIGGERED",
		"result_errors": "[STATUS] == 200 (expected 200, got 503)"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for TRIGGERED, got %d", w.Code)
	}

	// Send RESOLVED
	w = send(t, h, `{
		"endpoint_name": "My API",
		"endpoint_group": "",
		"endpoint_url": "https://api.example.com/health",
		"alert_description": "Health check failed",
		"status": "RESOLVED",
		"result_errors": ""
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for RESOLVED, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + TRIGGERED(ONGOING) + triggered_notif + resolved_notif + phase1(ONGOING) + phase2(ENDED) = 6
	if len(recorded) != 6 {
		t.Fatalf("expected 6 calls, got %d", len(recorded))
	}

	// Verify resolved notification
	var resolvedNotif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &resolvedNotif)
	if resolvedNotif.Category != "resolved" {
		t.Errorf("expected resolved notification category, got %s", resolvedNotif.Category)
	}
	if resolvedNotif.Level != pushward.LevelPassive {
		t.Errorf("expected passive level for resolved, got %s", resolvedNotif.Level)
	}

	// Phase 1: ONGOING with resolved content
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Resolved" {
		t.Errorf("expected state 'Resolved', got %q", phase1.Content.State)
	}
	if phase1.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}
	if phase1.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected icon checkmark.circle.fill, got %s", phase1.Content.Icon)
	}
	if phase1.Content.Severity != "info" {
		t.Errorf("expected severity info, got %s", phase1.Content.Severity)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[5].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Resolved" {
		t.Errorf("expected state 'Resolved', got %q", phase2.Content.State)
	}
}

func TestResolvedWithoutPriorTriggered(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"endpoint_name": "My API",
		"endpoint_group": "",
		"endpoint_url": "https://api.example.com/health",
		"alert_description": "Health check failed",
		"status": "RESOLVED",
		"result_errors": ""
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 calls for RESOLVED without prior TRIGGERED, got %d", len(recorded))
	}
}

func TestTriggeredWithGroup(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"endpoint_name": "My API",
		"endpoint_group": "production",
		"endpoint_url": "https://api.example.com/health",
		"alert_description": "Health check failed",
		"status": "TRIGGERED",
		"result_errors": "error"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING + notification = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.Subtitle != "Gatus \u00b7 production/My API" {
		t.Errorf("expected subtitle 'Gatus \u00b7 production/My API', got %q", update.Content.Subtitle)
	}

	// Verify notification includes endpoint_group
	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &notif)
	if notif.Metadata["endpoint_group"] != "production" {
		t.Errorf("expected endpoint_group 'production', got %s", notif.Metadata["endpoint_group"])
	}
}

func TestTriggeredFallbackState(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// result_errors is empty, should fall back to alert_description
	w := send(t, h, `{
		"endpoint_name": "My API",
		"endpoint_group": "",
		"endpoint_url": "https://api.example.com/health",
		"alert_description": "Health check failed",
		"status": "TRIGGERED",
		"result_errors": ""
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING + notification = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Health check failed" {
		t.Errorf("expected state 'Health check failed', got %s", update.Content.State)
	}
}
