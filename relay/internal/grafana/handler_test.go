package grafana

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
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

const testKey = "hlk_test"

func testConfig() *config.GrafanaConfig {
	return &config.GrafanaConfig{
		BaseProviderConfig: config.BaseProviderConfig{
			Enabled:      true,
			Priority:     5,
			CleanupDelay: 1 * time.Hour,
			StaleTimeout: 24 * time.Hour,
		},
		SeverityLabel:   "severity",
		DefaultSeverity: "warning",
		DefaultIcon:     "exclamationmark.triangle.fill",
	}
}

func newTestAPI(t *testing.T, cfg *config.GrafanaConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	pool := client.NewPool(srv.URL, nil)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, pool, cfg)

	return mux, calls, mu
}

func setup(t *testing.T) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	return newTestAPI(t, testConfig())
}

func setupWithConfig(t *testing.T, cfg *config.GrafanaConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	return newTestAPI(t, cfg)
}

func sendWebhook(t *testing.T, handler http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/grafana", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// --- Tests ---

func TestFiringSingleAlert(t *testing.T) {
	handler, calls, mu := setup(t)

	payload := `{
		"receiver": "pushward",
		"status": "firing",
		"alerts": [
			{
				"status": "firing",
				"labels": {
					"alertname": "HighCPUUsage",
					"severity": "critical",
					"instance": "node-exporter:9100",
					"job": "node"
				},
				"annotations": {
					"summary": "CPU usage is above 90% on node-exporter:9100"
				},
				"startsAt": "2026-02-18T10:30:00Z",
				"fingerprint": "abc123def456"
			}
		]
	}`

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (notification), got %d", len(recorded))
	}

	if recorded[0].Method != "POST" || recorded[0].Path != "/notifications" {
		t.Errorf("expected POST /notifications, got %s %s", recorded[0].Method, recorded[0].Path)
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Title != "HighCPUUsage" {
		t.Errorf("expected title HighCPUUsage, got %s", req.Title)
	}
	if req.Body != "CPU usage is above 90% on node-exporter:9100" {
		t.Errorf("expected summary as body, got %s", req.Body)
	}
	if req.Category != "critical" {
		t.Errorf("expected category critical, got %s", req.Category)
	}
	if req.Level != pushward.LevelActive {
		t.Errorf("expected level active, got %s", req.Level)
	}
	if !strings.Contains(req.Subtitle, "node-exporter:9100") {
		t.Errorf("expected subtitle to contain instance, got %s", req.Subtitle)
	}
	if req.Source != "grafana" {
		t.Errorf("expected source grafana, got %s", req.Source)
	}
	if req.ThreadID != "grafana-highcpuusage" {
		t.Errorf("expected thread_id grafana-highcpuusage, got %s", req.ThreadID)
	}
	if !req.Push {
		t.Error("expected push=true")
	}
}

func TestResolvedAlert(t *testing.T) {
	handler, calls, mu := setup(t)

	payload := `{
		"status": "resolved",
		"alerts": [{
			"status": "resolved",
			"labels": {
				"alertname": "DiskSpaceLow",
				"severity": "warning",
				"instance": "nas:9100"
			},
			"annotations": {
				"summary": "Disk space recovered on nas:9100"
			},
			"startsAt": "2026-02-18T10:00:00Z",
			"fingerprint": "f1e2d3c4b5a6"
		}]
	}`

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (notification), got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Title != "DiskSpaceLow" {
		t.Errorf("expected title DiskSpaceLow, got %s", req.Title)
	}
	if !strings.Contains(req.Body, "Resolved") {
		t.Errorf("expected body to contain 'Resolved', got %s", req.Body)
	}
	if !strings.Contains(req.Body, "Disk space recovered") {
		t.Errorf("expected body to contain summary, got %s", req.Body)
	}
	if req.Category != "resolved" {
		t.Errorf("expected category resolved, got %s", req.Category)
	}
	if req.Level != pushward.LevelPassive {
		t.Errorf("expected level passive, got %s", req.Level)
	}
}

func TestFiringThenResolved(t *testing.T) {
	handler, calls, mu := setup(t)

	// Fire
	sendWebhook(t, handler, `{
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "PodCrashLooping", "severity": "critical"},
			"annotations": {"summary": "Pod is crash looping"},
			"startsAt": "2026-02-18T14:22:33Z",
			"fingerprint": "d4e5f6a7b8c9"
		}]
	}`)

	// Resolve
	sendWebhook(t, handler, `{
		"alerts": [{
			"status": "resolved",
			"labels": {"alertname": "PodCrashLooping", "severity": "critical"},
			"annotations": {"summary": "Pod crash loop resolved"},
			"startsAt": "2026-02-18T14:22:33Z",
			"fingerprint": "d4e5f6a7b8c9"
		}]
	}`)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls (firing + resolved notifications), got %d", len(recorded))
	}

	// Firing notification
	var firingReq pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &firingReq)
	if firingReq.Category != "critical" {
		t.Errorf("expected firing category critical, got %s", firingReq.Category)
	}

	// Resolved notification
	var resolvedReq pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &resolvedReq)
	if resolvedReq.Category != "resolved" {
		t.Errorf("expected resolved category, got %s", resolvedReq.Category)
	}
}

func TestMultipleAlertsInSinglePayload(t *testing.T) {
	handler, calls, mu := setup(t)

	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU", "severity": "critical", "instance": "node1"},
				"annotations": {"summary": "CPU high on node1"},
				"fingerprint": "fp1"
			},
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU", "severity": "critical", "instance": "node2"},
				"annotations": {"summary": "CPU high on node2"},
				"fingerprint": "fp2"
			},
			{
				"status": "firing",
				"labels": {"alertname": "DiskFull", "severity": "warning"},
				"annotations": {"summary": "Disk is full"},
				"fingerprint": "fp3"
			}
		]
	}`

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 3 {
		t.Fatalf("expected 3 notifications (one per alert), got %d", len(recorded))
	}

	// Each alert gets its own notification
	for i, call := range recorded {
		if call.Path != "/notifications" {
			t.Errorf("call %d: expected POST /notifications, got %s %s", i, call.Method, call.Path)
		}
	}
}

func TestRefiringAlert_SendsNotification(t *testing.T) {
	handler, calls, mu := setup(t)

	payload := `{
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "MemoryHigh", "severity": "warning"},
			"annotations": {"summary": "Memory above 85%"},
			"fingerprint": "aabbccddee11"
		}]
	}`

	// First fire
	sendWebhook(t, handler, payload)
	// Second fire (refire)
	sendWebhook(t, handler, payload)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 notifications (one per fire), got %d", len(recorded))
	}
}

func TestSeverityMapping(t *testing.T) {
	handler, calls, mu := setup(t)

	tests := []struct {
		severity string
		wantCat  string
	}{
		{"critical", "critical"},
		{"warning", "warning"},
		{"info", "info"},
	}

	for _, tt := range tests {
		mu.Lock()
		*calls = (*calls)[:0]
		mu.Unlock()

		payload := `{
			"alerts": [{
				"status": "firing",
				"labels": {"alertname": "Test", "severity": "` + tt.severity + `"},
				"annotations": {"summary": "test"},
				"fingerprint": "sev-` + tt.severity + `"
			}]
		}`

		sendWebhook(t, handler, payload)
		recorded := testutil.GetCalls(calls, mu)
		if len(recorded) != 1 {
			t.Fatalf("severity %s: expected 1 call, got %d", tt.severity, len(recorded))
		}

		var req pushward.SendNotificationRequest
		testutil.UnmarshalBody(t, recorded[0].Body, &req)
		if req.Category != tt.wantCat {
			t.Errorf("severity %s: expected category %s, got %s", tt.severity, tt.wantCat, req.Category)
		}
	}
}

func TestMissingSeverityLabel_UsesDefault(t *testing.T) {
	handler, calls, mu := setup(t)

	payload := `{
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "NoSeverity"},
			"annotations": {"summary": "test"},
			"fingerprint": "nosev1"
		}]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Category != "warning" {
		t.Errorf("expected default severity warning as category, got %s", req.Category)
	}
}

func TestMissingAlertname_FallbackDefault(t *testing.T) {
	handler, calls, mu := setup(t)

	payload := `{
		"alerts": [{
			"status": "firing",
			"labels": {"severity": "info"},
			"annotations": {"summary": "test"},
			"fingerprint": "noname1"
		}]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Title != "Grafana Alert" {
		t.Errorf("expected fallback title 'Grafana Alert', got %s", req.Title)
	}
}

func TestNoInstanceLabel_SubtitleIsPlainGrafana(t *testing.T) {
	handler, calls, mu := setup(t)

	payload := `{
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "Test", "severity": "info"},
			"annotations": {"summary": "test"},
			"fingerprint": "noinst1"
		}]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Subtitle != "Grafana" {
		t.Errorf("expected subtitle 'Grafana', got %s", req.Subtitle)
	}
}

func TestCustomSeverityLabel(t *testing.T) {
	cfg := testConfig()
	cfg.SeverityLabel = "priority"
	handler, calls, mu := setupWithConfig(t, cfg)

	payload := `{
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "Test", "priority": "critical"},
			"annotations": {"summary": "test"},
			"fingerprint": "custom1"
		}]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Category != "critical" {
		t.Errorf("expected category critical from custom label, got %s", req.Category)
	}
}

func TestInvalidJSON(t *testing.T) {
	handler, _, _ := setup(t)
	w := sendWebhook(t, handler, `{invalid`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestEmptyAlertsArray(t *testing.T) {
	handler, calls, mu := setup(t)

	w := sendWebhook(t, handler, `{"alerts": []}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 calls for empty alerts, got %d", len(recorded))
	}
}

func TestUnknownAlertStatus_Ignored(t *testing.T) {
	handler, calls, mu := setup(t)

	payload := `{
		"alerts": [{
			"status": "silenced",
			"labels": {"alertname": "Test", "severity": "info"},
			"annotations": {"summary": "test"},
			"fingerprint": "unknown1"
		}]
	}`

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Fatalf("expected 0 calls for unknown status, got %d", len(recorded))
	}
}

func TestMissingAuthKey_Unauthorized(t *testing.T) {
	handler, _, _ := setup(t)
	req := httptest.NewRequest(http.MethodPost, "/grafana", strings.NewReader(`{"alerts":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestResolvedWithEmptySummary(t *testing.T) {
	handler, calls, mu := setup(t)

	payload := `{
		"alerts": [{
			"status": "resolved",
			"labels": {"alertname": "Test", "severity": "info"},
			"annotations": {},
			"fingerprint": "emptysummary1"
		}]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Body != "Resolved" {
		t.Errorf("expected body 'Resolved' when no summary, got %s", req.Body)
	}
}

func TestCollapseID_PerFingerprint(t *testing.T) {
	handler, calls, mu := setup(t)

	// Two alerts with same alertname but different fingerprints
	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU", "severity": "critical", "instance": "node1"},
				"annotations": {"summary": "CPU high on node1"},
				"fingerprint": "fp-node1"
			},
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU", "severity": "critical", "instance": "node2"},
				"annotations": {"summary": "CPU high on node2"},
				"fingerprint": "fp-node2"
			}
		]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}

	var req1, req2 pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req1)
	testutil.UnmarshalBody(t, recorded[1].Body, &req2)
	if req1.CollapseID == req2.CollapseID {
		t.Errorf("expected different collapse IDs for different fingerprints, both got %s", req1.CollapseID)
	}
}

func TestThreadID_PerAlertname(t *testing.T) {
	handler, calls, mu := setup(t)

	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU", "severity": "critical"},
				"annotations": {"summary": "CPU high"},
				"fingerprint": "fp1"
			},
			{
				"status": "firing",
				"labels": {"alertname": "DiskFull", "severity": "warning"},
				"annotations": {"summary": "Disk full"},
				"fingerprint": "fp2"
			}
		]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}

	var req1, req2 pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req1)
	testutil.UnmarshalBody(t, recorded[1].Body, &req2)
	if req1.ThreadID == req2.ThreadID {
		t.Errorf("expected different thread IDs for different alertnames, both got %s", req1.ThreadID)
	}
	if req1.ThreadID != "grafana-highcpu" {
		t.Errorf("expected thread_id grafana-highcpu, got %s", req1.ThreadID)
	}
	if req2.ThreadID != "grafana-diskfull" {
		t.Errorf("expected thread_id grafana-diskfull, got %s", req2.ThreadID)
	}
}

func TestThreadID_SameAlertnameDifferentFingerprints(t *testing.T) {
	handler, calls, mu := setup(t)

	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU", "severity": "critical", "instance": "node1"},
				"annotations": {"summary": "CPU high on node1"},
				"fingerprint": "fp-node1"
			},
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU", "severity": "critical", "instance": "node2"},
				"annotations": {"summary": "CPU high on node2"},
				"fingerprint": "fp-node2"
			}
		]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}

	var req1, req2 pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req1)
	testutil.UnmarshalBody(t, recorded[1].Body, &req2)
	if req1.ThreadID != req2.ThreadID {
		t.Errorf("expected same thread ID for same alertname, got %s and %s", req1.ThreadID, req2.ThreadID)
	}
	if req1.ThreadID != "grafana-highcpu" {
		t.Errorf("expected thread_id grafana-highcpu, got %s", req1.ThreadID)
	}
}

func TestMaxBytesReader_OversizedBody(t *testing.T) {
	handler, _, _ := setup(t)
	bigPayload := strings.Repeat("x", 2<<20)
	w := sendWebhook(t, handler, bigPayload)
	// Huma returns 413 Request Entity Too Large for oversized bodies.
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for oversized body, got %d", w.Code)
	}
}
