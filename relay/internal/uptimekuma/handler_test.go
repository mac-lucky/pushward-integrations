package uptimekuma

import (
	"context"
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
	"github.com/mac-lucky/pushward-integrations/relay/internal/state/statetest"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func testConfig() *config.UptimeKumaConfig {
	return &config.UptimeKumaConfig{
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

func newHandler(t *testing.T, cfg *config.UptimeKumaConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
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
	req := httptest.NewRequest(http.MethodPost, "/uptimekuma", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestMonitorDown(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"monitor": {"id": 1, "name": "My Website", "url": "https://example.com", "type": "http"},
		"heartbeat": {"status": 0, "time": "2024-01-15T10:30:00.000Z", "msg": "Connection refused", "ping": null, "duration": 0, "important": true},
		"msg": "My Website is DOWN"
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
	if createReq.Slug != "uptime-1" {
		t.Errorf("expected slug uptime-1, got %s", createReq.Slug)
	}
	if createReq.Name != "My Website" {
		t.Errorf("expected name 'My Website', got %s", createReq.Name)
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
	if update.Content.State != "Connection refused" {
		t.Errorf("expected state 'Connection refused', got %s", update.Content.State)
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
	if update.Content.Subtitle != "Uptime Kuma \u00b7 My Website" {
		t.Errorf("expected subtitle 'Uptime Kuma \u00b7 My Website', got %q", update.Content.Subtitle)
	}
	if update.Content.URL != "https://example.com" {
		t.Errorf("expected URL https://example.com, got %s", update.Content.URL)
	}

	// Verify notification
	if recorded[2].Method != "POST" || recorded[2].Path != "/notifications" {
		t.Errorf("expected POST /notifications, got %s %s", recorded[2].Method, recorded[2].Path)
	}
	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &notif)
	if notif.Title != "My Website" {
		t.Errorf("expected notification title 'My Website', got %s", notif.Title)
	}
	if notif.Source != "uptimekuma" {
		t.Errorf("expected source uptimekuma, got %s", notif.Source)
	}
	if notif.Metadata["alert_name"] != "My Website" {
		t.Errorf("expected alert_name 'My Website', got %s", notif.Metadata["alert_name"])
	}
	if notif.Metadata["fingerprint"] != "1" {
		t.Errorf("expected fingerprint '1', got %s", notif.Metadata["fingerprint"])
	}
}

func TestMonitorDownThenUp(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send DOWN
	w := send(t, h, `{
		"monitor": {"id": 1, "name": "My Website", "url": "https://example.com", "type": "http"},
		"heartbeat": {"status": 0, "time": "2024-01-15T10:30:00.000Z", "msg": "Connection refused", "ping": null, "duration": 0, "important": true},
		"msg": "My Website is DOWN"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for DOWN, got %d", w.Code)
	}

	// Send UP
	w = send(t, h, `{
		"monitor": {"id": 1, "name": "My Website", "url": "https://example.com", "type": "http"},
		"heartbeat": {"status": 1, "time": "2024-01-15T10:35:00.000Z", "msg": "", "ping": 42, "duration": 300, "important": true},
		"msg": "My Website is UP"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for UP, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + DOWN(ONGOING) + down_notif + up_notif + phase1(ONGOING) + phase2(ENDED) = 6
	if len(recorded) != 6 {
		t.Fatalf("expected 6 calls, got %d", len(recorded))
	}

	// Verify UP notification
	var upNotif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &upNotif)
	if upNotif.Level != pushward.LevelPassive {
		t.Errorf("expected passive level for resolved, got %s", upNotif.Level)
	}

	// Phase 1: ONGOING with resolved content
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Resolved \u00b7 42ms" {
		t.Errorf("expected state 'Resolved \u00b7 42ms', got %q", phase1.Content.State)
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
	if phase2.Content.State != "Resolved \u00b7 42ms" {
		t.Errorf("expected state 'Resolved \u00b7 42ms', got %q", phase2.Content.State)
	}
}

func TestMonitorUpWithoutPriorDown(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"monitor": {"id": 1, "name": "My Website", "url": "https://example.com", "type": "http"},
		"heartbeat": {"status": 1, "time": "2024-01-15T10:35:00.000Z", "msg": "", "ping": 42, "duration": 300, "important": true},
		"msg": "My Website is UP"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 calls for UP without prior DOWN, got %d", len(recorded))
	}
}

func TestMonitorPending(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"monitor": {"id": 2, "name": "API Server", "url": "https://api.example.com/health", "type": "http"},
		"heartbeat": {"status": 2, "time": "2024-01-15T10:30:00.000Z", "msg": "Request timed out", "ping": null, "duration": 0, "important": false},
		"msg": "API Server is PENDING"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING = 2
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}

	// Verify create
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Slug != "uptime-2" {
		t.Errorf("expected slug uptime-2, got %s", createReq.Slug)
	}

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.State != "Checking..." {
		t.Errorf("expected state 'Checking...', got %s", update.Content.State)
	}
	if update.Content.AccentColor != pushward.ColorOrange {
		t.Errorf("expected orange color, got %s", update.Content.AccentColor)
	}
	if update.Content.Icon != "hourglass" {
		t.Errorf("expected icon hourglass, got %s", update.Content.Icon)
	}
	if update.Content.Severity != "warning" {
		t.Errorf("expected severity warning, got %s", update.Content.Severity)
	}
}

func TestMonitorMaintenanceSendsTestNotification(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"monitor": {"id": 1, "name": "My Website", "url": "https://example.com", "type": "http"},
		"heartbeat": {"status": 3, "time": "2024-01-15T10:30:00.000Z", "msg": "", "ping": null, "duration": 0, "important": false},
		"msg": "My Website is under MAINTENANCE"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// selftest: create + update = 2 calls
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls for maintenance (selftest), got %d", len(recorded))
	}

	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if create.Slug != "relay-test-uptimekuma" {
		t.Errorf("expected slug relay-test-uptimekuma, got %s", create.Slug)
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "alert" {
		t.Errorf("expected template alert, got %s", update.Content.Template)
	}
}

// newHandlerWithStore wires an Uptime Kuma handler against a custom store and
// base URL, for the store-degradation and update-failure scenarios.
func newHandlerWithStore(t *testing.T, cfg *config.UptimeKumaConfig, store state.Store, baseURL string) http.Handler {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	pool := client.NewPool(baseURL, nil)
	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, store, pool, cfg)
	return mux
}

const downBody = `{
	"monitor": {"id": 1, "name": "My Website", "url": "https://example.com", "type": "http"},
	"heartbeat": {"status": 0, "time": "2024-01-15T10:30:00.000Z", "msg": "Connection refused", "ping": null, "duration": 0, "important": true},
	"msg": "My Website is DOWN"
}`

// TestMonitorDown_StoreErrorStillDelivers pins the best-effort store
// degradation: when the state store fails (Get + Set error), a brand-new DOWN
// alert is still treated as new and fully delivered (create + ONGOING update +
// notification) rather than being dropped on a DB blip.
func TestMonitorDown_StoreErrorStillDelivers(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	h := newHandlerWithStore(t, testConfig(), statetest.FailingStore{}, srv.URL)

	w := send(t, h, downBody)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 despite store errors, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING update + notification = 3 (alert NOT dropped).
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}
	if n := testutil.CountPath(recorded, "/notifications"); n != 1 {
		t.Fatalf("expected the new-alert notification to be sent, got %d notifications", n)
	}
}

// TestMonitorDown_UpdateFailureRollsBackDedup pins the rollback branch: when
// UpdateActivity fails for a brand-new alert, the dedup row written moments
// earlier is deleted, so a re-send is treated as new again and re-triggers the
// isNew-gated notification (instead of being permanently suppressed).
func TestMonitorDown_UpdateFailureRollsBackDedup(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServerFailingPatches(t, 1) // first PATCH fails, then succeeds.
	store := state.NewMemoryStore()
	h := newHandlerWithStore(t, testConfig(), store, srv.URL)

	// First send: create OK, UpdateActivity fails → dedup row rolled back → 502.
	w := send(t, h, downBody)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 on update failure, got %d", w.Code)
	}

	// Rollback: no uptimekuma dedup row should remain.
	entries, err := store.ListByProvider(context.Background(), "uptimekuma")
	if err != nil {
		t.Fatalf("ListByProvider: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected dedup row rolled back, got %d entries", len(entries))
	}
	if n := testutil.CountPath(testutil.GetCalls(calls, mu), "/notifications"); n != 0 {
		t.Fatalf("expected 0 notifications after failed update, got %d", n)
	}

	// Re-send identical: treated as new again, so the notification re-triggers.
	w = send(t, h, downBody)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on re-send, got %d", w.Code)
	}
	if n := testutil.CountPath(testutil.GetCalls(calls, mu), "/notifications"); n != 1 {
		t.Fatalf("expected 1 notification after dedup re-trigger, got %d", n)
	}
}

// TestOverrideChannelsNotificationUpClearsDedup pins the dedup cleanup on the
// UP path when the activity channel is suppressed: ScheduleEnd (which normally
// deletes the row) never runs, so the handler must drop the row itself or the
// next DOWN within the stale timeout would be silenced.
func TestOverrideChannelsNotificationUpClearsDedup(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	sendOv := func(payload string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/uptimekuma?channels=notification", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer hlk_test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
		}
	}

	upBody := `{
		"monitor": {"id": 1, "name": "My Website", "url": "https://example.com", "type": "http"},
		"heartbeat": {"status": 1, "time": "2024-01-15T10:35:00.000Z", "msg": "", "ping": 42, "duration": 300, "important": true},
		"msg": "My Website is UP"
	}`

	sendOv(downBody)
	sendOv(upBody)
	sendOv(downBody)

	recorded := testutil.GetCalls(calls, mu)
	for _, c := range recorded {
		if strings.HasPrefix(c.Path, "/activities") {
			t.Fatalf("expected no activity calls with channels=notification, got %s %s", c.Method, c.Path)
		}
	}
	if n := testutil.CountPath(recorded, "/notifications"); n != 3 {
		t.Fatalf("expected 3 notifications (down, resolved, down again), got %d", n)
	}
}
