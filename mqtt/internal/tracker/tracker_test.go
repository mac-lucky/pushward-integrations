package tracker

import (
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/mqtt/internal/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func testTrackerConfig() *Config {
	return &Config{
		EndDelay:       10 * time.Millisecond,
		EndDisplayTime: 10 * time.Millisecond,
		CleanupDelay:   1 * time.Hour,
		StaleTimeout:   30 * time.Minute,
		UpdateInterval: 10 * time.Millisecond,
	}
}

func fieldRule() *config.RuleConfig {
	return &config.RuleConfig{
		Name:       "Washer",
		Topic:      "zigbee2mqtt/washer",
		Slug:       "washer",
		Template:   "generic",
		Lifecycle:  "field",
		StateField: "running_state",
		StateMap: map[string]string{
			"idle":    "IGNORE",
			"running": "ONGOING",
			"done":    "ENDED",
		},
		Content: config.ContentMapping{
			State: "{running_state}",
		},
	}
}

func presenceRule() *config.RuleConfig {
	return &config.RuleConfig{
		Name:              "Sensor",
		Topic:             "sensor/temp",
		Slug:              "sensor",
		Template:          "generic",
		Lifecycle:         "presence",
		InactivityTimeout: 50 * time.Millisecond,
		Content: config.ContentMapping{
			State: "{temperature}",
		},
	}
}

func waitForCalls(calls *[]testutil.APICall, mu *sync.Mutex, count int, timeout time.Duration) []testutil.APICall {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c := testutil.GetCalls(calls, mu)
		if len(c) >= count {
			return c
		}
		time.Sleep(5 * time.Millisecond)
	}
	return testutil.GetCalls(calls, mu)
}

func TestFieldMode_Ongoing(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	tr := New(fieldRule(), pw, 1, testTrackerConfig())
	defer tr.Stop()

	tr.HandleMessage(map[string]any{"running_state": "running"})

	c := waitForCalls(calls, mu, 2, 500*time.Millisecond)
	if len(c) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(c))
	}

	// First call: create activity
	if c[0].Method != "POST" || c[0].Path != "/activities" {
		t.Errorf("call 0: %s %s, want POST /activities", c[0].Method, c[0].Path)
	}
	// Second call: update activity
	if c[1].Method != "PATCH" || c[1].Path != "/activity/washer" {
		t.Errorf("call 1: %s %s, want PATCH /activity/washer", c[1].Method, c[1].Path)
	}
}

func TestFieldMode_Ended(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	tr := New(fieldRule(), pw, 1, testTrackerConfig())
	defer tr.Stop()

	// Start tracking
	tr.HandleMessage(map[string]any{"running_state": "running"})
	waitForCalls(calls, mu, 2, 500*time.Millisecond)

	// End
	tr.HandleMessage(map[string]any{"running_state": "done"})

	// Two-phase end: ONGOING + ENDED (after endDelay + endDisplayTime)
	c := waitForCalls(calls, mu, 4, 500*time.Millisecond)
	if len(c) < 4 {
		t.Fatalf("expected at least 4 calls, got %d", len(c))
	}

	// Phase 1: ONGOING with end content
	if c[2].Method != "PATCH" || c[2].Path != "/activity/washer" {
		t.Errorf("call 2: %s %s, want PATCH /activity/washer", c[2].Method, c[2].Path)
	}
	// Phase 2: ENDED
	if c[3].Method != "PATCH" || c[3].Path != "/activity/washer" {
		t.Errorf("call 3: %s %s, want PATCH /activity/washer", c[3].Method, c[3].Path)
	}

	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, c[3].Body, &req)
	if req.State != "ENDED" {
		t.Errorf("final state = %q, want ENDED", req.State)
	}
}

func TestFieldMode_Ignore(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	tr := New(fieldRule(), pw, 1, testTrackerConfig())
	defer tr.Stop()

	tr.HandleMessage(map[string]any{"running_state": "idle"})

	time.Sleep(50 * time.Millisecond)
	c := testutil.GetCalls(calls, mu)
	if len(c) != 0 {
		t.Fatalf("expected 0 calls for IGNORE, got %d", len(c))
	}
}

func TestPresenceMode_AutoEnd(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	tr := New(presenceRule(), pw, 1, testTrackerConfig())
	defer tr.Stop()

	tr.HandleMessage(map[string]any{"temperature": "22.5"})

	// Wait for create + update + inactivity timeout + two-phase end
	c := waitForCalls(calls, mu, 4, 1*time.Second)
	if len(c) < 4 {
		t.Fatalf("expected at least 4 calls (create + update + 2-phase end), got %d", len(c))
	}

	// Last call should be ENDED
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, c[len(c)-1].Body, &req)
	if req.State != "ENDED" {
		t.Errorf("final state = %q, want ENDED", req.State)
	}
}

func TestDebounce(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	cfg := testTrackerConfig()
	cfg.UpdateInterval = 200 * time.Millisecond
	tr := New(fieldRule(), pw, 1, cfg)
	defer tr.Stop()

	// Send 5 rapid messages
	for i := 0; i < 5; i++ {
		tr.HandleMessage(map[string]any{"running_state": "running"})
	}

	time.Sleep(50 * time.Millisecond)
	c := testutil.GetCalls(calls, mu)

	// Should have: 1 create + 1 update (rest debounced)
	if len(c) != 2 {
		t.Fatalf("expected 2 calls (create + 1 update), got %d", len(c))
	}
}
