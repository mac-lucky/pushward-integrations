package backrest

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

func newTestAPI(t *testing.T, cfg *config.BackrestConfig) (http.Handler, *Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL, nil)

	mux, api := humautil.NewTestAPI()
	h := RegisterRoutes(api, store, pool, cfg)

	return mux, h, calls, mu
}

func newHandler(t *testing.T, cfg *config.BackrestConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	mux, _, calls, mu := newTestAPI(t, cfg)
	return mux, calls, mu
}

func send(t *testing.T, h http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/backrest", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
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
	if update.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", update.Content.Template)
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
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 1 {
		t.Errorf("expected current_step 1, got %v", update.Content.CurrentStep)
	}
	if update.Content.TotalSteps == nil || *update.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", update.Content.TotalSteps)
	}
	if len(update.Content.StepLabels) != 2 || update.Content.StepLabels[0] != "Running" || update.Content.StepLabels[1] != "Done" {
		t.Errorf("expected step_labels [Running, Done], got %v", update.Content.StepLabels)
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
	if phase1.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", phase1.Content.Template)
	}
	if phase1.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}
	if phase1.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected icon checkmark.circle.fill, got %s", phase1.Content.Icon)
	}
	if phase1.Content.CurrentStep == nil || *phase1.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", phase1.Content.CurrentStep)
	}
	if phase1.Content.TotalSteps == nil || *phase1.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", phase1.Content.TotalSteps)
	}
	if len(phase1.Content.StepLabels) != 2 || phase1.Content.StepLabels[0] != "Running" || phase1.Content.StepLabels[1] != "Done" {
		t.Errorf("expected step_labels [Running, Done], got %v", phase1.Content.StepLabels)
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
	if phase1.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", phase1.Content.Template)
	}
	if phase1.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red color, got %s", phase1.Content.AccentColor)
	}
	if phase1.Content.Icon != "xmark.circle.fill" {
		t.Errorf("expected icon xmark.circle.fill, got %s", phase1.Content.Icon)
	}
	if phase1.Content.CurrentStep == nil || *phase1.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", phase1.Content.CurrentStep)
	}
	if phase1.Content.TotalSteps == nil || *phase1.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", phase1.Content.TotalSteps)
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
	if phase1.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", phase1.Content.Template)
	}
	if phase1.Content.AccentColor != pushward.ColorOrange {
		t.Errorf("expected orange color, got %s", phase1.Content.AccentColor)
	}
	if phase1.Content.Icon != "exclamationmark.triangle.fill" {
		t.Errorf("expected icon exclamationmark.triangle.fill, got %s", phase1.Content.Icon)
	}
	if phase1.Content.CurrentStep == nil || *phase1.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", phase1.Content.CurrentStep)
	}
	if phase1.Content.TotalSteps == nil || *phase1.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", phase1.Content.TotalSteps)
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
	if update.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", update.Content.Template)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 1 {
		t.Errorf("expected current_step 1, got %v", update.Content.CurrentStep)
	}
	if update.Content.TotalSteps == nil || *update.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", update.Content.TotalSteps)
	}
	if len(update.Content.StepLabels) != 2 || update.Content.StepLabels[0] != "Running" || update.Content.StepLabels[1] != "Done" {
		t.Errorf("expected step_labels [Running, Done], got %v", update.Content.StepLabels)
	}

	// Phase 1: Pruned
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1)
	if phase1.Content.State != "Pruned" {
		t.Errorf("expected state 'Pruned', got %s", phase1.Content.State)
	}
	if phase1.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", phase1.Content.Template)
	}
	if phase1.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}
	if phase1.Content.CurrentStep == nil || *phase1.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", phase1.Content.CurrentStep)
	}
	if phase1.Content.TotalSteps == nil || *phase1.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", phase1.Content.TotalSteps)
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
	if update.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", update.Content.Template)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 1 {
		t.Errorf("expected current_step 1, got %v", update.Content.CurrentStep)
	}
	if update.Content.TotalSteps == nil || *update.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", update.Content.TotalSteps)
	}
	if len(update.Content.StepLabels) != 2 || update.Content.StepLabels[0] != "Running" || update.Content.StepLabels[1] != "Done" {
		t.Errorf("expected step_labels [Running, Done], got %v", update.Content.StepLabels)
	}

	// Phase 1: Check Passed
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1)
	if phase1.Content.State != "Check Passed" {
		t.Errorf("expected state 'Check Passed', got %s", phase1.Content.State)
	}
	if phase1.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", phase1.Content.Template)
	}
	if phase1.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}
	if phase1.Content.CurrentStep == nil || *phase1.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", phase1.Content.CurrentStep)
	}
	if phase1.Content.TotalSteps == nil || *phase1.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", phase1.Content.TotalSteps)
	}
}

func TestForgetLifecycle(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send FORGET_START
	w := send(t, h, `{
		"event": "CONDITION_FORGET_START",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Send FORGET_SUCCESS
	w = send(t, h, `{
		"event": "CONDITION_FORGET_SUCCESS",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// FORGET_START: create + ONGOING = 2
	// FORGET_SUCCESS: create + phase1(ONGOING) + phase2(ENDED) = 3
	// Total = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(recorded))
	}

	// Verify FORGET_START ONGOING
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Forgetting..." {
		t.Errorf("expected state 'Forgetting...', got %s", update.Content.State)
	}
	if update.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", update.Content.Template)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 1 {
		t.Errorf("expected current_step 1, got %v", update.Content.CurrentStep)
	}
	if update.Content.TotalSteps == nil || *update.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", update.Content.TotalSteps)
	}
	if len(update.Content.StepLabels) != 2 || update.Content.StepLabels[0] != "Running" || update.Content.StepLabels[1] != "Done" {
		t.Errorf("expected step_labels [Running, Done], got %v", update.Content.StepLabels)
	}

	// Phase 1: Forgotten
	var phase1f pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1f)
	if phase1f.Content.State != "Forgotten" {
		t.Errorf("expected state 'Forgotten', got %s", phase1f.Content.State)
	}
	if phase1f.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", phase1f.Content.Template)
	}
	if phase1f.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green color, got %s", phase1f.Content.AccentColor)
	}
	if phase1f.Content.CurrentStep == nil || *phase1f.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", phase1f.Content.CurrentStep)
	}
	if phase1f.Content.TotalSteps == nil || *phase1f.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", phase1f.Content.TotalSteps)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestForgetError(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send FORGET_START
	send(t, h, `{
		"event": "CONDITION_FORGET_START",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)

	// Send FORGET_ERROR
	w := send(t, h, `{
		"event": "CONDITION_FORGET_ERROR",
		"plan": "daily-backup",
		"repo": "local-repo",
		"error": "permission denied"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// FORGET_START: create + ONGOING = 2
	// FORGET_ERROR: create + phase1(ONGOING) + phase2(ENDED) = 3
	// Total = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(recorded))
	}

	// Phase 1: red/failed
	var phase1fe pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1fe)
	if phase1fe.Content.State != "Forget Failed" {
		t.Errorf("expected state 'Forget Failed', got %s", phase1fe.Content.State)
	}
	if phase1fe.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", phase1fe.Content.Template)
	}
	if phase1fe.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red color, got %s", phase1fe.Content.AccentColor)
	}
	if phase1fe.Content.Icon != "xmark.circle.fill" {
		t.Errorf("expected icon xmark.circle.fill, got %s", phase1fe.Content.Icon)
	}
	if phase1fe.Content.CurrentStep == nil || *phase1fe.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", phase1fe.Content.CurrentStep)
	}
	if phase1fe.Content.TotalSteps == nil || *phase1fe.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", phase1fe.Content.TotalSteps)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[4].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestAnyError(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"event": "CONDITION_ANY_ERROR",
		"plan": "daily-backup",
		"repo": "local-repo",
		"error": "repository lock held by PID 1234"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

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

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "alert" {
		t.Errorf("expected template 'alert', got %s", update.Content.Template)
	}
	if update.Content.Severity != "critical" {
		t.Errorf("expected severity 'critical', got %s", update.Content.Severity)
	}
	if update.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red color, got %s", update.Content.AccentColor)
	}
	if update.Content.Icon != "exclamationmark.triangle.fill" {
		t.Errorf("expected icon exclamationmark.triangle.fill, got %s", update.Content.Icon)
	}
	if update.Content.State != "repository lock held by PID 1234" {
		t.Errorf("expected state 'repository lock held by PID 1234', got %s", update.Content.State)
	}
	if update.Content.Subtitle != "Backrest · daily-backup · local-repo" {
		t.Errorf("expected subtitle 'Backrest · daily-backup · local-repo', got %q", update.Content.Subtitle)
	}

	// Phase 1: ONGOING with same content
	var phase1ae pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1ae)
	if phase1ae.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1ae.State)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestSnapshotSkipped(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"event": "CONDITION_SNAPSHOT_SKIPPED",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

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

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "alert" {
		t.Errorf("expected template 'alert', got %s", update.Content.Template)
	}
	if update.Content.Severity != "info" {
		t.Errorf("expected severity 'info', got %s", update.Content.Severity)
	}
	if update.Content.AccentColor != pushward.ColorBlue {
		t.Errorf("expected blue color, got %s", update.Content.AccentColor)
	}
	if update.Content.Icon != "info.circle.fill" {
		t.Errorf("expected icon info.circle.fill, got %s", update.Content.Icon)
	}
	if update.Content.State != "Snapshot Skipped" {
		t.Errorf("expected state 'Snapshot Skipped', got %s", update.Content.State)
	}
	if update.Content.Subtitle != "Backrest · daily-backup · local-repo" {
		t.Errorf("expected subtitle 'Backrest · daily-backup · local-repo', got %q", update.Content.Subtitle)
	}

	// Phase 1: ONGOING with same content
	var phase1ss pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1ss)
	if phase1ss.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1ss.State)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestSnapshotStart_APIFailure_Returns502(t *testing.T) {
	// Server returns 400 for all requests (not retried by client).
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL, nil)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, store, pool, testConfig())

	w := send(t, mux, `{
		"event": "CONDITION_SNAPSHOT_START",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
}
