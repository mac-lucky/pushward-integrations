package uptimekuma

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

func newHandler(t *testing.T, cfg *config.UptimeKumaConfig) (*Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL)
	h := NewHandler(store, pool, cfg)
	return h, calls, mu
}

func send(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/uptimekuma", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	auth.Middleware(h).ServeHTTP(w, req)
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
	// create + ONGOING = 2
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
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
	if update.Content.AccentColor != "#FF3B30" {
		t.Errorf("expected red color, got %s", update.Content.AccentColor)
	}
	if update.Content.Icon != "exclamationmark.triangle.fill" {
		t.Errorf("expected icon exclamationmark.triangle.fill, got %s", update.Content.Icon)
	}
	if update.Content.Severity != "error" {
		t.Errorf("expected severity error, got %s", update.Content.Severity)
	}
	if update.Content.Subtitle != "Uptime Kuma \u00b7 My Website" {
		t.Errorf("expected subtitle 'Uptime Kuma \u00b7 My Website', got %q", update.Content.Subtitle)
	}
	if update.Content.URL != "https://example.com" {
		t.Errorf("expected URL https://example.com, got %s", update.Content.URL)
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
	// create + DOWN(ONGOING) + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	// Phase 1: ONGOING with resolved content
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Resolved \u00b7 42ms" {
		t.Errorf("expected state 'Resolved \u00b7 42ms', got %q", phase1.Content.State)
	}
	if phase1.Content.AccentColor != "#34C759" {
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
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
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
	if update.Content.AccentColor != "#FF9500" {
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
