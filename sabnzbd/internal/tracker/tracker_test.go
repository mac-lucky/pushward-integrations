package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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
			resp := m.queueResp
			if m.queueFn != nil {
				resp = sabnzbd.QueueResponse{Queue: m.queueFn()}
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "history":
			if m.historyErr {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			h := m.historyResp.History
			if m.historyFn != nil {
				h = m.historyFn()
			}
			// Honor SABnzbd's start/limit pagination so getCompletedSummary's
			// multi-page paging is actually exercised. Returning the same page
			// for every offset (the old behavior) would either spin the paging
			// loop or double-count stats across pages.
			slots := h.Slots
			start := queryInt(r, "start", 0)
			limit := queryInt(r, "limit", len(slots))
			if start < 0 {
				start = 0
			}
			if start > len(slots) {
				start = len(slots)
			}
			end := start + limit
			if end < start {
				end = start
			}
			if end > len(slots) {
				end = len(slots)
			}
			_ = json.NewEncoder(w).Encode(sabnzbd.HistoryResponse{
				History: sabnzbd.History{Slots: slots[start:end]},
			})
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
	// historyFn, when set, supersedes historyResp and is invoked (while m.mu is
	// held) on every history request. Lets a test return different history per
	// read without sleeps, e.g. keep a PP status active for the first N reads.
	historyFn func() sabnzbd.History
	// queueFn, when set, supersedes queueResp and is invoked (while m.mu is
	// held) on every queue request. Lets a test drive phase transitions by
	// queue-read count instead of sleeps, e.g. propagate for the first N
	// reads, download for the next M, then go idle. Read counts map 1:1 to
	// tracker polls only while the client sends one request per GetQueue;
	// client-side retries would silently shift every phase boundary.
	queueFn    func() sabnzbd.Queue
	queueErr   bool
	historyErr bool
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

// setHistoryFn installs a per-read history provider. The provided fn is called
// under m.mu (the mock's lock) on each history request, so it must not re-lock
// m.mu; closing over a plain counter is safe.
func (m *sabMock) setHistoryFn(fn func() sabnzbd.History) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.historyFn = fn
}

// setQueueFn installs a per-read queue provider, the queue-side mirror of
// setHistoryFn. Same contract: fn runs under m.mu, so it must not re-lock it.
func (m *sabMock) setQueueFn(fn func() sabnzbd.Queue) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueFn = fn
}

const oneMB = int64(1024 * 1024)

// lifecycleDownloadQueue is the mid-download snapshot the lifecycle tests
// share. MB "500" and KBPerSec "51200" are load-bearing: they must stay
// coupled to the "50.0 MB/s" frame and "500 MB" summary assertions.
func lifecycleDownloadQueue(filename string) sabnzbd.Queue {
	return sabnzbd.Queue{
		Status:   "Downloading",
		MB:       "500",
		MBLeft:   "250",
		KBPerSec: "51200",
		TimeLeft: "0:00:05",
		Slots:    []sabnzbd.QueueSlot{{Filename: filename}},
	}
}

// futureCompletedSlot is a finished 500 MB download for a test's history. The
// far-future timestamp keeps it unconditionally past the session cutoff; a
// time.Now() stamp taken before track() starts can truncate into an earlier
// Unix second than the cutoff and vanish from the summary.
func futureCompletedSlot(name string) sabnzbd.HistorySlot {
	return sabnzbd.HistorySlot{
		Status: "Completed", Name: name, Bytes: 500 * oneMB, DownloadTime: 10,
		Completed: time.Now().Add(time.Hour).Unix(),
	}
}

// queryInt parses an integer query parameter, returning def when absent or
// malformed.
func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
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

func TestLiveProgress(t *testing.T) {
	sec := 600

	// Generic + a genuine positive ETA opts into live progress with a future end.
	lp, end := liveProgress(pushward.TemplateGeneric, &sec)
	if lp == nil || !*lp {
		t.Fatalf("generic+positive: expected live_progress=true, got %v", lp)
	}
	if end == nil || *end <= time.Now().Unix() {
		t.Fatalf("generic+positive: expected a future end_date, got %v", end)
	}

	// Generic without an ETA explicitly clears live progress (merge-patch would
	// otherwise preserve a prior true) and sends no end_date.
	for _, rem := range []*int{nil, ptrInt(0), ptrInt(-5)} {
		lp, end := liveProgress(pushward.TemplateGeneric, rem)
		if lp == nil || *lp {
			t.Errorf("generic+%v: expected live_progress=false, got %v", rem, lp)
		}
		if end != nil {
			t.Errorf("generic+%v: expected nil end_date, got %v", rem, *end)
		}
	}

	// Off the generic template the field is invalid, so leave both unset.
	if lp, end := liveProgress(pushward.TemplateTimeline, &sec); lp != nil || end != nil {
		t.Errorf("timeline: expected (nil, nil), got (%v, %v)", lp, end)
	}
}

func ptrInt(v int) *int { return &v }

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
	if got[0].Path != "/activities/sabnzbd" {
		t.Errorf("expected /activities/sabnzbd, got %s", got[0].Path)
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

	// Download for the first few queue reads, then idle so tracking moves on
	// to post-processing. Read 1 is waitForQueueActive; reads 2+ are
	// trackDownloads polls.
	var queueReads int
	sabMk.setQueueFn(func() sabnzbd.Queue {
		queueReads++
		if queueReads <= 4 {
			return lifecycleDownloadQueue("ubuntu-24.04.nzb")
		}
		return sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"}
	})
	// History reads only start once the queue has gone idle: post-processing
	// for the first two, then the completed slot.
	var historyReads int
	sabMk.setHistoryFn(func() sabnzbd.History {
		historyReads++
		if historyReads <= 2 {
			return sabnzbd.History{
				Slots: []sabnzbd.HistorySlot{{Status: "Extracting", Name: "ubuntu-24.04"}},
			}
		}
		return sabnzbd.History{Slots: []sabnzbd.HistorySlot{futureCompletedSlot("ubuntu-24.04")}}
	})

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

	tr.Wait()

	got := testutil.GetCalls(calls, mu)
	if len(got) < 3 {
		t.Fatalf("expected at least 3 PushWard calls, got %d", len(got))
	}

	// First call should be POST /activities (create)
	if got[0].Method != "POST" || got[0].Path != "/activities" {
		t.Errorf("first call: expected POST /activities, got %s %s", got[0].Method, got[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, got[0].Body, &createReq)
	if createReq.EndedTTL != 900 {
		t.Errorf("create ended_ttl: expected 900 (cleanup_delay 15m), got %d", createReq.EndedTTL)
	}

	// Last call should be PATCH with ENDED
	last := got[len(got)-1]
	if last.Method != "PATCH" || last.Path != "/activities/sabnzbd" {
		t.Errorf("last call: expected PATCH /activities/sabnzbd, got %s %s", last.Method, last.Path)
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

func TestTrackingLifecycle_Propagating_ThenDownload(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, calls, mu := testutil.MockPushWardServer(t)

	propagating := sabnzbd.Queue{
		Status:   "Idle",
		MB:       "500",
		MBLeft:   "500",
		KBPerSec: "0",
		TimeLeft: "0:00:00",
		Slots:    []sabnzbd.QueueSlot{{Filename: "ubuntu-24.04.nzb", Status: "Propagating"}},
	}
	idle := sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"}

	// Queue reads are deterministic: read 1 is waitForQueueActive (returns true
	// on the first propagating read), reads 2+ are trackDownloads polls, and
	// the post-PP waitForQueueActive drains the idle tail. Thresholds carry
	// margin, so an extra read in either loop shifts a phase boundary instead
	// of skipping a phase.
	var queueReads int
	sabMk.setQueueFn(func() sabnzbd.Queue {
		queueReads++
		switch {
		case queueReads <= 4:
			return propagating
		case queueReads <= 8:
			return lifecycleDownloadQueue("ubuntu-24.04.nzb")
		default:
			return idle
		}
	})
	// History is only read after the download phase (trackPostProcessing, then
	// getCompletedSummary), so the Completed slot can be present from the start.
	sabMk.setHistory(sabnzbd.History{Slots: []sabnzbd.HistorySlot{futureCompletedSlot("ubuntu-24.04")}})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(cfg, sab, pw)
	handler := tr.WebhookHandler(ctx)
	handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/webhook", nil))
	tr.Wait()

	got := testutil.GetCalls(calls, mu)
	if len(got) == 0 {
		t.Fatal("expected PushWard calls")
	}

	// Ordered walk: the hold must render, the download frame must follow it,
	// and nothing may end the activity before bytes have flowed.
	propIdx, speedIdx, firstEndedIdx := -1, -1, -1
	for i, c := range got {
		if c.Method != "PATCH" {
			continue
		}
		var r pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &r)
		if propIdx == -1 && r.Content.State == "Waiting for propagation" {
			propIdx = i
		}
		if speedIdx == -1 && r.Content.State == "50.0 MB/s" {
			speedIdx = i
		}
		if firstEndedIdx == -1 && r.State == pushward.StateEnded {
			firstEndedIdx = i
		}
	}
	if propIdx == -1 {
		t.Fatal("expected a propagation frame while the queue was held")
	}
	if speedIdx == -1 {
		t.Fatal("expected a download-speed frame after propagation cleared")
	}
	if speedIdx < propIdx {
		t.Errorf("download frame (call %d) must come after the propagation frame (call %d)", speedIdx, propIdx)
	}
	if firstEndedIdx != -1 && firstEndedIdx < speedIdx {
		t.Errorf("activity ended (call %d) before the download frame (call %d)", firstEndedIdx, speedIdx)
	}

	last := got[len(got)-1]
	var lastReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, last.Body, &lastReq)
	if lastReq.State != pushward.StateEnded {
		t.Errorf("last update state: expected ENDED, got %s", lastReq.State)
	}
	// The summary must report the real download, not an empty one.
	if !strings.Contains(lastReq.Content.State, "500 MB") {
		t.Errorf("expected the completion summary to report the download, got %q", lastReq.Content.State)
	}
}

// --- ResumeIfActive tests ---

func TestResumeIfActive_PropagatingQueue(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, _, _ := testutil.MockPushWardServer(t)

	// Read 1 is ResumeIfActive itself; the margin covers the resumed tracker's
	// own waitForQueueActive and first trackDownloads polls before the queue
	// clears.
	var queueReads int
	sabMk.setQueueFn(func() sabnzbd.Queue {
		queueReads++
		if queueReads <= 4 {
			return sabnzbd.Queue{
				Status: "Idle",
				MB:     "500",
				MBLeft: "500",
				Slots:  []sabnzbd.QueueSlot{{Filename: "held.nzb", Status: "Propagating"}},
			}
		}
		return sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"}
	})
	sabMk.setHistory(sabnzbd.History{Slots: []sabnzbd.HistorySlot{futureCompletedSlot("held")}})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(cfg, sab, pw)
	if !tr.ResumeIfActive(ctx) {
		t.Fatal("expected ResumeIfActive to resume for a propagating queue")
	}
	tr.Wait()
}

func TestResumeIfActive_ActiveDownload(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, _, _ := testutil.MockPushWardServer(t)

	// Read 1 is ResumeIfActive itself; the margin covers the resumed tracker's
	// waitForQueueActive and first trackDownloads polls before the queue clears.
	var queueReads int
	sabMk.setQueueFn(func() sabnzbd.Queue {
		queueReads++
		if queueReads <= 4 {
			return sabnzbd.Queue{Status: "Downloading", MB: "100", MBLeft: "50"}
		}
		return sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"}
	})
	sabMk.setHistory(sabnzbd.History{Slots: []sabnzbd.HistorySlot{futureCompletedSlot("test-file")}})

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
	tr.Wait()
}

func TestResumeIfActive_ActivePostProcessing(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, _, _ := testutil.MockPushWardServer(t)

	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})
	// Read 1 is ResumeIfActive's getPPStatus; the margin keeps PP active through
	// the tracker's fall-through check and first PP poll, then completes.
	var historyReads int
	sabMk.setHistoryFn(func() sabnzbd.History {
		historyReads++
		if historyReads <= 3 {
			return sabnzbd.History{
				Slots: []sabnzbd.HistorySlot{{Status: "Verifying", Name: "test-file"}},
			}
		}
		return sabnzbd.History{Slots: []sabnzbd.HistorySlot{futureCompletedSlot("test-file")}}
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

// --- waitForQueueActive tests ---

// Without this the propagation hold would exhaust maxPolls in ~60s and log
// "SABnzbd never started downloading, giving up", well inside a 15m delay.
func TestWaitForQueueActive_PropagatingCountsAsActive(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	sabMk.setQueue(sabnzbd.Queue{
		Status: "Idle",
		MB:     "500",
		MBLeft: "500",
		Slots:  []sabnzbd.QueueSlot{{Filename: "held.nzb", Status: "Propagating"}},
	})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	tr := New(cfg, sabnzbd.NewClient(sabSrv.URL, "test-key"), nil)

	if !tr.waitForQueueActive(context.Background(), 2) {
		t.Fatal("expected propagating slots to count as an active queue")
	}
}

func TestWaitForQueueActive_EmptyQueueGivesUp(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	tr := New(cfg, sabnzbd.NewClient(sabSrv.URL, "test-key"), nil)

	if tr.waitForQueueActive(context.Background(), 2) {
		t.Fatal("expected an empty idle queue to stay inactive")
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
	if req.Content.AccentColor != pushward.ColorBlue {
		t.Errorf("expected %q accent, got %s", pushward.ColorBlue, req.Content.AccentColor)
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

func TestSendDownloadProgress_Propagating_KeepsTracking(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	queue := &sabnzbd.Queue{
		Status:   "Idle",
		MB:       "500",
		MBLeft:   "500",
		KBPerSec: "0",
		TimeLeft: "0:00:00",
		Slots:    []sabnzbd.QueueSlot{{Filename: "ubuntu-24.04.nzb", Status: "Propagating"}},
	}

	if !tr.sendDownloadProgress(ctx, queue) {
		t.Fatal("expected sendDownloadProgress to keep tracking while propagating")
	}

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)
	if req.Content.State != "Waiting for propagation" {
		t.Errorf("expected propagation state, got %q", req.Content.State)
	}
	if req.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", req.State)
	}
}

// countPatches returns how many of the recorded calls were activity updates.
// The mock 404s a PATCH against a slug it never saw created, and a failed send
// deliberately skips recordSent so the next poll retries, so a test that cares
// about send throttling has to create the activity first.
func countPatches(calls *[]testutil.APICall, mu *sync.Mutex) int {
	n := 0
	for _, c := range testutil.GetCalls(calls, mu) {
		if c.Method == "PATCH" {
			n++
		}
	}
	return n
}

// A hold is static, so repeat polls must coalesce instead of spending a push
// every 30s. At the default 30m stale_timeout that is one push per 5 minutes.
func TestSendDownloadProgress_Propagating_ThrottlesRepeatPolls(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	if err := pw.CreateActivity(ctx, slug, "SABnzbd", 1, 900, 1800); err != nil {
		t.Fatalf("seeding the activity: %v", err)
	}

	queue := &sabnzbd.Queue{
		Status:   "Idle",
		MB:       "500",
		MBLeft:   "500",
		KBPerSec: "0",
		TimeLeft: "0:00:00",
		Slots:    []sabnzbd.QueueSlot{{Filename: "ubuntu-24.04.nzb", Status: "Propagating"}},
	}

	for range 5 {
		if !tr.sendDownloadProgress(ctx, queue) {
			t.Fatal("expected sendDownloadProgress to keep tracking while propagating")
		}
	}

	if got := countPatches(calls, mu); got != 1 {
		t.Errorf("expected the hold to send once and coalesce the rest, got %d pushes", got)
	}

	// Past the heartbeat the hold reasserts itself so stale_ttl cannot auto-end it.
	tr.lastSendTime = time.Now().Add(-2 * tr.propagationHeartbeat())
	if !tr.sendDownloadProgress(ctx, queue) {
		t.Fatal("expected sendDownloadProgress to keep tracking while propagating")
	}
	if got := countPatches(calls, mu); got != 2 {
		t.Errorf("expected a heartbeat past the interval, got %d pushes", got)
	}
}

// cleanup_delay 0 means "let the server decide", not "dismiss immediately", and
// omitempty is what carries that. A typed assertion cannot tell absent from 0,
// so this reads the raw body.
func TestCreateActivity_ZeroCleanupDelayOmitsEndedTTL(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, calls, mu := testutil.MockPushWardServer(t)

	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})
	sabMk.setHistory(sabnzbd.History{})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	cfg.PushWard.CleanupDelay = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(cfg, sabnzbd.NewClient(sabSrv.URL, "test-key"), pushward.NewClient(pwSrv.URL, "hlk_test"))
	handler := tr.WebhookHandler(ctx)
	handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/webhook", nil))
	tr.Wait()

	got := testutil.GetCalls(calls, mu)
	if len(got) == 0 || got[0].Path != "/activities" {
		t.Fatal("expected a create call")
	}
	var raw map[string]any
	testutil.UnmarshalBody(t, got[0].Body, &raw)
	if v, ok := raw["ended_ttl"]; ok {
		t.Errorf("cleanup_delay 0 must omit ended_ttl, got %v", v)
	}
}

// SABnzbd derives the release time from the NZB's average article date, so a
// future-dated NZB never clears. Without the cap the tracker would poll it
// forever and drop every later webhook as already-tracking.
func TestSendDownloadProgress_PropagationHoldCapped(t *testing.T) {
	cfg := testConfig()
	ctx := context.Background()
	tr := New(cfg, nil, nil)

	queue := &sabnzbd.Queue{
		Status: "Idle",
		MB:     "500",
		MBLeft: "500",
		Slots:  []sabnzbd.QueueSlot{{Filename: "stuck.nzb", Status: "Propagating"}},
	}

	tr.propagatingSince = time.Now().Add(-maxPropagationHold - time.Minute)
	if tr.sendDownloadProgress(ctx, queue) {
		t.Fatal("expected the tracker to give up once the hold exceeded the cap")
	}
}

// The hold latches on first sight and clears once the queue moves on, so a later
// hold in the same session gets a fresh cap rather than the first one's clock.
func TestPropagationActive_LatchesAndResets(t *testing.T) {
	tr := New(testConfig(), nil, nil)

	held := &sabnzbd.Queue{Slots: []sabnzbd.QueueSlot{{Filename: "a.nzb", Status: "Propagating"}}}
	if !tr.propagationActive(held) {
		t.Fatal("expected a propagating queue to be active")
	}
	first := tr.propagatingSince
	if first.IsZero() {
		t.Fatal("expected the hold start to latch")
	}
	if !tr.propagationActive(held) || !tr.propagatingSince.Equal(first) {
		t.Error("expected the latched start to survive a second poll")
	}

	downloading := &sabnzbd.Queue{Slots: []sabnzbd.QueueSlot{{Filename: "a.nzb", Status: "Downloading"}}}
	if tr.propagationActive(downloading) {
		t.Error("expected a downloading queue not to be propagating")
	}
	if !tr.propagatingSince.IsZero() {
		t.Error("expected the hold start to clear once the queue moved on")
	}
}

// The counterpart to the test above. Bytes stay queued so the slot status is the
// only thing deciding this, rather than the byte counters short-circuiting it.
func TestQueueHasWork(t *testing.T) {
	cases := []struct {
		name     string
		q        sabnzbd.Queue
		wantWork bool
		wantProp bool
	}{
		{"downloading", sabnzbd.Queue{Status: "Downloading", MB: "500"}, true, false},
		{"hold only", sabnzbd.Queue{Status: "Idle", MB: "500", Slots: []sabnzbd.QueueSlot{{Status: "Propagating"}}}, true, true},
		{"downloading with propagating sibling", sabnzbd.Queue{Status: "Downloading", MB: "500", Slots: []sabnzbd.QueueSlot{{}, {Status: "Propagating"}}}, true, true},
		{"idle empty", sabnzbd.Queue{Status: "Idle", MB: "0"}, false, false},
		{"paused counts as work", sabnzbd.Queue{Status: "Paused", MB: "500"}, true, false},
		// ParseFloat failure reads as zero bytes: a garbled queue ends the
		// session early rather than crashing or pinning the tracker.
		{"unparseable MB reads as no bytes", sabnzbd.Queue{Status: "Downloading", MB: "garbage"}, false, false},
		{"unparseable MB but hold still counts", sabnzbd.Queue{Status: "Idle", MB: "", Slots: []sabnzbd.QueueSlot{{Status: "Propagating"}}}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := New(testConfig(), nil, nil) // fresh per case: propagationActive latches state
			work, prop := tr.queueHasWork(&tc.q)
			if work != tc.wantWork || prop != tc.wantProp {
				t.Errorf("queueHasWork = (%v, %v), want (%v, %v)", work, prop, tc.wantWork, tc.wantProp)
			}
		})
	}
}

func TestSendDownloadProgress_IdleWithSlotsNotPropagating_ReturnsFalse(t *testing.T) {
	cfg := testConfig()
	ctx := context.Background()
	tr := New(cfg, nil, nil)

	queue := &sabnzbd.Queue{
		Status: "Idle",
		MB:     "500",
		MBLeft: "500",
		Slots:  []sabnzbd.QueueSlot{{Filename: "leftover.nzb", Status: "Queued"}},
	}

	if tr.sendDownloadProgress(ctx, queue) {
		t.Fatal("expected false for an idle queue with no propagating slots")
	}
}

func TestSendDownloadProgress_DownloadingWithPropagatingSibling_SendsSpeed(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	queue := &sabnzbd.Queue{
		Status:   "Downloading",
		MB:       "1000",
		MBLeft:   "400",
		KBPerSec: "51200",
		TimeLeft: "0:00:08",
		Slots: []sabnzbd.QueueSlot{
			{Filename: "first.nzb"},
			{Filename: "second.nzb", Status: "Propagating"},
		},
	}

	if !tr.sendDownloadProgress(ctx, queue) {
		t.Fatal("expected true for active download")
	}

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)
	if req.Content.State != "50.0 MB/s" {
		t.Errorf("expected speed state, got %q", req.Content.State)
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

	// Downloading long enough for the resumed tracker to observe the download
	// (read 1 is ResumeIfActive, read 2 its waitForQueueActive, read 3 the
	// first trackDownloads poll), then idle so the session ends.
	var queueReads int
	sabMk.setQueueFn(func() sabnzbd.Queue {
		queueReads++
		if queueReads <= 3 {
			return sabnzbd.Queue{Status: "Downloading", MB: "100", MBLeft: "50"}
		}
		return sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"}
	})
	sabMk.setHistory(sabnzbd.History{Slots: []sabnzbd.HistorySlot{futureCompletedSlot("test-file")}})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()

	tr := New(cfg, sab, pw)
	resumed := tr.ResumeIfActive(ctx)
	if !resumed {
		t.Fatal("expected resume")
	}
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

	// Template/Units are seed-only under merge-patch (preserved server-side
	// across ticks), so tick bodies carry Value + State only.
	if values := testutil.RequireValueMap(t, req.Content.Value); values == nil {
		// already failed
	} else if v, ok := values[seriesKey]; !ok {
		t.Fatal("expected value map with 'Speed' key")
	} else if v != 50.0 {
		t.Errorf("expected Speed value 50.0, got %.1f", v)
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

	// Tick bodies omit Template (preserved server-side under merge-patch).
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

	// Non-download sends (e.g. "Starting...", PP) pass nil for value. Send as
	// a tick since Units/Template are seed-only under merge-patch.
	_ = tr.send(ctx, 0.0, "Starting...", "arrow.down.circle", pushward.ColorBlue, nil, "", pushward.StateOngoing, nil)

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)

	if values := testutil.RequireValueMap(t, req.Content.Value); values == nil {
		// already failed
	} else if v, ok := values[seriesKey]; !ok || v != 0 {
		t.Errorf("expected value[%q]=0 for non-download phase, got %v (ok=%v)", seriesKey, v, ok)
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

	// Template/Units are seed-only (server preserves). Tick asserts Value/State.
	if values := testutil.RequireValueMap(t, req.Content.Value); values == nil {
		// already failed
	} else if v, ok := values[seriesKey]; !ok {
		t.Fatal("expected value map with 'Speed' key for paused")
	} else if v != 0 {
		t.Errorf("expected Speed value 0 for paused, got %f", v)
	}
	if req.Content.State != "Paused" {
		t.Errorf("expected state Paused for timeline paused, got %s", req.Content.State)
	}
}

func TestTimeline_FullLifecycle(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, calls, mu := testutil.MockPushWardServer(t)

	// Download for the first few queue reads, then idle. Read 1 is
	// waitForQueueActive; reads 2+ are trackDownloads polls. History is only
	// read after the download phase.
	var queueReads int
	sabMk.setQueueFn(func() sabnzbd.Queue {
		queueReads++
		if queueReads <= 4 {
			return lifecycleDownloadQueue("test.nzb")
		}
		return sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"}
	})
	sabMk.setHistory(sabnzbd.History{Slots: []sabnzbd.HistorySlot{futureCompletedSlot("test-file")}})

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

	tr.Wait()

	got := testutil.GetCalls(calls, mu)

	// First PATCH (the seed) must include the timeline template; subsequent
	// merge-patch ticks omit it because the server preserves it across updates.
	// Download-phase tick payloads must still carry a value sample so the
	// server's timeline series accumulates new points.
	var hasPositiveValue bool
	firstPatchSeen := false
	for _, c := range got {
		if c.Method != "PATCH" {
			continue
		}
		var r pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &r)
		if !firstPatchSeen {
			if r.Content.Template != "timeline" {
				t.Errorf("first PATCH (seed): expected timeline template, got %q", r.Content.Template)
			}
			firstPatchSeen = true
		}
		if r.Content.Value == nil {
			continue
		}
		values := testutil.RequireValueMap(t, r.Content.Value)
		if values == nil {
			continue
		}
		if v, ok := values[seriesKey]; ok && v > 0 {
			hasPositiveValue = true
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

	// Activity must exist before PATCH — the seed is committed only on a
	// successful send, so an unregistered slug (404) would re-seed on every
	// tick instead of exercising the seed-once path under test.
	if err := pw.CreateActivity(ctx, "sabnzbd", "SABnzbd", cfg.PushWard.Priority, 0, int(cfg.PushWard.StaleTimeout.Seconds())); err != nil {
		t.Fatalf("unexpected create error: %v", err)
	}

	speed := pushward.Float64Ptr(50.0)

	// First send with positive value should seed history
	if err := tr.send(ctx, 0.5, "50.0 MB/s", "arrow.down.circle.fill", pushward.ColorBlue, nil, "test.nzb", pushward.StateOngoing, speed); err != nil {
		t.Fatalf("first send: %v", err)
	}

	// Second send should NOT include history
	if err := tr.send(ctx, 0.6, "50.0 MB/s", "arrow.down.circle.fill", pushward.ColorBlue, nil, "test.nzb", pushward.StateOngoing, speed); err != nil {
		t.Fatalf("second send: %v", err)
	}

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

	// Activity must exist before PATCH — real tracker flow calls CreateActivity
	// before the first seed.
	if err := pw.CreateActivity(ctx, "sabnzbd", "SABnzbd", cfg.PushWard.Priority, 0, int(cfg.PushWard.StaleTimeout.Seconds())); err != nil {
		t.Fatalf("unexpected create error: %v", err)
	}

	// Display settings (smoothing/scale/decimals) are established on the seed.
	// Tick merge-patches rely on the server preserving them.
	if err := tr.sendSeed(ctx, 0.5, "50.0 MB/s", "arrow.down.circle.fill", pushward.ColorBlue, nil, "test.nzb", pushward.StateOngoing, pushward.Float64Ptr(50.0)); err != nil {
		t.Fatalf("unexpected seed error: %v", err)
	}

	got := testutil.GetCalls(calls, mu)
	// First call is CreateActivity, second is the seed PATCH.
	var req pushward.UpdateRequest
	var patchCall *testutil.APICall
	for i := range got {
		if got[i].Method == http.MethodPatch {
			patchCall = &got[i]
			break
		}
	}
	if patchCall == nil {
		t.Fatalf("no PATCH call recorded, got %d calls", len(got))
	}
	testutil.UnmarshalBody(t, patchCall.Body, &req)

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

// seedActivityAndReset registers the activity with the mock server (so PATCH
// ticks return 200 and recordSent fires the dedup state) then clears the
// recorded calls, leaving only the subsequent PATCHes for the assertions.
func seedActivityAndReset(t *testing.T, pw *pushward.Client, calls *[]testutil.APICall, mu *sync.Mutex) {
	t.Helper()
	if err := pw.CreateActivity(context.Background(), slug, "test", 5, 60, 60); err != nil {
		t.Fatalf("seed activity: %v", err)
	}
	mu.Lock()
	*calls = nil
	mu.Unlock()
}

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
	seedActivityAndReset(t, pw, calls, mu)

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
	seedActivityAndReset(t, pw, calls, mu)

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
	seedActivityAndReset(t, pw, calls, mu)

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
	seedActivityAndReset(t, pw, calls, mu)

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
	seedActivityAndReset(t, pw, calls, mu)

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
	seedActivityAndReset(t, pw, calls, mu)

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
	seedActivityAndReset(t, pw, calls, mu)

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

// --- send() failure branch: recordSent must be skipped so the next poll retries ---

func TestSendDownloadProgress_RetriesAfterSendFailure(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	// No seedActivityAndReset → the slug is unregistered → PATCH 404 (a
	// non-retryable 4xx) → send() returns an error → recordSent is skipped. The
	// dedup state therefore never advances, so a second identical poll must
	// re-attempt the PATCH rather than dedup it.
	q := downloadingQueue("51200", "500")
	tr.sendDownloadProgress(ctx, q)
	tr.sendDownloadProgress(ctx, q) // identical, but the prior send failed → retry

	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 PATCH attempts (no dedup after a failed send), got %d", len(got))
	}
	for i, c := range got {
		if c.Method != "PATCH" {
			t.Errorf("call %d: expected PATCH, got %s", i, c.Method)
		}
	}
}

func TestSendDownloadProgress_DedupsAfterSuccessfulRetry(t *testing.T) {
	pwSrv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx := context.Background()
	tr := New(cfg, nil, pw)

	q := downloadingQueue("51200", "500")

	// 1) Unseeded slug → PATCH 404 → send() fails → recordSent skipped.
	tr.sendDownloadProgress(ctx, q)

	// 2) Register the activity so PATCH now succeeds, and clear recorded calls so
	//    the assertion only sees the post-seed polls.
	seedActivityAndReset(t, pw, calls, mu)

	// 3) Identical poll now succeeds → recordSent fires, advancing dedup state.
	tr.sendDownloadProgress(ctx, q)
	// 4) Identical poll dedups because the prior send succeeded.
	tr.sendDownloadProgress(ctx, q)

	got := testutil.GetCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 PATCH after a successful send dedups the next identical poll, got %d", len(got))
	}
}

// --- getCompletedSummary pagination / cutoff ---

func TestGetCompletedSummary_SumsAcrossPagesAndStopsAtCutoff(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	tr := New(cfg, sab, nil)

	sessionStart := time.Unix(10_000, 0)
	inSession := int64(20_000) // >= cutoff
	older := int64(5_000)      // < cutoff (a previous session)

	var slots []sabnzbd.HistorySlot
	// Page 1: a full page of in-session Completed slots, 1 MB / 1s each.
	for i := 0; i < historyPageSize; i++ {
		slots = append(slots, sabnzbd.HistorySlot{
			Status: "Completed", Name: fmt.Sprintf("file-%d", i),
			Bytes: oneMB, DownloadTime: 1, Completed: inSession,
		})
	}
	// Page 2: one more in-session Completed slot (must be summed in — proves the
	// paging crossed onto the second page) ...
	slots = append(slots, sabnzbd.HistorySlot{
		Status: "Completed", Name: "page2-file",
		Bytes: 2 * oneMB, DownloadTime: 2, Completed: inSession,
	})
	// ... then an older Completed slot → the loop must STOP at the session cutoff.
	slots = append(slots, sabnzbd.HistorySlot{
		Status: "Completed", Name: "old-file",
		Bytes: 999 * oneMB, DownloadTime: 999, Completed: older,
	})
	// ... and trailing in-session slots that must NOT be counted, proving the
	// loop returned at the cutoff and did not keep paging.
	slots = append(slots, sabnzbd.HistorySlot{
		Status: "Completed", Name: "should-not-count",
		Bytes: 999 * oneMB, DownloadTime: 999, Completed: inSession,
	})

	sabMk.setHistory(sabnzbd.History{Slots: slots})

	totalBytes, totalDownloadTime, latestName := tr.getCompletedSummary(context.Background(), sessionStart)

	wantBytes := int64(historyPageSize)*oneMB + 2*oneMB
	if totalBytes != wantBytes {
		t.Errorf("totalBytes = %d, want %d", totalBytes, wantBytes)
	}
	wantTime := historyPageSize*1 + 2
	if totalDownloadTime != wantTime {
		t.Errorf("totalDownloadTime = %d, want %d", totalDownloadTime, wantTime)
	}
	if latestName != "file-0" {
		t.Errorf("latestName = %q, want %q", latestName, "file-0")
	}
}

func TestGetCompletedSummary_FailedSlotBeforeCutoffStops(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	tr := New(cfg, sab, nil)

	sessionStart := time.Unix(10_000, 0)
	inSession := int64(20_000)
	older := int64(5_000)

	slots := []sabnzbd.HistorySlot{
		{Status: "Completed", Name: "recent", Bytes: 3 * oneMB, DownloadTime: 3, Completed: inSession},
		// A Failed slot from a PREVIOUS session. Its non-Completed status must not
		// let it slip past the timestamp cutoff — the loop must STOP here. (The
		// regression: gating the cutoff on Completed status would `continue` past
		// this slot and page through the whole history.)
		{Status: "Failed", Name: "old-failed", Bytes: 0, DownloadTime: 0, Completed: older},
		// A later in-session Completed slot that must NOT be counted, proving the
		// failed slot's timestamp stopped the loop.
		{Status: "Completed", Name: "trailing", Bytes: 7 * oneMB, DownloadTime: 7, Completed: inSession},
	}
	sabMk.setHistory(sabnzbd.History{Slots: slots})

	totalBytes, totalDownloadTime, latestName := tr.getCompletedSummary(context.Background(), sessionStart)

	if totalBytes != 3*oneMB {
		t.Errorf("totalBytes = %d, want %d", totalBytes, 3*oneMB)
	}
	if totalDownloadTime != 3 {
		t.Errorf("totalDownloadTime = %d, want 3", totalDownloadTime)
	}
	if latestName != "recent" {
		t.Errorf("latestName = %q, want %q", latestName, "recent")
	}
}

// --- track() fall-through: queue idle but post-processing active must not give up ---

func TestTrack_QueueIdleButPostProcessingActive_DoesNotGiveUp(t *testing.T) {
	sabSrv, sabMk := mockSABnzbd(t)
	pwSrv, calls, mu := testutil.MockPushWardServer(t)

	// The queue stays Idle the whole time: the only signal that work is in flight
	// is active post-processing in history. This drives the track() fall-through
	// where waitForQueueActive fails but getPPStatus() != "" so it must keep
	// tracking instead of giving up with a "No downloads" ENDED.
	sabMk.setQueue(sabnzbd.Queue{Status: "Idle", MB: "0", MBLeft: "0"})

	extracting := sabnzbd.History{
		Slots: []sabnzbd.HistorySlot{{Status: "Extracting", Name: "test-file"}},
	}
	completed := sabnzbd.History{Slots: []sabnzbd.HistorySlot{futureCompletedSlot("test-file")}}

	// History reads happen in a deterministic order: #1 ResumeIfActive, #2 the
	// fall-through give-up check, #3 the first post-processing poll, then the
	// completion reads. Keep PP active (Extracting) for the first three reads so
	// the fall-through never sees an empty PP status, then flip to Completed so
	// the PP loop ends and the summary carries real stats. No sleeps → no race.
	var reads int
	sabMk.setHistoryFn(func() sabnzbd.History {
		reads++
		if reads <= 3 {
			return extracting
		}
		return completed
	})

	cfg := testConfig()
	cfg.SABnzbd.URL = sabSrv.URL
	sab := sabnzbd.NewClient(sabSrv.URL, "test-key")
	pw := pushward.NewClient(pwSrv.URL, "hlk_test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := New(cfg, sab, pw)
	if !tr.ResumeIfActive(ctx) {
		t.Fatal("expected ResumeIfActive to resume for active post-processing")
	}
	tr.Wait()

	got := testutil.GetCalls(calls, mu)
	if len(got) == 0 {
		t.Fatal("expected PushWard calls")
	}

	// The give-up path would emit a "No downloads" ENDED; it must never appear,
	// and the only ENDED must be the real completion summary.
	endedCount := 0
	for _, c := range got {
		if c.Method != "PATCH" {
			continue
		}
		var req pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &req)
		if req.Content.State == "No downloads" {
			t.Fatalf("tracker gave up with a 'No downloads' ENDED despite active post-processing")
		}
		if req.State == pushward.StateEnded {
			endedCount++
		}
	}
	// Resumed sessions send exactly one ENDED (no two-phase end).
	if endedCount != 1 {
		t.Fatalf("expected exactly 1 ENDED update, got %d", endedCount)
	}

	// The final ENDED must carry the real completion summary (500 MB @ 50 MB/s).
	last := got[len(got)-1]
	if last.Method != "PATCH" {
		t.Fatalf("expected last call to be PATCH, got %s %s", last.Method, last.Path)
	}
	var lastReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, last.Body, &lastReq)
	if lastReq.State != pushward.StateEnded {
		t.Errorf("last update: expected ENDED, got %s", lastReq.State)
	}
	if !strings.Contains(lastReq.Content.State, "MB/s avg") {
		t.Errorf("completion state should contain 'MB/s avg', got %q", lastReq.Content.State)
	}
	if !strings.Contains(lastReq.Content.State, "500 MB") {
		t.Errorf("completion state should contain '500 MB', got %q", lastReq.Content.State)
	}
}
