package argocd

import (
	"context"
	"encoding/json"
	"io"
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

const testKey = "hlk_test"

func testConfig() *config.ArgoCDConfig {
	return &config.ArgoCDConfig{
		Enabled:        true,
		Priority:       3,
		CleanupDelay:   1 * time.Hour,
		StaleTimeout:   30 * time.Minute,
		EndDelay:       10 * time.Millisecond,
		EndDisplayTime: 10 * time.Millisecond,
	}
}

func setupHandler(t *testing.T, cfg *config.ArgoCDConfig, srvURL string) (*Handler, state.Store) {
	t.Helper()
	store := state.NewMemoryStore()
	pool := client.NewPool(srvURL)
	h := NewHandler(store, pool, cfg)
	return h, store
}

func sendWebhook(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	wrapped := auth.Middleware(h)
	wrapped.ServeHTTP(w, req)
	return w
}

func appExists(t *testing.T, h *Handler, appName string) bool {
	t.Helper()
	_, exists, _ := h.loadApp(context.Background(), testKey, appName)
	return exists
}

// --- Test: Full happy path ---

func TestHappyPath_SyncRunning_SyncSucceeded_Deployed(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	// Step 1: sync-running
	w := sendWebhook(t, h, `{
		"app": "pushward-server",
		"project": "default",
		"event": "sync-running",
		"sync_status": "OutOfSync",
		"health_status": "Healthy",
		"operation_phase": "Running",
		"revision": "abc123",
		"message": "synchronization started"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("sync-running: expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("sync-running: expected 2 calls (create + update), got %d", len(recorded))
	}

	expectedSlug := slugForApp("pushward-server")
	if expectedSlug != "argocd-pushward-server" {
		t.Errorf("expected slug argocd-pushward-server, got %s", expectedSlug)
	}

	// Verify create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Slug != "argocd-pushward-server" {
		t.Errorf("expected slug argocd-pushward-server, got %s", createReq.Slug)
	}
	if createReq.Name != "pushward-server" {
		t.Errorf("expected name pushward-server, got %s", createReq.Name)
	}
	if createReq.Priority != 3 {
		t.Errorf("expected priority 3, got %d", createReq.Priority)
	}

	// Verify step 1/3 update
	var update1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update1)
	if update1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update1.State)
	}
	if update1.Content.Template != "pipeline" {
		t.Errorf("expected template pipeline, got %s", update1.Content.Template)
	}
	if update1.Content.State != "Syncing..." {
		t.Errorf("expected state 'Syncing...', got %s", update1.Content.State)
	}
	if update1.Content.Icon != "arrow.triangle.2.circlepath" {
		t.Errorf("expected sync icon, got %s", update1.Content.Icon)
	}
	if update1.Content.AccentColor != "#007AFF" {
		t.Errorf("expected blue color, got %s", update1.Content.AccentColor)
	}
	if update1.Content.Subtitle != "ArgoCD \u00b7 pushward-server" {
		t.Errorf("expected subtitle 'ArgoCD · pushward-server', got %s", update1.Content.Subtitle)
	}
	if update1.Content.CurrentStep == nil || *update1.Content.CurrentStep != 1 {
		t.Errorf("expected current_step 1, got %v", update1.Content.CurrentStep)
	}
	if update1.Content.TotalSteps == nil || *update1.Content.TotalSteps != 3 {
		t.Errorf("expected total_steps 3, got %v", update1.Content.TotalSteps)
	}

	// Step 2: sync-succeeded
	w = sendWebhook(t, h, `{
		"app": "pushward-server",
		"project": "default",
		"event": "sync-succeeded",
		"sync_status": "Synced",
		"health_status": "Progressing",
		"revision": "abc123",
		"message": "sync completed"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("sync-succeeded: expected 200, got %d", w.Code)
	}

	recorded = testutil.GetCalls(calls, mu)
	if len(recorded) != 3 {
		t.Fatalf("sync-succeeded: expected 3 calls, got %d", len(recorded))
	}

	// Verify step 2/3 update (no create)
	if recorded[2].Method != "PATCH" {
		t.Errorf("expected PATCH, got %s", recorded[2].Method)
	}
	var update2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update2)
	if update2.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update2.State)
	}
	if update2.Content.State != "Rolling out..." {
		t.Errorf("expected state 'Rolling out...', got %s", update2.Content.State)
	}
	if update2.Content.CurrentStep == nil || *update2.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", update2.Content.CurrentStep)
	}

	// Step 3: deployed (schedules async two-phase end)
	w = sendWebhook(t, h, `{
		"app": "pushward-server",
		"project": "default",
		"event": "deployed",
		"sync_status": "Synced",
		"health_status": "Healthy",
		"revision": "abc123",
		"message": "application deployed"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("deployed: expected 200, got %d", w.Code)
	}

	// Wait for two-phase end (EndDelay + EndDisplayTime)
	time.Sleep(100 * time.Millisecond)

	recorded = testutil.GetCalls(calls, mu)
	// create + step1 + step2 + phase1(ONGOING) + phase2(ENDED) = 5
	if len(recorded) != 5 {
		t.Fatalf("deployed: expected 5 calls, got %d", len(recorded))
	}

	// Phase 1: ONGOING with "Deployed" content
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Deployed" {
		t.Errorf("expected state 'Deployed', got %s", phase1.Content.State)
	}

	// Phase 2: ENDED with "Deployed" content
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Deployed" {
		t.Errorf("expected state 'Deployed', got %s", phase2.Content.State)
	}
	if phase2.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected checkmark icon, got %s", phase2.Content.Icon)
	}
	if phase2.Content.AccentColor != "#34C759" {
		t.Errorf("expected green color, got %s", phase2.Content.AccentColor)
	}
	if phase2.Content.CurrentStep == nil || *phase2.Content.CurrentStep != 3 {
		t.Errorf("expected current_step 3, got %v", phase2.Content.CurrentStep)
	}
	if phase2.Content.Progress != 1.0 {
		t.Errorf("expected progress 1.0, got %f", phase2.Content.Progress)
	}
}

// --- Test: Sync failure ---

func TestSyncRunning_ThenSyncFailed(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	// Start sync
	sendWebhook(t, h, `{"app":"my-app","event":"sync-running","revision":"rev1"}`)

	// Fail
	w := sendWebhook(t, h, `{"app":"my-app","event":"sync-failed","revision":"rev1","message":"sync error"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + step1 + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	var failReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &failReq)
	if failReq.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", failReq.State)
	}
	if failReq.Content.State != "Sync Failed" {
		t.Errorf("expected 'Sync Failed', got %s", failReq.Content.State)
	}
	if failReq.Content.Icon != "xmark.circle.fill" {
		t.Errorf("expected xmark icon, got %s", failReq.Content.Icon)
	}
	if failReq.Content.AccentColor != "#FF3B30" {
		t.Errorf("expected red color, got %s", failReq.Content.AccentColor)
	}
	// sync-running set step=1, sync-failed should use step 1
	if failReq.Content.CurrentStep == nil || *failReq.Content.CurrentStep != 1 {
		t.Errorf("expected current_step 1, got %v", failReq.Content.CurrentStep)
	}
}

// --- Test: Health degraded after sync succeeded ---

func TestSyncSucceeded_ThenHealthDegraded_ThenDeployed(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	sendWebhook(t, h, `{"app":"web-app","event":"sync-running","revision":"rev1"}`)
	sendWebhook(t, h, `{"app":"web-app","event":"sync-succeeded","revision":"rev1"}`)

	w := sendWebhook(t, h, `{"app":"web-app","event":"health-degraded","revision":"rev1","health_status":"Degraded"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// create + step1 + step2 + degraded(ONGOING) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls after degraded, got %d", len(recorded))
	}

	// Degraded should be ONGOING (transient warning), not ENDED
	var degradedReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &degradedReq)
	if degradedReq.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (transient warning), got %s", degradedReq.State)
	}
	if degradedReq.Content.State != "Degraded" {
		t.Errorf("expected 'Degraded', got %s", degradedReq.Content.State)
	}
	if degradedReq.Content.Icon != "exclamationmark.triangle.fill" {
		t.Errorf("expected warning icon, got %s", degradedReq.Content.Icon)
	}
	if degradedReq.Content.AccentColor != "#FF9500" {
		t.Errorf("expected orange color, got %s", degradedReq.Content.AccentColor)
	}
	if degradedReq.Content.CurrentStep == nil || *degradedReq.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", degradedReq.Content.CurrentStep)
	}

	// App should still be tracked
	if !appExists(t, h, "web-app") {
		t.Fatal("expected app to still be tracked after transient degraded")
	}

	// deployed arrives — should recover to 100% Deployed
	w = sendWebhook(t, h, `{"app":"web-app","event":"deployed","revision":"rev1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("deployed: expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded = testutil.GetCalls(calls, mu)
	// create + step1 + step2 + degraded(ONGOING) + phase1(ONGOING Deployed) + phase2(ENDED Deployed) = 6
	if len(recorded) != 6 {
		t.Fatalf("expected 6 calls total, got %d", len(recorded))
	}

	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[5].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", phase2.State)
	}
	if phase2.Content.State != "Deployed" {
		t.Errorf("expected 'Deployed', got %s", phase2.Content.State)
	}
	if phase2.Content.CurrentStep == nil || *phase2.Content.CurrentStep != 3 {
		t.Errorf("expected current_step 3, got %v", phase2.Content.CurrentStep)
	}
	if phase2.Content.Progress != 1.0 {
		t.Errorf("expected progress 1.0, got %f", phase2.Content.Progress)
	}
}

// --- Test: Untracked events (bridge restart) ---

func TestUntracked_SyncSucceeded(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	// No prior sync-running — bridge restarted
	w := sendWebhook(t, h, `{"app":"my-app","event":"sync-succeeded","revision":"rev1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// create + step2 update = 2
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}
	if recorded[0].Method != "POST" {
		t.Errorf("expected POST create, got %s", recorded[0].Method)
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Rolling out..." {
		t.Errorf("expected 'Rolling out...', got %s", update.Content.State)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", update.Content.CurrentStep)
	}
}

func TestUntracked_Deployed(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	w := sendWebhook(t, h, `{"app":"my-app","event":"deployed","revision":"rev1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + phase1(ONGOING) + phase2(ENDED) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}
	if recorded[0].Method != "POST" {
		t.Errorf("expected POST create, got %s", recorded[0].Method)
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update)
	if update.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", update.State)
	}
	if update.Content.State != "Deployed" {
		t.Errorf("expected 'Deployed', got %s", update.Content.State)
	}
}

func TestUntracked_SyncFailed(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	w := sendWebhook(t, h, `{"app":"my-app","event":"sync-failed","revision":"rev1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + phase1(ONGOING) + phase2(ENDED) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}
	if recorded[0].Method != "POST" {
		t.Errorf("expected POST create, got %s", recorded[0].Method)
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update)
	if update.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", update.State)
	}
	if update.Content.State != "Sync Failed" {
		t.Errorf("expected 'Sync Failed', got %s", update.Content.State)
	}
}

func TestUntracked_HealthDegraded(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	w := sendWebhook(t, h, `{"app":"my-app","event":"health-degraded","revision":"rev1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + phase1(ONGOING) + phase2(ENDED) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update)
	if update.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", update.State)
	}
	if update.Content.State != "Degraded" {
		t.Errorf("expected 'Degraded', got %s", update.Content.State)
	}
}

// --- Test: Re-sync same revision ---

func TestResyncSameRevision_SkipsCreate(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	// First sync-running
	sendWebhook(t, h, `{"app":"my-app","event":"sync-running","revision":"rev1"}`)

	// Re-fire sync-running with same revision
	sendWebhook(t, h, `{"app":"my-app","event":"sync-running","revision":"rev1"}`)

	recorded := testutil.GetCalls(calls, mu)
	// First: create + update. Second: update only (no create) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}
	if recorded[0].Method != "POST" {
		t.Errorf("first call should be POST, got %s", recorded[0].Method)
	}
	if recorded[1].Method != "PATCH" {
		t.Errorf("second call should be PATCH, got %s", recorded[1].Method)
	}
	if recorded[2].Method != "PATCH" {
		t.Errorf("third call should be PATCH (no create), got %s", recorded[2].Method)
	}
}

// --- Test: New revision during tracked sync ---

func TestNewRevision_ResetsTracking(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	// Start sync with rev1
	sendWebhook(t, h, `{"app":"my-app","event":"sync-running","revision":"rev1"}`)

	// New sync with rev2
	sendWebhook(t, h, `{"app":"my-app","event":"sync-running","revision":"rev2"}`)

	recorded := testutil.GetCalls(calls, mu)
	// First: create + update. Second: create + update = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}
	// Both should be POST (create) for different revisions
	if recorded[0].Method != "POST" {
		t.Errorf("first call should be POST, got %s", recorded[0].Method)
	}
	if recorded[2].Method != "POST" {
		t.Errorf("third call should be POST (new revision create), got %s", recorded[2].Method)
	}
}

// --- Test: Method not allowed ---

func TestMethodNotAllowed(t *testing.T) {
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, "http://unused")

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/webhook", nil)
			req.Header.Set("Authorization", "Bearer "+testKey)
			w := httptest.NewRecorder()
			auth.Middleware(h).ServeHTTP(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("expected 405 for %s, got %d", method, w.Code)
			}
		})
	}
}

// --- Test: Invalid JSON ---

func TestInvalidJSON(t *testing.T) {
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, "http://unused")

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("not json"))
	req.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	auth.Middleware(h).ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// --- Test: Missing app or event ---

func TestMissingApp(t *testing.T) {
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, "http://unused")

	w := sendWebhook(t, h, `{"event":"sync-running","revision":"r1"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestMissingEvent(t *testing.T) {
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, "http://unused")

	w := sendWebhook(t, h, `{"app":"my-app","revision":"r1"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// --- Test: Stale timer ---

func TestCreateActivity_IncludesTTLValues(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	sendWebhook(t, h, `{"app":"ttl-app","event":"sync-running","revision":"r1"}`)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 1 {
		t.Fatalf("expected at least 1 call, got %d", len(recorded))
	}

	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)

	expectedEndedTTL := int(cfg.CleanupDelay.Seconds())
	expectedStaleTTL := int(cfg.StaleTimeout.Seconds())

	if createReq.EndedTTL != expectedEndedTTL {
		t.Errorf("expected ended_ttl %d, got %d", expectedEndedTTL, createReq.EndedTTL)
	}
	if createReq.StaleTTL != expectedStaleTTL {
		t.Errorf("expected stale_ttl %d, got %d", expectedStaleTTL, createReq.StaleTTL)
	}
}

// --- Test: Cleanup timer ---

func TestCleanupAfterEnd_RemovesFromStore(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	sendWebhook(t, h, `{"app":"cleanup-app","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"cleanup-app","event":"deployed","revision":"r1"}`)

	// Wait for two-phase end (EndDelay + EndDisplayTime)
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + step1 + phase1(ONGOING) + phase2(ENDED) = 4
	// No DELETE call — server handles cleanup via ended_ttl
	for _, c := range recorded {
		if c.Method == "DELETE" {
			t.Error("unexpected DELETE call — server handles cleanup via ended_ttl")
		}
	}

	// App should be removed from store immediately after ENDED
	if appExists(t, h, "cleanup-app") {
		t.Error("expected app to be removed from store after ENDED")
	}
}

// --- Test: New sync cancels pending cleanup ---

func TestNewSync_CancelsPendingEnd(t *testing.T) {
	srv, _, _ := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.EndDelay = 100 * time.Millisecond
	cfg.EndDisplayTime = 100 * time.Millisecond
	h, _ := setupHandler(t, cfg, srv.URL)

	// Sync and deploy (starts endTimer)
	sendWebhook(t, h, `{"app":"flap-app","event":"sync-running","revision":"rev1"}`)
	sendWebhook(t, h, `{"app":"flap-app","event":"deployed","revision":"rev1"}`)

	// Immediately start new sync (should cancel endTimer via new revision reset)
	sendWebhook(t, h, `{"app":"flap-app","event":"sync-running","revision":"rev2"}`)

	// Wait longer than end delay + display time
	time.Sleep(300 * time.Millisecond)

	// App should still exist (endTimer was cancelled by new sync)
	if !appExists(t, h, "flap-app") {
		t.Error("expected app to survive re-sync (endTimer should have been cancelled)")
	}
}

// --- Test: Multiple apps tracked independently ---

func TestMultipleApps_Independent(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	// App 1: sync-running
	sendWebhook(t, h, `{"app":"app-one","event":"sync-running","revision":"r1"}`)

	// App 2: sync-running
	sendWebhook(t, h, `{"app":"app-two","event":"sync-running","revision":"r2"}`)

	// App 1: deployed (schedules async two-phase end)
	sendWebhook(t, h, `{"app":"app-one","event":"deployed","revision":"r1"}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// app-one: create + step1 + phase1(ONGOING) + phase2(ENDED) = 4
	// app-two: create + step1 = 2
	// Total = 6
	if len(recorded) != 6 {
		t.Fatalf("expected 6 calls, got %d", len(recorded))
	}

	// Verify app-two is still tracked
	if !appExists(t, h, "app-two") {
		t.Error("expected app-two to still be tracked")
	}
	if appExists(t, h, "app-one") {
		t.Error("expected app-one to be removed from store after ENDED")
	}
}

// --- Test: Slug sanitization ---

func TestSlugSanitization(t *testing.T) {
	tests := []struct {
		appName      string
		expectedSlug string
	}{
		{"pushward-server", "argocd-pushward-server"},
		{"My App", "argocd-my-app"},
		{"APP_WITH_UNDERSCORES", "argocd-app-with-underscores"},
		{"app.with.dots", "argocd-app-with-dots"},
		{"UPPERCASE", "argocd-uppercase"},
		{"--leading-trailing--", "argocd-leading-trailing"},
		{"multiple---dashes", "argocd-multiple-dashes"},
		{"special!@#chars", "argocd-special-chars"},
		{"simple", "argocd-simple"},
	}

	for _, tt := range tests {
		t.Run(tt.appName, func(t *testing.T) {
			got := slugForApp(tt.appName)
			if got != tt.expectedSlug {
				t.Errorf("slugForApp(%q) = %s, want %s", tt.appName, got, tt.expectedSlug)
			}
		})
	}
}

// --- Test: Unknown event ---

func TestUnknownEvent_Ignored(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	w := sendWebhook(t, h, `{"app":"my-app","event":"some-unknown-event","revision":"r1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Errorf("expected 0 API calls for unknown event, got %d", len(recorded))
	}
}

// --- Test: Sync failed at step 2 preserves step ---

func TestSyncFailed_AtStep2_PreservesStep(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	sendWebhook(t, h, `{"app":"my-app","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"my-app","event":"sync-succeeded","revision":"r1"}`)

	// Fail after sync-succeeded (step 2)
	sendWebhook(t, h, `{"app":"my-app","event":"sync-failed","revision":"r1"}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	lastCall := recorded[len(recorded)-1]
	var failReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, lastCall.Body, &failReq)

	if failReq.Content.CurrentStep == nil || *failReq.Content.CurrentStep != 2 {
		t.Errorf("expected step 2 preserved on failure, got %v", failReq.Content.CurrentStep)
	}
}

// --- Grace period tests ---

func TestGracePeriod_FastSync_Skipped(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SyncGracePeriod = 100 * time.Millisecond
	h, _ := setupHandler(t, cfg, srv.URL)

	// Full sync cycle within grace period — should be skipped as no-op
	sendWebhook(t, h, `{"app":"fast-app","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"fast-app","event":"sync-succeeded","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"fast-app","event":"deployed","revision":"r1"}`)

	// Wait for grace timer to fire (it shouldn't since it was cancelled)
	time.Sleep(200 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls for fast no-op sync, got %d", len(recorded))
	}

	if appExists(t, h, "fast-app") {
		t.Error("expected fast-app to be cleaned up from store")
	}
}

func TestGracePeriod_SlowSync_Created(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SyncGracePeriod = 50 * time.Millisecond
	h, _ := setupHandler(t, cfg, srv.URL)

	sendWebhook(t, h, `{"app":"slow-app","event":"sync-running","revision":"r1"}`)

	// No API calls yet (in grace period)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls during grace, got %d", len(recorded))
	}

	// Wait for grace to expire
	time.Sleep(150 * time.Millisecond)

	recorded = testutil.GetCalls(calls, mu)
	// create + step1 update = 2
	if len(recorded) != 2 {
		t.Fatalf("expected 2 API calls after grace expired, got %d", len(recorded))
	}
	if recorded[0].Method != "POST" {
		t.Errorf("expected POST create, got %s", recorded[0].Method)
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Syncing..." {
		t.Errorf("expected 'Syncing...', got %s", update.Content.State)
	}

	// Complete the sync normally
	sendWebhook(t, h, `{"app":"slow-app","event":"sync-succeeded","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"slow-app","event":"deployed","revision":"r1"}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded = testutil.GetCalls(calls, mu)
	// create + step1 + step2 + phase1(ONGOING) + phase2(ENDED) = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 API calls total, got %d", len(recorded))
	}
}

func TestGracePeriod_SyncSucceededDuringGrace_ExpiresAtStep2(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SyncGracePeriod = 100 * time.Millisecond
	h, _ := setupHandler(t, cfg, srv.URL)

	sendWebhook(t, h, `{"app":"step2-app","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"step2-app","event":"sync-succeeded","revision":"r1"}`)

	// Grace expires with step at 2
	time.Sleep(200 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 API calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Rolling out..." {
		t.Errorf("expected 'Rolling out...', got %s", update.Content.State)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 2 {
		t.Errorf("expected step 2, got %v", update.Content.CurrentStep)
	}
}

func TestGracePeriod_SyncFailed_BypassesGrace(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SyncGracePeriod = 5 * time.Second // long grace to prove bypass
	h, _ := setupHandler(t, cfg, srv.URL)

	sendWebhook(t, h, `{"app":"fail-app","event":"sync-running","revision":"r1"}`)

	// No API calls yet (in grace period)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls during grace, got %d", len(recorded))
	}

	// Sync fails — should bypass grace and create immediately
	sendWebhook(t, h, `{"app":"fail-app","event":"sync-failed","revision":"r1"}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded = testutil.GetCalls(calls, mu)
	// create + phase1(ONGOING) + phase2(ENDED) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 API calls after sync-failed, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update)
	if update.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", update.State)
	}
	if update.Content.State != "Sync Failed" {
		t.Errorf("expected 'Sync Failed', got %s", update.Content.State)
	}
}

func TestGracePeriod_HealthDegraded_BypassesGrace(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SyncGracePeriod = 5 * time.Second
	h, _ := setupHandler(t, cfg, srv.URL)

	sendWebhook(t, h, `{"app":"deg-app","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"deg-app","event":"health-degraded","revision":"r1"}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + phase1(ONGOING) + phase2(ENDED) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 API calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update)
	if update.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", update.State)
	}
	if update.Content.State != "Degraded" {
		t.Errorf("expected 'Degraded', got %s", update.Content.State)
	}
}

func TestGracePeriod_UntrackedDeployed_Recorded(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SyncGracePeriod = 100 * time.Millisecond
	h, _ := setupHandler(t, cfg, srv.URL)

	// Untracked deployed with grace period — recorded but no API calls
	sendWebhook(t, h, `{"app":"already-done","event":"deployed","revision":"r1"}`)

	time.Sleep(200 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls for untracked deployed, got %d", len(recorded))
	}
}

func TestGracePeriod_DeployedBeforeSyncSucceeded_Skipped(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SyncGracePeriod = 100 * time.Millisecond
	h, _ := setupHandler(t, cfg, srv.URL)

	// deployed arrives before sync-succeeded (out-of-order from ArgoCD notifications)
	sendWebhook(t, h, `{"app":"ooo-app","event":"deployed","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"ooo-app","event":"sync-succeeded","revision":"r1"}`)

	// Wait for grace timer (should NOT fire — the sync was detected as no-op)
	time.Sleep(200 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls for out-of-order deployed+sync-succeeded, got %d", len(recorded))
	}

	// App should not be tracked
	if appExists(t, h, "ooo-app") {
		t.Error("expected app to not be tracked after no-op skip")
	}
}

func TestGracePeriod_UntrackedSyncSucceeded_ThenDeployed_Skipped(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SyncGracePeriod = 100 * time.Millisecond
	h, _ := setupHandler(t, cfg, srv.URL)

	// Untracked sync-succeeded starts grace, then deployed during grace — skip
	sendWebhook(t, h, `{"app":"untracked-app","event":"sync-succeeded","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"untracked-app","event":"deployed","revision":"r1"}`)

	time.Sleep(200 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls, got %d", len(recorded))
	}
}

func TestGracePeriod_UntrackedSyncSucceeded_GraceExpires(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SyncGracePeriod = 50 * time.Millisecond
	h, _ := setupHandler(t, cfg, srv.URL)

	// Untracked sync-succeeded with grace — if no deployed arrives, create at step 2
	sendWebhook(t, h, `{"app":"untracked-rolling","event":"sync-succeeded","revision":"r1"}`)

	time.Sleep(150 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + step2 update = 2
	if len(recorded) != 2 {
		t.Fatalf("expected 2 API calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Rolling out..." {
		t.Errorf("expected 'Rolling out...', got %s", update.Content.State)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 2 {
		t.Errorf("expected step 2, got %v", update.Content.CurrentStep)
	}
}

func TestHealthDegraded_AtStep1_StillEnds(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	// sync-running (step 1) → health-degraded: should end immediately (not transient)
	sendWebhook(t, h, `{"app":"step1-app","event":"sync-running","revision":"rev1"}`)
	sendWebhook(t, h, `{"app":"step1-app","event":"health-degraded","revision":"rev1"}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + step1 + phase1(ONGOING Degraded) + phase2(ENDED Degraded) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	var endReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &endReq)
	if endReq.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", endReq.State)
	}
	if endReq.Content.State != "Degraded" {
		t.Errorf("expected 'Degraded', got %s", endReq.Content.State)
	}
	if endReq.Content.CurrentStep == nil || *endReq.Content.CurrentStep != 1 {
		t.Errorf("expected current_step 1, got %v", endReq.Content.CurrentStep)
	}

	// App should be removed from tracking
	if appExists(t, h, "step1-app") {
		t.Error("expected app to be removed from store after ENDED")
	}
}

func TestHealthDegraded_AtStep2_MultipleTimesBeforeDeployed(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	sendWebhook(t, h, `{"app":"multi-deg","event":"sync-running","revision":"rev1"}`)
	sendWebhook(t, h, `{"app":"multi-deg","event":"sync-succeeded","revision":"rev1"}`)

	// Two degraded events at step 2 — both should be transient ONGOING warnings
	sendWebhook(t, h, `{"app":"multi-deg","event":"health-degraded","revision":"rev1"}`)
	sendWebhook(t, h, `{"app":"multi-deg","event":"health-degraded","revision":"rev1"}`)

	recorded := testutil.GetCalls(calls, mu)
	// create + step1 + step2 + degraded1(ONGOING) + degraded2(ONGOING) = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls after two degraded, got %d", len(recorded))
	}

	// Both should be ONGOING
	for _, idx := range []int{3, 4} {
		var req pushward.UpdateRequest
		testutil.UnmarshalBody(t, recorded[idx].Body, &req)
		if req.State != pushward.StateOngoing {
			t.Errorf("call %d: expected ONGOING, got %s", idx, req.State)
		}
		if req.Content.State != "Degraded" {
			t.Errorf("call %d: expected 'Degraded', got %s", idx, req.Content.State)
		}
	}

	// App should still be tracked
	if !appExists(t, h, "multi-deg") {
		t.Fatal("expected app to still be tracked after multiple transient degraded")
	}

	// deployed recovers to 100%
	sendWebhook(t, h, `{"app":"multi-deg","event":"deployed","revision":"rev1"}`)
	time.Sleep(100 * time.Millisecond)

	recorded = testutil.GetCalls(calls, mu)
	// +phase1(ONGOING Deployed) + phase2(ENDED Deployed) = 7
	if len(recorded) != 7 {
		t.Fatalf("expected 7 calls total, got %d", len(recorded))
	}

	var endReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[6].Body, &endReq)
	if endReq.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", endReq.State)
	}
	if endReq.Content.State != "Deployed" {
		t.Errorf("expected 'Deployed', got %s", endReq.Content.State)
	}
	if endReq.Content.CurrentStep == nil || *endReq.Content.CurrentStep != 3 {
		t.Errorf("expected current_step 3, got %v", endReq.Content.CurrentStep)
	}
}

func TestGracePeriod_SyncRunning_DeployedBeforeSyncSucceeded_Skipped(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SyncGracePeriod = 100 * time.Millisecond
	h, _ := setupHandler(t, cfg, srv.URL)

	// sync-running starts grace period, deployed arrives before sync-succeeded
	sendWebhook(t, h, `{"app":"pending-ooo","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"pending-ooo","event":"deployed","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"pending-ooo","event":"sync-succeeded","revision":"r1"}`)

	// Wait for any grace timer to fire (should NOT — entire sync was no-op)
	time.Sleep(300 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls for pending deployed before sync-succeeded, got %d", len(recorded))
	}

	// App should not be tracked
	if appExists(t, h, "pending-ooo") {
		t.Error("expected app to not be tracked after no-op skip")
	}
}

func TestGracePeriod_DeployedThenSyncSucceededThenSyncRunning_Skipped(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SyncGracePeriod = 100 * time.Millisecond
	h, _ := setupHandler(t, cfg, srv.URL)

	// Out-of-order: deployed first, then sync-succeeded, then sync-running arrives late
	sendWebhook(t, h, `{"app":"late-app","event":"deployed","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"late-app","event":"sync-succeeded","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"late-app","event":"sync-running","revision":"r1"}`)

	// Wait for any grace timer to fire
	time.Sleep(300 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls for late sync-running after deploy, got %d", len(recorded))
	}

	if appExists(t, h, "late-app") {
		t.Error("expected app to not be tracked after skipped late sync-running")
	}
}

// --- Helper: mock server that fails a specific HTTP method with 400 ---

func customMockServer(t *testing.T, failMethod string) (*httptest.Server, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	var calls []testutil.APICall
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, testutil.APICall{Method: r.Method, Path: r.URL.Path, Body: json.RawMessage(body)})
		mu.Unlock()
		if r.Method == failMethod {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &mu
}

// --- Test: ContentURLs with ArgoCD URL and RepoURL ---

func TestContentURLs_WithArgoCDURLAndRepoURL(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.URL = "https://argocd.example.com"
	h, _ := setupHandler(t, cfg, srv.URL)

	w := sendWebhook(t, h, `{
		"app": "url-test-app",
		"event": "sync-running",
		"revision": "abc123",
		"repo_url": "https://github.com/org/repo.git"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)

	expectedURL := "https://argocd.example.com/applications/argocd/url-test-app"
	if update.Content.URL != expectedURL {
		t.Errorf("expected URL %s, got %s", expectedURL, update.Content.URL)
	}

	expectedSecondaryURL := "https://github.com/org/repo/commit/abc123"
	if update.Content.SecondaryURL != expectedSecondaryURL {
		t.Errorf("expected SecondaryURL %s, got %s", expectedSecondaryURL, update.Content.SecondaryURL)
	}
}

// --- Test: MaxBytesReader oversized body ---

func TestOversizedBody_ReturnsBadRequest(t *testing.T) {
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, "http://unused")

	oversized := strings.Repeat("a", (1<<20)+1)
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(oversized))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	auth.Middleware(h).ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for oversized body, got %d", w.Code)
	}
}

// --- Test: CreateActivity error paths ---

func TestCreateActivityFails_SyncRunning(t *testing.T) {
	srv, _, _ := customMockServer(t, http.MethodPost)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	w := sendWebhook(t, h, `{"app":"fail-create","event":"sync-running","revision":"r1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if appExists(t, h, "fail-create") {
		t.Error("expected app to be removed from store after create failure")
	}
}

func TestCreateActivityFails_UntrackedSyncSucceeded(t *testing.T) {
	srv, _, _ := customMockServer(t, http.MethodPost)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	w := sendWebhook(t, h, `{"app":"fail-create","event":"sync-succeeded","revision":"r1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if appExists(t, h, "fail-create") {
		t.Error("expected app to be removed from store after create failure")
	}
}

func TestCreateActivityFails_UntrackedDeployed(t *testing.T) {
	srv, _, _ := customMockServer(t, http.MethodPost)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	w := sendWebhook(t, h, `{"app":"fail-create","event":"deployed","revision":"r1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if appExists(t, h, "fail-create") {
		t.Error("expected app to be removed from store after create failure")
	}
}

func TestCreateActivityFails_SyncFailed(t *testing.T) {
	srv, _, _ := customMockServer(t, http.MethodPost)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	w := sendWebhook(t, h, `{"app":"fail-create","event":"sync-failed","revision":"r1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if appExists(t, h, "fail-create") {
		t.Error("expected app to be removed from store after create failure")
	}
}

func TestCreateActivityFails_HealthDegraded(t *testing.T) {
	srv, _, _ := customMockServer(t, http.MethodPost)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	w := sendWebhook(t, h, `{"app":"fail-create","event":"health-degraded","revision":"r1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if appExists(t, h, "fail-create") {
		t.Error("expected app to be removed from store after create failure")
	}
}

func TestCreateActivityFails_PendingSyncFailed(t *testing.T) {
	srv, _, _ := customMockServer(t, http.MethodPost)
	cfg := testConfig()
	cfg.SyncGracePeriod = 5 * time.Second
	h, _ := setupHandler(t, cfg, srv.URL)

	// sync-running enters grace (no API calls)
	sendWebhook(t, h, `{"app":"pending-fail","event":"sync-running","revision":"r1"}`)
	// sync-failed bypasses grace, tries create — fails
	sendWebhook(t, h, `{"app":"pending-fail","event":"sync-failed","revision":"r1"}`)

	if appExists(t, h, "pending-fail") {
		t.Error("expected app to be removed from store after create failure")
	}
}

func TestCreateActivityFails_PendingHealthDegraded(t *testing.T) {
	srv, _, _ := customMockServer(t, http.MethodPost)
	cfg := testConfig()
	cfg.SyncGracePeriod = 5 * time.Second
	h, _ := setupHandler(t, cfg, srv.URL)

	sendWebhook(t, h, `{"app":"pending-deg","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"pending-deg","event":"health-degraded","revision":"r1"}`)

	if appExists(t, h, "pending-deg") {
		t.Error("expected app to be removed from store after create failure")
	}
}

// --- Test: UpdateActivity error paths ---

func TestUpdateActivityFails_SyncRunning(t *testing.T) {
	srv, calls, mu := customMockServer(t, http.MethodPatch)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	w := sendWebhook(t, h, `{"app":"fail-update","event":"sync-running","revision":"r1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls (POST ok, PATCH fail), got %d", len(recorded))
	}

	// App should still be tracked (only create-fail removes from store)
	if !appExists(t, h, "fail-update") {
		t.Error("expected app to still be tracked after update failure")
	}
}

func TestUpdateActivityFails_SyncSucceeded(t *testing.T) {
	srv, calls, mu := customMockServer(t, http.MethodPatch)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	// sync-running: POST(create) ok, PATCH(update) fail
	sendWebhook(t, h, `{"app":"fail-update","event":"sync-running","revision":"r1"}`)
	// sync-succeeded: PATCH(update) fail
	w := sendWebhook(t, h, `{"app":"fail-update","event":"sync-succeeded","revision":"r1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// POST(create) + PATCH(step1 fail) + PATCH(step2 fail) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}
}

func TestTransientHealthDegraded_UpdateFails(t *testing.T) {
	var calls []testutil.APICall
	var mu sync.Mutex
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		callCount++
		n := callCount
		calls = append(calls, testutil.APICall{Method: r.Method, Path: r.URL.Path, Body: json.RawMessage(body)})
		mu.Unlock()
		// 4th call is the transient degraded PATCH — fail it
		if n == 4 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	sendWebhook(t, h, `{"app":"deg-fail","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"deg-fail","event":"sync-succeeded","revision":"r1"}`)
	w := sendWebhook(t, h, `{"app":"deg-fail","event":"health-degraded","revision":"r1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	mu.Lock()
	got := len(calls)
	mu.Unlock()
	// create + step1 + step2 + degraded(fail) = 4
	if got != 4 {
		t.Fatalf("expected 4 calls, got %d", got)
	}
}

// --- Test: scheduleEnd edge cases ---

func TestScheduleEnd_AppNotInStore(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	// Call scheduleEnd for non-existent app — should return immediately
	h.scheduleEnd(testKey, "non-existent", pushward.Content{})

	time.Sleep(50 * time.Millisecond)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Errorf("expected 0 API calls, got %d", len(recorded))
	}
}

func TestScheduleEnd_UpdateFails(t *testing.T) {
	srv, _, _ := customMockServer(t, http.MethodPatch)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	// Create app via sync-running (POST ok, PATCH fail — but app is tracked)
	sendWebhook(t, h, `{"app":"end-fail","event":"sync-running","revision":"r1"}`)
	// Deploy triggers scheduleEnd (PATCH phase1 fail, PATCH phase2 fail)
	sendWebhook(t, h, `{"app":"end-fail","event":"deployed","revision":"r1"}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	// App should be removed from store even when update fails
	if appExists(t, h, "end-fail") {
		t.Error("expected app to be removed from store after end")
	}
}

// --- Test: graceExpired edge cases ---

func TestGraceExpired_AppNotInStore(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	// Call graceExpired for non-existent app — should return immediately
	h.graceExpired(testKey, "non-existent")

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Errorf("expected 0 API calls, got %d", len(recorded))
	}
}

func TestGraceExpired_AppNotPending(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	// Manually add a tracked app that is NOT pending
	ctx := context.Background()
	_ = h.saveApp(ctx, testKey, "not-pending", &trackedAppState{
		Slug:    "argocd-not-pending",
		Step:    1,
		Pending: false,
	})

	h.graceExpired(testKey, "not-pending")

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Errorf("expected 0 API calls for non-pending app, got %d", len(recorded))
	}
}

func TestGraceExpired_DefaultStep(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	h, _ := setupHandler(t, cfg, srv.URL)

	// Manually inject a tracked app at step 5 (safety-net scenario)
	ctx := context.Background()
	_ = h.saveApp(ctx, testKey, "weird-app", &trackedAppState{
		Slug:    "argocd-weird-app",
		Step:    5,
		Pending: true,
	})

	h.graceExpired(testKey, "weird-app")

	time.Sleep(50 * time.Millisecond)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Step 5" {
		t.Errorf("expected 'Step 5', got %s", update.Content.State)
	}
}
