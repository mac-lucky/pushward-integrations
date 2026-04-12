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
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
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

func newTestAPI(t *testing.T, cfg *config.GrafanaConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex, *state.MemoryStore) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	pool := client.NewPool(srv.URL, nil)
	store := state.NewMemoryStore()

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, store, pool, cfg)

	return mux, calls, mu, store
}

func setup(t *testing.T) (http.Handler, *[]testutil.APICall, *sync.Mutex, *state.MemoryStore) {
	t.Helper()
	return newTestAPI(t, testConfig())
}

func setupWithConfig(t *testing.T, cfg *config.GrafanaConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex, *state.MemoryStore) {
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

// --- Single-alert tests (preserve existing behavior) ---

func TestFiringSingleAlert(t *testing.T) {
	handler, calls, mu, _ := setup(t)

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
	if req.Metadata["fingerprint"] != "abc123def456" {
		t.Errorf("expected fingerprint abc123def456 in metadata, got %q", req.Metadata["fingerprint"])
	}
}

func TestResolvedAlert(t *testing.T) {
	handler, calls, mu, _ := setup(t)

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
	handler, calls, mu, _ := setup(t)

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

func TestRefiringAlert_SendsNotification(t *testing.T) {
	handler, calls, mu, _ := setup(t)

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
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 notification on first fire, got %d", len(recorded))
	}

	// Second fire (same state) — should be skipped by dedup
	sendWebhook(t, handler, payload)
	recorded = testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected still 1 notification (dedup), got %d", len(recorded))
	}
}

func TestSeverityMapping(t *testing.T) {
	handler, calls, mu, _ := setup(t)

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
				"labels": {"alertname": "Test` + tt.severity + `", "severity": "` + tt.severity + `"},
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
	handler, calls, mu, _ := setup(t)

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
	handler, calls, mu, _ := setup(t)

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
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "TestNoInst", "severity": "info"},
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
	handler, calls, mu, _ := setupWithConfig(t, cfg)

	payload := `{
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "TestCustomSev", "priority": "critical"},
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
	handler, _, _, _ := setup(t)
	w := sendWebhook(t, handler, `{invalid`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestEmptyAlertsArray(t *testing.T) {
	handler, calls, mu, _ := setup(t)

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
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [{
			"status": "silenced",
			"labels": {"alertname": "TestSilenced", "severity": "info"},
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
	handler, _, _, _ := setup(t)
	req := httptest.NewRequest(http.MethodPost, "/grafana", strings.NewReader(`{"alerts":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestResolvedWithEmptySummary(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [{
			"status": "resolved",
			"labels": {"alertname": "TestEmptySummary", "severity": "info"},
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
	if req.Body != "Resolved · TestEmptySummary" {
		t.Errorf("expected body 'Resolved · TestEmptySummary', got %s", req.Body)
	}
}

func TestThreadID_PerAlertname(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU1", "severity": "critical"},
				"annotations": {"summary": "CPU high"},
				"fingerprint": "fp1"
			},
			{
				"status": "firing",
				"labels": {"alertname": "DiskFull1", "severity": "warning"},
				"annotations": {"summary": "Disk full"},
				"fingerprint": "fp2"
			}
		]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls (different alertnames), got %d", len(recorded))
	}

	var req1, req2 pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req1)
	testutil.UnmarshalBody(t, recorded[1].Body, &req2)
	if req1.ThreadID == req2.ThreadID {
		t.Errorf("expected different thread IDs for different alertnames, both got %s", req1.ThreadID)
	}
	if req1.ThreadID != "grafana-highcpu1" {
		t.Errorf("expected thread_id grafana-highcpu1, got %s", req1.ThreadID)
	}
	if req2.ThreadID != "grafana-diskfull1" {
		t.Errorf("expected thread_id grafana-diskfull1, got %s", req2.ThreadID)
	}
}

func TestMaxBytesReader_OversizedBody(t *testing.T) {
	handler, _, _, _ := setup(t)
	bigPayload := strings.Repeat("x", 2<<20)
	w := sendWebhook(t, handler, bigPayload)
	// Huma returns 413 Request Entity Too Large for oversized bodies.
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for oversized body, got %d", w.Code)
	}
}

// --- Grouped notification tests ---

func TestMultipleAlertsInSinglePayload(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU2", "severity": "critical", "instance": "node1"},
				"annotations": {"summary": "CPU high on node1"},
				"fingerprint": "fp1"
			},
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU2", "severity": "critical", "instance": "node2"},
				"annotations": {"summary": "CPU high on node2"},
				"fingerprint": "fp2"
			},
			{
				"status": "firing",
				"labels": {"alertname": "DiskFull2", "severity": "warning"},
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
	if len(recorded) != 2 {
		t.Fatalf("expected 2 notifications (grouped HighCPU2 + single DiskFull2), got %d", len(recorded))
	}

	// First: grouped HighCPU2 notification
	var req1 pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req1)
	if req1.Title != "HighCPU2" {
		t.Errorf("expected title HighCPU2, got %s", req1.Title)
	}
	if req1.Subtitle != "Grafana · 2 firing" {
		t.Errorf("expected subtitle 'Grafana · 2 firing', got %s", req1.Subtitle)
	}

	// Second: single DiskFull2 notification
	var req2 pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &req2)
	if req2.Title != "DiskFull2" {
		t.Errorf("expected title DiskFull2, got %s", req2.Title)
	}
}

func TestGroupedNotification_AllFiring(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU3", "severity": "critical", "instance": "node1:9100"},
				"annotations": {"summary": "CPU high"},
				"fingerprint": "fp1"
			},
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU3", "severity": "warning", "instance": "node2:9100"},
				"annotations": {"summary": "CPU elevated"},
				"fingerprint": "fp2"
			},
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU3", "severity": "info", "instance": "node3:9100"},
				"annotations": {"summary": "CPU above threshold"},
				"fingerprint": "fp3"
			}
		]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 grouped notification, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)

	if req.Title != "HighCPU3" {
		t.Errorf("expected title HighCPU3, got %s", req.Title)
	}
	if req.Subtitle != "Grafana · 3 firing" {
		t.Errorf("expected subtitle 'Grafana · 3 firing', got %s", req.Subtitle)
	}
	if req.Category != "critical" {
		t.Errorf("expected category critical (highest severity), got %s", req.Category)
	}
	if req.Level != pushward.LevelActive {
		t.Errorf("expected level active, got %s", req.Level)
	}
	if req.Body != "HighCPU3 · node1:9100, node2:9100, node3:9100" {
		t.Errorf("expected body listing instances, got %s", req.Body)
	}

	// Check metadata
	if req.Metadata["firing_count"] != "3" {
		t.Errorf("expected firing_count 3, got %s", req.Metadata["firing_count"])
	}
	if req.Metadata["resolved_count"] != "0" {
		t.Errorf("expected resolved_count 0, got %s", req.Metadata["resolved_count"])
	}
	// Per-instance metadata entries should exist for each instance.
	for _, inst := range []string{"node1:9100", "node2:9100", "node3:9100"} {
		if _, ok := req.Metadata[inst]; !ok {
			t.Errorf("expected per-instance metadata key %q, got keys %v", inst, req.Metadata)
		}
	}
}

func TestGroupedNotification_AllResolved(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [
			{
				"status": "resolved",
				"labels": {"alertname": "HighCPU4", "severity": "warning", "instance": "node1"},
				"annotations": {"summary": "CPU recovered"},
				"fingerprint": "fp1"
			},
			{
				"status": "resolved",
				"labels": {"alertname": "HighCPU4", "severity": "warning", "instance": "node2"},
				"annotations": {"summary": "CPU recovered on node2"},
				"fingerprint": "fp2"
			}
		]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 grouped notification, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)

	if req.Subtitle != "Grafana · 2 resolved" {
		t.Errorf("expected subtitle 'Grafana · 2 resolved', got %s", req.Subtitle)
	}
	if req.Category != "resolved" {
		t.Errorf("expected category resolved, got %s", req.Category)
	}
	if req.Level != pushward.LevelPassive {
		t.Errorf("expected level passive, got %s", req.Level)
	}
	if !strings.HasPrefix(req.Body, "Resolved") {
		t.Errorf("expected body to start with 'Resolved', got %s", req.Body)
	}
}

func TestGroupedNotification_MixedFiringAndResolved(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU5", "severity": "critical", "instance": "node1"},
				"annotations": {"summary": "CPU high on node1"},
				"fingerprint": "fp1"
			},
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU5", "severity": "warning", "instance": "node2"},
				"annotations": {"summary": "CPU high on node2"},
				"fingerprint": "fp2"
			},
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU5", "severity": "warning", "instance": "node3"},
				"annotations": {"summary": "CPU high on node3"},
				"fingerprint": "fp3"
			},
			{
				"status": "resolved",
				"labels": {"alertname": "HighCPU5", "severity": "warning", "instance": "node4"},
				"annotations": {"summary": "CPU recovered on node4"},
				"fingerprint": "fp4"
			}
		]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 grouped notification, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)

	if req.Subtitle != "Grafana · 3 firing, 1 resolved" {
		t.Errorf("expected subtitle 'Grafana · 3 firing, 1 resolved', got %s", req.Subtitle)
	}
	if req.Level != pushward.LevelActive {
		t.Errorf("expected level active (some still firing), got %s", req.Level)
	}
	if req.Category != "critical" {
		t.Errorf("expected category critical (highest severity), got %s", req.Category)
	}
}

func TestGroupedNotification_MixedSeverities(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU6", "severity": "info", "instance": "node1"},
				"annotations": {"summary": "low"},
				"fingerprint": "fp1"
			},
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU6", "severity": "critical", "instance": "node2"},
				"annotations": {"summary": "high"},
				"fingerprint": "fp2"
			},
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU6", "severity": "warning", "instance": "node3"},
				"annotations": {"summary": "medium"},
				"fingerprint": "fp3"
			}
		]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Category != "critical" {
		t.Errorf("expected highest severity critical, got %s", req.Category)
	}
}

func TestGroupedNotification_CollapseID(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	// Two alerts with same alertname → grouped → alertname-based CollapseID
	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU7", "severity": "critical", "instance": "node1"},
				"annotations": {"summary": "CPU high on node1"},
				"fingerprint": "fp-node1"
			},
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU7", "severity": "critical", "instance": "node2"},
				"annotations": {"summary": "CPU high on node2"},
				"fingerprint": "fp-node2"
			}
		]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 grouped call, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)

	// Grouped CollapseID should be based on alertname only (no fingerprint)
	expectedCollapseID := "grafana-" // prefix
	if !strings.HasPrefix(req.CollapseID, expectedCollapseID) {
		t.Errorf("expected collapse ID to start with 'grafana-', got %s", req.CollapseID)
	}

	// Verify it differs from per-fingerprint CollapseID
	singleFPCollapseID := "grafana-" + "HighCPU7:fp-node1"
	if strings.Contains(req.CollapseID, singleFPCollapseID) {
		t.Error("grouped CollapseID should not contain fingerprint")
	}
}

func TestGroupedNotification_DifferentAlertnames(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU8", "severity": "critical", "instance": "node1"},
				"annotations": {"summary": "CPU high"},
				"fingerprint": "fp1"
			},
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU8", "severity": "warning", "instance": "node2"},
				"annotations": {"summary": "CPU high"},
				"fingerprint": "fp2"
			},
			{
				"status": "firing",
				"labels": {"alertname": "DiskFull8", "severity": "warning", "instance": "nas1"},
				"annotations": {"summary": "Disk full"},
				"fingerprint": "fp3"
			},
			{
				"status": "firing",
				"labels": {"alertname": "DiskFull8", "severity": "warning", "instance": "nas2"},
				"annotations": {"summary": "Disk full"},
				"fingerprint": "fp4"
			}
		]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 grouped notifications, got %d", len(recorded))
	}

	var req1, req2 pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req1)
	testutil.UnmarshalBody(t, recorded[1].Body, &req2)

	if req1.Title != "HighCPU8" {
		t.Errorf("expected first title HighCPU8, got %s", req1.Title)
	}
	if req2.Title != "DiskFull8" {
		t.Errorf("expected second title DiskFull8, got %s", req2.Title)
	}
	if req1.Subtitle != "Grafana · 2 firing" {
		t.Errorf("expected first subtitle 'Grafana · 2 firing', got %s", req1.Subtitle)
	}
	if req2.Subtitle != "Grafana · 2 firing" {
		t.Errorf("expected second subtitle 'Grafana · 2 firing', got %s", req2.Subtitle)
	}
}

func TestGroupedNotification_EmptySummary(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU9", "severity": "critical", "instance": "node1"},
				"annotations": {},
				"fingerprint": "fp1"
			},
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU9", "severity": "critical", "instance": "node2"},
				"annotations": {},
				"fingerprint": "fp2"
			}
		]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Body != "HighCPU9 · node1, node2" {
		t.Errorf("expected body listing instances, got %s", req.Body)
	}
}

func TestGroupedNotification_Metadata(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "HighCPU10", "severity": "critical", "instance": "node1:9100", "job": "node"},
				"annotations": {"summary": "CPU high"},
				"fingerprint": "fp1",
				"startsAt": "2026-02-18T10:30:00Z",
				"values": {"A": 95},
				"valueString": "[ var='A' value=95 ]"
			},
			{
				"status": "resolved",
				"labels": {"alertname": "HighCPU10", "severity": "warning", "instance": "node2:9100", "job": "node"},
				"annotations": {"summary": "CPU recovered"},
				"fingerprint": "fp2",
				"startsAt": "2026-02-18T10:00:00Z"
			}
		]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)

	if req.Metadata["firing_count"] != "1" {
		t.Errorf("expected firing_count 1, got %s", req.Metadata["firing_count"])
	}
	if req.Metadata["resolved_count"] != "1" {
		t.Errorf("expected resolved_count 1, got %s", req.Metadata["resolved_count"])
	}
	// Per-instance detail entries.
	node1Detail := req.Metadata["node1:9100"]
	if !strings.Contains(node1Detail, "Firing") || !strings.Contains(node1Detail, "CPU high") {
		t.Errorf("expected node1:9100 detail with status and summary, got %q", node1Detail)
	}
	if !strings.Contains(node1Detail, "A = 95") {
		t.Errorf("expected node1:9100 detail to contain values from Values map, got %q", node1Detail)
	}
	node2Detail := req.Metadata["node2:9100"]
	if !strings.Contains(node2Detail, "Resolved") || !strings.Contains(node2Detail, "CPU recovered") {
		t.Errorf("expected node2:9100 detail with status and summary, got %q", node2Detail)
	}
	if req.Metadata["alertname"] != "HighCPU10" {
		t.Errorf("expected alertname in metadata, got %s", req.Metadata["alertname"])
	}
	if req.Metadata["job"] != "node" {
		t.Errorf("expected job in metadata, got %s", req.Metadata["job"])
	}
}

// --- State dedup tests ---

func TestStateDedup_SameWebhookTwice(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "DedupTest1", "severity": "critical"},
			"annotations": {"summary": "test"},
			"fingerprint": "fp1"
		}]
	}`

	// First send
	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 notification on first send, got %d", len(recorded))
	}

	// Second send with same payload — should be skipped
	sendWebhook(t, handler, payload)
	recorded = testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected still 1 notification after dedup, got %d", len(recorded))
	}
}

func TestStateDedup_StateChange(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	// Fire with 1 instance
	sendWebhook(t, handler, `{
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "DedupTest2", "severity": "critical"},
			"annotations": {"summary": "test"},
			"fingerprint": "fp1"
		}]
	}`)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(recorded))
	}

	// State changes: new fingerprint added
	sendWebhook(t, handler, `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "DedupTest2", "severity": "critical"},
				"annotations": {"summary": "test"},
				"fingerprint": "fp1"
			},
			{
				"status": "firing",
				"labels": {"alertname": "DedupTest2", "severity": "critical"},
				"annotations": {"summary": "test"},
				"fingerprint": "fp2"
			}
		]
	}`)
	recorded = testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 notifications (state changed), got %d", len(recorded))
	}
}

func TestStateDedup_ColdResolve(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	// Resolved webhook with no prior state — should still send
	payload := `{
		"alerts": [{
			"status": "resolved",
			"labels": {"alertname": "ColdResolve1", "severity": "warning"},
			"annotations": {"summary": "recovered"},
			"fingerprint": "fp1"
		}]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 notification for cold resolve, got %d", len(recorded))
	}
}

func TestStateDedup_FullResolveDeletesState(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	// Fire
	sendWebhook(t, handler, `{
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "ResolveDelete1", "severity": "critical"},
			"annotations": {"summary": "test"},
			"fingerprint": "fp1"
		}]
	}`)

	// Resolve (all resolved → state deleted)
	sendWebhook(t, handler, `{
		"alerts": [{
			"status": "resolved",
			"labels": {"alertname": "ResolveDelete1", "severity": "critical"},
			"annotations": {"summary": "recovered"},
			"fingerprint": "fp1"
		}]
	}`)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 notifications (fire + resolve), got %d", len(recorded))
	}

	// Send resolved again — should re-send because state was deleted
	sendWebhook(t, handler, `{
		"alerts": [{
			"status": "resolved",
			"labels": {"alertname": "ResolveDelete1", "severity": "critical"},
			"annotations": {"summary": "recovered"},
			"fingerprint": "fp1"
		}]
	}`)

	recorded = testutil.GetCalls(calls, mu)
	if len(recorded) != 3 {
		t.Fatalf("expected 3 notifications (state was deleted, re-sent), got %d", len(recorded))
	}
}

func TestStateDedup_GroupedSameWebhookTwice(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "GroupDedup1", "severity": "critical", "instance": "node1"},
				"annotations": {"summary": "test"},
				"fingerprint": "fp1"
			},
			{
				"status": "firing",
				"labels": {"alertname": "GroupDedup1", "severity": "critical", "instance": "node2"},
				"annotations": {"summary": "test"},
				"fingerprint": "fp2"
			}
		]
	}`

	// First send
	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 grouped notification, got %d", len(recorded))
	}

	// Second send — dedup
	sendWebhook(t, handler, payload)
	recorded = testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected still 1 notification after dedup, got %d", len(recorded))
	}
}

func TestSingleAlertPreservesCollapseID(t *testing.T) {
	handler, calls, mu, _ := setup(t)

	payload := `{
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "SingleCollapse1", "severity": "critical", "instance": "node1"},
			"annotations": {"summary": "test"},
			"fingerprint": "specific-fp"
		}]
	}`

	sendWebhook(t, handler, payload)
	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)

	// Single alert should use fingerprint-based CollapseID
	expectedCollapseID := "grafana-" // just verify prefix format
	if !strings.HasPrefix(req.CollapseID, expectedCollapseID) {
		t.Errorf("expected collapse ID with grafana prefix, got %s", req.CollapseID)
	}
	// Subtitle should show instance, not counts
	if !strings.Contains(req.Subtitle, "node1") {
		t.Errorf("expected subtitle to contain instance, got %s", req.Subtitle)
	}
}
