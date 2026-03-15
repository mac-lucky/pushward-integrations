package unmanic

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
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func testConfig() *config.UnmanicConfig {
	return &config.UnmanicConfig{
		Enabled:        true,
		Priority:       1,
		CleanupDelay:   5 * time.Minute,
		StaleTimeout:   30 * time.Minute,
		EndDelay:       10 * time.Millisecond,
		EndDisplayTime: 10 * time.Millisecond,
	}
}

func newHandler(t *testing.T, cfg *config.UnmanicConfig) (*Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	pool := client.NewPool(srv.URL)
	h := NewHandler(pool, cfg)
	return h, calls, mu
}

func send(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/unmanic", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	auth.Middleware(h).ServeHTTP(w, req)
	return w
}

func TestSuccess(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"version": "1.0",
		"title": "Unmanic - Task Completed",
		"message": "Successfully processed: /library/movies/Inception.mkv",
		"type": "success"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + phase1(ONGOING) + phase2(ENDED) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls (create + phase1 + phase2), got %d", len(recorded))
	}

	// Verify create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if !strings.HasPrefix(createReq.Slug, "unmanic-") {
		t.Errorf("expected slug with unmanic- prefix, got %s", createReq.Slug)
	}
	if createReq.Name != "Inception.mkv" {
		t.Errorf("expected name 'Inception.mkv', got %s", createReq.Name)
	}
	if createReq.Priority != 1 {
		t.Errorf("expected priority 1, got %d", createReq.Priority)
	}

	// Phase 1: ONGOING with "Complete"
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &phase1)
	if phase1.State != "ONGOING" {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Complete" {
		t.Errorf("expected state 'Complete', got %s", phase1.Content.State)
	}
	if phase1.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected checkmark icon, got %s", phase1.Content.Icon)
	}
	if phase1.Content.AccentColor != "#34C759" {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}

	// Phase 2: ENDED with "Complete"
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase2)
	if phase2.State != "ENDED" {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Complete" {
		t.Errorf("expected state 'Complete', got %s", phase2.Content.State)
	}
}

func TestFailure(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"version": "1.0",
		"title": "Unmanic - Task Failed",
		"message": "Failed to process: /library/movies/Corrupted.mkv",
		"type": "failure"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + phase1(ONGOING) + phase2(ENDED) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls (create + phase1 + phase2), got %d", len(recorded))
	}

	// Phase 1: ONGOING with "Failed"
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &phase1)
	if phase1.State != "ONGOING" {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Failed" {
		t.Errorf("expected state 'Failed', got %s", phase1.Content.State)
	}
	if phase1.Content.Icon != "xmark.circle.fill" {
		t.Errorf("expected xmark icon, got %s", phase1.Content.Icon)
	}
	if phase1.Content.AccentColor != "#FF3B30" {
		t.Errorf("expected red color, got %s", phase1.Content.AccentColor)
	}

	// Phase 2: ENDED with "Failed"
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase2)
	if phase2.State != "ENDED" {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Failed" {
		t.Errorf("expected state 'Failed', got %s", phase2.Content.State)
	}
}

func TestInfoIgnored(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"version": "1.0",
		"title": "Unmanic - Test",
		"message": "This is a test notification",
		"type": "info"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(50 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 API calls for info type, got %d", len(recorded))
	}
}
