package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
	"github.com/mac-lucky/pushward-integrations/starr/internal/config"

	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
)

func testConfig() *config.Config {
	return &config.Config{
		PushWard: sharedconfig.PushWardConfig{
			Priority:       1,
			CleanupDelay:   1 * time.Hour,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
	}
}

func sendRadarrWebhook(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/radarr/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleRadarrWebhook(w, req)
	return w
}

func sendRadarrWebhookWithBasicAuth(t *testing.T, h *Handler, payload, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/radarr/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if username != "" {
		req.SetBasicAuth(username, password)
	}
	w := httptest.NewRecorder()
	h.HandleRadarrWebhook(w, req)
	return w
}

// --- Test: Grab ---

func TestRadarrGrab(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendRadarrWebhook(t, h, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120, "indexer": "NZBgeek", "releaseTitle": "Inception.2010.1080p.BluRay"},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_abc123"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls (create + update), got %d", len(recorded))
	}

	// Verify create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Slug != slugForDownload("radarr-", "SABnzbd_nzo_abc123") {
		t.Errorf("expected slug %s, got %s", slugForDownload("radarr-", "SABnzbd_nzo_abc123"), createReq.Slug)
	}
	if createReq.Name != "Inception (2010)" {
		t.Errorf("expected name 'Inception (2010)', got %s", createReq.Name)
	}
	if createReq.Priority != 1 {
		t.Errorf("expected priority 1, got %d", createReq.Priority)
	}

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != "ONGOING" {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "generic" {
		t.Errorf("expected template generic, got %s", update.Content.Template)
	}
	if update.Content.State != "Grabbed" {
		t.Errorf("expected state 'Grabbed', got %s", update.Content.State)
	}
	if update.Content.Icon != "arrow.down.circle" {
		t.Errorf("expected icon arrow.down.circle, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != "#007AFF" {
		t.Errorf("expected blue color, got %s", update.Content.AccentColor)
	}
	if !strings.Contains(update.Content.Subtitle, "Inception (2010)") {
		t.Errorf("expected subtitle to contain 'Inception (2010)', got %s", update.Content.Subtitle)
	}
	if !strings.Contains(update.Content.Subtitle, "Bluray-1080p") {
		t.Errorf("expected subtitle to contain 'Bluray-1080p', got %s", update.Content.Subtitle)
	}
}

// --- Test: Grab + Download ---

func TestRadarrGrabAndDownload(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Grab
	sendRadarrWebhook(t, h, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_abc123"
	}`)

	// Download
	sendRadarrWebhook(t, h, `{
		"eventType": "Download",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"movieFile": {"relativePath": "Inception (2010)/Inception.2010.1080p.BluRay.mkv", "quality": "Bluray-1080p", "size": 5368709120},
		"isUpgrade": false,
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_abc123"
	}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + grab_update + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	// Phase 1: ONGOING with "Imported"
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1)
	if phase1.State != "ONGOING" {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Imported" {
		t.Errorf("expected state 'Imported', got %s", phase1.Content.State)
	}
	if phase1.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected checkmark icon, got %s", phase1.Content.Icon)
	}
	if phase1.Content.AccentColor != "#34C759" {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}

	// Phase 2: ENDED with "Imported"
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != "ENDED" {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Imported" {
		t.Errorf("expected state 'Imported', got %s", phase2.Content.State)
	}
}

// --- Test: Grab + Download with IsUpgrade ---

func TestRadarrGrabAndDownload_IsUpgrade(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	sendRadarrWebhook(t, h, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-2160p", "size": 10737418240},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_upgrade1"
	}`)

	sendRadarrWebhook(t, h, `{
		"eventType": "Download",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"movieFile": {"relativePath": "Inception.2010.2160p.mkv", "quality": "Bluray-2160p", "size": 10737418240},
		"isUpgrade": true,
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_upgrade1"
	}`)

	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1)
	if phase1.Content.State != "Upgraded" {
		t.Errorf("expected state 'Upgraded', got %s", phase1.Content.State)
	}
}

// --- Test: Concurrent downloads ---

func TestRadarrConcurrentDownloads(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Grab movie 1
	sendRadarrWebhook(t, h, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-1"
	}`)

	// Grab movie 2
	sendRadarrWebhook(t, h, `{
		"eventType": "Grab",
		"movie": {"id": 2, "title": "Interstellar", "year": 2014},
		"release": {"quality": "Bluray-2160p", "size": 10737418240},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-2"
	}`)

	recorded := testutil.GetCalls(calls, mu)
	// 2 creates + 2 updates = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	// Verify different slugs were used
	var create1 pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create1)
	var create2 pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &create2)
	if create1.Slug == create2.Slug {
		t.Errorf("expected different slugs, both got %s", create1.Slug)
	}

	// Download movie 1 only
	sendRadarrWebhook(t, h, `{
		"eventType": "Download",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"movieFile": {"relativePath": "Inception.mkv", "quality": "Bluray-1080p", "size": 5368709120},
		"isUpgrade": false,
		"downloadClient": "SABnzbd",
		"downloadId": "dl-1"
	}`)

	time.Sleep(100 * time.Millisecond)

	recorded = testutil.GetCalls(calls, mu)
	// 4 + phase1 + phase2 = 6
	if len(recorded) != 6 {
		t.Fatalf("expected 6 calls, got %d", len(recorded))
	}

	// Movie 2 should still be tracked
	h.mu.Lock()
	_, stillTracked := h.downloads["radarr:dl-2"]
	h.mu.Unlock()
	if !stillTracked {
		t.Error("expected dl-2 to still be tracked")
	}
}

// --- Test: Download without prior Grab ---

func TestRadarrDownloadWithoutGrab(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	sendRadarrWebhook(t, h, `{
		"eventType": "Download",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"movieFile": {"relativePath": "Inception.mkv", "quality": "Bluray-1080p", "size": 5368709120},
		"isUpgrade": false,
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_untracked"
	}`)

	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + phase1(ONGOING) + phase2(ENDED) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}

	// Verify create was called
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}

	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "Inception (2010)" {
		t.Errorf("expected name 'Inception (2010)', got %s", createReq.Name)
	}

	// Phase 2 should be ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase2)
	if phase2.State != "ENDED" {
		t.Errorf("expected ENDED, got %s", phase2.State)
	}
	if phase2.Content.State != "Imported" {
		t.Errorf("expected state 'Imported', got %s", phase2.Content.State)
	}
}

// --- Test: HTTP Basic Auth ---

func TestRadarrBasicAuth_Valid(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.Radarr.Username = "user"
	cfg.Radarr.Password = "pass"
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendRadarrWebhookWithBasicAuth(t, h, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_secret1"
	}`, "user", "pass")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}
}

func TestRadarrBasicAuth_Invalid(t *testing.T) {
	srv, _, _ := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.Radarr.Username = "user"
	cfg.Radarr.Password = "pass"
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendRadarrWebhookWithBasicAuth(t, h, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_secret2"
	}`, "user", "wrong")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRadarrBasicAuth_NoAuthConfigured(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendRadarrWebhook(t, h, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_noauth"
	}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}
}

// --- Test: Unknown event type ---

func TestRadarrUnknownEventType(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendRadarrWebhook(t, h, `{"eventType": "Test"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 calls for unknown event, got %d", len(recorded))
	}
}
