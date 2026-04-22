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

	"github.com/mac-lucky/pushward-integrations/sabnzbd/internal/config"
	"github.com/mac-lucky/pushward-integrations/sabnzbd/internal/sabnzbd"
	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func testConfig() *config.Config {
	return &config.Config{
		SABnzbd: config.SABnzbdConfig{
			URL:      "http://placeholder",
			APIKey:   "test-key",
			Template: "generic",
			Timeline: sharedconfig.TimelineConfig{
				Smoothing: pushward.BoolPtr(true),
				Scale:     "linear",
				Decimals:  pushward.IntPtr(0),
			},
		},
		PushWard: sharedconfig.PushWardConfig{
			Priority:       1,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
		Polling: config.PollingConfig{
			Interval: 10 * time.Millisecond,
		},
	}
}

// mockSABnzbd creates a mock SABnzbd API server that returns configurable queue/history.
func mockSABnzbd(t *testing.T) (*httptest.Server, *sabMock) {
	t.Helper()
	m := &sabMock{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("mode")
		m.mu.Lock()
		defer m.mu.Unlock()
		switch mode {
		case "queue":
			if m.queueErr {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(m.queueResp)
		case "history":
			if m.historyErr {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(m.historyResp)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, m
}

type sabMock struct {
	mu          sync.Mutex
	queueResp   sabnzbd.QueueResponse
	historyResp sabnzbd.HistoryResponse
	queueErr    bool
	historyErr  bool
}

func (m *sabMock) setQueue(q sabnzbd.Queue) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueResp = sabnzbd.QueueResponse{Queue: q}
}

func (m *sabMock) setHistory(h sabnzbd.History) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.historyResp = sabnzbd.HistoryResponse{History: h}
}

// --- HandleWebhook tests ---

func TestHandleWebhook_ValidPost(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, _, _ := testutil.MockPushWardServer(t)

	// Queue goes idle immediately so tracking finishes quickly
	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})
	sabMk.setHistory(sabnzbd.History{})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(cfg, sab, pw)
	handler := tr.WebhookHandler(ctx)

	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "tracking_started") {
		t.Fatalf("expected tracking_started in body, got: %s", w.Body.String())
	}

	tr.Wait()
}

func TestHandleWebhook_WrongMethod(t *testing.T) {
	cfg := testConfig()
	ctx := context.Background()
	tr := New(cfg, nil, nil)
	handler := tr.WebhookHandler(ctx)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/webhook", nil)
		w := httptest.NewRecorder()
		handler(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: expected 405, got %d", method, w.Code)
		}
	}
}

func TestHandleWebhook_SecretValidation(t *testing.T) {
	cfg := testConfig()
	cfg.SABnzbd.WebhookSecret = "my-secret"
	ctx := context.Background()

	// Use real mock servers so valid-secret test can proceed without nil panics
	sabSrv, sabMk := mockSABnzbd(t)
	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})
	sabMk.setHistory(sabnzbd.History{})
	pwSrv, _, _ := testutil.MockPushWardServer(t)

	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")

	tr := New(cfg, sab, pw)
	handler := tr.WebhookHandler(ctx)

	// Wrong secret → 401
	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	req.Header.Set("X-Webhook-Secret", "wrong-secret")
	w := httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: expected 401, got %d", w.Code)
	}

	// Missing secret → 401
	req = httptest.NewRequest(http.MethodPost, "/webhook", nil)
	w = httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing secret: expected 401, got %d", w.Code)
	}

	// Correct secret → 200
	req = httptest.NewRequest(http.MethodPost, "/webhook", nil)
	req.Header.Set("X-Webhook-Secret", "my-secret")
	w = httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("correct secret: expected 200, got %d", w.Code)
	}

	tr.Wait()
}

func TestHandleWebhook_AlreadyActive(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, _, _ := testutil.MockPushWardServer(t)

	// Queue goes idle so tracking finishes
	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})
	sabMk.setHistory(sabnzbd.History{})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(cfg, sab, pw)
	handler := tr.WebhookHandler(ctx)

	// First webhook → tracking_started
	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first webhook: expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "tracking_started") {
		t.Fatalf("expected tracking_started, got: %s", w.Body.String())
	}

	// Second webhook while active → already_tracking
	// Need to ensure first goroutine is still running; send immediately
	req = httptest.NewRequest(http.MethodPost, "/webhook", nil)
	w = httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("second webhook: expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "already_tracking") {
		t.Fatalf("expected already_tracking, got: %s", w.Body.String())
	}

	tr.Wait()
}

// --- parseTimeLeft tests ---

func TestParseTimeLeft(t *testing.T) {
	tests := []struct {
		input string
		want  *int
	}{
		{"0:05:30", pushward.IntPtr(330)},
		{"1:30:00", pushward.IntPtr(5400)},
		{"0:00:10", pushward.IntPtr(10)},
		{"0:00:00", pushward.IntPtr(0)},
		{"invalid", nil},
		{"1:2", nil},
		{"a:b:c", nil},
		{"", nil},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseTimeLeft(tt.input)
			if tt.want == nil {
				if got != nil {
					t.Errorf("parseTimeLeft(%q) = %d, want nil", tt.input, *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("parseTimeLeft(%q) = nil, want %d", tt.input, *tt.want)
			}
			if *got != *tt.want {
				t.Errorf("parseTimeLeft(%q) = %d, want %d", tt.input, *got, *tt.want)
			}
		})
	}
}

// --- formatDuration tests ---

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		seconds int
		want    string
	}{
		{0, "0s"},
		{30, "30s"},
		{59, "59s"},
		{60, "1m 0s"},
		{90, "1m 30s"},
		{3599, "59m 59s"},
		{3600, "1h 0m"},
		{3661, "1h 1m"},
		{7200, "2h 0m"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%ds", tt.seconds), func(t *testing.T) {
			got := formatDuration(tt.seconds)
			if got != tt.want {
				t.Errorf("formatDuration(%d) = %q, want %q", tt.seconds, got, tt.want)
			}
		})
	}
}

// --- formatSize tests ---

func TestFormatSize(t *testing.T) {
	tests := []struct {
		mb   float64
		want string
	}{
		{500, "500 MB"},
		{1023, "1023 MB"},
		{1024, "1.0 GB"},
		{2048, "2.0 GB"},
		{1536, "1.5 GB"},
		{0, "0 MB"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatSize(tt.mb)
			if got != tt.want {
				t.Errorf("formatSize(%f) = %q, want %q", tt.mb, got, tt.want)
			}
		})
	}
}

// --- Cleanup tests ---

func TestCleanup_SendsEndedUpdate(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()

	tr := New(cfg, nil, pw)
	tr.Cleanup(ctx)

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	if got[0].Method != "PATCH" {
		t.Errorf("expected PATCH, got %s", got[0].Method)
	}
	if got[0].Path != "/activity/sabnzbd" {
		t.Errorf("expected /activity/sabnzbd, got %s", got[0].Path)
	}

	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)
	if req.State != pushward.StateEnded {
		t.Errorf("expected ENDED state, got %s", req.State)
	}
}

// --- Context cancellation test ---

func TestContextCancellation_StopsTracking(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, _, _ := testutil.MockPushWardServer(t)

	// Keep queue active so tracking would run forever
	sabMk.setQueue(sabnzbd.Queue{
		Status:   "Downloading",
		MB:       "1000",
		MBLeft:   "500",
		KBPerSec: "10240",
		TimeLeft: "0:00:50",
		Slots:    []sabnzbd.QueueSlot{{Filename: "test.nzb"}},
	})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx, cancel := context.WithCancel(context.Background())

	tr := New(cfg, sab, pw)
	handler := tr.WebhookHandler(ctx)

	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Let it run briefly then cancel
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Wait should return within a reasonable time
	done := make(chan struct{})
	go func() {
		tr.Wait()
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("tracker did not stop after context cancellation")
	}
}

// --- Full tracking lifecycle test ---

func TestTrackingLifecycle_Download_PP_Complete(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, calls, mu := testutil.MockPushWardServer(t)

	// Start with active download
	sabMk.setQueue(sabnzbd.Queue{
		Status:   "Downloading",
		MB:       "500",
		MBLeft:   "250",
		KBPerSec: "51200",
		TimeLeft: "0:00:05",
		Slots:    []sabnzbd.QueueSlot{{Filename: "ubuntu-24.04.nzb"}},
	})
	sabMk.setHistory(sabnzbd.History{})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(cfg, sab, pw)
	handler := tr.WebhookHandler(ctx)

	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	// Let download tracking run for a bit
	time.Sleep(80 * time.Millisecond)

	// Transition: download done → post-processing
	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})
	sabMk.setHistory(sabnzbd.History{
		Slots: []sabnzbd.HistorySlot{{Status: "Extracting", Name: "ubuntu-24.04"}},
	})

	time.Sleep(80 * time.Millisecond)

	// Transition: PP done → completed
	sabMk.setHistory(sabnzbd.History{
		Slots: []sabnzbd.HistorySlot{{Status: "Completed", Name: "ubuntu-24.04", Bytes: 524288000, DownloadTime: 10}},
	})

	tr.Wait()

	got := testutil.GetCalls(calls, mu)
	if len(got) < 3 {
		t.Fatalf("expected at least 3 PushWard calls, got %d", len(got))
	}

	// First call should be POST /activities (create)
	if got[0].Method != "POST" || got[0].Path != "/activities" {
		t.Errorf("first call: expected POST /activities, got %s %s", got[0].Method, got[0].Path)
	}

	// Last call should be PATCH with ENDED
	last := got[len(got)-1]
	if last.Method != "PATCH" || last.Path != "/activity/sabnzbd" {
		t.Errorf("last call: expected PATCH /activity/sabnzbd, got %s %s", last.Method, last.Path)
	}
	var lastReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, last.Body, &lastReq)
	if lastReq.State != pushward.StateEnded {
		t.Errorf("last update state: expected ENDED, got %s", lastReq.State)
	}

	// Check that some ONGOING updates were sent with download progress
	hasOngoing := false
	for _, c := range got {
		if c.Method == "PATCH" {
			var r pushward.UpdateRequest
			testutil.UnmarshalBody(t, c.Body, &r)
			if r.State == pushward.StateOngoing {
				hasOngoing = true
				break
			}
		}
	}
	if !hasOngoing {
		t.Error("expected at least one ONGOING update")
	}
}

// --- ResumeIfActive tests ---

func TestResumeIfActive_ActiveDownload(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, _, _ := testutil.MockPushWardServer(t)

	sabMk.setQueue(sabnzbd.Queue{Status: "Downloading", MB: "100", MBLeft: "50"})
	sabMk.setHistory(sabnzbd.History{})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(cfg, sab, pw)
	resumed := tr.ResumeIfActive(ctx)
	if !resumed {
		t.Fatal("expected ResumeIfActive to return true")
	}

	// Let it settle, then stop by making queue idle
	time.Sleep(50 * time.Millisecond)
	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})
	sabMk.setHistory(sabnzbd.History{
		Slots: []sabnzbd.HistorySlot{{Status: "Completed", Name: "test-file", Bytes: 104857600, DownloadTime: 5}},
	})

	tr.Wait()
}

func TestResumeIfActive_ActivePostProcessing(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, _, _ := testutil.MockPushWardServer(t)

	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})
	sabMk.setHistory(sabnzbd.History{
		Slots: []sabnzbd.HistorySlot{{Status: "Verifying", Name: "test-file"}},
	})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(cfg, sab, pw)
	resumed := tr.ResumeIfActive(ctx)
	if !resumed {
		t.Fatal("expected ResumeIfActive to return true for active PP")
	}

	// Transition: PP done
	time.Sleep(50 * time.Millisecond)
	sabMk.setHistory(sabnzbd.History{
		Slots: []sabnzbd.HistorySlot{{Status: "Completed", Name: "test-file", Bytes: 104857600, DownloadTime: 5}},
	})

	tr.Wait()
}

func TestResumeIfActive_NoActivity(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, _, _ := testutil.MockPushWardServer(t)

	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})
	sabMk.setHistory(sabnzbd.History{})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()

	tr := New(cfg, sab, pw)
	resumed := tr.ResumeIfActive(ctx)
	if resumed {
		t.Fatal("expected ResumeIfActive to return false when idle")
	}
}

// --- sendDownloadProgress tests ---

func TestSendDownloadProgress_Paused(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	queue := &sabnzbd.Queue{
		Status:   "Paused",
		MB:       "1000",
		MBLeft:   "500",
		KBPerSec: "0",
		TimeLeft: "0:00:00",
		Slots:    []sabnzbd.QueueSlot{{Filename: "test.nzb"}},
	}

	result := tr.sendDownloadProgress(ctx, queue)
	if !result {
		t.Fatal("expected sendDownloadProgress to return true for Paused")
	}

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)
	if req.Content.State != "Paused" {
		t.Errorf("expected state Paused, got %s", req.Content.State)
	}
	if req.Content.AccentColor != "blue" {
		t.Errorf("expected blue accent, got %s", req.Content.AccentColor)
	}
}

func TestSendDownloadProgress_Idle_ReturnsFalse(t *testing.T) {
	cfg := testConfig()
	ctx := context.Background()
	tr := New(cfg, nil, nil)

	queue := &sabnzbd.Queue{
		Status: "Idle",
		MB:     "0",
		MBLeft: "0",
	}

	result := tr.sendDownloadProgress(ctx, queue)
	if result {
		t.Fatal("expected sendDownloadProgress to return false for Idle")
	}
}

func TestSendDownloadProgress_MultipleSlots(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	queue := &sabnzbd.Queue{
		Status:   "Downloading",
		MB:       "2000",
		MBLeft:   "1000",
		KBPerSec: "51200",
		TimeLeft: "0:00:20",
		Slots: []sabnzbd.QueueSlot{
			{Filename: "first-download.nzb"},
			{Filename: "second-download.nzb"},
			{Filename: "third-download.nzb"},
		},
	}

	result := tr.sendDownloadProgress(ctx, queue)
	if !result {
		t.Fatal("expected true for active download")
	}

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)
	// Multiple slots → "X/Y · name" format (current is first slot of 3).
	if !strings.HasPrefix(req.Content.Subtitle, "1/3 · ") {
		t.Errorf("expected subtitle prefixed with '1/3 · ', got %q", req.Content.Subtitle)
	}
	if !strings.Contains(req.Content.Subtitle, "first-download") {
		t.Errorf("expected subtitle to contain current filename, got %q", req.Content.Subtitle)
	}
}

// --- Resumed tracking skips two-phase end ---

func TestResumedTracking_SkipsTwoPhaseEnd(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, calls, mu := testutil.MockPushWardServer(t)

	// Queue idle from start so tracking ends immediately
	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})
	sabMk.setHistory(sabnzbd.History{
		Slots: []sabnzbd.HistorySlot{{Status: "Completed", Name: "test-file", Bytes: 104857600, DownloadTime: 5}},
	})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()

	tr := New(cfg, sab, pw)

	// Simulate resumed tracking (directly call track with resumed=true)
	// We can't easily call track directly, so we use ResumeIfActive with pre-active queue
	// Instead, set queue active, resume, then immediately go idle
	sabMk.setQueue(sabnzbd.Queue{Status: "Downloading", MB: "100", MBLeft: "50"})
	resumed := tr.ResumeIfActive(ctx)
	if !resumed {
		t.Fatal("expected resume")
	}

	// Immediately make queue idle
	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})

	tr.Wait()

	// For resumed sessions, the final update should be a single ENDED (no two-phase)
	got := testutil.GetCalls(calls, mu)
	// Find the last PATCH call
	var endedCount int
	for _, c := range got {
		if c.Method == "PATCH" {
			var req pushward.UpdateRequest
			testutil.UnmarshalBody(t, c.Body, &req)
			if req.State == pushward.StateEnded {
				endedCount++
			}
		}
	}
	// Resumed sessions should send exactly 1 ENDED (not 2 like two-phase)
	if endedCount != 1 {
		t.Errorf("resumed session: expected exactly 1 ENDED call, got %d", endedCount)
	}
}

// --- Timeline template tests ---

func TestSendDownloadProgress_Timeline_SendsValueAndUnit(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SABnzbd.Template = "timeline"
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	queue := &sabnzbd.Queue{
		Status:   "Downloading",
		MB:       "1000",
		MBLeft:   "500",
		KBPerSec: "51200", // 50 MB/s
		TimeLeft: "0:00:10",
		Slots:    []sabnzbd.QueueSlot{{Filename: "test.nzb"}},
	}

	result := tr.sendDownloadProgress(ctx, queue)
	if !result {
		t.Fatal("expected true for active download")
	}

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)

	if req.Content.Template != "timeline" {
		t.Errorf("expected template timeline, got %s", req.Content.Template)
	}
	if values := testutil.RequireValueMap(t, req.Content.Value); values == nil {
		// already failed
	} else if v, ok := values[seriesKey]; !ok {
		t.Fatal("expected value map with 'Download' key")
	} else if v != 50.0 {
		t.Errorf("expected Download value 50.0, got %.1f", v)
	}
	if u, ok := req.Content.Units[seriesKey]; !ok {
		t.Fatal("expected units map with 'Download' key")
	} else if u != "MB/s" {
		t.Errorf("expected Download unit MB/s, got %s", u)
	}
	if req.Content.State != "50.0 MB/s" {
		t.Errorf("expected state to be speed for timeline (same as generic), got %s", req.Content.State)
	}
}

func TestSendDownloadProgress_Generic_NoValueOrUnit(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	// Template is "generic" by default from testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	queue := &sabnzbd.Queue{
		Status:   "Downloading",
		MB:       "1000",
		MBLeft:   "500",
		KBPerSec: "51200",
		TimeLeft: "0:00:10",
		Slots:    []sabnzbd.QueueSlot{{Filename: "test.nzb"}},
	}

	tr.sendDownloadProgress(ctx, queue)

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)

	if req.Content.Template != "generic" {
		t.Errorf("expected template generic, got %s", req.Content.Template)
	}
	if req.Content.Value != nil {
		t.Errorf("expected no value for generic template, got %v", req.Content.Value)
	}
	if req.Content.Unit != "" {
		t.Errorf("expected no unit for generic template, got %s", req.Content.Unit)
	}
	if req.Content.State != "50.0 MB/s" {
		t.Errorf("expected state to be speed for generic, got %s", req.Content.State)
	}
}

func TestTimeline_NonDownloadPhase_SendsZeroValue(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SABnzbd.Template = "timeline"
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	// Non-download sends (e.g. "Starting...", PP) pass nil for value. The
	// server rejects timeline payloads without a labeled value map, so the
	// integration substitutes 0 to keep updates accepted while the sparkline
	// tapers cleanly to zero.
	tr.send(ctx, 0.0, "Starting...", "arrow.down.circle", "blue", nil, "", pushward.StateOngoing, nil)

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)

	if req.Content.Template != "timeline" {
		t.Errorf("expected timeline template, got %s", req.Content.Template)
	}
	if values := testutil.RequireValueMap(t, req.Content.Value); values == nil {
		// already failed
	} else if v, ok := values[seriesKey]; !ok || v != 0 {
		t.Errorf("expected value[%q]=0 for non-download phase, got %v (ok=%v)", seriesKey, v, ok)
	}
	if u := req.Content.Units[seriesKey]; u != "MB/s" {
		t.Errorf("expected units[%q]=MB/s, got %q", seriesKey, u)
	}
	if req.Content.History != nil {
		t.Errorf("expected no history seeded for zero sample, got %v", req.Content.History)
	}
}

func TestSendDownloadProgress_Timeline_Paused_SendsZeroValue(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SABnzbd.Template = "timeline"
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	queue := &sabnzbd.Queue{
		Status:   "Paused",
		MB:       "1000",
		MBLeft:   "500",
		KBPerSec: "0",
		TimeLeft: "0:00:00",
		Slots:    []sabnzbd.QueueSlot{{Filename: "test.nzb"}},
	}

	result := tr.sendDownloadProgress(ctx, queue)
	if !result {
		t.Fatal("expected true for paused download")
	}

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)

	if req.Content.Template != "timeline" {
		t.Errorf("expected timeline template for paused download, got %s", req.Content.Template)
	}
	if values := testutil.RequireValueMap(t, req.Content.Value); values == nil {
		// already failed
	} else if v, ok := values[seriesKey]; !ok {
		t.Fatal("expected value map with 'Speed' key for paused")
	} else if v != 0 {
		t.Errorf("expected Speed value 0 for paused, got %f", v)
	}
	if u, ok := req.Content.Units[seriesKey]; !ok {
		t.Fatal("expected units map with 'Speed' key for paused")
	} else if u != "MB/s" {
		t.Errorf("expected Speed unit MB/s, got %s", u)
	}
	if req.Content.State != "Paused" {
		t.Errorf("expected state Paused for timeline paused, got %s", req.Content.State)
	}
}

func TestTimeline_FullLifecycle(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, calls, mu := testutil.MockPushWardServer(t)

	sabMk.setQueue(sabnzbd.Queue{
		Status:   "Downloading",
		MB:       "500",
		MBLeft:   "250",
		KBPerSec: "51200",
		TimeLeft: "0:00:05",
		Slots:    []sabnzbd.QueueSlot{{Filename: "test.nzb"}},
	})
	sabMk.setHistory(sabnzbd.History{})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	cfg.SABnzbd.Template = "timeline"
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(cfg, sab, pw)

	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	w := httptest.NewRecorder()
	tr.WebhookHandler(ctx)(w, req)

	// Let download tracking run briefly
	time.Sleep(80 * time.Millisecond)

	// Transition: download done → completed
	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})
	sabMk.setHistory(sabnzbd.History{
		Slots: []sabnzbd.HistorySlot{{Status: "Completed", Name: "test-file", Bytes: 524288000, DownloadTime: 10}},
	})

	tr.Wait()

	got := testutil.GetCalls(calls, mu)

	// Download-phase updates must have values[seriesKey] with units
	var hasPositiveValue bool
	for _, c := range got {
		if c.Method == "PATCH" {
			var r pushward.UpdateRequest
			testutil.UnmarshalBody(t, c.Body, &r)
			if r.Content.Template != "timeline" {
				t.Errorf("expected timeline template, got %s", r.Content.Template)
			}
			if r.Content.Value == nil {
				continue // PP and non-download phases skip value
			}
			values := testutil.RequireValueMap(t, r.Content.Value)
			if values == nil {
				continue
			}
			if v, ok := values[seriesKey]; ok && v > 0 {
				hasPositiveValue = true
			}
		}
	}
	if !hasPositiveValue {
		t.Error("expected at least one timeline update with positive Speed value (download phase)")
	}

	// Last ENDED update: subtitle should contain filename, state should have stats
	last := got[len(got)-1]
	var lastReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, last.Body, &lastReq)
	if lastReq.State != pushward.StateEnded {
		t.Errorf("last update should be ENDED, got %s", lastReq.State)
	}
	if lastReq.Content.Template != "timeline" {
		t.Errorf("summary ENDED should use timeline, got %s", lastReq.Content.Template)
	}
	if !strings.Contains(lastReq.Content.State, "MB/s avg") {
		t.Errorf("completion state should contain 'MB/s avg', got %s", lastReq.Content.State)
	}
	if lastReq.Content.Subtitle != "test-file" {
		t.Errorf("completion subtitle should be filename, got %s", lastReq.Content.Subtitle)
	}
	// Completion must keep the "Speed" series (not switch to a new key) so the
	// server preserves the accumulated download history instead of pruning it.
	if lastReq.Content.Value == nil {
		t.Fatal("completion should include a value map to keep the Speed series")
	}
	values := testutil.RequireValueMap(t, lastReq.Content.Value)
	if values == nil {
		t.Fatal("completion value must be a map[string]float64")
	}
	if _, ok := values[seriesKey]; !ok {
		t.Errorf("completion value should keep %q key, got %v", seriesKey, values)
	}
	if _, ok := values["Avg"]; ok {
		t.Errorf("completion value must not introduce an 'Avg' key (would prune %q history), got %v", seriesKey, values)
	}
}

func TestTimeline_HistorySeeding(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SABnzbd.Template = "timeline"
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	speed := pushward.Float64Ptr(50.0)

	// First send with positive value should seed history
	tr.send(ctx, 0.5, "50.0 MB/s", "arrow.down.circle.fill", "blue", nil, "test.nzb", pushward.StateOngoing, speed)

	// Second send should NOT include history
	tr.send(ctx, 0.6, "50.0 MB/s", "arrow.down.circle.fill", "blue", nil, "test.nzb", pushward.StateOngoing, speed)

	got := testutil.GetCalls(calls, mu)
	patchCalls := 0
	for _, c := range got {
		if c.Method == "PATCH" {
			var req pushward.UpdateRequest
			testutil.UnmarshalBody(t, c.Body, &req)

			if patchCalls == 0 {
				// First update: history should be seeded
				if req.Content.History == nil {
					t.Error("first timeline update should seed history")
				} else {
					pts, ok := req.Content.History[seriesKey]
					if !ok {
						t.Error("history should have 'Download' key matching values series")
					} else if len(pts) != 2 {
						t.Errorf("expected 2 seed points, got %d", len(pts))
					}
				}
			} else {
				// Subsequent updates: no history
				if req.Content.History != nil {
					t.Error("subsequent timeline updates should not include history")
				}
			}
			patchCalls++
		}
	}
	if patchCalls < 2 {
		t.Fatalf("expected at least 2 PATCH calls, got %d", patchCalls)
	}
}

func TestTimeline_DisplaySettings(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SABnzbd.Template = "timeline"
	cfg.SABnzbd.Timeline = sharedconfig.TimelineConfig{
		Smoothing: pushward.BoolPtr(true),
		Scale:     "logarithmic",
		Decimals:  pushward.IntPtr(2),
	}
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	tr.send(ctx, 0.5, "50.0 MB/s", "arrow.down.circle.fill", "blue", nil, "test.nzb", pushward.StateOngoing, pushward.Float64Ptr(50.0))

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)

	if req.Content.Smoothing == nil || !*req.Content.Smoothing {
		t.Error("expected smoothing=true from config")
	}
	if req.Content.Scale != "logarithmic" {
		t.Errorf("expected scale=logarithmic, got %s", req.Content.Scale)
	}
	if req.Content.Decimals == nil || *req.Content.Decimals != 2 {
		t.Error("expected decimals=2 from config")
	}
}

// --- Change-detection guard tests ---

func downloadingQueue(kbPerSec string, mbLeft string) *sabnzbd.Queue {
	return &sabnzbd.Queue{
		Status:   "Downloading",
		MB:       "1000",
		MBLeft:   mbLeft,
		KBPerSec: kbPerSec,
		TimeLeft: "0:00:10",
		Slots:    []sabnzbd.QueueSlot{{Filename: "test.nzb"}},
	}
}

func TestSendDownloadProgress_SkipsUnchangedPoll(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	q := downloadingQueue("51200", "500") // 50 MB/s, progress 0.5
	tr.sendDownloadProgress(ctx, q)
	tr.sendDownloadProgress(ctx, q) // identical — should be deduped

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 PATCH (second poll deduped), got %d", len(got))
	}
}

func TestSendDownloadProgress_HeartbeatAfterInterval(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	q := downloadingQueue("51200", "500")
	tr.sendDownloadProgress(ctx, q) // first poll, sends
	// Rewind lastSendTime past the heartbeat interval.
	tr.lastSendTime = time.Now().Add(-heartbeatInterval - time.Second)
	tr.sendDownloadProgress(ctx, q) // unchanged, but heartbeat due → should send

	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 PATCH calls (heartbeat fires), got %d", len(got))
	}
}

func TestSendDownloadProgress_SpeedBoundary(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	// 45.1 MB/s → rounds to 45
	tr.sendDownloadProgress(ctx, downloadingQueue("46182", "500"))
	// 45.4 MB/s → still rounds to 45, and progress unchanged → dedup
	tr.sendDownloadProgress(ctx, downloadingQueue("46489", "500"))
	// 45.6 MB/s → rounds to 46, crosses boundary → send
	tr.sendDownloadProgress(ctx, downloadingQueue("46694", "500"))

	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 PATCH calls (boundary cross), got %d", len(got))
	}
}

func TestSendDownloadProgress_ProgressBucket(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	// progress 0.500 (mbLeft=500/1000)
	tr.sendDownloadProgress(ctx, downloadingQueue("51200", "500"))
	// progress 0.515 (mbLeft=485/1000) — <2%, speed identical → dedup
	tr.sendDownloadProgress(ctx, downloadingQueue("51200", "485"))
	// progress 0.525 (mbLeft=475/1000) — >=2% vs last sent (0.500) → send
	tr.sendDownloadProgress(ctx, downloadingQueue("51200", "475"))

	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 PATCH calls (progress bucket crossed), got %d", len(got))
	}
}

func TestSendDownloadProgress_SubtitleChange(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	q1 := downloadingQueue("51200", "500")
	q1.Slots = []sabnzbd.QueueSlot{{Filename: "first.nzb"}}
	tr.sendDownloadProgress(ctx, q1)

	q2 := downloadingQueue("51200", "500")
	q2.Slots = []sabnzbd.QueueSlot{{Filename: "second.nzb"}}
	tr.sendDownloadProgress(ctx, q2) // filename changed → send

	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 PATCH calls (subtitle change), got %d", len(got))
	}
}

func TestSendDownloadProgress_PausedDedupsRepeatedPolls(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	pausedQ := &sabnzbd.Queue{
		Status: "Paused", MB: "1000", MBLeft: "500", KBPerSec: "0",
		TimeLeft: "0:00:00", Slots: []sabnzbd.QueueSlot{{Filename: "test.nzb"}},
	}
	tr.sendDownloadProgress(ctx, pausedQ)
	tr.sendDownloadProgress(ctx, pausedQ) // identical pause state → dedup

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 PATCH (second paused poll deduped), got %d", len(got))
	}
}

func TestSendDownloadProgress_PauseResumeTransitionsSend(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	// Downloading → Paused → Downloading. Each transition changes mode → send.
	tr.sendDownloadProgress(ctx, downloadingQueue("51200", "500"))
	tr.sendDownloadProgress(ctx, &sabnzbd.Queue{
		Status: "Paused", MB: "1000", MBLeft: "500", KBPerSec: "0",
		TimeLeft: "0:00:00", Slots: []sabnzbd.QueueSlot{{Filename: "test.nzb"}},
	})
	tr.sendDownloadProgress(ctx, downloadingQueue("51200", "500"))

	got := testutil.GetCalls(calls, mu)
	if len(got) != 3 {
		t.Fatalf("expected 3 PATCH calls (mode transitions), got %d", len(got))
	}
}

func TestShouldSend_PPStageTransitions(t *testing.T) {
	cfg := testConfig()
	tr := New(cfg, nil, nil)

	// First PP poll always sends.
	if !tr.shouldSend(1.0, 0, "Verifying", "ubuntu-24.04") {
		t.Fatal("first PP poll should send")
	}
	tr.recordSent(1.0, 0, "Verifying", "ubuntu-24.04")

	// Same stage, same subtitle → dedup.
	if tr.shouldSend(1.0, 0, "Verifying", "ubuntu-24.04") {
		t.Fatal("repeated same-stage poll should dedup")
	}

	// Stage change → send.
	if !tr.shouldSend(1.0, 0, "Repairing", "ubuntu-24.04") {
		t.Fatal("stage transition should send")
	}
	tr.recordSent(1.0, 0, "Repairing", "ubuntu-24.04")

	// Heartbeat fires after interval even if stage unchanged.
	tr.lastSendTime = time.Now().Add(-heartbeatInterval - time.Second)
	if !tr.shouldSend(1.0, 0, "Repairing", "ubuntu-24.04") {
		t.Fatal("heartbeat should force send after interval")
	}
}
