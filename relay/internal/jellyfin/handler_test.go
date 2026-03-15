package jellyfin

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

func testConfig() *config.JellyfinConfig {
	return &config.JellyfinConfig{
		Enabled:          true,
		Priority:         1,
		CleanupDelay:     1 * time.Hour,
		StaleTimeout:     30 * time.Minute,
		EndDelay:         10 * time.Millisecond,
		EndDisplayTime:   10 * time.Millisecond,
		ProgressDebounce: 10 * time.Millisecond,
	}
}

func newHandler(t *testing.T, cfg *config.JellyfinConfig) (*Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL)
	h := NewHandler(store, pool, cfg)
	return h, calls, mu
}

func send(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/jellyfin", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	auth.Middleware(h).ServeHTTP(w, req)
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
	if update.State != "ONGOING" {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "generic" {
		t.Errorf("expected template generic, got %s", update.Content.Template)
	}
	if update.Content.State != "Playing on Apple TV" {
		t.Errorf("expected state 'Playing on Apple TV', got %s", update.Content.State)
	}
	if update.Content.Icon != "play.circle.fill" {
		t.Errorf("expected icon play.circle.fill, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != "#007AFF" {
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

	// Phase 1: ONGOING with "Watched on Apple TV"
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1)
	if phase1.State != "ONGOING" {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Watched on Apple TV" {
		t.Errorf("expected state 'Watched on Apple TV', got %s", phase1.Content.State)
	}
	if phase1.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected checkmark icon, got %s", phase1.Content.Icon)
	}
	if phase1.Content.AccentColor != "#34C759" {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}

	// Phase 2: ENDED with "Watched on Apple TV"
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != "ENDED" {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Watched on Apple TV" {
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

func TestGenericUpdateNotification(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"NotificationType": "GenericUpdateNotification",
		"ServerName": "My Jellyfin"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 calls for test notification, got %d", len(recorded))
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

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING("Added to library") + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	// Verify create
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "Dune: Part Two" {
		t.Errorf("expected name 'Dune: Part Two', got %s", createReq.Name)
	}

	// Verify initial ONGOING
	var ongoing pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &ongoing)
	if ongoing.State != "ONGOING" {
		t.Errorf("expected ONGOING, got %s", ongoing.State)
	}
	if ongoing.Content.State != "Added to library" {
		t.Errorf("expected state 'Added to library', got %s", ongoing.Content.State)
	}
	if ongoing.Content.Icon != "plus.circle.fill" {
		t.Errorf("expected icon plus.circle.fill, got %s", ongoing.Content.Icon)
	}

	// Verify ENDED
	var ended pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &ended)
	if ended.State != "ENDED" {
		t.Errorf("expected ENDED, got %s", ended.State)
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

	// Task completed
	send(t, h, `{
		"NotificationType": "ScheduledTaskCompleted",
		"TaskName": "Scan All Libraries",
		"TaskId": "abc123",
		"TaskResult": "Completed"
	}`)

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + ONGOING("Running...") + phase1(ONGOING "Complete") + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	// Verify create
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "Scan All Libraries" {
		t.Errorf("expected name 'Scan All Libraries', got %s", createReq.Name)
	}

	// Verify Running update
	var running pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &running)
	if running.Content.State != "Running..." {
		t.Errorf("expected state 'Running...', got %s", running.Content.State)
	}
	if running.Content.Icon != "arrow.triangle.2.circlepath" {
		t.Errorf("expected icon arrow.triangle.2.circlepath, got %s", running.Content.Icon)
	}

	// Verify Phase 1 (ONGOING with "Complete")
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1)
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

	// Verify Phase 2 (ENDED)
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != "ENDED" {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}
