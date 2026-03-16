package paperless

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

func testConfig() *config.PaperlessConfig {
	return &config.PaperlessConfig{
		Enabled:        true,
		Priority:       1,
		CleanupDelay:   1 * time.Hour,
		StaleTimeout:   30 * time.Minute,
		EndDelay:       10 * time.Millisecond,
		EndDisplayTime: 10 * time.Millisecond,
	}
}

func newHandler(t *testing.T, cfg *config.PaperlessConfig) (*Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL)
	h := NewHandler(store, pool, cfg)
	return h, calls, mu
}

func send(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/paperless", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	auth.Middleware(h).ServeHTTP(w, req)
	return w
}

func TestDocumentAdded(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"event": "added",
		"doc_id": 42,
		"title": "Invoice from Acme Corp",
		"correspondent": "Acme Corp",
		"document_type": "Invoice",
		"doc_url": "https://paperless.example.com/documents/42/details",
		"filename": "scan_2024.pdf",
		"tags": "finance,receipts"
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
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Slug != "paperless-42" {
		t.Errorf("expected slug paperless-42, got %s", createReq.Slug)
	}
	if createReq.Name != "Invoice from Acme Corp" {
		t.Errorf("expected name 'Invoice from Acme Corp', got %s", createReq.Name)
	}
	if createReq.Priority != 1 {
		t.Errorf("expected priority 1, got %d", createReq.Priority)
	}

	// Verify initial ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.State != "Processed" {
		t.Errorf("expected state 'Processed', got %s", update.Content.State)
	}
	if update.Content.Icon != "doc.text.fill" {
		t.Errorf("expected icon doc.text.fill, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != "#34C759" {
		t.Errorf("expected green color, got %s", update.Content.AccentColor)
	}
	if update.Content.Progress != 1.0 {
		t.Errorf("expected progress 1.0, got %f", update.Content.Progress)
	}
	if update.Content.Subtitle != "Paperless \u00b7 Invoice \u00b7 Acme Corp" {
		t.Errorf("expected subtitle 'Paperless \u00b7 Invoice \u00b7 Acme Corp', got %q", update.Content.Subtitle)
	}
	if update.Content.URL != "https://paperless.example.com/documents/42/details" {
		t.Errorf("expected doc_url in URL, got %s", update.Content.URL)
	}

	// Phase 1: ONGOING with final content
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Processed" {
		t.Errorf("expected state 'Processed', got %s", phase1.Content.State)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Processed" {
		t.Errorf("expected state 'Processed', got %s", phase2.Content.State)
	}
}

func TestDocumentUpdated(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"event": "updated",
		"doc_id": 42,
		"title": "Invoice from Acme Corp (Paid)",
		"correspondent": "Acme Corp",
		"document_type": "Invoice",
		"doc_url": "https://paperless.example.com/documents/42/details",
		"filename": "scan_2024.pdf",
		"tags": "finance,receipts,paid"
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

	// Verify initial ONGOING update has "Updated" state
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.State != "Updated" {
		t.Errorf("expected state 'Updated', got %s", update.Content.State)
	}

	// Phase 2: ENDED with "Updated"
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Updated" {
		t.Errorf("expected state 'Updated', got %s", phase2.Content.State)
	}
}

func TestUnknownEvent(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{"event": "deleted"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 calls for unknown event, got %d", len(recorded))
	}
}
