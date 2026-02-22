package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-docker/argocd/internal/config"
	"github.com/mac-lucky/pushward-docker/argocd/internal/pushward"
)

// apiCall records a PushWard API call made by the handler.
type apiCall struct {
	Method string
	Path   string
	Body   json.RawMessage
}

// mockPushWardServer starts an httptest server that records all requests.
func mockPushWardServer(t *testing.T) (*httptest.Server, *[]apiCall, *sync.Mutex) {
	t.Helper()
	var calls []apiCall
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, apiCall{
			Method: r.Method,
			Path:   r.URL.Path,
			Body:   json.RawMessage(body),
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &mu
}

func testConfig() *config.Config {
	return &config.Config{
		PushWard: config.PushWardConfig{
			Priority:        3,
			CleanupDelay:    1 * time.Hour,
			StaleTimeout:    30 * time.Minute,
			SyncGracePeriod: 0, // disabled for existing tests
			EndDelay:        10 * time.Millisecond,
			EndDisplayTime:  10 * time.Millisecond,
		},
	}
}

func getCalls(calls *[]apiCall, mu *sync.Mutex) []apiCall {
	mu.Lock()
	defer mu.Unlock()
	result := make([]apiCall, len(*calls))
	copy(result, *calls)
	return result
}

func unmarshalBody(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("failed to unmarshal body: %v (body: %s)", err, string(raw))
	}
}

func sendWebhook(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)
	return w
}

// --- Test: Full happy path ---

func TestHappyPath_SyncRunning_SyncSucceeded_Deployed(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

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

	recorded := getCalls(calls, mu)
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
	unmarshalBody(t, recorded[0].Body, &createReq)
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
	unmarshalBody(t, recorded[1].Body, &update1)
	if update1.State != "ONGOING" {
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

	recorded = getCalls(calls, mu)
	if len(recorded) != 3 {
		t.Fatalf("sync-succeeded: expected 3 calls, got %d", len(recorded))
	}

	// Verify step 2/3 update (no create)
	if recorded[2].Method != "PATCH" {
		t.Errorf("expected PATCH, got %s", recorded[2].Method)
	}
	var update2 pushward.UpdateRequest
	unmarshalBody(t, recorded[2].Body, &update2)
	if update2.State != "ONGOING" {
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

	recorded = getCalls(calls, mu)
	// create + step1 + step2 + phase1(ONGOING) + phase2(ENDED) = 5
	if len(recorded) != 5 {
		t.Fatalf("deployed: expected 5 calls, got %d", len(recorded))
	}

	// Phase 1: ONGOING with "Deployed" content
	var phase1 pushward.UpdateRequest
	unmarshalBody(t, recorded[3].Body, &phase1)
	if phase1.State != "ONGOING" {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Deployed" {
		t.Errorf("expected state 'Deployed', got %s", phase1.Content.State)
	}

	// Phase 2: ENDED with "Deployed" content
	var phase2 pushward.UpdateRequest
	unmarshalBody(t, recorded[4].Body, &phase2)
	if phase2.State != "ENDED" {
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
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Start sync
	sendWebhook(t, h, `{"app":"my-app","event":"sync-running","revision":"rev1"}`)

	// Fail
	w := sendWebhook(t, h, `{"app":"my-app","event":"sync-failed","revision":"rev1","message":"sync error"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := getCalls(calls, mu)
	// create + step1 + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	var failReq pushward.UpdateRequest
	unmarshalBody(t, recorded[3].Body, &failReq)
	if failReq.State != "ENDED" {
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

func TestSyncSucceeded_ThenHealthDegraded(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	sendWebhook(t, h, `{"app":"web-app","event":"sync-running","revision":"rev1"}`)
	sendWebhook(t, h, `{"app":"web-app","event":"sync-succeeded","revision":"rev1"}`)

	w := sendWebhook(t, h, `{"app":"web-app","event":"health-degraded","revision":"rev1","health_status":"Degraded"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := getCalls(calls, mu)
	// create + step1 + step2 + phase1(ONGOING) + phase2(ENDED) = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(recorded))
	}

	var degradedReq pushward.UpdateRequest
	unmarshalBody(t, recorded[4].Body, &degradedReq)
	if degradedReq.State != "ENDED" {
		t.Errorf("expected ENDED, got %s", degradedReq.State)
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
	// Was at step 2, should end at step 2
	if degradedReq.Content.CurrentStep == nil || *degradedReq.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", degradedReq.Content.CurrentStep)
	}
}

// --- Test: Untracked events (bridge restart) ---

func TestUntracked_SyncSucceeded(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// No prior sync-running — bridge restarted
	w := sendWebhook(t, h, `{"app":"my-app","event":"sync-succeeded","revision":"rev1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := getCalls(calls, mu)
	// create + step2 update = 2
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}
	if recorded[0].Method != "POST" {
		t.Errorf("expected POST create, got %s", recorded[0].Method)
	}

	var update pushward.UpdateRequest
	unmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Rolling out..." {
		t.Errorf("expected 'Rolling out...', got %s", update.Content.State)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", update.Content.CurrentStep)
	}
}

func TestUntracked_Deployed(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendWebhook(t, h, `{"app":"my-app","event":"deployed","revision":"rev1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := getCalls(calls, mu)
	// create + phase1(ONGOING) + phase2(ENDED) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}
	if recorded[0].Method != "POST" {
		t.Errorf("expected POST create, got %s", recorded[0].Method)
	}

	var update pushward.UpdateRequest
	unmarshalBody(t, recorded[2].Body, &update)
	if update.State != "ENDED" {
		t.Errorf("expected ENDED, got %s", update.State)
	}
	if update.Content.State != "Deployed" {
		t.Errorf("expected 'Deployed', got %s", update.Content.State)
	}
}

func TestUntracked_SyncFailed(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendWebhook(t, h, `{"app":"my-app","event":"sync-failed","revision":"rev1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := getCalls(calls, mu)
	// create + phase1(ONGOING) + phase2(ENDED) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}
	if recorded[0].Method != "POST" {
		t.Errorf("expected POST create, got %s", recorded[0].Method)
	}

	var update pushward.UpdateRequest
	unmarshalBody(t, recorded[2].Body, &update)
	if update.State != "ENDED" {
		t.Errorf("expected ENDED, got %s", update.State)
	}
	if update.Content.State != "Sync Failed" {
		t.Errorf("expected 'Sync Failed', got %s", update.Content.State)
	}
}

func TestUntracked_HealthDegraded(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendWebhook(t, h, `{"app":"my-app","event":"health-degraded","revision":"rev1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := getCalls(calls, mu)
	// create + phase1(ONGOING) + phase2(ENDED) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	unmarshalBody(t, recorded[2].Body, &update)
	if update.State != "ENDED" {
		t.Errorf("expected ENDED, got %s", update.State)
	}
	if update.Content.State != "Degraded" {
		t.Errorf("expected 'Degraded', got %s", update.Content.State)
	}
}

// --- Test: Re-sync same revision ---

func TestResyncSameRevision_SkipsCreate(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// First sync-running
	sendWebhook(t, h, `{"app":"my-app","event":"sync-running","revision":"rev1"}`)

	// Re-fire sync-running with same revision
	sendWebhook(t, h, `{"app":"my-app","event":"sync-running","revision":"rev1"}`)

	recorded := getCalls(calls, mu)
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
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Start sync with rev1
	sendWebhook(t, h, `{"app":"my-app","event":"sync-running","revision":"rev1"}`)

	// New sync with rev2
	sendWebhook(t, h, `{"app":"my-app","event":"sync-running","revision":"rev2"}`)

	recorded := getCalls(calls, mu)
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

// --- Test: Webhook secret validation ---

func TestWebhookSecret_Valid(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	cfg.ArgoCD.WebhookSecret = "my-secret"
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"app":"test","event":"sync-running","revision":"r1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Secret", "my-secret")
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	recorded := getCalls(calls, mu)
	if len(recorded) != 2 {
		t.Errorf("expected 2 calls, got %d", len(recorded))
	}
}

func TestWebhookSecret_Invalid(t *testing.T) {
	cfg := testConfig()
	cfg.ArgoCD.WebhookSecret = "my-secret"
	client := pushward.NewClient("http://unused", "hlk_test")
	h := New(client, cfg)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"app":"test","event":"sync-running"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Secret", "wrong-secret")
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestWebhookSecret_MissingWhenRequired(t *testing.T) {
	cfg := testConfig()
	cfg.ArgoCD.WebhookSecret = "my-secret"
	client := pushward.NewClient("http://unused", "hlk_test")
	h := New(client, cfg)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"app":"test","event":"sync-running"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestWebhookSecret_NotConfigured(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	// No secret configured — any request should pass
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendWebhook(t, h, `{"app":"test","event":"sync-running","revision":"r1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	recorded := getCalls(calls, mu)
	if len(recorded) != 2 {
		t.Errorf("expected 2 calls, got %d", len(recorded))
	}
}

// --- Test: Method not allowed ---

func TestMethodNotAllowed(t *testing.T) {
	cfg := testConfig()
	client := pushward.NewClient("http://unused", "hlk_test")
	h := New(client, cfg)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/webhook", nil)
			w := httptest.NewRecorder()
			h.HandleWebhook(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("expected 405 for %s, got %d", method, w.Code)
			}
		})
	}
}

// --- Test: Invalid JSON ---

func TestInvalidJSON(t *testing.T) {
	cfg := testConfig()
	client := pushward.NewClient("http://unused", "hlk_test")
	h := New(client, cfg)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// --- Test: Missing app or event ---

func TestMissingApp(t *testing.T) {
	cfg := testConfig()
	client := pushward.NewClient("http://unused", "hlk_test")
	h := New(client, cfg)

	w := sendWebhook(t, h, `{"event":"sync-running","revision":"r1"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestMissingEvent(t *testing.T) {
	cfg := testConfig()
	client := pushward.NewClient("http://unused", "hlk_test")
	h := New(client, cfg)

	w := sendWebhook(t, h, `{"app":"my-app","revision":"r1"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// --- Test: Stale timer ---

func TestStaleTimer_ForceEnds(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.StaleTimeout = 50 * time.Millisecond
	cfg.PushWard.CleanupDelay = 50 * time.Millisecond
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	sendWebhook(t, h, `{"app":"stale-app","event":"sync-running","revision":"r1"}`)

	// Wait for stale timer + two-phase end + cleanup
	time.Sleep(300 * time.Millisecond)

	recorded := getCalls(calls, mu)
	// create + step1 + phase1(ONGOING stale) + phase2(ENDED stale) + delete = 5
	if len(recorded) < 4 {
		t.Fatalf("expected at least 4 calls (create, update, phase1, phase2), got %d", len(recorded))
	}

	// Find the force-end ENDED call (phase 2)
	var forceEndReq pushward.UpdateRequest
	for _, c := range recorded {
		if c.Method == "PATCH" {
			var req pushward.UpdateRequest
			unmarshalBody(t, c.Body, &req)
			if req.State == "ENDED" && req.Content.State == "Stale sync (auto-ended)" {
				forceEndReq = req
				break
			}
		}
	}
	if forceEndReq.State != "ENDED" {
		t.Error("expected a force-end PATCH with state ENDED and 'Stale sync (auto-ended)'")
	}
	if forceEndReq.Content.Icon != "clock.badge.xmark" {
		t.Errorf("expected stale icon, got %s", forceEndReq.Content.Icon)
	}
	if forceEndReq.Content.AccentColor != "#8E8E93" {
		t.Errorf("expected gray color, got %s", forceEndReq.Content.AccentColor)
	}
}

// --- Test: Cleanup timer ---

func TestCleanupTimer_DeletesActivity(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.CleanupDelay = 50 * time.Millisecond
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	sendWebhook(t, h, `{"app":"cleanup-app","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"cleanup-app","event":"deployed","revision":"r1"}`)

	// Wait for two-phase end (EndDelay + EndDisplayTime) + cleanup
	time.Sleep(250 * time.Millisecond)

	recorded := getCalls(calls, mu)
	// create + step1 + deployed(ENDED) + delete = 4
	found := false
	for _, c := range recorded {
		if c.Method == "DELETE" {
			found = true
			expectedPath := "/activities/argocd-cleanup-app"
			if c.Path != expectedPath {
				t.Errorf("expected DELETE %s, got %s", expectedPath, c.Path)
			}
		}
	}
	if !found {
		t.Error("expected a DELETE call for cleanup")
	}

	// App should be removed from tracked map
	h.mu.Lock()
	_, exists := h.apps["cleanup-app"]
	h.mu.Unlock()
	if exists {
		t.Error("expected app to be cleaned up from map")
	}
}

// --- Test: New sync cancels pending cleanup ---

func TestNewSync_CancelsPendingCleanup(t *testing.T) {
	srv, _, _ := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.CleanupDelay = 100 * time.Millisecond
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Sync and deploy (starts cleanup timer)
	sendWebhook(t, h, `{"app":"flap-app","event":"sync-running","revision":"rev1"}`)
	sendWebhook(t, h, `{"app":"flap-app","event":"deployed","revision":"rev1"}`)

	// Immediately start new sync (should cancel cleanup)
	sendWebhook(t, h, `{"app":"flap-app","event":"sync-running","revision":"rev2"}`)

	// Wait longer than cleanup delay
	time.Sleep(200 * time.Millisecond)

	// App should still exist (cleanup was cancelled)
	h.mu.Lock()
	_, exists := h.apps["flap-app"]
	h.mu.Unlock()
	if !exists {
		t.Error("expected app to survive re-sync (cleanup should have been cancelled)")
	}
}

// --- Test: Multiple apps tracked independently ---

func TestMultipleApps_Independent(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// App 1: sync-running
	sendWebhook(t, h, `{"app":"app-one","event":"sync-running","revision":"r1"}`)

	// App 2: sync-running
	sendWebhook(t, h, `{"app":"app-two","event":"sync-running","revision":"r2"}`)

	// App 1: deployed (schedules async two-phase end)
	sendWebhook(t, h, `{"app":"app-one","event":"deployed","revision":"r1"}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := getCalls(calls, mu)
	// app-one: create + step1 + phase1(ONGOING) + phase2(ENDED) = 4
	// app-two: create + step1 = 2
	// Total = 6
	if len(recorded) != 6 {
		t.Fatalf("expected 6 calls, got %d", len(recorded))
	}

	// Verify app-two is still tracked
	h.mu.Lock()
	_, app2Exists := h.apps["app-two"]
	app1, app1Exists := h.apps["app-one"]
	h.mu.Unlock()
	if !app2Exists {
		t.Error("expected app-two to still be tracked")
	}
	if !app1Exists {
		t.Error("expected app-one to still exist (cleanup timer pending)")
	}
	if app1.step != 3 {
		t.Errorf("expected app-one at step 3, got %d", app1.step)
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
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendWebhook(t, h, `{"app":"my-app","event":"some-unknown-event","revision":"r1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := getCalls(calls, mu)
	if len(recorded) != 0 {
		t.Errorf("expected 0 API calls for unknown event, got %d", len(recorded))
	}
}

// --- Test: Sync failed at step 2 preserves step ---

func TestSyncFailed_AtStep2_PreservesStep(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	sendWebhook(t, h, `{"app":"my-app","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"my-app","event":"sync-succeeded","revision":"r1"}`)

	// Fail after sync-succeeded (step 2)
	sendWebhook(t, h, `{"app":"my-app","event":"sync-failed","revision":"r1"}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := getCalls(calls, mu)
	lastCall := recorded[len(recorded)-1]
	var failReq pushward.UpdateRequest
	unmarshalBody(t, lastCall.Body, &failReq)

	if failReq.Content.CurrentStep == nil || *failReq.Content.CurrentStep != 2 {
		t.Errorf("expected step 2 preserved on failure, got %v", failReq.Content.CurrentStep)
	}
}

// --- Grace period tests ---

func TestGracePeriod_FastSync_Skipped(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.SyncGracePeriod = 100 * time.Millisecond
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Full sync cycle within grace period — should be skipped as no-op
	sendWebhook(t, h, `{"app":"fast-app","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"fast-app","event":"sync-succeeded","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"fast-app","event":"deployed","revision":"r1"}`)

	// Wait for grace timer to fire (it shouldn't since it was cancelled)
	time.Sleep(200 * time.Millisecond)

	recorded := getCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls for fast no-op sync, got %d", len(recorded))
	}

	h.mu.Lock()
	_, exists := h.apps["fast-app"]
	h.mu.Unlock()
	if exists {
		t.Error("expected fast-app to be cleaned up from map")
	}
}

func TestGracePeriod_SlowSync_Created(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.SyncGracePeriod = 50 * time.Millisecond
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	sendWebhook(t, h, `{"app":"slow-app","event":"sync-running","revision":"r1"}`)

	// No API calls yet (in grace period)
	recorded := getCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls during grace, got %d", len(recorded))
	}

	// Wait for grace to expire
	time.Sleep(150 * time.Millisecond)

	recorded = getCalls(calls, mu)
	// create + step1 update = 2
	if len(recorded) != 2 {
		t.Fatalf("expected 2 API calls after grace expired, got %d", len(recorded))
	}
	if recorded[0].Method != "POST" {
		t.Errorf("expected POST create, got %s", recorded[0].Method)
	}

	var update pushward.UpdateRequest
	unmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Syncing..." {
		t.Errorf("expected 'Syncing...', got %s", update.Content.State)
	}

	// Complete the sync normally
	sendWebhook(t, h, `{"app":"slow-app","event":"sync-succeeded","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"slow-app","event":"deployed","revision":"r1"}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded = getCalls(calls, mu)
	// create + step1 + step2 + phase1(ONGOING) + phase2(ENDED) = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 API calls total, got %d", len(recorded))
	}
}

func TestGracePeriod_SyncSucceededDuringGrace_ExpiresAtStep2(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.SyncGracePeriod = 100 * time.Millisecond
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	sendWebhook(t, h, `{"app":"step2-app","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"step2-app","event":"sync-succeeded","revision":"r1"}`)

	// Grace expires with step at 2
	time.Sleep(200 * time.Millisecond)

	recorded := getCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 API calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	unmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Rolling out..." {
		t.Errorf("expected 'Rolling out...', got %s", update.Content.State)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 2 {
		t.Errorf("expected step 2, got %v", update.Content.CurrentStep)
	}
}

func TestGracePeriod_SyncFailed_BypassesGrace(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.SyncGracePeriod = 5 * time.Second // long grace to prove bypass
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	sendWebhook(t, h, `{"app":"fail-app","event":"sync-running","revision":"r1"}`)

	// No API calls yet (in grace period)
	recorded := getCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls during grace, got %d", len(recorded))
	}

	// Sync fails — should bypass grace and create immediately
	sendWebhook(t, h, `{"app":"fail-app","event":"sync-failed","revision":"r1"}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded = getCalls(calls, mu)
	// create + phase1(ONGOING) + phase2(ENDED) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 API calls after sync-failed, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	unmarshalBody(t, recorded[2].Body, &update)
	if update.State != "ENDED" {
		t.Errorf("expected ENDED, got %s", update.State)
	}
	if update.Content.State != "Sync Failed" {
		t.Errorf("expected 'Sync Failed', got %s", update.Content.State)
	}
}

func TestGracePeriod_HealthDegraded_BypassesGrace(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.SyncGracePeriod = 5 * time.Second
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	sendWebhook(t, h, `{"app":"deg-app","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"deg-app","event":"health-degraded","revision":"r1"}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := getCalls(calls, mu)
	// create + phase1(ONGOING) + phase2(ENDED) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 API calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	unmarshalBody(t, recorded[2].Body, &update)
	if update.State != "ENDED" {
		t.Errorf("expected ENDED, got %s", update.State)
	}
	if update.Content.State != "Degraded" {
		t.Errorf("expected 'Degraded', got %s", update.Content.State)
	}
}

func TestGracePeriod_UntrackedDeployed_Recorded(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.SyncGracePeriod = 100 * time.Millisecond
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Untracked deployed with grace period — recorded but no API calls
	sendWebhook(t, h, `{"app":"already-done","event":"deployed","revision":"r1"}`)

	time.Sleep(200 * time.Millisecond)

	recorded := getCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls for untracked deployed, got %d", len(recorded))
	}
}

func TestGracePeriod_DeployedBeforeSyncSucceeded_Skipped(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.SyncGracePeriod = 100 * time.Millisecond
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// deployed arrives before sync-succeeded (out-of-order from ArgoCD notifications)
	sendWebhook(t, h, `{"app":"ooo-app","event":"deployed","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"ooo-app","event":"sync-succeeded","revision":"r1"}`)

	// Wait for grace timer (should NOT fire — the sync was detected as no-op)
	time.Sleep(200 * time.Millisecond)

	recorded := getCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls for out-of-order deployed+sync-succeeded, got %d", len(recorded))
	}

	// App should not be tracked
	h.mu.Lock()
	_, exists := h.apps["ooo-app"]
	h.mu.Unlock()
	if exists {
		t.Error("expected app to not be tracked after no-op skip")
	}
}

func TestGracePeriod_UntrackedSyncSucceeded_ThenDeployed_Skipped(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.SyncGracePeriod = 100 * time.Millisecond
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Untracked sync-succeeded starts grace, then deployed during grace — skip
	sendWebhook(t, h, `{"app":"untracked-app","event":"sync-succeeded","revision":"r1"}`)
	sendWebhook(t, h, `{"app":"untracked-app","event":"deployed","revision":"r1"}`)

	time.Sleep(200 * time.Millisecond)

	recorded := getCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls, got %d", len(recorded))
	}
}

func TestGracePeriod_UntrackedSyncSucceeded_GraceExpires(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.SyncGracePeriod = 50 * time.Millisecond
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Untracked sync-succeeded with grace — if no deployed arrives, create at step 2
	sendWebhook(t, h, `{"app":"untracked-rolling","event":"sync-succeeded","revision":"r1"}`)

	time.Sleep(150 * time.Millisecond)

	recorded := getCalls(calls, mu)
	// create + step2 update = 2
	if len(recorded) != 2 {
		t.Fatalf("expected 2 API calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	unmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Rolling out..." {
		t.Errorf("expected 'Rolling out...', got %s", update.Content.State)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 2 {
		t.Errorf("expected step 2, got %v", update.Content.CurrentStep)
	}
}
