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
			URL:    "http://placeholder",
			APIKey: "test-key",
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
			json.NewEncoder(w).Encode(m.queueResp)
		case "history":
			if m.historyErr {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(m.historyResp)
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

	tr := New(ctx, cfg, sab, pw)

	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	w := httptest.NewRecorder()
	tr.HandleWebhook(w, req)

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
	tr := New(ctx, cfg, nil, nil)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/webhook", nil)
		w := httptest.NewRecorder()
		tr.HandleWebhook(w, req)

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

	tr := New(ctx, cfg, sab, pw)

	// Wrong secret → 401
	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	req.Header.Set("X-Webhook-Secret", "wrong-secret")
	w := httptest.NewRecorder()
	tr.HandleWebhook(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: expected 401, got %d", w.Code)
	}

	// Missing secret → 401
	req = httptest.NewRequest(http.MethodPost, "/webhook", nil)
	w = httptest.NewRecorder()
	tr.HandleWebhook(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing secret: expected 401, got %d", w.Code)
	}

	// Correct secret → 200
	req = httptest.NewRequest(http.MethodPost, "/webhook", nil)
	req.Header.Set("X-Webhook-Secret", "my-secret")
	w = httptest.NewRecorder()
	tr.HandleWebhook(w, req)
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

	tr := New(ctx, cfg, sab, pw)

	// First webhook → tracking_started
	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	w := httptest.NewRecorder()
	tr.HandleWebhook(w, req)
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
	tr.HandleWebhook(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("second webhook: expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "already_tracking") {
		t.Fatalf("expected already_tracking, got: %s", w.Body.String())
	}

	tr.Wait()
}

// --- truncate tests ---

func TestTruncate(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"shorter than limit", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"ASCII truncation", "hello world foo bar", 10, "hello w..."},
		{"UTF-8 rune-aware", "héllo wörld", 8, "héllo..."},
		{"CJK characters", "你好世界测试数据", 6, "你好世..."},
		{"maxLen 3", "abcdef", 3, "abc"},
		{"maxLen 2", "abcdef", 2, "ab"},
		{"maxLen 1", "abcdef", 1, "a"},
		{"empty string", "", 5, ""},
		{"maxLen 4 with ellipsis", "abcdef", 4, "a..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
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

	tr := New(ctx, cfg, nil, pw)
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

	tr := New(ctx, cfg, sab, pw)

	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	w := httptest.NewRecorder()
	tr.HandleWebhook(w, req)

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

	tr := New(ctx, cfg, sab, pw)

	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	w := httptest.NewRecorder()
	tr.HandleWebhook(w, req)

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
		Slots: []sabnzbd.HistorySlot{{Status: "Completed", Name: "ubuntu-24.04"}},
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

	tr := New(ctx, cfg, sab, pw)
	resumed := tr.ResumeIfActive()
	if !resumed {
		t.Fatal("expected ResumeIfActive to return true")
	}

	// Let it settle, then stop by making queue idle
	time.Sleep(50 * time.Millisecond)
	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})
	sabMk.setHistory(sabnzbd.History{
		Slots: []sabnzbd.HistorySlot{{Status: "Completed", Name: "test-file"}},
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

	tr := New(ctx, cfg, sab, pw)
	resumed := tr.ResumeIfActive()
	if !resumed {
		t.Fatal("expected ResumeIfActive to return true for active PP")
	}

	// Transition: PP done
	time.Sleep(50 * time.Millisecond)
	sabMk.setHistory(sabnzbd.History{
		Slots: []sabnzbd.HistorySlot{{Status: "Completed", Name: "test-file"}},
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

	tr := New(ctx, cfg, sab, pw)
	resumed := tr.ResumeIfActive()
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
	tr := New(ctx, cfg, nil, pw)

	queue := &sabnzbd.Queue{
		Status:   "Paused",
		MB:       "1000",
		MBLeft:   "500",
		KBPerSec: "0",
		TimeLeft: "0:00:00",
		Slots:    []sabnzbd.QueueSlot{{Filename: "test.nzb"}},
	}

	result := tr.sendDownloadProgress(ctx, queue, time.Now())
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
	tr := New(ctx, cfg, nil, nil)

	queue := &sabnzbd.Queue{
		Status: "Idle",
		MB:     "0",
		MBLeft: "0",
	}

	result := tr.sendDownloadProgress(ctx, queue, time.Now())
	if result {
		t.Fatal("expected sendDownloadProgress to return false for Idle")
	}
}

func TestSendDownloadProgress_MultipleSlots(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(ctx, cfg, nil, pw)

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

	result := tr.sendDownloadProgress(ctx, queue, time.Now())
	if !result {
		t.Fatal("expected true for active download")
	}

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)
	// Multiple slots → "name +N more" format
	if !strings.Contains(req.Content.Subtitle, "+2 more") {
		t.Errorf("expected subtitle with +2 more, got %q", req.Content.Subtitle)
	}
}

// --- Resumed tracking skips two-phase end ---

func TestResumedTracking_SkipsTwoPhaseEnd(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, calls, mu := testutil.MockPushWardServer(t)

	// Queue idle from start so tracking ends immediately
	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})
	sabMk.setHistory(sabnzbd.History{
		Slots: []sabnzbd.HistorySlot{{Status: "Completed", Name: "test-file"}},
	})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()

	tr := New(ctx, cfg, sab, pw)

	// Simulate resumed tracking (directly call track with resumed=true)
	// We can't easily call track directly, so we use ResumeIfActive with pre-active queue
	// Instead, set queue active, resume, then immediately go idle
	sabMk.setQueue(sabnzbd.Queue{Status: "Downloading", MB: "100", MBLeft: "50"})
	resumed := tr.ResumeIfActive()
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
