package proxmox

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

func testConfig() *config.ProxmoxConfig {
	return &config.ProxmoxConfig{
		BaseProviderConfig: config.BaseProviderConfig{
			Enabled:        true,
			Priority:       2,
			CleanupDelay:   1 * time.Hour,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
	}
}

func newHandler(t *testing.T, cfg *config.ProxmoxConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
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
	req := httptest.NewRequest(http.MethodPost, "/proxmox", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestBackupLifecycle(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send backup start
	w := send(t, h, `{
		"type": "vzdump",
		"title": "Backup of VM 100 started",
		"message": "Starting backup of VM 100 (pbs-full)",
		"severity": "info",
		"hostname": "pve1"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for async operations from start
	time.Sleep(50 * time.Millisecond)

	// Verify start: create + ONGOING = 2 calls
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls after start, got %d", len(recorded))
	}

	// Verify create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Priority != 2 {
		t.Errorf("expected priority 2, got %d", createReq.Priority)
	}

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.State != "Backing up..." {
		t.Errorf("expected state 'Backing up...', got %s", update.Content.State)
	}
	if update.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", update.Content.Template)
	}
	if update.Content.AccentColor != pushward.ColorBlue {
		t.Errorf("expected blue accent, got %s", update.Content.AccentColor)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 1 {
		t.Errorf("expected current_step 1")
	}
	if update.Content.TotalSteps == nil || *update.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2")
	}

	// Send backup success
	w = send(t, h, `{
		"type": "vzdump",
		"title": "Backup of VM 100 finished",
		"message": "Backup of VM 100 finished successfully",
		"severity": "info",
		"hostname": "pve1"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded = testutil.GetCalls(calls, mu)
	// create + ONGOING + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls total, got %d", len(recorded))
	}

	// Phase 1: ONGOING with final content
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Backup Complete" {
		t.Errorf("expected state 'Backup Complete', got %s", phase1.Content.State)
	}
	if phase1.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green accent, got %s", phase1.Content.AccentColor)
	}
	if phase1.Content.CurrentStep == nil || *phase1.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2")
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Backup Complete" {
		t.Errorf("expected state 'Backup Complete', got %s", phase2.Content.State)
	}
}

func TestBackupFailure(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send backup start
	send(t, h, `{
		"type": "vzdump",
		"title": "Backup of VM 100 started",
		"message": "Starting backup of VM 100 (pbs-full)",
		"severity": "info",
		"hostname": "pve1"
	}`)

	// Wait for start
	time.Sleep(50 * time.Millisecond)

	// Send backup failure
	send(t, h, `{
		"type": "vzdump",
		"title": "Backup of VM 100 failed",
		"message": "Backup of VM 100 failed: connection error",
		"severity": "error",
		"hostname": "pve1"
	}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls total, got %d", len(recorded))
	}

	// Phase 1: ONGOING with failure content
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1)
	if phase1.Content.State != "Backup Failed" {
		t.Errorf("expected state 'Backup Failed', got %s", phase1.Content.State)
	}
	if phase1.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red accent, got %s", phase1.Content.AccentColor)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Backup Failed" {
		t.Errorf("expected state 'Backup Failed', got %s", phase2.Content.State)
	}
}

func TestFencingAlert(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"type": "fencing",
		"title": "Node pve2 fenced",
		"message": "Node pve2 has been fenced by HA manager",
		"severity": "error",
		"hostname": "pve2"
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

	// Verify create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "alert" {
		t.Errorf("expected template 'alert', got %s", update.Content.Template)
	}
	if update.Content.Icon != "exclamationmark.octagon.fill" {
		t.Errorf("expected icon exclamationmark.octagon.fill, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red accent, got %s", update.Content.AccentColor)
	}
	if update.Content.Subtitle != "Proxmox \u00b7 pve2" {
		t.Errorf("expected subtitle 'Proxmox \u00b7 pve2', got %q", update.Content.Subtitle)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestUpdatesNotification(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"type": "package-updates",
		"title": "5 updates available",
		"message": "5 package updates available on pve1",
		"severity": "info",
		"hostname": "pve1"
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

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "alert" {
		t.Errorf("expected template 'alert', got %s", update.Content.Template)
	}
	if update.Content.Icon != "arrow.down.circle" {
		t.Errorf("expected icon arrow.down.circle, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != pushward.ColorBlue {
		t.Errorf("expected blue accent, got %s", update.Content.AccentColor)
	}
	if update.Content.State != "5 updates available" {
		t.Errorf("expected state '5 updates available', got %s", update.Content.State)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestSystemEventSendsTestNotification(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"type": "system",
		"title": "System event",
		"message": "Something happened",
		"severity": "info",
		"hostname": "pve1"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// selftest: create + update = 2 calls
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls for system event (selftest), got %d", len(recorded))
	}

	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if create.Slug != "relay-test-proxmox" {
		t.Errorf("expected slug relay-test-proxmox, got %s", create.Slug)
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "steps" {
		t.Errorf("expected template steps, got %s", update.Content.Template)
	}
}
