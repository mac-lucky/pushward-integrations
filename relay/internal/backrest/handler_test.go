package backrest

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

func testConfig() *config.BackrestConfig {
	return &config.BackrestConfig{
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

func newHandler(t *testing.T, cfg *config.BackrestConfig) (*Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL)
	h := NewHandler(store, pool, cfg)
	return h, calls, mu
}

func send(t *testing.T, h *Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/backrest", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	auth.Middleware(h).ServeHTTP(w, req)
	return w
}

func TestSnapshotLifecycle(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send START
	w := send(t, h, `{
		"event": "CONDITION_SNAPSHOT_START",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Send SUCCESS
	w = send(t, h, `{
		"event": "CONDITION_SNAPSHOT_SUCCESS",
		"plan": "daily-backup",
		"repo": "local-repo",
		"snapshot_id": "abc123def",
		"data_added": 2468421632,
		"files_new": 42,
		"files_changed": 156,
		"duration_ms": 45000
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// START: create + ONGOING = 2
	// SUCCESS: create + phase1(ONGOING) + phase2(ENDED) = 3
	// Total = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(recorded))
	}

	// Verify create from START
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "daily-backup" {
		t.Errorf("expected name 'daily-backup', got %s", createReq.Name)
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
	if update.Content.State != "Backing up..." {
		t.Errorf("expected state 'Backing up...', got %s", update.Content.State)
	}
	if update.Content.Icon != "arrow.triangle.2.circlepath" {
		t.Errorf("expected icon arrow.triangle.2.circlepath, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != pushward.ColorBlue {
		t.Errorf("expected blue color, got %s", update.Content.AccentColor)
	}
	if update.Content.Progress != 0 {
		t.Errorf("expected progress 0, got %f", update.Content.Progress)
	}
	if update.Content.Subtitle != "Backrest · daily-backup · local-repo" {
		t.Errorf("expected subtitle 'Backrest · daily-backup · local-repo', got %q", update.Content.Subtitle)
	}

	// Phase 1: ONGOING with final content (from SUCCESS)
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Complete · 2.3 GB" {
		t.Errorf("expected state 'Complete · 2.3 GB', got %s", phase1.Content.State)
	}
	if phase1.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}
	if phase1.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected icon checkmark.circle.fill, got %s", phase1.Content.Icon)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != "Complete · 2.3 GB" {
		t.Errorf("expected state 'Complete · 2.3 GB', got %s", phase2.Content.State)
	}
}

func TestSnapshotError(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send START
	send(t, h, `{
		"event": "CONDITION_SNAPSHOT_START",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)

	// Send ERROR
	w := send(t, h, `{
		"event": "CONDITION_SNAPSHOT_ERROR",
		"plan": "daily-backup",
		"repo": "local-repo",
		"duration_ms": 5000,
		"error": "repository not found"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// START: create + ONGOING = 2
	// ERROR: create + phase1(ONGOING) + phase2(ENDED) = 3
	// Total = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(recorded))
	}

	// Phase 1: red/failed
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1)
	if phase1.Content.State != "Failed: repository not found" {
		t.Errorf("expected state 'Failed: repository not found', got %s", phase1.Content.State)
	}
	if phase1.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red color, got %s", phase1.Content.AccentColor)
	}
	if phase1.Content.Icon != "xmark.circle.fill" {
		t.Errorf("expected icon xmark.circle.fill, got %s", phase1.Content.Icon)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestSnapshotWarning(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send START
	send(t, h, `{
		"event": "CONDITION_SNAPSHOT_START",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)

	// Send WARNING
	w := send(t, h, `{
		"event": "CONDITION_SNAPSHOT_WARNING",
		"plan": "daily-backup",
		"repo": "local-repo",
		"data_added": 1048576
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(recorded))
	}

	// Phase 1: orange/warning
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1)
	if phase1.Content.State != "Complete (warnings)" {
		t.Errorf("expected state 'Complete (warnings)', got %s", phase1.Content.State)
	}
	if phase1.Content.AccentColor != pushward.ColorOrange {
		t.Errorf("expected orange color, got %s", phase1.Content.AccentColor)
	}
	if phase1.Content.Icon != "exclamationmark.triangle.fill" {
		t.Errorf("expected icon exclamationmark.triangle.fill, got %s", phase1.Content.Icon)
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1 KB"},
		{1536, "2 KB"},
		{1048576, "1 MB"},
		{1073741824, "1.0 GB"},
		{2468421632, "2.3 GB"},
	}

	for _, tc := range tests {
		got := formatBytes(tc.input)
		if got != tc.expected {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestPruneLifecycle(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send PRUNE_START
	w := send(t, h, `{
		"event": "CONDITION_PRUNE_START",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Send PRUNE_SUCCESS
	w = send(t, h, `{
		"event": "CONDITION_PRUNE_SUCCESS",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// PRUNE_START: create + ONGOING = 2
	// PRUNE_SUCCESS: create + phase1(ONGOING) + phase2(ENDED) = 3
	// Total = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(recorded))
	}

	// Verify PRUNE_START ONGOING
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Pruning..." {
		t.Errorf("expected state 'Pruning...', got %s", update.Content.State)
	}

	// Phase 1: Pruned
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1)
	if phase1.Content.State != "Pruned" {
		t.Errorf("expected state 'Pruned', got %s", phase1.Content.State)
	}
	if phase1.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}
}

func TestCheckLifecycle(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send CHECK_START
	w := send(t, h, `{
		"event": "CONDITION_CHECK_START",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Send CHECK_SUCCESS
	w = send(t, h, `{
		"event": "CONDITION_CHECK_SUCCESS",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(recorded))
	}

	// Verify CHECK_START ONGOING
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Checking..." {
		t.Errorf("expected state 'Checking...', got %s", update.Content.State)
	}

	// Phase 1: Check Passed
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1)
	if phase1.Content.State != "Check Passed" {
		t.Errorf("expected state 'Check Passed', got %s", phase1.Content.State)
	}
	if phase1.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}
}
