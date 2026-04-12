package jellyfin

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

func testConfig() *config.JellyfinConfig {
	return &config.JellyfinConfig{
		BaseProviderConfig: config.BaseProviderConfig{
			Enabled:        true,
			Priority:       1,
			CleanupDelay:   1 * time.Hour,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
		ProgressDebounce: 10 * time.Millisecond,
		PauseTimeout:     1 * time.Hour, // long default so existing tests aren't affected
	}
}

func newHandler(t *testing.T, cfg *config.JellyfinConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
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
	req := httptest.NewRequest(http.MethodPost, "/jellyfin", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestPlaybackStart(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"NotificationType": "PlaybackStart",
		"ItemId": "abc123",
		"ItemType": "Episode",
		"Name": "Pilot",
		"SeriesName": "Breaking Bad",
		"SeasonNumber": 1,
		"EpisodeNumber": 1,
		"ProductionYear": 2008,
		"RunTimeTicks": 27630000000,
		"PlaybackPositionTicks": 0,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": false
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
	if createReq.Name != "Breaking Bad" {
		t.Errorf("expected name 'Breaking Bad', got %s", createReq.Name)
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
	if update.Content.Template != "generic" {
		t.Errorf("expected template generic, got %s", update.Content.Template)
	}
	if update.Content.State != "Playing on Apple TV by john" {
		t.Errorf("expected state 'Playing on Apple TV', got %s", update.Content.State)
	}
	if update.Content.Icon != "play.circle.fill" {
		t.Errorf("expected icon play.circle.fill, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != pushward.ColorBlue {
		t.Errorf("expected blue color, got %s", update.Content.AccentColor)
	}
	if update.Content.Progress != 0 {
		t.Errorf("expected progress 0, got %f", update.Content.Progress)
	}
	if !strings.Contains(update.Content.Subtitle, "S01E01") {
		t.Errorf("expected subtitle to contain 'S01E01', got %s", update.Content.Subtitle)
	}
	if !strings.Contains(update.Content.Subtitle, "Pilot") {
		t.Errorf("expected subtitle to contain 'Pilot', got %s", update.Content.Subtitle)
	}
}

func TestPlaybackStartPausedIgnored(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"NotificationType": "PlaybackStart",
		"ItemId": "abc123",
		"ItemType": "Episode",
		"Name": "Pilot",
		"SeriesName": "Breaking Bad",
		"SeasonNumber": 1,
		"EpisodeNumber": 1,
		"ProductionYear": 2008,
		"RunTimeTicks": 27630000000,
		"PlaybackPositionTicks": 0,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": true
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 calls (paused start ignored), got %d", len(recorded))
	}
}

func TestPlaybackStartPausedThenResume(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Paused start — should be ignored
	send(t, h, `{
		"NotificationType": "PlaybackStart",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 10000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": true
	}`)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 calls after paused start, got %d", len(recorded))
	}

	// Resume — should create activity via late join
	send(t, h, `{
		"NotificationType": "PlaybackProgress",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 12000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": false
	}`)

	recorded = testutil.GetCalls(calls, mu)
	// create + update = 2
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls (late-join create + update), got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Playing on Apple TV by john" {
		t.Errorf("expected state 'Playing on Apple TV', got %s", update.Content.State)
	}
}

func TestPlaybackProgressPausedNoActivityIgnored(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// PlaybackProgress with IsPaused=true and no prior start
	w := send(t, h, `{
		"NotificationType": "PlaybackProgress",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 10000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": true
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 calls (paused progress without activity ignored), got %d", len(recorded))
	}
}

func TestPlaybackStartAndStop(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Start
	send(t, h, `{
		"NotificationType": "PlaybackStart",
		"ItemId": "abc123",
		"ItemType": "Episode",
		"Name": "Pilot",
		"SeriesName": "Breaking Bad",
		"SeasonNumber": 1,
		"EpisodeNumber": 1,
		"ProductionYear": 2008,
		"RunTimeTicks": 27630000000,
		"PlaybackPositionTicks": 0,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": false
	}`)

	// Stop
	send(t, h, `{
		"NotificationType": "PlaybackStop",
		"ItemId": "abc123",
		"ItemType": "Episode",
		"Name": "Pilot",
		"SeriesName": "Breaking Bad",
		"SeasonNumber": 1,
		"EpisodeNumber": 1,
		"ProductionYear": 2008,
		"RunTimeTicks": 27630000000,
		"PlaybackPositionTicks": 25870000000,
		"PlayedToCompletion": true,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": false
	}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + start_update + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	// Phase 1: ONGOING with "Watched on Apple TV by john"
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Watched on Apple TV by john" {
		t.Errorf("expected state 'Watched on Apple TV', got %s", phase1.Content.State)
	}
	if phase1.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected checkmark icon, got %s", phase1.Content.Icon)
	}
	if phase1.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}

	// Phase 2: ENDED with "Watched on Apple TV by john"
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Watched on Apple TV by john" {
		t.Errorf("expected state 'Watched on Apple TV', got %s", phase2.Content.State)
	}
}

func TestPlaybackProgressDebounce(t *testing.T) {
	cfg := testConfig()
	cfg.ProgressDebounce = 500 * time.Millisecond // Long debounce
	h, calls, mu := newHandler(t, cfg)

	// Start playback
	send(t, h, `{
		"NotificationType": "PlaybackStart",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"SeriesName": "",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 0,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": false
	}`)

	// Send progress immediately (within debounce window)
	send(t, h, `{
		"NotificationType": "PlaybackProgress",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"SeriesName": "",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 10000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": false
	}`)

	recorded := testutil.GetCalls(calls, mu)
	// create + start_update = 2 (progress debounced)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls (progress debounced), got %d", len(recorded))
	}
}

func TestPlaybackProgressStateChangeBypassesDebounce(t *testing.T) {
	cfg := testConfig()
	cfg.ProgressDebounce = 500 * time.Millisecond // Long debounce
	h, calls, mu := newHandler(t, cfg)

	// Start playback (not paused)
	send(t, h, `{
		"NotificationType": "PlaybackStart",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 0,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": false
	}`)

	// Send progress with IsPaused=true (state change, should bypass debounce)
	send(t, h, `{
		"NotificationType": "PlaybackProgress",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 10000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": true
	}`)

	recorded := testutil.GetCalls(calls, mu)
	// create + start_update + progress_update = 3 (not debounced because state changed)
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls (state change bypasses debounce), got %d", len(recorded))
	}

	// Verify the progress update shows "Paused"
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update)
	if update.Content.State != "Paused on Apple TV by john" {
		t.Errorf("expected state 'Paused on Apple TV', got %s", update.Content.State)
	}
	if update.Content.Icon != "pause.circle.fill" {
		t.Errorf("expected icon pause.circle.fill, got %s", update.Content.Icon)
	}
}

func TestPlaybackProgressSuppressedWhilePaused(t *testing.T) {
	cfg := testConfig()
	cfg.ProgressDebounce = 10 * time.Millisecond
	h, calls, mu := newHandler(t, cfg)

	// Start playback
	send(t, h, `{
		"NotificationType": "PlaybackStart",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 0,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": false
	}`)

	// Pause (state change → goes through)
	send(t, h, `{
		"NotificationType": "PlaybackProgress",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 10000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": true
	}`)

	// Wait for debounce to expire
	time.Sleep(20 * time.Millisecond)

	// Still paused — should be suppressed even though debounce expired
	send(t, h, `{
		"NotificationType": "PlaybackProgress",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 10000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": true
	}`)

	recorded := testutil.GetCalls(calls, mu)
	// create + start_update + pause_update = 3 (second paused progress suppressed)
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls (paused progress suppressed), got %d", len(recorded))
	}
}

func TestPlaybackProgressCreatesActivity(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send PlaybackProgress without a prior PlaybackStart
	w := send(t, h, `{
		"NotificationType": "PlaybackProgress",
		"ItemId": "abc123",
		"ItemType": "Episode",
		"Name": "Pilot",
		"SeriesName": "Breaking Bad",
		"SeasonNumber": 1,
		"EpisodeNumber": 1,
		"ProductionYear": 2008,
		"RunTimeTicks": 27630000000,
		"PlaybackPositionTicks": 10000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": false
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// create + update = 2
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls (create + update), got %d", len(recorded))
	}

	// Verify create
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "Breaking Bad" {
		t.Errorf("expected name 'Breaking Bad', got %s", createReq.Name)
	}

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.State != "Playing on Apple TV by john" {
		t.Errorf("expected state 'Playing on Apple TV', got %s", update.Content.State)
	}
}

func TestGenericUpdateNotification(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"NotificationType": "GenericUpdateNotification",
		"ServerName": "My Jellyfin"
	}`)
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
	if create.Slug != "relay-test-jellyfin" {
		t.Errorf("expected slug relay-test-jellyfin, got %s", create.Slug)
	}
}

func TestItemAdded(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"NotificationType": "ItemAdded",
		"ItemId": "movie123",
		"ItemType": "Movie",
		"Name": "Dune: Part Two",
		"SeriesName": "",
		"ProductionYear": 2024,
		"RunTimeTicks": 10404000000000
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
	if req.Title != "Dune: Part Two" {
		t.Errorf("expected title 'Dune: Part Two', got %s", req.Title)
	}
	if req.Body != "Added · Dune: Part Two" {
		t.Errorf("expected body 'Added · Dune: Part Two', got %s", req.Body)
	}
	if req.Subtitle != "Jellyfin \u00b7 2024" {
		t.Errorf("expected subtitle 'Jellyfin · 2024', got %s", req.Subtitle)
	}
	if req.Source != "jellyfin" {
		t.Errorf("expected source 'jellyfin', got %s", req.Source)
	}
	if !req.Push {
		t.Error("expected push=true")
	}
}

func TestPauseAutoEnd(t *testing.T) {
	cfg := testConfig()
	cfg.PauseTimeout = 50 * time.Millisecond
	h, calls, mu := newHandler(t, cfg)

	// Start playback
	send(t, h, `{
		"NotificationType": "PlaybackStart",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 10000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": false
	}`)

	// Pause (state change → goes through, starts pause timer)
	send(t, h, `{
		"NotificationType": "PlaybackProgress",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 10000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": true
	}`)

	// Wait for pause timeout + end phases
	time.Sleep(200 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + start_update + pause_update + phase1(ONGOING "Paused") + phase2(ENDED) = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls (auto-end after pause timeout), got %d", len(recorded))
	}

	// Phase 1: ONGOING with "Paused on Apple TV by john"
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Paused on Apple TV by john" {
		t.Errorf("expected state 'Paused on Apple TV', got %s", phase1.Content.State)
	}
	if phase1.Content.Icon != "pause.circle.fill" {
		t.Errorf("expected pause icon, got %s", phase1.Content.Icon)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestPauseResumeCancelsAutoEnd(t *testing.T) {
	cfg := testConfig()
	cfg.PauseTimeout = 100 * time.Millisecond
	h, calls, mu := newHandler(t, cfg)

	// Start playback
	send(t, h, `{
		"NotificationType": "PlaybackStart",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 10000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": false
	}`)

	// Pause (state change)
	send(t, h, `{
		"NotificationType": "PlaybackProgress",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 10000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": true
	}`)

	// Resume before timeout (state change → bypasses debounce)
	send(t, h, `{
		"NotificationType": "PlaybackProgress",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 12000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": false
	}`)

	// Wait past the timeout — should NOT auto-end
	time.Sleep(200 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + start_update + pause_update + resume_update = 4 (no auto-end)
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls (no auto-end after resume), got %d", len(recorded))
	}
}

func TestPauseStopCancelsAutoEnd(t *testing.T) {
	cfg := testConfig()
	cfg.PauseTimeout = 200 * time.Millisecond
	h, calls, mu := newHandler(t, cfg)

	// Start playback
	send(t, h, `{
		"NotificationType": "PlaybackStart",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 10000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": false
	}`)

	// Pause
	send(t, h, `{
		"NotificationType": "PlaybackProgress",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 10000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": true
	}`)

	// Stop before pause timeout
	send(t, h, `{
		"NotificationType": "PlaybackStop",
		"ItemId": "abc123",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88320000000,
		"PlaybackPositionTicks": 10000000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"ClientName": "Infuse",
		"IsPaused": false
	}`)

	// Wait for end phases (from PlaybackStop) and past pause timeout
	time.Sleep(300 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + start_update + pause_update + phase1("Watched") + phase2(ENDED) = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls (stop ends, not pause auto-end), got %d", len(recorded))
	}

	// Verify it ended with "Watched" (from PlaybackStop), not "Paused"
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1)
	if phase1.Content.State != "Watched on Apple TV by john" {
		t.Errorf("expected state 'Watched on Apple TV', got %s", phase1.Content.State)
	}
}

func TestTaskStartedAndCompleted(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Task started
	send(t, h, `{
		"NotificationType": "ScheduledTaskStarted",
		"TaskName": "Scan All Libraries",
		"TaskId": "abc123"
	}`)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (started notification), got %d", len(recorded))
	}
	if recorded[0].Method != "POST" || recorded[0].Path != "/notifications" {
		t.Errorf("expected POST /notifications, got %s %s", recorded[0].Method, recorded[0].Path)
	}

	var started pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &started)
	if started.Title != "Scan All Libraries" {
		t.Errorf("expected title 'Scan All Libraries', got %s", started.Title)
	}
	if started.Body != "Started · Scan All Libraries" {
		t.Errorf("expected body 'Started · Scan All Libraries', got %s", started.Body)
	}

	// Task completed
	send(t, h, `{
		"NotificationType": "ScheduledTaskCompleted",
		"TaskName": "Scan All Libraries",
		"TaskId": "abc123",
		"TaskResult": "Completed"
	}`)

	recorded = testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls (started + completed), got %d", len(recorded))
	}

	var completed pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &completed)
	if completed.Title != "Scan All Libraries" {
		t.Errorf("expected title 'Scan All Libraries', got %s", completed.Title)
	}
	if completed.Body != "Complete · Scan All Libraries" {
		t.Errorf("expected body 'Complete · Scan All Libraries', got %s", completed.Body)
	}
	if completed.CollapseID != "jellyfin-task-Scan All Libraries" {
		t.Errorf("expected collapse_id to match started, got %s", completed.CollapseID)
	}
}

func TestTaskFailed(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	send(t, h, `{
		"NotificationType": "ScheduledTaskCompleted",
		"TaskName": "Scan All Libraries",
		"TaskId": "abc123",
		"TaskResult": "Failed"
	}`)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Body != "Failed · Scan All Libraries" {
		t.Errorf("expected body 'Failed · Scan All Libraries', got %s", req.Body)
	}
	if req.Level != pushward.LevelActive {
		t.Errorf("expected level 'active' for failure, got %s", req.Level)
	}
}
