package starr

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

func testConfig() *config.StarrConfig {
	return &config.StarrConfig{
		BaseProviderConfig: config.BaseProviderConfig{
			Enabled:        true,
			Priority:       1,
			CleanupDelay:   1 * time.Hour,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
	}
}

func newHandler(t *testing.T, cfg *config.StarrConfig) (*Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL, nil)
	h := NewHandler(store, pool, cfg)
	return h, calls, mu
}

// sendRadarr sends a Radarr webhook through auth middleware with Bearer hlk_test.
func sendRadarr(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/starr/radarr", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	auth.Middleware(h.RadarrHandler()).ServeHTTP(w, req)
	return w
}

// sendRadarrBasicAuth sends a Radarr webhook through auth middleware with
// Basic Auth where password=hlk_test (integration key).
func sendRadarrBasicAuth(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/starr/radarr", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("radarr", "hlk_test")
	w := httptest.NewRecorder()
	auth.Middleware(h.RadarrHandler()).ServeHTTP(w, req)
	return w
}

// sendSonarr sends a Sonarr webhook through auth middleware with Bearer hlk_test.
func sendSonarr(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/starr/sonarr", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	auth.Middleware(h.SonarrHandler()).ServeHTTP(w, req)
	return w
}

// sendSonarrBasicAuth sends a Sonarr webhook through auth middleware with
// Basic Auth where password=hlk_test (integration key).
func sendSonarrBasicAuth(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/starr/sonarr", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("sonarr", "hlk_test")
	w := httptest.NewRecorder()
	auth.Middleware(h.SonarrHandler()).ServeHTTP(w, req)
	return w
}

// ============================================================
// Radarr Tests
// ============================================================

func TestRadarrGrab(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendRadarr(t, h, `{
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
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "steps" {
		t.Errorf("expected template steps, got %s", update.Content.Template)
	}
	if update.Content.State != "Grabbed" {
		t.Errorf("expected state 'Grabbed', got %s", update.Content.State)
	}
	if update.Content.Icon != "arrow.down.circle" {
		t.Errorf("expected icon arrow.down.circle, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != pushward.ColorBlue {
		t.Errorf("expected blue color, got %s", update.Content.AccentColor)
	}
	if !strings.Contains(update.Content.Subtitle, "Inception (2010)") {
		t.Errorf("expected subtitle to contain 'Inception (2010)', got %s", update.Content.Subtitle)
	}
	if !strings.Contains(update.Content.Subtitle, "Bluray-1080p") {
		t.Errorf("expected subtitle to contain 'Bluray-1080p', got %s", update.Content.Subtitle)
	}
}

func TestRadarrGrabAndDownload(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Grab
	sendRadarr(t, h, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_abc123"
	}`)

	// Download
	sendRadarr(t, h, `{
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
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Imported" {
		t.Errorf("expected state 'Imported', got %s", phase1.Content.State)
	}
	if phase1.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected checkmark icon, got %s", phase1.Content.Icon)
	}
	if phase1.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}

	// Phase 2: ENDED with "Imported"
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Imported" {
		t.Errorf("expected state 'Imported', got %s", phase2.Content.State)
	}
}

func TestRadarrGrabAndDownload_IsUpgrade(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	sendRadarr(t, h, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-2160p", "size": 10737418240},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_upgrade1"
	}`)

	sendRadarr(t, h, `{
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

func TestRadarrConcurrentDownloads(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Grab movie 1
	sendRadarr(t, h, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-1"
	}`)

	// Grab movie 2
	sendRadarr(t, h, `{
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
	sendRadarr(t, h, `{
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

	// Movie 2 should still be tracked in state store
	_, stillTracked := h.getTrackedSlug(t.Context(), "hlk_test", "radarr:dl-2")
	if !stillTracked {
		t.Error("expected dl-2 to still be tracked")
	}
}

func TestRadarrDownloadWithoutGrab(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	sendRadarr(t, h, `{
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
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", phase2.State)
	}
	if phase2.Content.State != "Imported" {
		t.Errorf("expected state 'Imported', got %s", phase2.Content.State)
	}
}

func TestRadarrBasicAuth_KeyInPassword(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendRadarrBasicAuth(t, h, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_basic"
	}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}
}

func TestRadarrUnknownEventType(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendRadarr(t, h, `{"eventType": "FooBar"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 calls for unknown event, got %d", len(recorded))
	}
}

// ============================================================
// Sonarr Tests
// ============================================================

func TestSonarrGrab(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendSonarr(t, h, `{
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
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.State != "Grabbed" {
		t.Errorf("expected state Grabbed, got %s", update.Content.State)
	}
	if update.Content.Subtitle != "Breaking Bad - S02E05 · 1080p" {
		t.Errorf("expected subtitle 'Breaking Bad - S02E05 · 1080p', got %q", update.Content.Subtitle)
	}
}

func TestSonarrGrabAndDownload(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Grab
	sendSonarr(t, h, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 5, "seasonNumber": 2, "title": "Breakage"}],
		"release": {"quality": "1080p", "size": 1500000000},
		"downloadClient": "SABnzbd",
		"downloadId": "abc-123"
	}`)

	// Download
	sendSonarr(t, h, `{
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
	if phase1.State != pushward.StateOngoing {
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
	if phase2.State != pushward.StateEnded {
		t.Errorf("phase 2: expected ENDED, got %s", phase2.State)
	}
	if phase2.Content.State != "Downloaded" {
		t.Errorf("phase 2: expected Downloaded, got %s", phase2.Content.State)
	}
}

func TestSonarrGrabAndDownload_IsUpgrade(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	sendSonarr(t, h, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 5, "seasonNumber": 2, "title": "Breakage"}],
		"release": {"quality": "2160p", "size": 5000000000},
		"downloadClient": "SABnzbd",
		"downloadId": "upgrade-1"
	}`)

	sendSonarr(t, h, `{
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

func TestSonarrConcurrentDownloads(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Two grabs
	sendSonarr(t, h, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 1, "seasonNumber": 1, "title": "Pilot"}],
		"release": {"quality": "1080p", "size": 1000000000},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-1"
	}`)
	sendSonarr(t, h, `{
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
	sendSonarr(t, h, `{
		"eventType": "Download",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 1, "seasonNumber": 1, "title": "Pilot"}],
		"episodeFile": {"relativePath": "S01/Breaking.Bad.S01E01.mkv", "quality": "1080p", "size": 1000000000},
		"isUpgrade": false,
		"downloadClient": "SABnzbd",
		"downloadId": "dl-1"
	}`)
	sendSonarr(t, h, `{
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
			if u.State == pushward.StateEnded {
				endedSlugs = append(endedSlugs, c.Path)
			}
		}
	}
	if len(endedSlugs) != 2 {
		t.Errorf("expected 2 ENDED calls, got %d", len(endedSlugs))
	}
}

func TestSonarrDownloadWithoutGrab(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendSonarr(t, h, `{
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
	if endReq.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", endReq.State)
	}
	if endReq.Content.State != "Downloaded" {
		t.Errorf("expected Downloaded, got %s", endReq.Content.State)
	}
}

// ============================================================
// Radarr Test Event
// ============================================================

func TestRadarrTestEvent(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendRadarr(t, h, `{"eventType": "Test"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify dispatch to selftest (content details tested in selftest/provider_test.go)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls (create + update), got %d", len(recorded))
	}

	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if create.Slug != "relay-test-radarr" {
		t.Errorf("expected slug relay-test-radarr, got %s", create.Slug)
	}
}

// ============================================================
// Health / HealthRestored Tests
// ============================================================

func TestRadarrHealth(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendRadarr(t, h, `{
		"eventType": "Health",
		"level": "warning",
		"message": "Indexer NZBgeek is unavailable due to failures",
		"type": "IndexerStatusCheck",
		"wikiUrl": "https://wiki.servarr.com/radarr/system#indexers"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls (create + update), got %d", len(recorded))
	}

	// Create
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "Radarr Health" {
		t.Errorf("expected name 'Radarr Health', got %s", createReq.Name)
	}

	// ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "alert" {
		t.Errorf("expected template alert, got %s", update.Content.Template)
	}
	if update.Content.Severity != "warning" {
		t.Errorf("expected severity warning, got %s", update.Content.Severity)
	}
	if update.Content.Icon != "exclamationmark.triangle.fill" {
		t.Errorf("expected warning icon, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != pushward.ColorOrange {
		t.Errorf("expected orange color, got %s", update.Content.AccentColor)
	}
	if update.Content.URL != "https://wiki.servarr.com/radarr/system#indexers" {
		t.Errorf("expected wiki URL, got %s", update.Content.URL)
	}
}

func TestRadarrHealthError(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendRadarr(t, h, `{
		"eventType": "Health",
		"level": "error",
		"message": "Disk space low",
		"type": "DiskSpaceCheck",
		"wikiUrl": "https://wiki.servarr.com"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.Severity != "critical" {
		t.Errorf("expected severity critical, got %s", update.Content.Severity)
	}
	if update.Content.Icon != "exclamationmark.octagon.fill" {
		t.Errorf("expected error icon, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red color, got %s", update.Content.AccentColor)
	}
}

func TestRadarrHealthAndRestored(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Health
	sendRadarr(t, h, `{
		"eventType": "Health",
		"level": "warning",
		"message": "Indexer NZBgeek is unavailable",
		"type": "IndexerStatusCheck",
		"wikiUrl": "https://wiki.servarr.com"
	}`)

	// HealthRestored
	sendRadarr(t, h, `{
		"eventType": "HealthRestored",
		"level": "warning",
		"message": "Indexer NZBgeek is available again",
		"type": "IndexerStatusCheck",
		"previousLevel": "warning"
	}`)

	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + health_update + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	// Phase 1: ONGOING with restored content
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected checkmark icon, got %s", phase1.Content.Icon)
	}
	if phase1.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestSonarrHealth(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendSonarr(t, h, `{
		"eventType": "Health",
		"level": "error",
		"message": "No indexers available",
		"type": "IndexerRssCheck",
		"wikiUrl": "https://wiki.servarr.com/sonarr"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}

	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "Sonarr Health" {
		t.Errorf("expected name 'Sonarr Health', got %s", createReq.Name)
	}
}

// ============================================================
// ManualInteractionRequired Tests
// ============================================================

func TestRadarrManualInteraction(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Grab first to create a tracked download
	sendRadarr(t, h, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_manual1"
	}`)

	// ManualInteractionRequired
	sendRadarr(t, h, `{
		"eventType": "ManualInteractionRequired",
		"downloadId": "SABnzbd_nzo_manual1",
		"downloadInfo": {
			"quality": "Bluray-1080p",
			"title": "Inception.2010.1080p.BluRay.x264-SPARKS",
			"status": "Warning",
			"statusMessages": [{
				"title": "Inception.2010.1080p.BluRay.x264-SPARKS",
				"messages": ["No files found eligible for import"]
			}]
		}
	}`)

	recorded := testutil.GetCalls(calls, mu)
	// create + grab_update + manual_update = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}

	// Verify manual interaction update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.State != "Import Failed" {
		t.Errorf("expected state 'Import Failed', got %s", update.Content.State)
	}
	if update.Content.Icon != "exclamationmark.triangle.fill" {
		t.Errorf("expected warning icon, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != pushward.ColorOrange {
		t.Errorf("expected orange color, got %s", update.Content.AccentColor)
	}
}

func TestRadarrManualInteractionUntracked(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// ManualInteractionRequired without a prior Grab — should be silently ignored
	w := sendRadarr(t, h, `{
		"eventType": "ManualInteractionRequired",
		"downloadId": "SABnzbd_nzo_untracked",
		"downloadInfo": {
			"quality": "Bluray-1080p",
			"title": "Movie.mkv",
			"status": "Warning",
			"statusMessages": []
		}
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 calls for untracked manual interaction, got %d", len(recorded))
	}
}

func TestRadarrGrabManualInteractionDownload(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Grab
	sendRadarr(t, h, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_full"
	}`)

	// ManualInteractionRequired
	sendRadarr(t, h, `{
		"eventType": "ManualInteractionRequired",
		"downloadId": "SABnzbd_nzo_full",
		"downloadInfo": {
			"quality": "Bluray-1080p",
			"title": "Inception.2010.1080p.BluRay",
			"status": "Warning",
			"statusMessages": [{"title": "test", "messages": ["No files found"]}]
		}
	}`)

	// Download (eventually succeeds)
	sendRadarr(t, h, `{
		"eventType": "Download",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"movieFile": {"relativePath": "Inception.mkv", "quality": "Bluray-1080p", "size": 5368709120},
		"isUpgrade": false,
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_full"
	}`)

	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + grab_update + manual_update + phase1(ONGOING) + phase2(ENDED) = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(recorded))
	}

	// Phase 2 should be ENDED with "Imported"
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", phase2.State)
	}
	if phase2.Content.State != "Imported" {
		t.Errorf("expected 'Imported', got %s", phase2.Content.State)
	}
}

func TestSonarrTestEvent(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendSonarr(t, h, `{"eventType": "Test"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify dispatch to selftest (content details tested in selftest/provider_test.go)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls (create + update), got %d", len(recorded))
	}

	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if create.Slug != "relay-test-sonarr" {
		t.Errorf("expected slug relay-test-sonarr, got %s", create.Slug)
	}
}

func TestSonarrBasicAuth_KeyInPassword(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendSonarrBasicAuth(t, h, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Test", "year": 2024},
		"episodes": [{"episodeNumber": 1, "seasonNumber": 1, "title": "Pilot"}],
		"release": {"quality": "1080p", "size": 1000},
		"downloadClient": "SABnzbd",
		"downloadId": "basic-test"
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
