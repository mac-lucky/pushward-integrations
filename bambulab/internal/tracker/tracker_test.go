package tracker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/bambulab/internal/bambulab"
	"github.com/mac-lucky/pushward-integrations/bambulab/internal/config"
	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// --- Test helpers ---

type mockPrinter struct {
	mu       sync.Mutex
	state    bambulab.MergedState
	updateCh chan struct{}
}

func newMockPrinter() *mockPrinter {
	return &mockPrinter{updateCh: make(chan struct{}, 1)}
}

func (m *mockPrinter) State() bambulab.MergedState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *mockPrinter) UpdateCh() <-chan struct{} {
	return m.updateCh
}

func (m *mockPrinter) SetState(s bambulab.MergedState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = s
}

func testConfig() *config.Config {
	return &config.Config{
		BambuLab: config.BambuLabConfig{
			Host:       "192.168.1.100",
			AccessCode: "12345678",
			Serial:     "01P00A000000001",
		},
		PushWard: sharedconfig.PushWardConfig{
			URL:            "http://localhost",
			APIKey:         "hlk_test",
			Priority:       1,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   60 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
		Polling: config.PollingConfig{
			UpdateInterval: 5 * time.Second,
		},
	}
}

func newTestTracker(printer *mockPrinter, pwClient *pushward.Client, cfg *config.Config) *Tracker {
	return New(cfg, printer, pwClient)
}

// --- buildSubtitle tests ---

func TestBuildSubtitle_FileAndTemp(t *testing.T) {
	state := &bambulab.MergedState{
		SubtaskName:  "Benchy.3mf",
		NozzleTemper: 235.0,
		NozzleTarget: 235.0,
	}
	got := buildSubtitle(state)
	want := "Benchy.3mf · 235/235°C"
	if got != want {
		t.Errorf("buildSubtitle = %q, want %q", got, want)
	}
}

func TestBuildSubtitle_FileOnly(t *testing.T) {
	state := &bambulab.MergedState{
		SubtaskName:  "Model.gcode",
		NozzleTemper: 0,
	}
	got := buildSubtitle(state)
	if got != "Model.gcode" {
		t.Errorf("buildSubtitle = %q, want Model.gcode", got)
	}
}

func TestBuildSubtitle_TempOnly(t *testing.T) {
	state := &bambulab.MergedState{
		NozzleTemper: 220.0,
		NozzleTarget: 230.0,
	}
	got := buildSubtitle(state)
	if got != "220/230°C" {
		t.Errorf("buildSubtitle = %q, want 220/230°C", got)
	}
}

func TestBuildSubtitle_Empty(t *testing.T) {
	state := &bambulab.MergedState{}
	got := buildSubtitle(state)
	if got != "" {
		t.Errorf("buildSubtitle = %q, want empty", got)
	}
}

func TestBuildSubtitle_LongFilename(t *testing.T) {
	state := &bambulab.MergedState{
		SubtaskName:  "Very Long Filename For A 3D Print.3mf",
		NozzleTemper: 200.0,
		NozzleTarget: 220.0,
	}
	got := buildSubtitle(state)
	// buildSubtitle truncates to 20 runes: 17 chars + "..."
	want := "Very Long Filenam... · 200/220°C"
	if got != want {
		t.Errorf("buildSubtitle = %q, want %q", got, want)
	}
}

// --- State transition tests ---

func TestProcess_PrepareToRunningToFinish(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.URL = srv.URL
	printer := newMockPrinter()
	client := pushward.NewClient(srv.URL, "hlk_test")
	tr := newTestTracker(printer, client, cfg)

	// Step 1: PREPARE — should start tracking and create activity
	printer.SetState(bambulab.MergedState{
		GcodeState:  bambulab.StatePrepare,
		SubtaskName: "Benchy.3mf",
	})
	ctx := context.Background()
	tr.process(ctx)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("PREPARE: expected >= 2 calls (create + update), got %d", len(recorded))
	}

	// Verify create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("call 0: expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Slug != "bambu-01p00a000000001" {
		t.Errorf("slug = %q, want bambu-01p00a000000001", createReq.Slug)
	}
	if createReq.Name != "Benchy.3mf" {
		t.Errorf("name = %q, want Benchy.3mf", createReq.Name)
	}

	// Verify preparing update
	if recorded[1].Method != "PATCH" {
		t.Errorf("call 1: expected PATCH, got %s", recorded[1].Method)
	}
	var prepUpdate pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &prepUpdate)
	if prepUpdate.State != pushward.StateOngoing {
		t.Errorf("prepare state = %q, want ONGOING", prepUpdate.State)
	}
	if prepUpdate.Content.State != "Preparing..." {
		t.Errorf("content state = %q, want Preparing...", prepUpdate.Content.State)
	}
	if prepUpdate.Content.Icon != "arrow.triangle.2.circlepath" {
		t.Errorf("icon = %q, want arrow.triangle.2.circlepath", prepUpdate.Content.Icon)
	}

	if !tr.tracking {
		t.Fatal("tracker should be in tracking state")
	}

	// Step 2: RUNNING — should send progress update
	printer.SetState(bambulab.MergedState{
		GcodeState:    bambulab.StateRunning,
		SubtaskName:   "Benchy.3mf",
		Percent:       25,
		RemainingTime: 60,
		LayerNum:      50,
		TotalLayerNum: 200,
		NozzleTemper:  220.0,
		NozzleTarget:  220.0,
	})
	tr.process(ctx)

	recorded = testutil.GetCalls(calls, mu)
	lastCall := recorded[len(recorded)-1]
	var runUpdate pushward.UpdateRequest
	testutil.UnmarshalBody(t, lastCall.Body, &runUpdate)
	if runUpdate.State != pushward.StateOngoing {
		t.Errorf("running state = %q, want ONGOING", runUpdate.State)
	}
	if runUpdate.Content.State != "Layer 50/200" {
		t.Errorf("content state = %q, want Layer 50/200", runUpdate.Content.State)
	}
	if runUpdate.Content.Icon != "printer.fill" {
		t.Errorf("icon = %q, want printer.fill", runUpdate.Content.Icon)
	}
	if runUpdate.Content.AccentColor != "blue" {
		t.Errorf("accent = %q, want blue", runUpdate.Content.AccentColor)
	}
	if runUpdate.Content.Progress != 0.25 {
		t.Errorf("progress = %f, want 0.25", runUpdate.Content.Progress)
	}
	if runUpdate.Content.RemainingTime == nil || *runUpdate.Content.RemainingTime != 3600 {
		t.Errorf("remaining = %v, want 3600", runUpdate.Content.RemainingTime)
	}

	// Step 3: FINISH — should trigger two-phase end
	printer.SetState(bambulab.MergedState{
		GcodeState:    bambulab.StateFinish,
		SubtaskName:   "Benchy.3mf",
		Percent:       100,
		RemainingTime: 0,
		LayerNum:      200,
		TotalLayerNum: 200,
	})
	tr.process(ctx)

	// tracking should be false immediately (reset before timer fires)
	if tr.tracking {
		t.Error("tracker should not be tracking after FINISH")
	}

	// Wait for two-phase end to complete (endDelay + endDisplayTime = 20ms)
	time.Sleep(80 * time.Millisecond)

	recorded = testutil.GetCalls(calls, mu)
	// Find the ONGOING "Complete" and ENDED "Complete" calls
	var foundOngoing, foundEnded bool
	for _, c := range recorded {
		if c.Method != "PATCH" {
			continue
		}
		var req pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &req)
		if req.Content.State == "Complete" && req.Content.Icon == "checkmark.circle.fill" {
			if req.State == pushward.StateOngoing {
				foundOngoing = true
			}
			if req.State == pushward.StateEnded {
				foundEnded = true
			}
		}
	}
	if !foundOngoing {
		t.Error("two-phase end: missing ONGOING with Complete content")
	}
	if !foundEnded {
		t.Error("two-phase end: missing ENDED")
	}
}

func TestProcess_PauseAndResume(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	printer := newMockPrinter()
	client := pushward.NewClient(srv.URL, "hlk_test")
	tr := newTestTracker(printer, client, cfg)

	// Start with RUNNING
	printer.SetState(bambulab.MergedState{
		GcodeState:    bambulab.StateRunning,
		SubtaskName:   "Part.3mf",
		Percent:       50,
		RemainingTime: 30,
		LayerNum:      100,
		TotalLayerNum: 200,
	})
	ctx := context.Background()
	tr.process(ctx) // starts tracking

	// Pause
	printer.SetState(bambulab.MergedState{
		GcodeState:    bambulab.StatePause,
		SubtaskName:   "Part.3mf",
		Percent:       50,
		RemainingTime: 30,
		LayerNum:      100,
		TotalLayerNum: 200,
		NozzleTemper:  220.0,
		NozzleTarget:  220.0,
	})
	tr.process(ctx)

	recorded := testutil.GetCalls(calls, mu)
	lastCall := recorded[len(recorded)-1]
	var pauseUpdate pushward.UpdateRequest
	testutil.UnmarshalBody(t, lastCall.Body, &pauseUpdate)
	if pauseUpdate.Content.State != "Paused" {
		t.Errorf("pause state = %q, want Paused", pauseUpdate.Content.State)
	}
	if pauseUpdate.Content.Icon != "pause.circle.fill" {
		t.Errorf("pause icon = %q, want pause.circle.fill", pauseUpdate.Content.Icon)
	}
	if pauseUpdate.Content.AccentColor != "orange" {
		t.Errorf("pause accent = %q, want orange", pauseUpdate.Content.AccentColor)
	}

	// Resume to RUNNING
	printer.SetState(bambulab.MergedState{
		GcodeState:    bambulab.StateRunning,
		SubtaskName:   "Part.3mf",
		Percent:       51,
		RemainingTime: 29,
		LayerNum:      102,
		TotalLayerNum: 200,
		NozzleTemper:  220.0,
		NozzleTarget:  220.0,
	})
	tr.process(ctx)

	recorded = testutil.GetCalls(calls, mu)
	lastCall = recorded[len(recorded)-1]
	var resumeUpdate pushward.UpdateRequest
	testutil.UnmarshalBody(t, lastCall.Body, &resumeUpdate)
	if resumeUpdate.Content.State != "Layer 102/200" {
		t.Errorf("resume state = %q, want Layer 102/200", resumeUpdate.Content.State)
	}
	if resumeUpdate.Content.Icon != "printer.fill" {
		t.Errorf("resume icon = %q, want printer.fill", resumeUpdate.Content.Icon)
	}
}

func TestProcess_Failed(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	printer := newMockPrinter()
	client := pushward.NewClient(srv.URL, "hlk_test")
	tr := newTestTracker(printer, client, cfg)

	// Start with RUNNING
	printer.SetState(bambulab.MergedState{
		GcodeState:    bambulab.StateRunning,
		SubtaskName:   "Vase.gcode",
		Percent:       30,
		TotalLayerNum: 100,
		LayerNum:      30,
	})
	ctx := context.Background()
	tr.process(ctx) // starts tracking

	// Fail
	printer.SetState(bambulab.MergedState{
		GcodeState:    bambulab.StateFailed,
		SubtaskName:   "Vase.gcode",
		Percent:       30,
		PrintError:    50348044,
		FailReason:    "Nozzle clogged",
		TotalLayerNum: 100,
		LayerNum:      30,
	})
	tr.process(ctx)

	if tr.tracking {
		t.Error("tracker should not be tracking after FAILED")
	}

	// Wait for two-phase end
	time.Sleep(80 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	var foundOngoing, foundEnded bool
	for _, c := range recorded {
		if c.Method != "PATCH" {
			continue
		}
		var req pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &req)
		if req.Content.State == "Failed" && req.Content.Icon == "xmark.circle.fill" {
			if req.Content.AccentColor != "red" {
				t.Errorf("failure accent = %q, want red", req.Content.AccentColor)
			}
			if req.State == pushward.StateOngoing {
				foundOngoing = true
				if req.Content.Progress != 0.3 {
					t.Errorf("failure progress = %f, want 0.3", req.Content.Progress)
				}
			}
			if req.State == pushward.StateEnded {
				foundEnded = true
			}
		}
	}
	if !foundOngoing {
		t.Error("two-phase fail: missing ONGOING with Failed content")
	}
	if !foundEnded {
		t.Error("two-phase fail: missing ENDED")
	}
}

func TestProcess_IdleCancellation(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	printer := newMockPrinter()
	client := pushward.NewClient(srv.URL, "hlk_test")
	tr := newTestTracker(printer, client, cfg)

	// Start with RUNNING
	printer.SetState(bambulab.MergedState{
		GcodeState:  bambulab.StateRunning,
		SubtaskName: "Print.3mf",
		Percent:     10,
	})
	ctx := context.Background()
	tr.process(ctx) // starts tracking

	// Go IDLE (cancellation)
	printer.SetState(bambulab.MergedState{
		GcodeState: bambulab.StateIdle,
	})
	tr.process(ctx)

	if tr.tracking {
		t.Error("tracker should not be tracking after IDLE")
	}

	recorded := testutil.GetCalls(calls, mu)
	lastCall := recorded[len(recorded)-1]
	var endReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, lastCall.Body, &endReq)
	if endReq.State != pushward.StateEnded {
		t.Errorf("idle end state = %q, want ENDED", endReq.State)
	}
	if endReq.Content.State != "Cancelled" {
		t.Errorf("idle content state = %q, want Cancelled", endReq.Content.State)
	}
	if endReq.Content.AccentColor != "orange" {
		t.Errorf("idle accent = %q, want orange", endReq.Content.AccentColor)
	}
}

func TestProcess_NoTrackingIgnoresIdle(t *testing.T) {
	srv, _, _ := testutil.MockPushWardServer(t)
	cfg := testConfig()
	printer := newMockPrinter()
	client := pushward.NewClient(srv.URL, "hlk_test")
	tr := newTestTracker(printer, client, cfg)

	// IDLE when not tracking — should do nothing
	printer.SetState(bambulab.MergedState{
		GcodeState: bambulab.StateIdle,
	})
	tr.process(context.Background())

	if tr.tracking {
		t.Error("should not start tracking on IDLE")
	}
}

func TestProcess_NoTrackingIgnoresFinish(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	printer := newMockPrinter()
	client := pushward.NewClient(srv.URL, "hlk_test")
	tr := newTestTracker(printer, client, cfg)

	// FINISH when not tracking — should do nothing
	printer.SetState(bambulab.MergedState{
		GcodeState: bambulab.StateFinish,
	})
	tr.process(context.Background())

	if tr.tracking {
		t.Error("should not start tracking on FINISH")
	}
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Errorf("expected 0 API calls, got %d", len(recorded))
	}
}

func TestProcess_PercentWithoutLayers(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	printer := newMockPrinter()
	client := pushward.NewClient(srv.URL, "hlk_test")
	tr := newTestTracker(printer, client, cfg)

	// Start tracking with RUNNING, no layer info
	printer.SetState(bambulab.MergedState{
		GcodeState:    bambulab.StateRunning,
		SubtaskName:   "NoLayers.gcode",
		Percent:       42,
		TotalLayerNum: 0,
		LayerNum:      0,
	})
	tr.process(context.Background())

	recorded := testutil.GetCalls(calls, mu)
	// Last PATCH should have percent-based state text
	lastPatch := recorded[len(recorded)-1]
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, lastPatch.Body, &req)
	if req.Content.State != "42%" {
		t.Errorf("state = %q, want 42%%", req.Content.State)
	}
}

func TestProcess_DuplicateFinishIgnored(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	printer := newMockPrinter()
	client := pushward.NewClient(srv.URL, "hlk_test")
	tr := newTestTracker(printer, client, cfg)

	// Start tracking
	printer.SetState(bambulab.MergedState{
		GcodeState:  bambulab.StateRunning,
		SubtaskName: "Test.3mf",
		Percent:     50,
	})
	ctx := context.Background()
	tr.process(ctx)

	// First FINISH
	printer.SetState(bambulab.MergedState{
		GcodeState:  bambulab.StateFinish,
		SubtaskName: "Test.3mf",
		Percent:     100,
	})
	tr.process(ctx)

	callsAfterFirst := len(testutil.GetCalls(calls, mu))

	// Second FINISH — lastState is now FINISH, so it should be a no-op
	// (tracking is already false, so process returns early)
	tr.process(ctx)

	callsAfterSecond := len(testutil.GetCalls(calls, mu))
	if callsAfterSecond != callsAfterFirst {
		t.Errorf("duplicate FINISH generated %d extra calls", callsAfterSecond-callsAfterFirst)
	}
}

func TestSlugGeneration(t *testing.T) {
	cfg := testConfig()
	printer := newMockPrinter()
	client := pushward.NewClient("http://localhost", "hlk_test")
	tr := newTestTracker(printer, client, cfg)

	want := "bambu-01p00a000000001"
	if tr.slug != want {
		t.Errorf("slug = %q, want %q", tr.slug, want)
	}
}

func TestSlugGeneration_UppercaseSerial(t *testing.T) {
	cfg := testConfig()
	cfg.BambuLab.Serial = "01P00A000000ABC"
	printer := newMockPrinter()
	client := pushward.NewClient("http://localhost", "hlk_test")
	tr := New(cfg, printer, client)

	want := "bambu-01p00a000000abc"
	if tr.slug != want {
		t.Errorf("slug = %q, want %q", tr.slug, want)
	}
}

func TestProcess_StartTrackingWithSubtaskName(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	printer := newMockPrinter()
	client := pushward.NewClient(srv.URL, "hlk_test")
	tr := newTestTracker(printer, client, cfg)

	// PREPARE with subtask name
	printer.SetState(bambulab.MergedState{
		GcodeState:  bambulab.StatePrepare,
		SubtaskName: "My Cool Model With A Long Name.3mf",
	})
	tr.process(context.Background())

	recorded := testutil.GetCalls(calls, mu)
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	// Name should be truncated to 40 chars
	if createReq.Name != "My Cool Model With A Long Name.3mf" {
		t.Errorf("name = %q, want My Cool Model With A Long Name.3mf", createReq.Name)
	}
}

func TestProcess_StartTrackingWithoutSubtaskName(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	printer := newMockPrinter()
	client := pushward.NewClient(srv.URL, "hlk_test")
	tr := newTestTracker(printer, client, cfg)

	// PREPARE without subtask name
	printer.SetState(bambulab.MergedState{
		GcodeState: bambulab.StatePrepare,
	})
	tr.process(context.Background())

	recorded := testutil.GetCalls(calls, mu)
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "BambuLab Print" {
		t.Errorf("name = %q, want BambuLab Print", createReq.Name)
	}
}

func TestFinishActivity_TwoPhaseEnd(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	printer := newMockPrinter()
	client := pushward.NewClient(srv.URL, "hlk_test")
	tr := newTestTracker(printer, client, cfg)

	// Setup: mark as tracking
	tr.tracking = true
	tr.lastState = bambulab.StateRunning

	state := &bambulab.MergedState{
		SubtaskName: "Benchy.3mf",
		Percent:     100,
	}

	tr.finishActivity(context.Background(), state)

	// Should immediately reset tracking
	if tr.tracking {
		t.Error("finishActivity should reset tracking immediately")
	}
	if tr.lastState != "" {
		t.Error("finishActivity should clear lastState")
	}

	// Wait for async two-phase end
	time.Sleep(80 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("expected >= 2 calls, got %d", len(recorded))
	}

	// Phase 1: ONGOING with "Complete"
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("phase 1 state = %q, want ONGOING", phase1.State)
	}
	if phase1.Content.State != "Complete" {
		t.Errorf("phase 1 content state = %q, want Complete", phase1.Content.State)
	}
	if phase1.Content.AccentColor != "green" {
		t.Errorf("phase 1 accent = %q, want green", phase1.Content.AccentColor)
	}
	if phase1.Content.Progress != 1.0 {
		t.Errorf("phase 1 progress = %f, want 1.0", phase1.Content.Progress)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("phase 2 state = %q, want ENDED", phase2.State)
	}
}

func TestFailActivity_TwoPhaseEnd(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	printer := newMockPrinter()
	client := pushward.NewClient(srv.URL, "hlk_test")
	tr := newTestTracker(printer, client, cfg)

	// Setup: mark as tracking
	tr.tracking = true
	tr.lastState = bambulab.StateRunning

	state := &bambulab.MergedState{
		SubtaskName: "Broken.gcode",
		Percent:     42,
		PrintError:  50348044,
	}

	tr.failActivity(context.Background(), state)

	if tr.tracking {
		t.Error("failActivity should reset tracking immediately")
	}

	// Wait for async two-phase end
	time.Sleep(80 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("expected >= 2 calls, got %d", len(recorded))
	}

	// Phase 1: ONGOING "Failed"
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("phase 1 state = %q, want ONGOING", phase1.State)
	}
	if phase1.Content.State != "Failed" {
		t.Errorf("phase 1 content state = %q, want Failed", phase1.Content.State)
	}
	if phase1.Content.AccentColor != "red" {
		t.Errorf("phase 1 accent = %q, want red", phase1.Content.AccentColor)
	}
	if phase1.Content.Progress != 0.42 {
		t.Errorf("phase 1 progress = %f, want 0.42", phase1.Content.Progress)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("phase 2 state = %q, want ENDED", phase2.State)
	}
}

func TestEndActivity_Immediate(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	printer := newMockPrinter()
	client := pushward.NewClient(srv.URL, "hlk_test")
	tr := newTestTracker(printer, client, cfg)
	tr.tracking = true

	tr.endActivity(context.Background(), "Cancelled", "xmark.circle.fill", "orange")

	if tr.tracking {
		t.Error("endActivity should reset tracking")
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.State != pushward.StateEnded {
		t.Errorf("state = %q, want ENDED", req.State)
	}
	if req.Content.State != "Cancelled" {
		t.Errorf("content state = %q, want Cancelled", req.Content.State)
	}
}
