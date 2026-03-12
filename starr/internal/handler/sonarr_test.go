package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func sendSonarrWebhook(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/sonarr/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleSonarrWebhook(w, req)
	return w
}

func sendSonarrWebhookWithBasicAuth(t *testing.T, h *Handler, payload, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/sonarr/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if username != "" {
		req.SetBasicAuth(username, password)
	}
	w := httptest.NewRecorder()
	h.HandleSonarrWebhook(w, req)
	return w
}

// --- Test: Grab (single episode) ---

func TestSonarrGrab(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendSonarrWebhook(t, h, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 5, "seasonNumber": 2, "title": "Breakage"}],
		"release": {"quality": "1080p", "size": 1500000000},
		"downloadClient": "SABnzbd",
		"downloadId": "abc-123"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(50 * time.Millisecond)

	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 calls (create + update), got %d", len(got))
	}

	// Call 1: create activity
	if got[0].Method != "POST" || got[0].Path != "/activities" {
		t.Errorf("call 0: expected POST /activities, got %s %s", got[0].Method, got[0].Path)
	}
	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, got[0].Body, &create)
	if create.Slug != "sonarr-abc-123" {
		t.Errorf("expected slug sonarr-abc-123, got %s", create.Slug)
	}

	// Call 2: ONGOING update
	if got[1].Method != "PATCH" || got[1].Path != "/activity/sonarr-abc-123" {
		t.Errorf("call 1: expected PATCH /activity/sonarr-abc-123, got %s %s", got[1].Method, got[1].Path)
	}
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &update)
	if update.State != "ONGOING" {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.State != "Grabbed" {
		t.Errorf("expected state Grabbed, got %s", update.Content.State)
	}
	if update.Content.Subtitle != "Breaking Bad - S02E05 · 1080p" {
		t.Errorf("expected subtitle 'Breaking Bad - S02E05 · 1080p', got %q", update.Content.Subtitle)
	}
}

// --- Test: Full Grab + Download happy path ---

func TestSonarrGrabAndDownload(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Grab
	sendSonarrWebhook(t, h, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 5, "seasonNumber": 2, "title": "Breakage"}],
		"release": {"quality": "1080p", "size": 1500000000},
		"downloadClient": "SABnzbd",
		"downloadId": "abc-123"
	}`)

	// Download
	sendSonarrWebhook(t, h, `{
		"eventType": "Download",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 5, "seasonNumber": 2, "title": "Breakage"}],
		"episodeFile": {"relativePath": "Season 02/Breaking.Bad.S02E05.1080p.mkv", "quality": "1080p", "size": 1500000000},
		"isUpgrade": false,
		"downloadClient": "SABnzbd",
		"downloadId": "abc-123"
	}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	got := testutil.GetCalls(calls, mu)
	// create + grabbed + end-phase1 (ONGOING) + end-phase2 (ENDED) = 4
	if len(got) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(got))
	}

	// Phase 1: ONGOING with final state
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[2].Body, &phase1)
	if phase1.State != "ONGOING" {
		t.Errorf("phase 1: expected ONGOING, got %s", phase1.State)
	}
	if phase1.Content.State != "Downloaded" {
		t.Errorf("phase 1: expected Downloaded, got %s", phase1.Content.State)
	}
	if phase1.Content.Progress != 1.0 {
		t.Errorf("phase 1: expected progress 1.0, got %f", phase1.Content.Progress)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[3].Body, &phase2)
	if phase2.State != "ENDED" {
		t.Errorf("phase 2: expected ENDED, got %s", phase2.State)
	}
	if phase2.Content.State != "Downloaded" {
		t.Errorf("phase 2: expected Downloaded, got %s", phase2.Content.State)
	}
}

// --- Test: Grab + Download with isUpgrade ---

func TestSonarrGrabAndDownload_IsUpgrade(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	sendSonarrWebhook(t, h, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 5, "seasonNumber": 2, "title": "Breakage"}],
		"release": {"quality": "2160p", "size": 5000000000},
		"downloadClient": "SABnzbd",
		"downloadId": "upgrade-1"
	}`)

	sendSonarrWebhook(t, h, `{
		"eventType": "Download",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 5, "seasonNumber": 2, "title": "Breakage"}],
		"episodeFile": {"relativePath": "Season 02/Breaking.Bad.S02E05.2160p.mkv", "quality": "2160p", "size": 5000000000},
		"isUpgrade": true,
		"downloadClient": "SABnzbd",
		"downloadId": "upgrade-1"
	}`)

	time.Sleep(100 * time.Millisecond)

	got := testutil.GetCalls(calls, mu)
	if len(got) < 3 {
		t.Fatalf("expected at least 3 calls, got %d", len(got))
	}

	// Find the ENDED call and check its state is "Upgraded"
	var endCall pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[len(got)-1].Body, &endCall)
	if endCall.Content.State != "Upgraded" {
		t.Errorf("expected Upgraded, got %s", endCall.Content.State)
	}
}

// --- Test: Concurrent downloads ---

func TestSonarrConcurrentDownloads(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Two grabs
	sendSonarrWebhook(t, h, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 1, "seasonNumber": 1, "title": "Pilot"}],
		"release": {"quality": "1080p", "size": 1000000000},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-1"
	}`)
	sendSonarrWebhook(t, h, `{
		"eventType": "Grab",
		"series": {"id": 2, "title": "Better Call Saul", "year": 2015},
		"episodes": [{"episodeNumber": 1, "seasonNumber": 1, "title": "Uno"}],
		"release": {"quality": "720p", "size": 800000000},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-2"
	}`)

	time.Sleep(50 * time.Millisecond)

	// Both should have created separate activities
	got := testutil.GetCalls(calls, mu)
	if len(got) != 4 {
		t.Fatalf("expected 4 calls (2x create + 2x update), got %d", len(got))
	}

	slugs := map[string]bool{}
	for _, c := range got {
		if c.Method == "POST" {
			var create pushward.CreateActivityRequest
			testutil.UnmarshalBody(t, c.Body, &create)
			slugs[create.Slug] = true
		}
	}
	if !slugs["sonarr-dl-1"] || !slugs["sonarr-dl-2"] {
		t.Errorf("expected slugs sonarr-dl-1 and sonarr-dl-2, got %v", slugs)
	}

	// Complete both
	sendSonarrWebhook(t, h, `{
		"eventType": "Download",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 1, "seasonNumber": 1, "title": "Pilot"}],
		"episodeFile": {"relativePath": "S01/Breaking.Bad.S01E01.mkv", "quality": "1080p", "size": 1000000000},
		"isUpgrade": false,
		"downloadClient": "SABnzbd",
		"downloadId": "dl-1"
	}`)
	sendSonarrWebhook(t, h, `{
		"eventType": "Download",
		"series": {"id": 2, "title": "Better Call Saul", "year": 2015},
		"episodes": [{"episodeNumber": 1, "seasonNumber": 1, "title": "Uno"}],
		"episodeFile": {"relativePath": "S01/Better.Call.Saul.S01E01.mkv", "quality": "720p", "size": 800000000},
		"isUpgrade": false,
		"downloadClient": "SABnzbd",
		"downloadId": "dl-2"
	}`)

	time.Sleep(100 * time.Millisecond)

	got = testutil.GetCalls(calls, mu)
	// 4 (grabs) + 2x2 (two-phase end each) = 8
	if len(got) != 8 {
		t.Fatalf("expected 8 calls total, got %d", len(got))
	}

	// Verify both ended
	var endedSlugs []string
	for _, c := range got {
		if c.Method == "PATCH" {
			var u pushward.UpdateRequest
			testutil.UnmarshalBody(t, c.Body, &u)
			if u.State == "ENDED" {
				endedSlugs = append(endedSlugs, c.Path)
			}
		}
	}
	if len(endedSlugs) != 2 {
		t.Errorf("expected 2 ENDED calls, got %d", len(endedSlugs))
	}
}

// --- Test: Download without prior Grab ---

func TestSonarrDownloadWithoutGrab(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendSonarrWebhook(t, h, `{
		"eventType": "Download",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 5, "seasonNumber": 2, "title": "Breakage"}],
		"episodeFile": {"relativePath": "Season 02/Breaking.Bad.S02E05.1080p.mkv", "quality": "1080p", "size": 1500000000},
		"isUpgrade": false,
		"downloadClient": "SABnzbd",
		"downloadId": "no-grab-1"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(100 * time.Millisecond)

	got := testutil.GetCalls(calls, mu)
	// create + end-phase1 (ONGOING) + end-phase2 (ENDED) = 3
	if len(got) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(got))
	}

	// Create
	if got[0].Method != "POST" || got[0].Path != "/activities" {
		t.Errorf("call 0: expected POST /activities, got %s %s", got[0].Method, got[0].Path)
	}
	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, got[0].Body, &create)
	if create.Slug != "sonarr-no-grab-1" {
		t.Errorf("expected slug sonarr-no-grab-1, got %s", create.Slug)
	}

	// Phase 2: ENDED with Downloaded
	var endReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[2].Body, &endReq)
	if endReq.State != "ENDED" {
		t.Errorf("expected ENDED, got %s", endReq.State)
	}
	if endReq.Content.State != "Downloaded" {
		t.Errorf("expected Downloaded, got %s", endReq.Content.State)
	}
}

// --- Test: HTTP Basic Auth ---

func TestSonarrBasicAuth_Valid(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.Sonarr.Username = "user"
	cfg.Sonarr.Password = "pass"
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendSonarrWebhookWithBasicAuth(t, h, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Test", "year": 2024},
		"episodes": [{"episodeNumber": 1, "seasonNumber": 1, "title": "Pilot"}],
		"release": {"quality": "1080p", "size": 1000},
		"downloadClient": "SABnzbd",
		"downloadId": "secret-test"
	}`, "user", "pass")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(50 * time.Millisecond)
	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(got))
	}
}

func TestSonarrBasicAuth_Invalid(t *testing.T) {
	srv, _, _ := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.Sonarr.Username = "user"
	cfg.Sonarr.Password = "pass"
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendSonarrWebhookWithBasicAuth(t, h, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Test", "year": 2024},
		"episodes": [{"episodeNumber": 1, "seasonNumber": 1, "title": "Pilot"}],
		"release": {"quality": "1080p", "size": 1000},
		"downloadClient": "SABnzbd",
		"downloadId": "secret-test"
	}`, "user", "wrong")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestSonarrBasicAuth_NoAuthConfigured(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	w := sendSonarrWebhook(t, h, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Test", "year": 2024},
		"episodes": [{"episodeNumber": 1, "seasonNumber": 1, "title": "Pilot"}],
		"release": {"quality": "1080p", "size": 1000},
		"downloadClient": "SABnzbd",
		"downloadId": "no-auth-test"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(50 * time.Millisecond)
	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(got))
	}
}
