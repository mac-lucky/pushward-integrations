package komodo

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

const (
	bodyUnreachable = `{
		"_id": {"$oid": "6650f1c2a1b2c3d4e5f60789"},
		"ts": 1730000000000, "resolved": false, "level": "CRITICAL",
		"target": {"type": "Server", "id": "srv-1"},
		"data": {"type": "ServerUnreachable", "data": {"id": "srv-1", "name": "prod-node", "region": null, "err": {"error": "connection refused"}}},
		"resolved_ts": null
	}`
	bodyUnreachableResolved = `{
		"_id": {"$oid": "6650f1c2a1b2c3d4e5f60789"},
		"ts": 1730000600000, "resolved": true, "level": "OK",
		"target": {"type": "Server", "id": "srv-1"},
		"data": {"type": "ServerUnreachable", "data": {"id": "srv-1", "name": "prod-node", "region": null, "err": {"error": "connection refused"}}},
		"resolved_ts": 1730000600000
	}`
	bodyCPU = `{
		"ts": 1730000000000, "resolved": false, "level": "WARNING",
		"target": {"type": "Server", "id": "srv-1"},
		"data": {"type": "ServerCpu", "data": {"id": "srv-1", "name": "prod-node", "region": null, "percentage": 92.5}}
	}`
	bodyContainer = `{
		"ts": 1730000000000, "resolved": false, "level": "WARNING",
		"target": {"type": "Deployment", "id": "dep-1"},
		"data": {"type": "ContainerStateChange", "data": {"id": "dep-1", "name": "web", "from": "Running", "to": "Exited"}}
	}`
	bodyBuildFailed = `{
		"ts": 1730000000000, "resolved": false, "level": "CRITICAL",
		"target": {"type": "Build", "id": "bld-1"},
		"data": {"type": "BuildFailed", "data": {"id": "bld-1", "name": "api"}}
	}`
	bodyCustom = `{
		"_id": "custom-alert-1",
		"ts": 1730000000000, "resolved": false, "level": "OK",
		"target": {"type": "System", "id": "system"},
		"data": {"type": "Custom", "data": {"message": "Nightly backup complete", "details": "42 GB archived"}}
	}`
	bodyTest = `{
		"ts": 1730000000000, "resolved": false, "level": "OK",
		"target": {"type": "System", "id": "system"},
		"data": {"type": "Test", "data": {"id": "system", "name": "Test Alert"}}
	}`
)

func testConfig() *config.KomodoConfig {
	return &config.KomodoConfig{
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

func send(t *testing.T, h http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	return sendTo(t, h, "/komodo", payload)
}

func sendTo(t *testing.T, h http.Handler, target, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	return w
}

func countActivityCalls(calls []testutil.APICall) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c.Path, "/activities") {
			n++
		}
	}
	return n
}

func TestServerUnreachableActive(t *testing.T) {
	h, calls, mu := newHandler(t)
	send(t, h, bodyUnreachable)

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING + notification = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}
	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if !strings.HasPrefix(create.Slug, "komodo-") {
		t.Errorf("expected komodo- slug, got %s", create.Slug)
	}
	if create.Name != "prod-node" {
		t.Errorf("expected name prod-node, got %s", create.Name)
	}

	var upd pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &upd)
	if upd.Content.Template != pushward.TemplateAlert {
		t.Errorf("expected alert template, got %s", upd.Content.Template)
	}
	if upd.Content.State != "Unreachable" {
		t.Errorf("expected state Unreachable, got %s", upd.Content.State)
	}
	if upd.Content.AccentColor != pushward.ColorRed || upd.Content.Severity != "critical" {
		t.Errorf("expected red/critical, got %s/%s", upd.Content.AccentColor, upd.Content.Severity)
	}
	if upd.Content.FiredAt == nil || *upd.Content.FiredAt != 1730000000 {
		t.Errorf("expected fired_at 1730000000 (ms/1000), got %v", upd.Content.FiredAt)
	}

	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &notif)
	if notif.Level != pushward.LevelActive {
		t.Errorf("expected active notification, got %s", notif.Level)
	}
}

func TestServerUnreachableActiveThenResolved(t *testing.T) {
	h, calls, mu := newHandler(t)
	send(t, h, bodyUnreachable)
	send(t, h, bodyUnreachableResolved)
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

	// Final ENDED frame renders Resolved (never the stale error), green.
	var end pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[5].Body, &end)
	if end.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", end.State)
	}
	if end.Content.State != "Resolved" {
		t.Errorf("expected Resolved, got %s", end.Content.State)
	}
	if end.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green, got %s", end.Content.AccentColor)
	}
}

func TestResolvedWithoutActive(t *testing.T) {
	h, calls, mu := newHandler(t)
	send(t, h, bodyUnreachableResolved)
	if n := len(testutil.GetCalls(calls, mu)); n != 0 {
		t.Fatalf("expected 0 calls for a resolve without a prior active alert, got %d", n)
	}
}

func TestServerCpuStateText(t *testing.T) {
	h, calls, mu := newHandler(t)
	send(t, h, bodyCPU)

	recorded := testutil.GetCalls(calls, mu)
	var upd pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &upd)
	if upd.Content.State != "CPU 92%" {
		t.Errorf("expected 'CPU 92%%', got %q", upd.Content.State)
	}
	if upd.Content.AccentColor != pushward.ColorOrange || upd.Content.Severity != "warning" {
		t.Errorf("expected orange/warning, got %s/%s", upd.Content.AccentColor, upd.Content.Severity)
	}
}

func TestOneShotContainerStateChange(t *testing.T) {
	h, calls, mu := newHandler(t)
	send(t, h, bodyContainer)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (notification only), got %d", len(recorded))
	}
	if recorded[0].Path != "/notifications" {
		t.Fatalf("expected /notifications, got %s", recorded[0].Path)
	}
	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &notif)
	if notif.Body != "Running -> Exited" {
		t.Errorf("expected body 'Running -> Exited', got %q", notif.Body)
	}
	if notif.Level != pushward.LevelActive { // WARNING
		t.Errorf("expected active level for WARNING, got %s", notif.Level)
	}
	if notif.Source != "komodo" {
		t.Errorf("expected source komodo, got %s", notif.Source)
	}
}

func TestOneShotBuildFailedTimeSensitive(t *testing.T) {
	h, calls, mu := newHandler(t)
	send(t, h, bodyBuildFailed)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(recorded))
	}
	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &notif)
	if notif.Level != pushward.LevelTimeSensitive { // CRITICAL
		t.Errorf("expected time-sensitive for CRITICAL, got %s", notif.Level)
	}
	if notif.Body != "Build failed" {
		t.Errorf("expected body 'Build failed', got %q", notif.Body)
	}
}

func TestOneShotCustomPassive(t *testing.T) {
	h, calls, mu := newHandler(t)
	send(t, h, bodyCustom)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(recorded))
	}
	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &notif)
	if notif.Body != "Nightly backup complete" {
		t.Errorf("expected custom message body, got %q", notif.Body)
	}
	if notif.Level != pushward.LevelPassive { // OK
		t.Errorf("expected passive for OK, got %s", notif.Level)
	}
}

func TestOverrideChannelsNotificationFallsBackToOneShot(t *testing.T) {
	h, calls, mu := newHandler(t)
	sendTo(t, h, "/komodo?channels=notification", bodyUnreachable)

	recorded := testutil.GetCalls(calls, mu)
	if n := countActivityCalls(recorded); n != 0 {
		t.Fatalf("expected no activity calls with channels=notification, got %d", n)
	}
	if n := testutil.CountPath(recorded, "/notifications"); n != 1 {
		t.Fatalf("expected 1 fallback notification, got %d", n)
	}
	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &notif)
	if notif.Level != pushward.LevelActive {
		t.Errorf("expected active level, got %s", notif.Level)
	}
}

func TestOverrideChannelsNotificationResolveClearsDedup(t *testing.T) {
	h, calls, mu := newHandler(t)
	// down -> up -> down again: the resolve must clear the dedup row even
	// though ScheduleEnd is suppressed, so the second outage still notifies.
	sendTo(t, h, "/komodo?channels=notification", bodyUnreachable)
	sendTo(t, h, "/komodo?channels=notification", bodyUnreachableResolved)
	sendTo(t, h, "/komodo?channels=notification", bodyUnreachable)

	recorded := testutil.GetCalls(calls, mu)
	if n := countActivityCalls(recorded); n != 0 {
		t.Fatalf("expected no activity calls with channels=notification, got %d", n)
	}
	if n := testutil.CountPath(recorded, "/notifications"); n != 3 {
		t.Fatalf("expected 3 notifications (down, resolved, down again), got %d", n)
	}
}

func TestOverrideChannelsActivitySuppressesNotifications(t *testing.T) {
	h, calls, mu := newHandler(t)
	sendTo(t, h, "/komodo?channels=activity", bodyUnreachable)
	sendTo(t, h, "/komodo?channels=activity", bodyUnreachableResolved)
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if n := testutil.CountPath(recorded, "/notifications"); n != 0 {
		t.Fatalf("expected no notifications on new or resolved path with channels=activity, got %d", n)
	}
	// create + ONGOING (new) + phase1 ONGOING + phase2 ENDED (resolve) = 4
	if n := countActivityCalls(recorded); n != 4 {
		t.Fatalf("expected 4 activity calls, got %d", n)
	}
}

func TestOverridePriorityReachesCreateActivity(t *testing.T) {
	h, calls, mu := newHandler(t)
	sendTo(t, h, "/komodo?priority=2", bodyUnreachable)

	recorded := testutil.GetCalls(calls, mu)
	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if create.Priority != 2 {
		t.Errorf("expected priority 2 (config default is 5), got %d", create.Priority)
	}
}

func TestOverrideLevelReachesSendNotification(t *testing.T) {
	h, calls, mu := newHandler(t)
	sendTo(t, h, "/komodo?level=critical", bodyUnreachable)

	recorded := testutil.GetCalls(calls, mu)
	var notif pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &notif)
	if notif.Level != pushward.LevelCritical {
		t.Errorf("expected level override critical, got %s", notif.Level)
	}
}

func TestOverrideInvalidParamRejected(t *testing.T) {
	h, _, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/komodo?priority=99", strings.NewReader(bodyUnreachable))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for out-of-range priority, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestKomodoTestSelfTest(t *testing.T) {
	h, calls, mu := newHandler(t)
	send(t, h, bodyTest)

	recorded := testutil.GetCalls(calls, mu)
	// selftest: create + update = 2
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls for selftest, got %d", len(recorded))
	}
	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if create.Slug != "relay-test-komodo" {
		t.Errorf("expected relay-test-komodo slug, got %s", create.Slug)
	}
}
