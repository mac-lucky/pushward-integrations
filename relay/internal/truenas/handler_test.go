package truenas

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

const alias = "a1b2c3d4-1234-5678-9abc-def012345678"

const createBody = `{
	"message": "Pool tank is DEGRADED",
	"alias": "a1b2c3d4-1234-5678-9abc-def012345678",
	"description": "Pool tank is DEGRADED: a device experienced an error."
}`

func testConfig() *config.TrueNASConfig {
	return &config.TrueNASConfig{
		BaseProviderConfig: config.BaseProviderConfig{
			Enabled:        true,
			Priority:       5,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
			CleanupDelay:   1 * time.Hour,
			StaleTimeout:   24 * time.Hour,
		},
	}
}

func newHandler(t *testing.T) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL, nil)
	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, store, pool, testConfig())
	return mux, calls, mu
}

// sendCreate posts an OpsGenie create-alert, authenticating with the GenieKey
// scheme that TrueNAS uses.
func sendCreate(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/truenas/v2/alerts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "GenieKey hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func sendDelete(t *testing.T, h http.Handler, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/truenas/v2/alerts/"+id+"?identifierType=alias", nil)
	req.Header.Set("Authorization", "GenieKey hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestOverrideChannelsOnDeleteRoute confirms the overrides middleware reaches
// the DELETE route: channels=activity on the clear call must keep the two-phase
// end but drop the resolved notification.
func TestOverrideChannelsOnDeleteRoute(t *testing.T) {
	h, calls, mu := newHandler(t)
	sendCreate(t, h, createBody)

	req := httptest.NewRequest(http.MethodDelete, "/truenas/v2/alerts/"+alias+"?identifierType=alias&channels=activity", nil)
	req.Header.Set("Authorization", "GenieKey hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// The create path sent one active notification; the clear path must add none.
	if n := testutil.CountPath(recorded, "/notifications"); n != 1 {
		t.Fatalf("expected 1 notification (from create only), got %d", n)
	}
	// Clear still runs the two-phase end (phase1 ONGOING + phase2 ENDED).
	var sawEnded bool
	for _, c := range recorded {
		if c.Method == http.MethodPatch {
			var upd pushward.UpdateRequest
			testutil.UnmarshalBody(t, c.Body, &upd)
			if upd.State == pushward.StateEnded {
				sawEnded = true
			}
		}
	}
	if !sawEnded {
		t.Error("expected the activity to be ended despite channels=activity")
	}
}

func TestOverrideInvalidLevelRejectedOnDelete(t *testing.T) {
	h, _, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodDelete, "/truenas/v2/alerts/"+alias+"?level=bogus", nil)
	req.Header.Set("Authorization", "GenieKey hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid level on DELETE, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestCreateAlert(t *testing.T) {
	h, calls, mu := newHandler(t)

	w := sendCreate(t, h, createBody)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING + notification = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}
	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if !strings.HasPrefix(create.Slug, "truenas-") {
		t.Errorf("expected truenas- slug, got %s", create.Slug)
	}

	var upd pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &upd)
	if upd.Content.Template != pushward.TemplateAlert {
		t.Errorf("expected alert template, got %s", upd.Content.Template)
	}
	if upd.Content.State != "Pool tank is DEGRADED" {
		t.Errorf("expected message as state, got %q", upd.Content.State)
	}
	if upd.Content.AccentColor != pushward.ColorOrange || upd.Content.Severity != "warning" {
		t.Errorf("expected orange/warning, got %s/%s", upd.Content.AccentColor, upd.Content.Severity)
	}
	if upd.Content.FiredAt == nil || *upd.Content.FiredAt <= 0 {
		t.Errorf("expected a positive fired_at, got %v", upd.Content.FiredAt)
	}

	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &notif)
	if notif.Level != pushward.LevelActive {
		t.Errorf("expected active notification, got %s", notif.Level)
	}
	if notif.Source != "truenas" {
		t.Errorf("expected source truenas, got %s", notif.Source)
	}
}

func TestCreateThenDelete(t *testing.T) {
	h, calls, mu := newHandler(t)

	if w := sendCreate(t, h, createBody); w.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d", w.Code)
	}
	if w := sendDelete(t, h, alias); w.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING + active notif + passive notif + phase1 ONGOING + phase2 ENDED = 6
	if len(recorded) != 6 {
		t.Fatalf("expected 6 calls, got %d", len(recorded))
	}

	var resolveNotif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &resolveNotif)
	if resolveNotif.Level != pushward.LevelPassive {
		t.Errorf("expected passive resolve notification, got %s", resolveNotif.Level)
	}

	var end pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[5].Body, &end)
	if end.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", end.State)
	}
	if end.Content.State != "Resolved" || end.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected Resolved/green, got %s/%s", end.Content.State, end.Content.AccentColor)
	}
}

func TestDeleteUnknownAliasNoOp(t *testing.T) {
	h, calls, mu := newHandler(t)

	w := sendDelete(t, h, "never-seen-alias")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for unknown alias, got %d", w.Code)
	}
	if n := len(testutil.GetCalls(calls, mu)); n != 0 {
		t.Fatalf("expected 0 calls for an unknown-alias clear, got %d", n)
	}
}

func TestGenieKeyRequired(t *testing.T) {
	h, _, _ := newHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/truenas/v2/alerts", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	// No Authorization header.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a key, got %d", w.Code)
	}
}

// TestOverrideChannelsNotificationClearDropsDedup pins the dedup cleanup on
// the clear path when the activity channel is suppressed: ScheduleEnd (which
// normally deletes the row) never runs, so the handler must drop the row
// itself or a re-fired alert within the stale timeout would be silenced.
func TestOverrideChannelsNotificationClearDropsDedup(t *testing.T) {
	h, calls, mu := newHandler(t)

	create := func() {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/truenas/v2/alerts?channels=notification", strings.NewReader(createBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "GenieKey hlk_test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("create: expected 200, got %d (%s)", w.Code, w.Body.String())
		}
	}

	create()
	req := httptest.NewRequest(http.MethodDelete, "/truenas/v2/alerts/"+alias+"?identifierType=alias&channels=notification", nil)
	req.Header.Set("Authorization", "GenieKey hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("clear: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	create()

	recorded := testutil.GetCalls(calls, mu)
	for _, c := range recorded {
		if strings.HasPrefix(c.Path, "/activities") {
			t.Fatalf("expected no activity calls with channels=notification, got %s %s", c.Method, c.Path)
		}
	}
	if n := testutil.CountPath(recorded, "/notifications"); n != 3 {
		t.Fatalf("expected 3 notifications (alert, cleared, alert again), got %d", n)
	}
}
