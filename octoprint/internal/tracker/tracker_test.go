package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/octoprint/internal/api"
	"github.com/mac-lucky/pushward-integrations/octoprint/internal/config"
	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// --- Mock OctoPrint API ---

type mockOctoPrint struct {
	mu      sync.Mutex
	job     api.JobResponse
	printer api.PrinterResponse
}

func newMockOctoPrint() *mockOctoPrint {
	return &mockOctoPrint{}
}

func (m *mockOctoPrint) GetJob(_ context.Context) (*api.JobResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.job
	return &j, nil
}

func (m *mockOctoPrint) GetPrinter(_ context.Context) (*api.PrinterResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.printer
	return &p, nil
}

func (m *mockOctoPrint) SetJob(j api.JobResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.job = j
}

func (m *mockOctoPrint) SetPrinter(p api.PrinterResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.printer = p
}

// --- Test helpers ---

func testConfig() *config.Config {
	return &config.Config{
		Server: sharedconfig.ServerConfig{Address: ":8090"},
		OctoPrint: config.OctoPrintConfig{
			URL:    "http://localhost:5000",
			APIKey: "test-api-key",
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
			Interval: 20 * time.Millisecond,
		},
	}
}

func printingJob(filename string, completion float64, timeLeft int) api.JobResponse {
	comp := completion
	return api.JobResponse{
		Job: api.JobInfo{File: api.FileInfo{Name: filename}},
		Progress: api.ProgressInfo{
			Completion:    &comp,
			PrintTimeLeft: &timeLeft,
		},
		State: "Printing",
	}
}

func operationalJob() api.JobResponse {
	return api.JobResponse{
		State: "Operational",
	}
}

func errorJob(filename string, completion float64) api.JobResponse {
	comp := completion
	return api.JobResponse{
		Job:      api.JobInfo{File: api.FileInfo{Name: filename}},
		Progress: api.ProgressInfo{Completion: &comp},
		State:    "Error",
	}
}

func pausedJob(filename string, completion float64) api.JobResponse {
	comp := completion
	return api.JobResponse{
		Job:      api.JobInfo{File: api.FileInfo{Name: filename}},
		Progress: api.ProgressInfo{Completion: &comp},
		State:    "Paused",
	}
}

// --- Tests ---

func TestWebhookStartsTracking(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	octo := newMockOctoPrint()
	pw := pushward.NewClient(srv.URL, "hlk_test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(ctx, cfg, octo, pw)

	// Set OctoPrint to printing state
	octo.SetJob(printingJob("Benchy.gcode", 25.0, 1800))

	// Send webhook
	payload := `{"topic": "PrintStarted"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	tr.HandleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for first poll
	time.Sleep(100 * time.Millisecond)
	cancel()
	tr.Wait()

	recorded := testutil.GetCalls(calls, mu)
	// create + "Starting..." ONGOING + at least one progress ONGOING
	if len(recorded) < 3 {
		t.Fatalf("expected >= 3 calls, got %d", len(recorded))
	}

	// Verify create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Slug != "octoprint" {
		t.Errorf("expected slug octoprint, got %s", createReq.Slug)
	}

	// Verify starting update
	var starting pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &starting)
	if starting.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", starting.State)
	}
	if starting.Content.State != "Starting..." {
		t.Errorf("expected state 'Starting...', got %s", starting.Content.State)
	}
}

func TestWebhookSecretValidation(t *testing.T) {
	srv, _, _ := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.OctoPrint.WebhookSecret = "my-secret"
	octo := newMockOctoPrint()
	pw := pushward.NewClient(srv.URL, "hlk_test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(ctx, cfg, octo, pw)

	// Without secret
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"topic":"PrintStarted"}`))
	w := httptest.NewRecorder()
	tr.HandleWebhook(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without secret, got %d", w.Code)
	}

	// With wrong secret
	req2 := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"topic":"PrintStarted"}`))
	req2.Header.Set("X-Webhook-Secret", "wrong")
	w2 := httptest.NewRecorder()
	tr.HandleWebhook(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong secret, got %d", w2.Code)
	}

	// With correct secret
	req3 := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"topic":"PrintStarted"}`))
	req3.Header.Set("X-Webhook-Secret", "my-secret")
	w3 := httptest.NewRecorder()
	octo.SetJob(printingJob("test.gcode", 0, 3600))
	tr.HandleWebhook(w3, req3)
	if w3.Code != http.StatusOK {
		t.Errorf("expected 200 with correct secret, got %d", w3.Code)
	}
	cancel()
	tr.Wait()
}

func TestDuplicateWebhookIgnored(t *testing.T) {
	srv, _, _ := testutil.MockPushWardServer(t)
	cfg := testConfig()
	octo := newMockOctoPrint()
	pw := pushward.NewClient(srv.URL, "hlk_test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(ctx, cfg, octo, pw)
	octo.SetJob(printingJob("test.gcode", 10.0, 3600))

	// First webhook starts tracking
	payload := `{"topic": "PrintStarted"}`
	req1 := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w1 := httptest.NewRecorder()
	tr.HandleWebhook(w1, req1)

	// Second webhook should be ignored
	req2 := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w2 := httptest.NewRecorder()
	tr.HandleWebhook(w2, req2)

	var resp map[string]string
	json.NewDecoder(w2.Body).Decode(&resp)
	if resp["status"] != "already_tracking" {
		t.Errorf("expected already_tracking, got %s", resp["status"])
	}

	cancel()
	tr.Wait()
}

func TestTrackingProgressToComplete(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	octo := newMockOctoPrint()
	pw := pushward.NewClient(srv.URL, "hlk_test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(ctx, cfg, octo, pw)

	// Start printing
	octo.SetJob(printingJob("Benchy.gcode", 50.0, 900))

	payload := `{"topic": "PrintStarted"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	tr.HandleWebhook(w, req)

	// Wait for a couple polls
	time.Sleep(60 * time.Millisecond)

	// Finish the print
	octo.SetJob(operationalJob())

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)
	tr.Wait()

	recorded := testutil.GetCalls(calls, mu)

	// Find the Complete ONGOING and ENDED
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

func TestTrackingError(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	octo := newMockOctoPrint()
	pw := pushward.NewClient(srv.URL, "hlk_test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(ctx, cfg, octo, pw)
	octo.SetJob(printingJob("Test.gcode", 30.0, 1200))

	payload := `{"topic": "PrintStarted"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	tr.HandleWebhook(w, req)

	// Wait for a poll
	time.Sleep(60 * time.Millisecond)

	// Error out
	octo.SetJob(errorJob("Test.gcode", 30.0))

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)
	tr.Wait()

	recorded := testutil.GetCalls(calls, mu)

	var foundFailed bool
	for _, c := range recorded {
		if c.Method != "PATCH" {
			continue
		}
		var req pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &req)
		if req.Content.State == "Failed" && req.State == pushward.StateEnded {
			foundFailed = true
			if req.Content.AccentColor != "#FF3B30" {
				t.Errorf("failed accent = %q, want #FF3B30", req.Content.AccentColor)
			}
		}
	}
	if !foundFailed {
		t.Error("missing ENDED with Failed content")
	}
}

func TestTrackingPaused(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	octo := newMockOctoPrint()
	pw := pushward.NewClient(srv.URL, "hlk_test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(ctx, cfg, octo, pw)
	octo.SetJob(printingJob("Part.gcode", 40.0, 600))

	payload := `{"topic": "PrintStarted"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	tr.HandleWebhook(w, req)

	// Wait for a poll
	time.Sleep(60 * time.Millisecond)

	// Pause the print
	octo.SetJob(pausedJob("Part.gcode", 40.0))

	// Wait for paused update
	time.Sleep(60 * time.Millisecond)
	cancel()
	tr.Wait()

	recorded := testutil.GetCalls(calls, mu)

	var foundPaused bool
	for _, c := range recorded {
		if c.Method != "PATCH" {
			continue
		}
		var req pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &req)
		if req.Content.State == "Paused" && req.Content.Icon == "pause.circle.fill" {
			foundPaused = true
			if req.Content.AccentColor != "#FF9500" {
				t.Errorf("paused accent = %q, want #FF9500", req.Content.AccentColor)
			}
		}
	}
	if !foundPaused {
		t.Error("missing Paused update")
	}
}

func TestResumeIfActive_Printing(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	octo := newMockOctoPrint()
	pw := pushward.NewClient(srv.URL, "hlk_test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(ctx, cfg, octo, pw)
	octo.SetJob(printingJob("Benchy.gcode", 60.0, 300))

	if !tr.ResumeIfActive() {
		t.Fatal("expected ResumeIfActive to return true for printing state")
	}

	// Wait for one poll
	time.Sleep(60 * time.Millisecond)
	cancel()
	tr.Wait()

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("expected >= 2 calls, got %d", len(recorded))
	}
}

func TestResumeIfActive_Idle(t *testing.T) {
	cfg := testConfig()
	octo := newMockOctoPrint()
	pw := pushward.NewClient("http://localhost", "hlk_test")

	tr := New(context.Background(), cfg, octo, pw)
	octo.SetJob(operationalJob())

	if tr.ResumeIfActive() {
		t.Fatal("expected ResumeIfActive to return false for idle state")
	}
}

func TestNormalizeEvent(t *testing.T) {
	tests := []struct {
		topic string
		state string
		want  string
	}{
		{"PrintStarted", "", "started"},
		{"PrintDone", "", "done"},
		{"PrintFailed", "", "failed"},
		{"PrintCancelled", "", "cancelled"},
		{"PrintPaused", "", "paused"},
		{"PrintResumed", "", "resumed"},
		{"", "Started", "started"},
		{"", "Done", "done"},
		{"UnknownTopic", "", ""},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%s", tt.topic, tt.state), func(t *testing.T) {
			got := normalizeEvent(tt.topic, tt.state)
			if got != tt.want {
				t.Errorf("normalizeEvent(%q, %q) = %q, want %q", tt.topic, tt.state, got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"Hello World!", 8, "Hello..."},
		{"hi", 3, "hi"},
		{"Hello", 3, "Hel"},
		{"Hello", 1, "H"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_%d", tt.input, tt.maxLen), func(t *testing.T) {
			got := truncate(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestBuildSubtitle_WithTemp(t *testing.T) {
	octo := newMockOctoPrint()
	octo.SetPrinter(api.PrinterResponse{
		Temperature: api.TemperatureData{
			Tool0: &api.ToolTemp{Actual: 210.0, Target: 210.0},
		},
	})

	got := buildSubtitle(context.Background(), octo, "Benchy.gcode")
	want := "Benchy.gcode · 210/210°C"
	if got != want {
		t.Errorf("buildSubtitle = %q, want %q", got, want)
	}
}

func TestBuildSubtitle_NoTemp(t *testing.T) {
	octo := newMockOctoPrint()
	octo.SetPrinter(api.PrinterResponse{
		Temperature: api.TemperatureData{},
	})

	got := buildSubtitle(context.Background(), octo, "Benchy.gcode")
	if got != "Benchy.gcode" {
		t.Errorf("buildSubtitle = %q, want Benchy.gcode", got)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	cfg := testConfig()
	octo := newMockOctoPrint()
	pw := pushward.NewClient("http://localhost", "hlk_test")
	tr := New(context.Background(), cfg, octo, pw)

	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	w := httptest.NewRecorder()
	tr.HandleWebhook(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}
