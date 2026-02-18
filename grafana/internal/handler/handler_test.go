package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-docker/grafana/internal/config"
	"github.com/mac-lucky/pushward-docker/grafana/internal/pushward"
)

// apiCall records a PushWard API call made by the handler.
type apiCall struct {
	Method string
	Path   string
	Body   json.RawMessage
}

// mockPushWardServer starts an httptest server that records all requests.
func mockPushWardServer(t *testing.T) (*httptest.Server, *[]apiCall, *sync.Mutex) {
	t.Helper()
	var calls []apiCall
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, apiCall{
			Method: r.Method,
			Path:   r.URL.Path,
			Body:   json.RawMessage(body),
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &mu
}

func testConfig() *config.Config {
	return &config.Config{
		Grafana: config.GrafanaConfig{
			SeverityLabel:   "severity",
			DefaultSeverity: "warning",
			DefaultIcon:     "exclamationmark.triangle.fill",
		},
		PushWard: config.PushWardConfig{
			Priority:     5,
			CleanupDelay: 1 * time.Hour,
			StaleTimeout: 24 * time.Hour,
		},
	}
}

func getCalls(calls *[]apiCall, mu *sync.Mutex) []apiCall {
	mu.Lock()
	defer mu.Unlock()
	result := make([]apiCall, len(*calls))
	copy(result, *calls)
	return result
}

// unmarshalBody decodes the JSON body of a recorded API call.
func unmarshalBody(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("failed to unmarshal body: %v (body: %s)", err, string(raw))
	}
}

// --- Real Grafana Alert Payloads ---

// Based on Grafana Alerting webhook format
// https://grafana.com/docs/grafana/latest/alerting/configure-notifications/manage-contact-points/integrations/webhook-notifier/

func TestFiringSingleAlert(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

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
					"summary": "CPU usage is above 90% on node-exporter:9100",
					"description": "CPU has been above 90% for the last 5 minutes."
				},
				"startsAt": "2026-02-18T10:30:00Z",
				"endsAt": "0001-01-01T00:00:00Z",
				"generatorURL": "https://grafana.example.com/alerting/grafana/abc123def456/view",
				"dashboardURL": "https://grafana.example.com/d/node-dashboard/node?orgId=1",
				"panelURL": "https://grafana.example.com/d/node-dashboard/node?orgId=1&viewPanel=2",
				"fingerprint": "abc123def456"
			}
		],
		"groupLabels": {"alertname": "HighCPUUsage"},
		"commonLabels": {"alertname": "HighCPUUsage", "severity": "critical"},
		"commonAnnotations": {"summary": "CPU usage is above 90%"},
		"externalURL": "https://grafana.example.com/",
		"version": "1",
		"groupKey": "{}:{alertname=\"HighCPUUsage\"}"
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.HandleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := getCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 API calls (create + update), got %d", len(recorded))
	}

	// First call: POST /activities (create)
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	unmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Slug != "grafana-abc123def456" {
		t.Errorf("expected slug grafana-abc123def456, got %s", createReq.Slug)
	}
	if createReq.Name != "HighCPUUsage" {
		t.Errorf("expected name HighCPUUsage, got %s", createReq.Name)
	}
	if createReq.Priority != 5 {
		t.Errorf("expected priority 5, got %d", createReq.Priority)
	}

	// Second call: PATCH /activity/<slug> (update)
	if recorded[1].Method != "PATCH" || recorded[1].Path != "/activity/grafana-abc123def456" {
		t.Errorf("expected PATCH /activity/grafana-abc123def456, got %s %s", recorded[1].Method, recorded[1].Path)
	}
	var updateReq pushward.UpdateRequest
	unmarshalBody(t, recorded[1].Body, &updateReq)
	if updateReq.State != "ONGOING" {
		t.Errorf("expected state ONGOING, got %s", updateReq.State)
	}
	if updateReq.Content.Template != "alert" {
		t.Errorf("expected template alert, got %s", updateReq.Content.Template)
	}
	if updateReq.Content.Icon != "exclamationmark.octagon.fill" {
		t.Errorf("expected critical icon, got %s", updateReq.Content.Icon)
	}
	if updateReq.Content.AccentColor != "#FF3B30" {
		t.Errorf("expected critical color #FF3B30, got %s", updateReq.Content.AccentColor)
	}
	if updateReq.Content.Severity != "critical" {
		t.Errorf("expected severity critical, got %s", updateReq.Content.Severity)
	}
	if updateReq.Content.Subtitle != "Grafana · node-exporter:9100" {
		t.Errorf("expected subtitle with instance, got %s", updateReq.Content.Subtitle)
	}
	if updateReq.Content.State != "CPU usage is above 90% on node-exporter:9100" {
		t.Errorf("expected summary as state, got %s", updateReq.Content.State)
	}
	if updateReq.Content.URL != "https://grafana.example.com/alerting/grafana/abc123def456/view" {
		t.Errorf("expected generatorURL, got %s", updateReq.Content.URL)
	}
	if updateReq.Content.SecondaryURL != "https://grafana.example.com/d/node-dashboard/node?orgId=1&viewPanel=2" {
		t.Errorf("expected panelURL as secondaryURL, got %s", updateReq.Content.SecondaryURL)
	}
	expectedFiredAt := time.Date(2026, 2, 18, 10, 30, 0, 0, time.UTC).Unix()
	if updateReq.Content.FiredAt == nil || *updateReq.Content.FiredAt != expectedFiredAt {
		t.Errorf("expected firedAt %d, got %v", expectedFiredAt, updateReq.Content.FiredAt)
	}
}

func TestResolvedAlert(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Pre-populate the handler with an active alert so resolved has something to work with
	h.activeAlerts["grafana-f1e2d3c4b5a6"] = &activeAlert{
		slug:    "grafana-f1e2d3c4b5a6",
		firedAt: time.Date(2026, 2, 18, 10, 0, 0, 0, time.UTC).Unix(),
		staleTimer: time.AfterFunc(24*time.Hour, func() {
			// no-op for test
		}),
	}

	payload := `{
		"receiver": "pushward",
		"status": "resolved",
		"alerts": [
			{
				"status": "resolved",
				"labels": {
					"alertname": "DiskSpaceLow",
					"severity": "warning",
					"instance": "nas:9100",
					"job": "node"
				},
				"annotations": {
					"summary": "Disk space recovered on nas:9100"
				},
				"startsAt": "2026-02-18T10:00:00Z",
				"endsAt": "2026-02-18T10:45:00Z",
				"generatorURL": "https://grafana.example.com/alerting/grafana/f1e2d3c4b5a6/view",
				"dashboardURL": "https://grafana.example.com/d/disk-dashboard/disk",
				"panelURL": "",
				"fingerprint": "f1e2d3c4b5a6"
			}
		],
		"groupLabels": {"alertname": "DiskSpaceLow"},
		"commonLabels": {"alertname": "DiskSpaceLow", "severity": "warning"},
		"commonAnnotations": {},
		"externalURL": "https://grafana.example.com/",
		"version": "1",
		"groupKey": "{}:{alertname=\"DiskSpaceLow\"}"
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.HandleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := getCalls(calls, mu)
	// Resolved → single PATCH (no create)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 API call (update only), got %d", len(recorded))
	}

	if recorded[0].Method != "PATCH" || recorded[0].Path != "/activity/grafana-f1e2d3c4b5a6" {
		t.Errorf("expected PATCH /activity/grafana-f1e2d3c4b5a6, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var updateReq pushward.UpdateRequest
	unmarshalBody(t, recorded[0].Body, &updateReq)
	if updateReq.State != "ENDED" {
		t.Errorf("expected ENDED, got %s", updateReq.State)
	}
	if updateReq.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected resolved icon checkmark.circle.fill, got %s", updateReq.Content.Icon)
	}
	if updateReq.Content.AccentColor != "#34C759" {
		t.Errorf("expected resolved color #34C759, got %s", updateReq.Content.AccentColor)
	}
	// panelURL was empty, should fall back to dashboardURL
	if updateReq.Content.SecondaryURL != "https://grafana.example.com/d/disk-dashboard/disk" {
		t.Errorf("expected dashboardURL as secondaryURL fallback, got %s", updateReq.Content.SecondaryURL)
	}
}

func TestFiringThenResolved_FullLifecycle(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Step 1: Firing alert
	firingPayload := `{
		"receiver": "pushward",
		"status": "firing",
		"alerts": [
			{
				"status": "firing",
				"labels": {
					"alertname": "PodCrashLooping",
					"severity": "critical",
					"namespace": "production",
					"pod": "api-server-6d8f9b7c5-x2k4p"
				},
				"annotations": {
					"summary": "Pod api-server-6d8f9b7c5-x2k4p is crash looping",
					"runbook_url": "https://runbooks.example.com/pod-crash-loop"
				},
				"startsAt": "2026-02-18T14:22:33Z",
				"endsAt": "0001-01-01T00:00:00Z",
				"generatorURL": "https://grafana.example.com/alerting/grafana/d4e5f6a7b8c9/view",
				"dashboardURL": "",
				"panelURL": "",
				"fingerprint": "d4e5f6a7b8c9"
			}
		],
		"version": "1"
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(firingPayload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("firing: expected 200, got %d", w.Code)
	}

	recorded := getCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("firing: expected 2 calls, got %d", len(recorded))
	}

	// Step 2: Resolved alert
	resolvedPayload := `{
		"receiver": "pushward",
		"status": "resolved",
		"alerts": [
			{
				"status": "resolved",
				"labels": {
					"alertname": "PodCrashLooping",
					"severity": "critical",
					"namespace": "production",
					"pod": "api-server-6d8f9b7c5-x2k4p"
				},
				"annotations": {
					"summary": "Pod crash loop resolved"
				},
				"startsAt": "2026-02-18T14:22:33Z",
				"endsAt": "2026-02-18T14:35:00Z",
				"generatorURL": "https://grafana.example.com/alerting/grafana/d4e5f6a7b8c9/view",
				"dashboardURL": "",
				"panelURL": "",
				"fingerprint": "d4e5f6a7b8c9"
			}
		],
		"version": "1"
	}`

	req = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(resolvedPayload))
	w = httptest.NewRecorder()
	h.HandleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("resolved: expected 200, got %d", w.Code)
	}

	recorded = getCalls(calls, mu)
	// firing: create + update, resolved: update → 3 total
	if len(recorded) != 3 {
		t.Fatalf("expected 3 total API calls, got %d", len(recorded))
	}

	var endReq pushward.UpdateRequest
	unmarshalBody(t, recorded[2].Body, &endReq)
	if endReq.State != "ENDED" {
		t.Errorf("expected ENDED, got %s", endReq.State)
	}
	if endReq.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected resolved icon, got %s", endReq.Content.Icon)
	}
}

func TestRefiringAlert_SkipsCreate(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "MemoryHigh", "severity": "warning"},
			"annotations": {"summary": "Memory above 85%"},
			"startsAt": "2026-02-18T09:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "aabbccddee11"
		}]
	}`

	// First fire
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first fire: expected 200, got %d", w.Code)
	}

	// Second fire (re-fire) with updated summary
	refirePayload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "MemoryHigh", "severity": "warning"},
			"annotations": {"summary": "Memory above 90% (escalating)"},
			"startsAt": "2026-02-18T09:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "aabbccddee11"
		}]
	}`

	req = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(refirePayload))
	w = httptest.NewRecorder()
	h.HandleWebhook(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("refire: expected 200, got %d", w.Code)
	}

	recorded := getCalls(calls, mu)
	// first: create + update, second: update only → 3 total
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls (create, update, update), got %d", len(recorded))
	}
	if recorded[0].Method != "POST" {
		t.Errorf("first call should be POST (create), got %s", recorded[0].Method)
	}
	if recorded[1].Method != "PATCH" {
		t.Errorf("second call should be PATCH (update), got %s", recorded[1].Method)
	}
	if recorded[2].Method != "PATCH" {
		t.Errorf("third call should be PATCH (update), got %s", recorded[2].Method)
	}

	// Verify the re-fire updated the summary
	var updateReq pushward.UpdateRequest
	unmarshalBody(t, recorded[2].Body, &updateReq)
	if updateReq.Content.State != "Memory above 90% (escalating)" {
		t.Errorf("expected updated summary, got %s", updateReq.Content.State)
	}
}

func TestMultipleAlertsInSinglePayload(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Grafana can send multiple alerts in a single webhook (grouped alerts)
	payload := `{
		"receiver": "pushward",
		"status": "firing",
		"alerts": [
			{
				"status": "firing",
				"labels": {
					"alertname": "HighCPU",
					"severity": "critical",
					"instance": "web-1:9100"
				},
				"annotations": {"summary": "CPU > 95% on web-1"},
				"startsAt": "2026-02-18T11:00:00Z",
				"endsAt": "0001-01-01T00:00:00Z",
				"generatorURL": "https://grafana.example.com/alerting/1/view",
				"dashboardURL": "",
				"panelURL": "",
				"fingerprint": "111111111111"
			},
			{
				"status": "firing",
				"labels": {
					"alertname": "HighCPU",
					"severity": "warning",
					"instance": "web-2:9100"
				},
				"annotations": {"summary": "CPU > 85% on web-2"},
				"startsAt": "2026-02-18T11:01:00Z",
				"endsAt": "0001-01-01T00:00:00Z",
				"generatorURL": "https://grafana.example.com/alerting/2/view",
				"dashboardURL": "",
				"panelURL": "",
				"fingerprint": "222222222222"
			},
			{
				"status": "resolved",
				"labels": {
					"alertname": "HighCPU",
					"severity": "info",
					"instance": "web-3:9100"
				},
				"annotations": {"summary": "CPU recovered on web-3"},
				"startsAt": "2026-02-18T10:50:00Z",
				"endsAt": "2026-02-18T11:02:00Z",
				"generatorURL": "https://grafana.example.com/alerting/3/view",
				"dashboardURL": "",
				"panelURL": "",
				"fingerprint": "333333333333"
			}
		],
		"groupLabels": {"alertname": "HighCPU"},
		"commonLabels": {"alertname": "HighCPU"},
		"version": "1"
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := getCalls(calls, mu)
	// Alert 1 (firing): create + update = 2
	// Alert 2 (firing): create + update = 2
	// Alert 3 (resolved): update only = 1
	// Total = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 API calls, got %d", len(recorded))
	}

	// Verify distinct slugs
	slugsSeen := map[string]bool{}
	for _, c := range recorded {
		if c.Method == "POST" {
			var cr pushward.CreateActivityRequest
			unmarshalBody(t, c.Body, &cr)
			slugsSeen[cr.Slug] = true
		}
	}
	if !slugsSeen["grafana-111111111111"] || !slugsSeen["grafana-222222222222"] {
		t.Errorf("expected two distinct slugs for firing alerts, got %v", slugsSeen)
	}
}

func TestSeverityMapping(t *testing.T) {
	tests := []struct {
		name          string
		severity      string
		expectedIcon  string
		expectedColor string
	}{
		{
			name:          "critical",
			severity:      "critical",
			expectedIcon:  "exclamationmark.octagon.fill",
			expectedColor: "#FF3B30",
		},
		{
			name:          "warning",
			severity:      "warning",
			expectedIcon:  "exclamationmark.triangle.fill",
			expectedColor: "#FF9500",
		},
		{
			name:          "info",
			severity:      "info",
			expectedIcon:  "info.circle.fill",
			expectedColor: "#007AFF",
		},
		{
			name:          "unknown severity falls back to default",
			severity:      "low",
			expectedIcon:  "exclamationmark.triangle.fill",
			expectedColor: "#FF9500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, calls, mu := mockPushWardServer(t)
			cfg := testConfig()
			client := pushward.NewClient(srv.URL, "hlk_test")
			h := New(client, cfg)

			payload := `{
				"status": "firing",
				"alerts": [{
					"status": "firing",
					"labels": {
						"alertname": "TestAlert",
						"severity": "` + tt.severity + `"
					},
					"annotations": {"summary": "test"},
					"startsAt": "2026-02-18T12:00:00Z",
					"endsAt": "0001-01-01T00:00:00Z",
					"generatorURL": "",
					"dashboardURL": "",
					"panelURL": "",
					"fingerprint": "sev_` + tt.severity + `00"
				}]
			}`

			req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
			w := httptest.NewRecorder()
			h.HandleWebhook(w, req)

			recorded := getCalls(calls, mu)
			if len(recorded) < 2 {
				t.Fatalf("expected at least 2 calls, got %d", len(recorded))
			}

			var updateReq pushward.UpdateRequest
			unmarshalBody(t, recorded[1].Body, &updateReq)
			if updateReq.Content.Icon != tt.expectedIcon {
				t.Errorf("icon: expected %s, got %s", tt.expectedIcon, updateReq.Content.Icon)
			}
			if updateReq.Content.AccentColor != tt.expectedColor {
				t.Errorf("color: expected %s, got %s", tt.expectedColor, updateReq.Content.AccentColor)
			}
		})
	}
}

func TestMissingSeverityLabel_UsesDefault(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Alert without "severity" label
	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {
				"alertname": "Watchdog"
			},
			"annotations": {},
			"startsAt": "2026-02-18T08:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "nosev1234567"
		}]
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	recorded := getCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(recorded))
	}

	var updateReq pushward.UpdateRequest
	unmarshalBody(t, recorded[1].Body, &updateReq)

	// Default severity is "warning" → icon = default icon, color = #FF9500
	if updateReq.Content.Severity != "warning" {
		t.Errorf("expected default severity 'warning', got %s", updateReq.Content.Severity)
	}
	if updateReq.Content.Icon != "exclamationmark.triangle.fill" {
		t.Errorf("expected default icon, got %s", updateReq.Content.Icon)
	}
}

func TestMissingAlertname_FallbackDefault(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Alert without "alertname" label (unusual but possible)
	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {
				"severity": "info"
			},
			"annotations": {"summary": "Some info alert"},
			"startsAt": "2026-02-18T15:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "noname123456"
		}]
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	recorded := getCalls(calls, mu)
	if len(recorded) < 1 {
		t.Fatalf("expected at least 1 call, got %d", len(recorded))
	}

	var createReq pushward.CreateActivityRequest
	unmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "Grafana Alert" {
		t.Errorf("expected fallback name 'Grafana Alert', got %s", createReq.Name)
	}
}

func TestNoInstanceLabel_SubtitleIsPlainGrafana(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {
				"alertname": "PrometheusDown",
				"severity": "critical"
			},
			"annotations": {"summary": "Prometheus is unreachable"},
			"startsAt": "2026-02-18T16:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "noinst123456"
		}]
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	recorded := getCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(recorded))
	}

	var updateReq pushward.UpdateRequest
	unmarshalBody(t, recorded[1].Body, &updateReq)
	if updateReq.Content.Subtitle != "Grafana" {
		t.Errorf("expected plain 'Grafana' subtitle without instance, got %s", updateReq.Content.Subtitle)
	}
}

func TestURLFallback_PanelURL_Over_DashboardURL(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Has both panelURL and dashboardURL → panelURL wins
	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "Test", "severity": "info"},
			"annotations": {"summary": "test"},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "https://grafana.example.com/alerting/1/view",
			"dashboardURL": "https://grafana.example.com/d/dash/1",
			"panelURL": "https://grafana.example.com/d/dash/1?viewPanel=5",
			"fingerprint": "urlfallback01"
		}]
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	recorded := getCalls(calls, mu)
	var updateReq pushward.UpdateRequest
	unmarshalBody(t, recorded[1].Body, &updateReq)

	if updateReq.Content.URL != "https://grafana.example.com/alerting/1/view" {
		t.Errorf("expected generatorURL as URL, got %s", updateReq.Content.URL)
	}
	if updateReq.Content.SecondaryURL != "https://grafana.example.com/d/dash/1?viewPanel=5" {
		t.Errorf("expected panelURL as secondaryURL, got %s", updateReq.Content.SecondaryURL)
	}
}

func TestURLFallback_DashboardURL_WhenNoPanelURL(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "Test", "severity": "info"},
			"annotations": {"summary": "test"},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "https://grafana.example.com/alerting/2/view",
			"dashboardURL": "https://grafana.example.com/d/dash/2",
			"panelURL": "",
			"fingerprint": "urlfallback02"
		}]
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	recorded := getCalls(calls, mu)
	var updateReq pushward.UpdateRequest
	unmarshalBody(t, recorded[1].Body, &updateReq)

	if updateReq.Content.SecondaryURL != "https://grafana.example.com/d/dash/2" {
		t.Errorf("expected dashboardURL fallback, got %s", updateReq.Content.SecondaryURL)
	}
}

func TestNoURLs(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "Test", "severity": "info"},
			"annotations": {"summary": "test"},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "nourls123456"
		}]
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	recorded := getCalls(calls, mu)
	var updateReq pushward.UpdateRequest
	unmarshalBody(t, recorded[1].Body, &updateReq)

	if updateReq.Content.URL != "" {
		t.Errorf("expected empty URL, got %s", updateReq.Content.URL)
	}
	if updateReq.Content.SecondaryURL != "" {
		t.Errorf("expected empty secondaryURL, got %s", updateReq.Content.SecondaryURL)
	}
}

func TestSlugTruncation_ShortFingerprint(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Fingerprint shorter than 12 chars
	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "Test"},
			"annotations": {},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "short"
		}]
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	recorded := getCalls(calls, mu)
	var createReq pushward.CreateActivityRequest
	unmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Slug != "grafana-short" {
		t.Errorf("expected slug grafana-short, got %s", createReq.Slug)
	}
}

func TestCustomSeverityLabel(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	cfg.Grafana.SeverityLabel = "priority" // Custom label
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {
				"alertname": "CustomLabel",
				"priority": "critical",
				"severity": "info"
			},
			"annotations": {"summary": "test custom label"},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "customlbl001"
		}]
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	recorded := getCalls(calls, mu)
	var updateReq pushward.UpdateRequest
	unmarshalBody(t, recorded[1].Body, &updateReq)

	// Should use "priority" label (critical) not "severity" label (info)
	if updateReq.Content.Severity != "critical" {
		t.Errorf("expected severity from custom label 'priority'=critical, got %s", updateReq.Content.Severity)
	}
	if updateReq.Content.Icon != "exclamationmark.octagon.fill" {
		t.Errorf("expected critical icon, got %s", updateReq.Content.Icon)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	cfg := testConfig()
	client := pushward.NewClient("http://unused", "hlk_test")
	h := New(client, cfg)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/webhook", nil)
			w := httptest.NewRecorder()
			h.HandleWebhook(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("expected 405 for %s, got %d", method, w.Code)
			}
		})
	}
}

func TestInvalidJSON(t *testing.T) {
	cfg := testConfig()
	client := pushward.NewClient("http://unused", "hlk_test")
	h := New(client, cfg)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestEmptyAlertsArray(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	payload := `{"status": "firing", "alerts": []}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := getCalls(calls, mu)
	if len(recorded) != 0 {
		t.Errorf("expected 0 API calls for empty alerts, got %d", len(recorded))
	}
}

func TestCleanupTimerScheduledOnResolved(t *testing.T) {
	srv, _, _ := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.CleanupDelay = 50 * time.Millisecond // Short delay for test
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Fire an alert
	firingPayload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "TestCleanup", "severity": "warning"},
			"annotations": {},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "cleanup12345"
		}]
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(firingPayload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	// Resolve it
	resolvedPayload := `{
		"status": "resolved",
		"alerts": [{
			"status": "resolved",
			"labels": {"alertname": "TestCleanup", "severity": "warning"},
			"annotations": {},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "2026-02-18T12:05:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "cleanup12345"
		}]
	}`

	req = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(resolvedPayload))
	w = httptest.NewRecorder()
	h.HandleWebhook(w, req)

	// Wait for cleanup timer
	time.Sleep(150 * time.Millisecond)

	// Verify alert was cleaned up
	h.mu.Lock()
	_, exists := h.activeAlerts["grafana-cleanup12345"]
	h.mu.Unlock()

	if exists {
		t.Error("expected alert to be cleaned up after timer, but it still exists")
	}
}

func TestStaleTimerForceEnds(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.StaleTimeout = 50 * time.Millisecond  // Short stale timeout for test
	cfg.PushWard.CleanupDelay = 50 * time.Millisecond  // Short cleanup delay
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "StaleAlert", "severity": "info"},
			"annotations": {"summary": "This will go stale"},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "stale1234567"
		}]
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	// Wait for stale timer + cleanup
	time.Sleep(200 * time.Millisecond)

	recorded := getCalls(calls, mu)
	// create + update (firing) + update (force-end) + delete (cleanup)
	if len(recorded) < 3 {
		t.Fatalf("expected at least 3 calls (create, update, force-end), got %d", len(recorded))
	}

	// Find the force-end call
	var forceEndReq pushward.UpdateRequest
	for _, c := range recorded {
		if c.Method == "PATCH" {
			var req pushward.UpdateRequest
			unmarshalBody(t, c.Body, &req)
			if req.Content.State == "Stale alert (auto-ended)" {
				forceEndReq = req
				break
			}
		}
	}
	if forceEndReq.State != "ENDED" {
		t.Error("expected a force-end PATCH with state 'Stale alert (auto-ended)'")
	}
	if forceEndReq.Content.Icon != "clock.badge.xmark" {
		t.Errorf("expected stale icon clock.badge.xmark, got %s", forceEndReq.Content.Icon)
	}
}

func TestRealisticGrafanaPayload_PrometheusAlertmanager(t *testing.T) {
	// Full realistic payload as Grafana would send from Prometheus-style alerting rules
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	payload := `{
		"receiver": "pushward-webhook",
		"status": "firing",
		"alerts": [
			{
				"status": "firing",
				"labels": {
					"__alert_rule_uid__": "a1b2c3d4",
					"alertname": "KubePodNotReady",
					"container": "pushward-server",
					"endpoint": "http",
					"grafana_folder": "Kubernetes",
					"instance": "10.0.1.5:8080",
					"job": "pushward",
					"namespace": "default",
					"pod": "pushward-server-78d4f6b9c-lm2nk",
					"severity": "critical"
				},
				"annotations": {
					"description": "Pod pushward-server-78d4f6b9c-lm2nk in namespace default has been in a non-ready state for longer than 15 minutes.",
					"runbook_url": "https://runbooks.prometheus-operator.dev/runbooks/kubernetes/kubepodnotready",
					"summary": "Pod has been in a non-ready state for more than 15 minutes."
				},
				"startsAt": "2026-02-18T08:15:30.123456789Z",
				"endsAt": "0001-01-01T00:00:00Z",
				"generatorURL": "https://grafana.homelab.local/alerting/grafana/a1b2c3d4/view",
				"fingerprint": "e4f5a6b7c8d9",
				"silenceURL": "https://grafana.homelab.local/alerting/silence/new?alertmanager=grafana&matcher=alertname%3DKubePodNotReady",
				"dashboardURL": "https://grafana.homelab.local/d/kubernetes-pods/kubernetes-pod-overview?orgId=1",
				"panelURL": "https://grafana.homelab.local/d/kubernetes-pods/kubernetes-pod-overview?orgId=1&viewPanel=4"
			}
		],
		"groupLabels": {
			"alertname": "KubePodNotReady",
			"grafana_folder": "Kubernetes"
		},
		"commonLabels": {
			"alertname": "KubePodNotReady",
			"severity": "critical"
		},
		"commonAnnotations": {
			"summary": "Pod has been in a non-ready state for more than 15 minutes."
		},
		"externalURL": "https://grafana.homelab.local/",
		"version": "1",
		"groupKey": "{}/{grafana_folder=\"Kubernetes\"}:{alertname=\"KubePodNotReady\"}",
		"truncatedAlerts": 0,
		"orgId": 1,
		"title": "[FIRING:1] KubePodNotReady Kubernetes (default pushward-server-78d4f6b9c-lm2nk critical)"
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.HandleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := getCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}

	var createReq pushward.CreateActivityRequest
	unmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Slug != "grafana-e4f5a6b7c8d9" {
		t.Errorf("slug: expected grafana-e4f5a6b7c8d9, got %s", createReq.Slug)
	}
	if createReq.Name != "KubePodNotReady" {
		t.Errorf("name: expected KubePodNotReady, got %s", createReq.Name)
	}

	var updateReq pushward.UpdateRequest
	unmarshalBody(t, recorded[1].Body, &updateReq)
	if updateReq.Content.Subtitle != "Grafana · 10.0.1.5:8080" {
		t.Errorf("subtitle: expected 'Grafana · 10.0.1.5:8080', got %s", updateReq.Content.Subtitle)
	}
	if updateReq.Content.State != "Pod has been in a non-ready state for more than 15 minutes." {
		t.Errorf("state (summary): unexpected value: %s", updateReq.Content.State)
	}
	if updateReq.Content.URL != "https://grafana.homelab.local/alerting/grafana/a1b2c3d4/view" {
		t.Errorf("URL: unexpected value: %s", updateReq.Content.URL)
	}
	if updateReq.Content.SecondaryURL != "https://grafana.homelab.local/d/kubernetes-pods/kubernetes-pod-overview?orgId=1&viewPanel=4" {
		t.Errorf("secondaryURL: unexpected value: %s", updateReq.Content.SecondaryURL)
	}
}

func TestRealisticGrafanaPayload_MultiAlertGroup(t *testing.T) {
	// Multiple alerts grouped by alertname, some firing and some resolved
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	// Pre-seed a tracked alert that will be resolved
	h.activeAlerts["grafana-resolved0000"] = &activeAlert{
		slug:    "grafana-resolved0000",
		firedAt: time.Date(2026, 2, 18, 7, 0, 0, 0, time.UTC).Unix(),
		staleTimer: time.AfterFunc(24*time.Hour, func() {}),
	}

	payload := `{
		"receiver": "pushward-webhook",
		"status": "firing",
		"alerts": [
			{
				"status": "firing",
				"labels": {
					"alertname": "TargetDown",
					"severity": "critical",
					"instance": "mariadb:9104",
					"job": "mariadb-exporter"
				},
				"annotations": {
					"summary": "mariadb-exporter target is down"
				},
				"startsAt": "2026-02-18T09:10:00Z",
				"endsAt": "0001-01-01T00:00:00Z",
				"generatorURL": "https://grafana.homelab.local/alerting/grafana/uid1/view",
				"dashboardURL": "",
				"panelURL": "",
				"fingerprint": "firing00000a"
			},
			{
				"status": "firing",
				"labels": {
					"alertname": "TargetDown",
					"severity": "warning",
					"instance": "cadvisor:8080",
					"job": "cadvisor"
				},
				"annotations": {
					"summary": "cadvisor target is down"
				},
				"startsAt": "2026-02-18T09:12:00Z",
				"endsAt": "0001-01-01T00:00:00Z",
				"generatorURL": "https://grafana.homelab.local/alerting/grafana/uid2/view",
				"dashboardURL": "",
				"panelURL": "",
				"fingerprint": "firing00000b"
			},
			{
				"status": "resolved",
				"labels": {
					"alertname": "TargetDown",
					"severity": "warning",
					"instance": "node-exporter:9100",
					"job": "node"
				},
				"annotations": {
					"summary": "node-exporter target recovered"
				},
				"startsAt": "2026-02-18T07:00:00Z",
				"endsAt": "2026-02-18T09:15:00Z",
				"generatorURL": "https://grafana.homelab.local/alerting/grafana/uid3/view",
				"dashboardURL": "",
				"panelURL": "",
				"fingerprint": "resolved0000"
			}
		],
		"groupLabels": {"alertname": "TargetDown"},
		"commonLabels": {"alertname": "TargetDown"},
		"externalURL": "https://grafana.homelab.local/",
		"version": "1",
		"title": "[FIRING:2] TargetDown"
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewBufferString(payload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := getCalls(calls, mu)
	// firing00000a: create + update = 2
	// firing00000b: create + update = 2
	// resolved0000: update only = 1
	// Total = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 API calls, got %d", len(recorded))
	}

	// Verify the resolved call is ENDED
	lastCall := recorded[4]
	if lastCall.Method != "PATCH" {
		t.Errorf("last call should be PATCH, got %s", lastCall.Method)
	}
	var endReq pushward.UpdateRequest
	unmarshalBody(t, lastCall.Body, &endReq)
	if endReq.State != "ENDED" {
		t.Errorf("resolved alert should be ENDED, got %s", endReq.State)
	}
}

func TestUnknownAlertStatus_Ignored(t *testing.T) {
	srv, calls, mu := mockPushWardServer(t)
	cfg := testConfig()
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "suppressed",
			"labels": {"alertname": "Suppressed"},
			"annotations": {},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "suppressed01"
		}]
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := getCalls(calls, mu)
	if len(recorded) != 0 {
		t.Errorf("expected 0 API calls for unknown status, got %d", len(recorded))
	}
}

func TestResolvedCancelsCleanupOnRefire(t *testing.T) {
	srv, _, _ := mockPushWardServer(t)
	cfg := testConfig()
	cfg.PushWard.CleanupDelay = 100 * time.Millisecond
	client := pushward.NewClient(srv.URL, "hlk_test")
	h := New(client, cfg)

	fingerprint := "refire123456"
	slug := "grafana-refire123456"

	makePayload := func(status string) string {
		return `{
			"status": "` + status + `",
			"alerts": [{
				"status": "` + status + `",
				"labels": {"alertname": "FlapAlert", "severity": "warning"},
				"annotations": {"summary": "flapping"},
				"startsAt": "2026-02-18T12:00:00Z",
				"endsAt": "0001-01-01T00:00:00Z",
				"generatorURL": "",
				"dashboardURL": "",
				"panelURL": "",
				"fingerprint": "` + fingerprint + `"
			}]
		}`
	}

	// Fire
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(makePayload("firing")))
	w := httptest.NewRecorder()
	h.HandleWebhook(w, req)

	// Resolve (starts cleanup timer)
	req = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(makePayload("resolved")))
	w = httptest.NewRecorder()
	h.HandleWebhook(w, req)

	// Immediately re-fire (should cancel cleanup timer)
	req = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(makePayload("firing")))
	w = httptest.NewRecorder()
	h.HandleWebhook(w, req)

	// Wait longer than cleanup delay
	time.Sleep(200 * time.Millisecond)

	// Alert should still exist (cleanup was cancelled by re-fire)
	h.mu.Lock()
	_, exists := h.activeAlerts[slug]
	h.mu.Unlock()

	if !exists {
		t.Error("expected alert to survive re-fire (cleanup should have been cancelled)")
	}
}
