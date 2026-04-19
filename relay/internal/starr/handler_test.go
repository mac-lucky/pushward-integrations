package starr

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

func newTestAPI(t *testing.T, cfg *config.StarrConfig) (http.Handler, *Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL, nil)

	mux, api := humautil.NewTestAPI()
	h := RegisterRoutes(api, store, pool, cfg)

	return mux, h, calls, mu
}

func newHandler(t *testing.T, cfg *config.StarrConfig) (http.Handler, *Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	return newTestAPI(t, cfg)
}

// sendRadarr sends a Radarr webhook with Bearer hlk_test.
func sendRadarr(t *testing.T, mux http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/radarr", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// sendRadarrBasicAuth sends a Radarr webhook with Basic Auth where password=hlk_test.
func sendRadarrBasicAuth(t *testing.T, mux http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/radarr", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("radarr", "hlk_test")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// sendSonarr sends a Sonarr webhook with Bearer hlk_test.
func sendSonarr(t *testing.T, mux http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/sonarr", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// sendSonarrBasicAuth sends a Sonarr webhook with Basic Auth where password=hlk_test.
func sendSonarrBasicAuth(t *testing.T, mux http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/sonarr", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("sonarr", "hlk_test")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// ============================================================
// Radarr Tests
// ============================================================

func TestRadarrGrab(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	w := sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010, "tmdbId": 27205},
		"release": {"quality": "Bluray-1080p", "size": 5368709120, "indexer": "NZBgeek", "releaseTitle": "Inception.2010.1080p.BluRay"},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_abc123"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls (notify + create + update), got %d", len(recorded))
	}

	// Verify notification
	if recorded[0].Method != "POST" || recorded[0].Path != "/notifications" {
		t.Errorf("expected POST /notifications, got %s %s", recorded[0].Method, recorded[0].Path)
	}

	// Verify create
	if recorded[1].Method != "POST" || recorded[1].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[1].Method, recorded[1].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &createReq)
	if createReq.Slug != "radarr-movie-27205" {
		t.Errorf("expected slug radarr-movie-27205, got %s", createReq.Slug)
	}
	if createReq.Name != "Inception (2010)" {
		t.Errorf("expected name 'Inception (2010)', got %s", createReq.Name)
	}
	if createReq.Priority != 1 {
		t.Errorf("expected priority 1, got %d", createReq.Priority)
	}

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update)
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
	mux, _, calls, mu := newHandler(t, testConfig())

	// Grab
	sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_abc123"
	}`)

	// Download
	sendRadarr(t, mux, `{
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
	// grab_notify + create + grab_update + download_notify + phase1(ONGOING) + phase2(ENDED) = 6
	if len(recorded) != 6 {
		t.Fatalf("expected 6 calls, got %d", len(recorded))
	}

	// Phase 1: ONGOING with "Imported"
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &phase1)
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
	testutil.UnmarshalBody(t, recorded[5].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Imported" {
		t.Errorf("expected state 'Imported', got %s", phase2.Content.State)
	}
}

func TestRadarrGrabAndDownload_IsUpgrade(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-2160p", "size": 10737418240},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_upgrade1"
	}`)

	sendRadarr(t, mux, `{
		"eventType": "Download",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"movieFile": {"relativePath": "Inception.2010.2160p.mkv", "quality": "Bluray-2160p", "size": 10737418240},
		"isUpgrade": true,
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_upgrade1"
	}`)

	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 6 {
		t.Fatalf("expected 6 calls, got %d", len(recorded))
	}

	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &phase1)
	if phase1.Content.State != "Upgraded" {
		t.Errorf("expected state 'Upgraded', got %s", phase1.Content.State)
	}
}

func TestRadarrConcurrentDownloads(t *testing.T) {
	mux, h, calls, mu := newHandler(t, testConfig())

	// Grab movie 1
	sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-1"
	}`)

	// Grab movie 2
	sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 2, "title": "Interstellar", "year": 2014},
		"release": {"quality": "Bluray-2160p", "size": 10737418240},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-2"
	}`)

	recorded := testutil.GetCalls(calls, mu)
	// 2 * (notify + create + update) = 6
	if len(recorded) != 6 {
		t.Fatalf("expected 6 calls, got %d", len(recorded))
	}

	// Verify different slugs were used (creates are at index 1 and 4)
	var create1 pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &create1)
	var create2 pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &create2)
	if create1.Slug == create2.Slug {
		t.Errorf("expected different slugs, both got %s", create1.Slug)
	}

	// Download movie 1 only
	sendRadarr(t, mux, `{
		"eventType": "Download",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"movieFile": {"relativePath": "Inception.mkv", "quality": "Bluray-1080p", "size": 5368709120},
		"isUpgrade": false,
		"downloadClient": "SABnzbd",
		"downloadId": "dl-1"
	}`)

	time.Sleep(100 * time.Millisecond)

	recorded = testutil.GetCalls(calls, mu)
	// 6 + download_notify + phase1 + phase2 = 9
	if len(recorded) != 9 {
		t.Fatalf("expected 9 calls, got %d", len(recorded))
	}

	// Movie 2 (internal id=2, no tmdbId) should still be tracked in state store
	_, stillTracked := h.getTrackedSlug(t.Context(), "hlk_test", "radarr:movie:id:2")
	if !stillTracked {
		t.Error("expected movie 2 to still be tracked")
	}
}

func TestRadarrDownloadWithoutGrab(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	sendRadarr(t, mux, `{
		"eventType": "Download",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"movieFile": {"relativePath": "Inception.mkv", "quality": "Bluray-1080p", "size": 5368709120},
		"isUpgrade": false,
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_untracked"
	}`)

	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// notify + create + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	// Verify notification
	if recorded[0].Method != "POST" || recorded[0].Path != "/notifications" {
		t.Errorf("expected POST /notifications, got %s %s", recorded[0].Method, recorded[0].Path)
	}

	// Verify create was called
	if recorded[1].Method != "POST" || recorded[1].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[1].Method, recorded[1].Path)
	}

	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &createReq)
	if createReq.Name != "Inception (2010)" {
		t.Errorf("expected name 'Inception (2010)', got %s", createReq.Name)
	}

	// Phase 2 should be ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", phase2.State)
	}
	if phase2.Content.State != "Imported" {
		t.Errorf("expected state 'Imported', got %s", phase2.Content.State)
	}
}

func TestRadarrBasicAuth_KeyInPassword(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	w := sendRadarrBasicAuth(t, mux, `{
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
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls (notify + create + update), got %d", len(recorded))
	}
}

func TestRadarrUnknownEventType(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	w := sendRadarr(t, mux, `{"eventType": "FooBar"}`)
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
	mux, _, calls, mu := newHandler(t, testConfig())

	w := sendSonarr(t, mux, `{
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
	if len(got) != 3 {
		t.Fatalf("expected 3 calls (notify + create + update), got %d", len(got))
	}

	// Call 1: notification
	if got[0].Method != "POST" || got[0].Path != "/notifications" {
		t.Errorf("call 0: expected POST /notifications, got %s %s", got[0].Method, got[0].Path)
	}

	// Call 2: create activity
	if got[1].Method != "POST" || got[1].Path != "/activities" {
		t.Errorf("call 1: expected POST /activities, got %s %s", got[1].Method, got[1].Path)
	}
	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, got[1].Body, &create)
	// No tvdbId in payload → series-id fallback; no episode id → series-level slug.
	if create.Slug != "sonarr-series-id-1" {
		t.Errorf("expected slug sonarr-series-id-1, got %s", create.Slug)
	}

	// Call 3: ONGOING update
	if got[2].Method != "PATCH" || got[2].Path != "/activity/sonarr-series-id-1" {
		t.Errorf("call 2: expected PATCH /activity/sonarr-series-id-1, got %s %s", got[2].Method, got[2].Path)
	}
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[2].Body, &update)
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
	mux, _, calls, mu := newHandler(t, testConfig())

	// Grab
	sendSonarr(t, mux, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 5, "seasonNumber": 2, "title": "Breakage"}],
		"release": {"quality": "1080p", "size": 1500000000},
		"downloadClient": "SABnzbd",
		"downloadId": "abc-123"
	}`)

	// Download
	sendSonarr(t, mux, `{
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
	// grab_notify + create + grabbed + download_notify + end-phase1 (ONGOING) + end-phase2 (ENDED) = 6
	if len(got) != 6 {
		t.Fatalf("expected 6 calls, got %d", len(got))
	}

	// Phase 1: ONGOING with final state
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[4].Body, &phase1)
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
	testutil.UnmarshalBody(t, got[5].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("phase 2: expected ENDED, got %s", phase2.State)
	}
	if phase2.Content.State != "Downloaded" {
		t.Errorf("phase 2: expected Downloaded, got %s", phase2.Content.State)
	}
}

func TestSonarrGrabAndDownload_IsUpgrade(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	sendSonarr(t, mux, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 5, "seasonNumber": 2, "title": "Breakage"}],
		"release": {"quality": "2160p", "size": 5000000000},
		"downloadClient": "SABnzbd",
		"downloadId": "upgrade-1"
	}`)

	sendSonarr(t, mux, `{
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
	if len(got) < 5 {
		t.Fatalf("expected at least 5 calls, got %d", len(got))
	}

	// Find the ENDED call and check its state is "Upgraded"
	var endCall pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[len(got)-1].Body, &endCall)
	if endCall.Content.State != "Upgraded" {
		t.Errorf("expected Upgraded, got %s", endCall.Content.State)
	}
}

func TestSonarrConcurrentDownloads(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	// Two grabs
	sendSonarr(t, mux, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 1, "seasonNumber": 1, "title": "Pilot"}],
		"release": {"quality": "1080p", "size": 1000000000},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-1"
	}`)
	sendSonarr(t, mux, `{
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
	// 2 * (notify + create + update) = 6
	if len(got) != 6 {
		t.Fatalf("expected 6 calls (2x (notify + create + update)), got %d", len(got))
	}

	slugs := map[string]bool{}
	for _, c := range got {
		if c.Method == "POST" && c.Path == "/activities" {
			var create pushward.CreateActivityRequest
			testutil.UnmarshalBody(t, c.Body, &create)
			slugs[create.Slug] = true
		}
	}
	// Content-keyed: no tvdbId → series-id fallback per series.
	if !slugs["sonarr-series-id-1"] || !slugs["sonarr-series-id-2"] {
		t.Errorf("expected slugs sonarr-series-id-1 and sonarr-series-id-2, got %v", slugs)
	}

	// Complete both
	sendSonarr(t, mux, `{
		"eventType": "Download",
		"series": {"id": 1, "title": "Breaking Bad", "year": 2008},
		"episodes": [{"episodeNumber": 1, "seasonNumber": 1, "title": "Pilot"}],
		"episodeFile": {"relativePath": "S01/Breaking.Bad.S01E01.mkv", "quality": "1080p", "size": 1000000000},
		"isUpgrade": false,
		"downloadClient": "SABnzbd",
		"downloadId": "dl-1"
	}`)
	sendSonarr(t, mux, `{
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
	// 6 (grabs) + 2 * (notify + phase1 + phase2) = 12
	if len(got) != 12 {
		t.Fatalf("expected 12 calls total, got %d", len(got))
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
	mux, _, calls, mu := newHandler(t, testConfig())

	w := sendSonarr(t, mux, `{
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
	// notify + create + end-phase1 (ONGOING) + end-phase2 (ENDED) = 4
	if len(got) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(got))
	}

	// Notification
	if got[0].Method != "POST" || got[0].Path != "/notifications" {
		t.Errorf("call 0: expected POST /notifications, got %s %s", got[0].Method, got[0].Path)
	}

	// Create
	if got[1].Method != "POST" || got[1].Path != "/activities" {
		t.Errorf("call 1: expected POST /activities, got %s %s", got[1].Method, got[1].Path)
	}
	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, got[1].Body, &create)
	// Content-keyed: series-id fallback (no tvdbId, no episode id).
	if create.Slug != "sonarr-series-id-1" {
		t.Errorf("expected slug sonarr-series-id-1, got %s", create.Slug)
	}

	// Phase 2: ENDED with Downloaded
	var endReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[3].Body, &endReq)
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
	mux, _, calls, mu := newHandler(t, testConfig())

	w := sendRadarr(t, mux, `{"eventType": "Test"}`)
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
	mux, _, calls, mu := newHandler(t, testConfig())

	w := sendRadarr(t, mux, `{
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
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (notification), got %d", len(recorded))
	}

	if recorded[0].Method != "POST" || recorded[0].Path != "/notifications" {
		t.Errorf("expected POST /notifications, got %s %s", recorded[0].Method, recorded[0].Path)
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Title != "Radarr Health" {
		t.Errorf("expected title 'Radarr Health', got %s", req.Title)
	}
	if req.Body != "Warning · Indexer NZBgeek is unavailable due to failures" {
		t.Errorf("expected body with health message, got %s", req.Body)
	}
	if req.Category != "health" {
		t.Errorf("expected category 'health', got %s", req.Category)
	}
	if !req.Push {
		t.Error("expected push=true")
	}
}

func TestRadarrHealthError(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	w := sendRadarr(t, mux, `{
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
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (notification), got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Body != "Critical · Disk space low" {
		t.Errorf("expected body with health message, got %s", req.Body)
	}
	if req.Source != "radarr" {
		t.Errorf("expected source 'radarr', got %s", req.Source)
	}
}

func TestRadarrHealthAndRestored(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	// Health
	sendRadarr(t, mux, `{
		"eventType": "Health",
		"level": "warning",
		"message": "Indexer NZBgeek is unavailable",
		"type": "IndexerStatusCheck",
		"wikiUrl": "https://wiki.servarr.com"
	}`)

	// HealthRestored
	sendRadarr(t, mux, `{
		"eventType": "HealthRestored",
		"level": "warning",
		"message": "Indexer NZBgeek is available again",
		"type": "IndexerStatusCheck",
		"previousLevel": "warning"
	}`)

	recorded := testutil.GetCalls(calls, mu)
	// health_notification + restored_notification = 2
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls (health + restored notifications), got %d", len(recorded))
	}

	// Health notification
	var healthReq pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &healthReq)
	if healthReq.Body != "Warning · Indexer NZBgeek is unavailable" {
		t.Errorf("expected body with health message, got %s", healthReq.Body)
	}

	// Restored notification
	var restoredReq pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &restoredReq)
	if restoredReq.Body != "Resolved · Indexer NZBgeek is available again" {
		t.Errorf("expected body with restored message, got %s", restoredReq.Body)
	}
	if restoredReq.Category != "health-restored" {
		t.Errorf("expected category 'health-restored', got %s", restoredReq.Category)
	}
}

func TestSonarrHealth(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	w := sendSonarr(t, mux, `{
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
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (notification), got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Title != "Sonarr Health" {
		t.Errorf("expected title 'Sonarr Health', got %s", req.Title)
	}
	if req.Body != "Critical · No indexers available" {
		t.Errorf("expected body with health message, got %s", req.Body)
	}
}

// ============================================================
// ManualInteractionRequired Tests
// ============================================================

func TestRadarrManualInteraction(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	// Grab first to create a tracked download
	sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_manual1"
	}`)

	// ManualInteractionRequired
	sendRadarr(t, mux, `{
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
	// grab_notify + create + grab_update + manual_notification = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	// Verify manual interaction notification
	if recorded[3].Path != "/notifications" {
		t.Errorf("expected POST /notifications, got %s %s", recorded[3].Method, recorded[3].Path)
	}
	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &req)
	if req.Body != "No files found eligible for import" {
		t.Errorf("expected body with status message, got %s", req.Body)
	}
	if req.Category != "manual-interaction" {
		t.Errorf("expected category 'manual-interaction', got %s", req.Category)
	}
	if req.Subtitle != "Inception.2010.1080p.BluRay.x264-SPARKS" {
		t.Errorf("expected subtitle with download title, got %s", req.Subtitle)
	}
}

func TestRadarrManualInteractionUntracked(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	// ManualInteractionRequired without a prior Grab — still sends notification
	w := sendRadarr(t, mux, `{
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
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (notification), got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Body != "Import requires manual interaction" {
		t.Errorf("expected default reason, got %s", req.Body)
	}
}

func TestRadarrGrabManualInteractionDownload(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	// Grab
	sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_full"
	}`)

	// ManualInteractionRequired
	sendRadarr(t, mux, `{
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
	sendRadarr(t, mux, `{
		"eventType": "Download",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"movieFile": {"relativePath": "Inception.mkv", "quality": "Bluray-1080p", "size": 5368709120},
		"isUpgrade": false,
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_full"
	}`)

	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// grab_notify + create + grab_update + manual_notification + download_notify + phase1(ONGOING) + phase2(ENDED) = 7
	if len(recorded) != 7 {
		t.Fatalf("expected 7 calls, got %d", len(recorded))
	}

	// Manual interaction should be a notification
	if recorded[3].Path != "/notifications" {
		t.Errorf("expected manual interaction as notification, got %s %s", recorded[3].Method, recorded[3].Path)
	}

	// Phase 2 should be ENDED with "Imported"
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[6].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", phase2.State)
	}
	if phase2.Content.State != "Imported" {
		t.Errorf("expected 'Imported', got %s", phase2.Content.State)
	}
}

func TestSonarrTestEvent(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	w := sendSonarr(t, mux, `{"eventType": "Test"}`)
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
	mux, _, calls, mu := newHandler(t, testConfig())

	w := sendSonarrBasicAuth(t, mux, `{
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
	if len(got) != 3 {
		t.Fatalf("expected 3 calls (notify + create + update), got %d", len(got))
	}
}

// ============================================================
// Smart Mode Tests
// ============================================================

func smartConfig() *config.StarrConfig {
	cfg := testConfig()
	cfg.Mode = config.ModeSmart
	return cfg
}

func notifyConfig() *config.StarrConfig {
	cfg := testConfig()
	cfg.Mode = config.ModeNotify
	return cfg
}

func TestRadarrGrab_SmartMode_SendsNotification(t *testing.T) {
	mux, _, calls, mu := newHandler(t, smartConfig())

	w := sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120, "indexer": "NZBgeek", "releaseTitle": "Inception.2010.1080p.BluRay"},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_smart1"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// Smart mode: should send notification only, no activity creation
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (notification only), got %d: %+v", len(recorded), pathsOf(recorded))
	}
	if recorded[0].Method != "POST" || recorded[0].Path != "/notifications" {
		t.Errorf("expected POST /notifications, got %s %s", recorded[0].Method, recorded[0].Path)
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Title != "Radarr" {
		t.Errorf("expected title Radarr, got %s", req.Title)
	}
	if req.Subtitle != "Inception (2010)" {
		t.Errorf("expected subtitle 'Inception (2010)', got %s", req.Subtitle)
	}
	if !req.Push {
		t.Error("expected push=true in smart mode for Grab")
	}
	if req.ThreadID != "radarr" {
		t.Errorf("expected thread_id radarr, got %s", req.ThreadID)
	}
}

func TestRadarrGrab_ActivityMode_CreatesActivity(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig()) // default = activity mode

	w := sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"release": {"quality": "Bluray-1080p", "size": 5368709120, "indexer": "NZBgeek", "releaseTitle": "Inception.2010.1080p.BluRay"},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_activity1"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// Activity mode: notification + create activity + update activity = 3 calls
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls (notify + create + update), got %d: %+v", len(recorded), pathsOf(recorded))
	}
	if recorded[0].Path != "/notifications" {
		t.Errorf("first call should be notification, got %s", recorded[0].Path)
	}
	if recorded[1].Path != "/activities" {
		t.Errorf("second call should be create activity, got %s", recorded[1].Path)
	}

	// Verify notification has push=false in activity mode
	var notifReq pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &notifReq)
	if notifReq.Push {
		t.Error("expected push=false in activity mode for Grab")
	}
}

func TestRadarrDownload_SmartMode_SendsNotification(t *testing.T) {
	mux, _, calls, mu := newHandler(t, smartConfig())

	w := sendRadarr(t, mux, `{
		"eventType": "Download",
		"movie": {"id": 1, "title": "Inception", "year": 2010},
		"movieFile": {"quality": "Bluray-1080p"},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_smart2",
		"isUpgrade": false
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (notification only), got %d: %+v", len(recorded), pathsOf(recorded))
	}
	if recorded[0].Path != "/notifications" {
		t.Errorf("expected POST /notifications, got %s", recorded[0].Path)
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Body != "Imported · Inception (2010)" {
		t.Errorf("expected body with movie title, got %s", req.Body)
	}
}

func TestSonarrGrab_SmartMode_SendsNotification(t *testing.T) {
	mux, _, calls, mu := newHandler(t, smartConfig())

	w := sendSonarr(t, mux, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "tvdbId": 81189},
		"episodes": [{"id": 1, "episodeNumber": 5, "seasonNumber": 2, "title": "Breakage"}],
		"release": {"quality": "HDTV-720p", "size": 500000000, "indexer": "NZBgeek", "releaseTitle": "Breaking.Bad.S02E05.720p"},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_sonarr_smart1"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (notification only), got %d: %+v", len(recorded), pathsOf(recorded))
	}
	if recorded[0].Path != "/notifications" {
		t.Errorf("expected POST /notifications, got %s", recorded[0].Path)
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Title != "Sonarr" {
		t.Errorf("expected title Sonarr, got %s", req.Title)
	}
	if req.ThreadID != "media-tv-81189" {
		t.Errorf("expected thread_id media-tv-81189, got %s", req.ThreadID)
	}
}

func TestHealth_SmartMode_SendsNotification(t *testing.T) {
	mux, _, calls, mu := newHandler(t, smartConfig())

	w := sendRadarr(t, mux, `{
		"eventType": "Health",
		"level": "warning",
		"message": "Disk space low",
		"type": "DiskSpace",
		"wikiUrl": "https://wiki.servarr.com/radarr/system#disk-space"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (notification), got %d: %+v", len(recorded), pathsOf(recorded))
	}
	if recorded[0].Path != "/notifications" {
		t.Errorf("expected POST /notifications, got %s %s", recorded[0].Method, recorded[0].Path)
	}
}

func TestRadarrGrab_NotifyMode_SendsNotification(t *testing.T) {
	mux, _, calls, mu := newHandler(t, notifyConfig())

	w := sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Matrix", "year": 1999},
		"release": {"quality": "1080p"},
		"downloadClient": "SABnzbd",
		"downloadId": "SABnzbd_nzo_notify1"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d: %+v", len(recorded), pathsOf(recorded))
	}
	if recorded[0].Path != "/notifications" {
		t.Errorf("expected POST /notifications, got %s", recorded[0].Path)
	}
}

// ============================================================
// Retry consolidation tests (content-based dedup)
// ============================================================

// TestRadarrRetryConsolidatesActivity verifies that a failed download followed
// by a retry (different downloadId, same movie) keeps a single Live Activity
// instead of spawning a second orphaned one.
func TestRadarrRetryConsolidatesActivity(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	// First grab — release A
	sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010, "tmdbId": 27205},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-A"
	}`)

	// Release A fails — ManualInteractionRequired (with movie so activity updates)
	sendRadarr(t, mux, `{
		"eventType": "ManualInteractionRequired",
		"downloadId": "dl-A",
		"movie": {"id": 1, "title": "Inception", "year": 2010, "tmdbId": 27205},
		"downloadInfo": {
			"quality": "Bluray-1080p",
			"title": "Inception.Release.A",
			"status": "Warning",
			"statusMessages": [{"title": "t", "messages": ["No files eligible"]}]
		}
	}`)

	// User picks release B — different downloadId, same movie
	sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010, "tmdbId": 27205},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-B"
	}`)

	recorded := testutil.GetCalls(calls, mu)

	// Only one CreateActivity should be recorded, slugged by tmdbId.
	var creates []pushward.CreateActivityRequest
	for _, c := range recorded {
		if c.Method == "POST" && c.Path == "/activities" {
			var cr pushward.CreateActivityRequest
			testutil.UnmarshalBody(t, c.Body, &cr)
			creates = append(creates, cr)
		}
	}
	if len(creates) != 1 {
		t.Fatalf("expected 1 CreateActivity, got %d", len(creates))
	}
	if creates[0].Slug != "radarr-movie-27205" {
		t.Errorf("expected slug radarr-movie-27205, got %s", creates[0].Slug)
	}

	// PATCHes must all target the same slug.
	for _, c := range recorded {
		if c.Method == "PATCH" && !strings.HasSuffix(c.Path, "/radarr-movie-27205") {
			t.Errorf("unexpected PATCH target %s", c.Path)
		}
	}

	// Manual-interaction must have produced a "Needs attention" update.
	var sawNeedsAttention bool
	for _, c := range recorded {
		if c.Method != "PATCH" {
			continue
		}
		var u pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &u)
		if u.Content.State == "Needs attention" && u.Content.AccentColor == pushward.ColorOrange {
			sawNeedsAttention = true
		}
	}
	if !sawNeedsAttention {
		t.Error("expected a PATCH updating activity to 'Needs attention'")
	}
}

// TestSonarrRetryConsolidatesActivity verifies that Sonarr's retry flow also
// collapses into a single activity (same series + episode set).
func TestSonarrRetryConsolidatesActivity(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	// Release A
	sendSonarr(t, mux, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "tvdbId": 81189},
		"episodes": [{"id": 67, "episodeNumber": 5, "seasonNumber": 2, "title": "Breakage"}],
		"release": {"quality": "1080p", "size": 1500000000},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-A"
	}`)

	// Release B — same series + same episode set
	sendSonarr(t, mux, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "tvdbId": 81189},
		"episodes": [{"id": 67, "episodeNumber": 5, "seasonNumber": 2, "title": "Breakage"}],
		"release": {"quality": "720p", "size": 800000000},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-B"
	}`)

	recorded := testutil.GetCalls(calls, mu)
	var creates []pushward.CreateActivityRequest
	for _, c := range recorded {
		if c.Method == "POST" && c.Path == "/activities" {
			var cr pushward.CreateActivityRequest
			testutil.UnmarshalBody(t, c.Body, &cr)
			creates = append(creates, cr)
		}
	}
	if len(creates) != 1 {
		t.Fatalf("expected 1 CreateActivity, got %d", len(creates))
	}
	if creates[0].Slug != "sonarr-series-81189-e-67" {
		t.Errorf("expected slug sonarr-series-81189-e-67, got %s", creates[0].Slug)
	}
}

// TestSonarrDifferentEpisodesProduceDistinctSlugs verifies two activities are
// created when the user downloads different episodes of the same series.
func TestSonarrDifferentEpisodesProduceDistinctSlugs(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	sendSonarr(t, mux, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "tvdbId": 81189},
		"episodes": [{"id": 67, "episodeNumber": 5, "seasonNumber": 2, "title": "Breakage"}],
		"release": {"quality": "1080p", "size": 1500000000},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-A"
	}`)
	sendSonarr(t, mux, `{
		"eventType": "Grab",
		"series": {"id": 1, "title": "Breaking Bad", "tvdbId": 81189},
		"episodes": [{"id": 68, "episodeNumber": 6, "seasonNumber": 2, "title": "Peekaboo"}],
		"release": {"quality": "1080p", "size": 1500000000},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-B"
	}`)

	recorded := testutil.GetCalls(calls, mu)
	slugs := map[string]bool{}
	for _, c := range recorded {
		if c.Method == "POST" && c.Path == "/activities" {
			var cr pushward.CreateActivityRequest
			testutil.UnmarshalBody(t, c.Body, &cr)
			slugs[cr.Slug] = true
		}
	}
	if !slugs["sonarr-series-81189-e-67"] || !slugs["sonarr-series-81189-e-68"] {
		t.Errorf("expected distinct slugs per episode, got %v", slugs)
	}
}

// TestRadarrManualInteractionUpdatesActivity verifies that ManualInteractionRequired
// with a tracked activity flips it to "Needs attention" (no second activity).
func TestRadarrManualInteractionUpdatesActivity(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010, "tmdbId": 27205},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-A"
	}`)
	sendRadarr(t, mux, `{
		"eventType": "ManualInteractionRequired",
		"downloadId": "dl-A",
		"movie": {"id": 1, "title": "Inception", "year": 2010, "tmdbId": 27205},
		"downloadInfo": {
			"quality": "Bluray-1080p",
			"title": "Inception.1080p",
			"status": "Warning",
			"statusMessages": [{"title": "t", "messages": ["Cannot import"]}]
		}
	}`)

	recorded := testutil.GetCalls(calls, mu)
	// grab_notify + create + grab_update + mi_notify + mi_update = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(recorded))
	}

	var mi pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &mi)
	if mi.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING on needs-attention update, got %s", mi.State)
	}
	if mi.Content.State != "Needs attention" {
		t.Errorf("expected content state 'Needs attention', got %s", mi.Content.State)
	}
	if mi.Content.AccentColor != pushward.ColorOrange {
		t.Errorf("expected orange accent, got %s", mi.Content.AccentColor)
	}
	if mi.Content.Icon != "exclamationmark.triangle.fill" {
		t.Errorf("expected warning icon, got %s", mi.Content.Icon)
	}
	if !strings.Contains(mi.Content.Subtitle, "Cannot import") {
		t.Errorf("expected subtitle to contain reason, got %q", mi.Content.Subtitle)
	}
}

// TestRadarrManualInteractionNoMovieFieldNotifiesOnly verifies that older
// payloads lacking the movie field fall through to notification-only.
func TestRadarrManualInteractionNoMovieFieldNotifiesOnly(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	// Grab creates a tracked activity.
	sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010, "tmdbId": 27205},
		"release": {"quality": "Bluray-1080p", "size": 5368709120},
		"downloadClient": "SABnzbd",
		"downloadId": "dl-A"
	}`)
	// ManualInteractionRequired without movie field.
	sendRadarr(t, mux, `{
		"eventType": "ManualInteractionRequired",
		"downloadId": "dl-A",
		"downloadInfo": {"quality": "x", "title": "t", "status": "Warning", "statusMessages": []}
	}`)

	recorded := testutil.GetCalls(calls, mu)
	// grab_notify + create + grab_update + mi_notify = 4 (no mi_update)
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}
	if recorded[3].Path != "/notifications" {
		t.Errorf("expected manual-interaction notification, got %s %s", recorded[3].Method, recorded[3].Path)
	}
}

// TestSonarrLargeEpisodeSetHashesSlug verifies slug stays within the 128-char
// server limit when a season pack lists many episodes.
func TestSonarrLargeEpisodeSetHashesSlug(t *testing.T) {
	series := SonarrSeries{TvdbID: 81189}
	eps := make([]SonarrEpisode, 50)
	for i := range eps {
		eps[i] = SonarrEpisode{ID: 1000 + i}
	}
	slug1, _ := sonarrContentKey(series, eps, "dl-X")
	if len(slug1) > 128 {
		t.Errorf("slug exceeds 128-char limit: len=%d", len(slug1))
	}
	// Determinism: same inputs produce the same slug.
	slug2, _ := sonarrContentKey(series, eps, "dl-Y")
	if slug1 != slug2 {
		t.Errorf("expected deterministic slug, got %s then %s", slug1, slug2)
	}
	// And the hashed form must use the series prefix.
	if !strings.HasPrefix(slug1, "sonarr-series-81189-e-") {
		t.Errorf("expected hashed slug to preserve series prefix, got %s", slug1)
	}
}

// TestRadarrContentKeyFallbacks exercises the fallback chain.
func TestRadarrContentKeyFallbacks(t *testing.T) {
	s1, mk1 := radarrContentKey(RadarrMovie{TmdbID: 27205, ID: 1}, "dl-1")
	if s1 != "radarr-movie-27205" || mk1 != "radarr:movie:tmdb:27205" {
		t.Errorf("tmdb path: got (%s, %s)", s1, mk1)
	}
	s2, mk2 := radarrContentKey(RadarrMovie{ID: 42}, "dl-1")
	if s2 != "radarr-movie-id-42" || mk2 != "radarr:movie:id:42" {
		t.Errorf("id fallback: got (%s, %s)", s2, mk2)
	}
	s3, mk3 := radarrContentKey(RadarrMovie{}, "SAB_nzo_1")
	if s3 != "radarr-sab-nzo-1" || mk3 != "radarr:SAB_nzo_1" {
		t.Errorf("downloadId fallback: got (%s, %s)", s3, mk3)
	}
}

// pathsOf is a test helper that extracts paths from API calls for error messages.
func pathsOf(calls []testutil.APICall) []string {
	paths := make([]string, len(calls))
	for i, c := range calls {
		paths[i] = c.Method + " " + c.Path
	}
	return paths
}
