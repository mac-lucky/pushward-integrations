package engine

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/mqtt/internal/config"
	"github.com/mac-lucky/pushward-integrations/mqtt/internal/tracker"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func testTrackerConfig() *tracker.Config {
	return &tracker.Config{
		EndDelay:       10 * time.Millisecond,
		EndDisplayTime: 10 * time.Millisecond,
		CleanupDelay:   1 * time.Hour,
		StaleTimeout:   30 * time.Minute,
		UpdateInterval: 10 * time.Millisecond,
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

func TestRoute_ExactMatch(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(srv.URL, "hlk_test")

	rule := &config.RuleConfig{
		Name:       "Washer",
		Topic:      "zigbee2mqtt/washer",
		Slug:       "washer",
		Template:   "generic",
		Lifecycle:  "field",
		StateField: "state",
		StateMap:   map[string]string{"on": pushward.StateOngoing},
		Content:    config.ContentMapping{State: "{state}"},
	}

	tr := tracker.New(rule, pw, 1, testTrackerConfig())
	e := New([]*RuleEntry{NewRuleEntry(rule, tr)})
	defer e.Stop()

	payload, _ := json.Marshal(map[string]any{"state": "on"})
	e.Route("zigbee2mqtt/washer", payload)

	c := waitForCalls(calls, mu, 2, 500*time.Millisecond)
	if len(c) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(c))
	}

	// Should not match different topic
	e.Route("zigbee2mqtt/dryer", payload)
	time.Sleep(50 * time.Millisecond)
	c = testutil.GetCalls(calls, mu)
	if len(c) != 2 {
		t.Fatalf("expected still 2 calls after non-matching topic, got %d", len(c))
	}
}

func TestRoute_WildcardPlus(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(srv.URL, "hlk_test")

	rule := &config.RuleConfig{
		Name:       "Any Sensor",
		Topic:      "sensors/+/temperature",
		Slug:       "sensor",
		Template:   "generic",
		Lifecycle:  "field",
		StateField: "state",
		StateMap:   map[string]string{"active": pushward.StateOngoing},
		Content:    config.ContentMapping{State: "{state}"},
	}

	tr := tracker.New(rule, pw, 1, testTrackerConfig())
	e := New([]*RuleEntry{NewRuleEntry(rule, tr)})
	defer e.Stop()

	payload, _ := json.Marshal(map[string]any{"state": "active"})
	e.Route("sensors/kitchen/temperature", payload)

	c := waitForCalls(calls, mu, 2, 500*time.Millisecond)
	if len(c) < 2 {
		t.Fatalf("expected at least 2 calls for wildcard match, got %d", len(c))
	}
}

func TestRoute_WildcardHash(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(srv.URL, "hlk_test")

	rule := &config.RuleConfig{
		Name:       "All ZigBee",
		Topic:      "zigbee2mqtt/#",
		Slug:       "zigbee",
		Template:   "generic",
		Lifecycle:  "field",
		StateField: "state",
		StateMap:   map[string]string{"active": pushward.StateOngoing},
		Content:    config.ContentMapping{State: "{state}"},
	}

	tr := tracker.New(rule, pw, 1, testTrackerConfig())
	e := New([]*RuleEntry{NewRuleEntry(rule, tr)})
	defer e.Stop()

	payload, _ := json.Marshal(map[string]any{"state": "active"})
	e.Route("zigbee2mqtt/device1/status", payload)

	c := waitForCalls(calls, mu, 2, 500*time.Millisecond)
	if len(c) < 2 {
		t.Fatalf("expected at least 2 calls for # wildcard match, got %d", len(c))
	}
}

func TestRoute_VirtualFields(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(srv.URL, "hlk_test")

	rule := &config.RuleConfig{
		Name:       "Sensor",
		Topic:      "sensors/+/data",
		Slug:       "sensor",
		Template:   "generic",
		Lifecycle:  "field",
		StateField: "state",
		StateMap:   map[string]string{"on": pushward.StateOngoing},
		Content: config.ContentMapping{
			State:    "{state}",
			Subtitle: "{_topic_segment:1}",
		},
	}

	tr := tracker.New(rule, pw, 1, testTrackerConfig())
	e := New([]*RuleEntry{NewRuleEntry(rule, tr)})
	defer e.Stop()

	payload, _ := json.Marshal(map[string]any{"state": "on"})
	e.Route("sensors/living-room/data", payload)

	c := waitForCalls(calls, mu, 2, 500*time.Millisecond)
	if len(c) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(c))
	}

	// Check that subtitle used the virtual field
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, c[1].Body, &req)
	if req.Content.Subtitle != "living-room" {
		t.Errorf("Subtitle = %q, want living-room", req.Content.Subtitle)
	}
}

func TestTopicMatches(t *testing.T) {
	tests := []struct {
		pattern string
		topic   string
		want    bool
	}{
		{"a/b/c", "a/b/c", true},
		{"a/b/c", "a/b/d", false},
		{"a/+/c", "a/b/c", true},
		{"a/+/c", "a/b/d", false},
		{"+/+/+", "a/b/c", true},
		{"a/#", "a/b/c", true},
		{"a/#", "a", false},
		{"#", "a/b/c", true},
		{"a/b", "a/b/c", false},
	}

	for _, tt := range tests {
		got := topicMatches(tt.pattern, tt.topic)
		if got != tt.want {
			t.Errorf("topicMatches(%q, %q) = %v, want %v", tt.pattern, tt.topic, got, tt.want)
		}
	}
}
