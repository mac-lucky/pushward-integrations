package grafana

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
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

func setup(t *testing.T) (*state.MemoryStore, http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	cfg := testConfig()
	pool := client.NewPool(srv.URL)
	h := NewHandler(store, pool, cfg)
	return store, auth.Middleware(h), calls, mu
}

func setupWithConfig(t *testing.T, cfg *config.GrafanaConfig) (*state.MemoryStore, http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL)
	h := NewHandler(store, pool, cfg)
	return store, auth.Middleware(h), calls, mu
}

func sendWebhook(t *testing.T, handler http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func seedInstance(t *testing.T, store *state.MemoryStore, alertname, fingerprint string, info instanceInfo) {
	t.Helper()
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(context.Background(), "grafana", testKey, alertname, fingerprint, data, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
}

// --- Tests ---

func TestFiringSingleAlert(t *testing.T) {
	_, handler, calls, mu := setup(t)

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

	w := sendWebhook(t, handler, payload)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 API calls (create + update), got %d", len(recorded))
	}

	expectedSlug := slugForAlertname("HighCPUUsage")

	// First call: POST /activities (create)
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Slug != expectedSlug {
		t.Errorf("expected slug %s, got %s", expectedSlug, createReq.Slug)
	}
	if createReq.Name != "HighCPUUsage" {
		t.Errorf("expected name HighCPUUsage, got %s", createReq.Name)
	}
	if createReq.Priority != 5 {
		t.Errorf("expected priority 5, got %d", createReq.Priority)
	}

	// Second call: PATCH /activity/<slug> (update)
	expectedPatchPath := "/activity/" + expectedSlug
	if recorded[1].Method != "PATCH" || recorded[1].Path != expectedPatchPath {
		t.Errorf("expected PATCH %s, got %s %s", expectedPatchPath, recorded[1].Method, recorded[1].Path)
	}
	var updateReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &updateReq)
	if updateReq.State != pushward.StateOngoing {
		t.Errorf("expected state ONGOING, got %s", updateReq.State)
	}
	if updateReq.Content.Template != "alert" {
		t.Errorf("expected template alert, got %s", updateReq.Content.Template)
	}
	if updateReq.Content.Icon != "exclamationmark.octagon.fill" {
		t.Errorf("expected critical icon, got %s", updateReq.Content.Icon)
	}
	if updateReq.Content.AccentColor != pushward.ColorRed {
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
	store, handler, calls, mu := setup(t)

	seedInstance(t, store, "DiskSpaceLow", "f1e2d3c4b5a6", instanceInfo{
		Severity:     "warning",
		FiredAt:      time.Date(2026, 2, 18, 10, 0, 0, 0, time.UTC).Unix(),
		Subtitle:     "Grafana \u00b7 nas:9100",
		GeneratorURL: "https://grafana.example.com/alerting/grafana/f1e2d3c4b5a6/view",
		SecondaryURL: "https://grafana.example.com/d/disk-dashboard/disk",
	})

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

	w := sendWebhook(t, handler, payload)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// Resolved -> single PATCH (no create)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 API call (update only), got %d", len(recorded))
	}

	expectedPath := "/activity/" + slugForAlertname("DiskSpaceLow")
	if recorded[0].Method != "PATCH" || recorded[0].Path != expectedPath {
		t.Errorf("expected PATCH %s, got %s %s", expectedPath, recorded[0].Method, recorded[0].Path)
	}
	var updateReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &updateReq)
	if updateReq.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", updateReq.State)
	}
	if updateReq.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected resolved icon checkmark.circle.fill, got %s", updateReq.Content.Icon)
	}
	if updateReq.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected resolved color #34C759, got %s", updateReq.Content.AccentColor)
	}
	// panelURL was empty, should fall back to dashboardURL
	if updateReq.Content.SecondaryURL != "https://grafana.example.com/d/disk-dashboard/disk" {
		t.Errorf("expected dashboardURL as secondaryURL fallback, got %s", updateReq.Content.SecondaryURL)
	}
}

func TestFiringThenResolved_FullLifecycle(t *testing.T) {
	_, handler, calls, mu := setup(t)

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

	w := sendWebhook(t, handler, firingPayload)
	if w.Code != http.StatusOK {
		t.Fatalf("firing: expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
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

	w = sendWebhook(t, handler, resolvedPayload)
	if w.Code != http.StatusOK {
		t.Fatalf("resolved: expected 200, got %d", w.Code)
	}

	recorded = testutil.GetCalls(calls, mu)
	// firing: create + update, resolved: update -> 3 total
	if len(recorded) != 3 {
		t.Fatalf("expected 3 total API calls, got %d", len(recorded))
	}

	var endReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &endReq)
	if endReq.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", endReq.State)
	}
	if endReq.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected resolved icon, got %s", endReq.Content.Icon)
	}
}

func TestRefiringAlert_SkipsCreate(t *testing.T) {
	_, handler, calls, mu := setup(t)

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
	w := sendWebhook(t, handler, payload)
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

	w = sendWebhook(t, handler, refirePayload)
	if w.Code != http.StatusOK {
		t.Fatalf("refire: expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// first: create + update, second: update only -> 3 total
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
	testutil.UnmarshalBody(t, recorded[2].Body, &updateReq)
	if updateReq.Content.State != "Memory above 90% (escalating)" {
		t.Errorf("expected updated summary, got %s", updateReq.Content.State)
	}
}

func TestMultipleAlertsInSinglePayload_GroupedByAlertname(t *testing.T) {
	_, handler, calls, mu := setup(t)

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

	w := sendWebhook(t, handler, payload)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 3 {
		t.Fatalf("expected 3 API calls (create, update x2), got %d", len(recorded))
	}

	expectedSlug := slugForAlertname("HighCPU")

	// First: POST create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var cr pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &cr)
	if cr.Slug != expectedSlug {
		t.Errorf("expected slug %s, got %s", expectedSlug, cr.Slug)
	}
	if cr.Name != "HighCPU" {
		t.Errorf("expected name HighCPU, got %s", cr.Name)
	}

	// Second: PATCH with 1 instance (web-1, critical)
	var update1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update1)
	if update1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update1.State)
	}
	if update1.Content.Severity != "critical" {
		t.Errorf("expected critical severity, got %s", update1.Content.Severity)
	}
	if update1.Content.State != "CPU > 95% on web-1" {
		t.Errorf("expected single-instance summary, got %s", update1.Content.State)
	}

	// Third: PATCH with 2 instances (worst = critical from web-1)
	var update2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &update2)
	if update2.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update2.State)
	}
	if update2.Content.State != "2 instances firing" {
		t.Errorf("expected '2 instances firing', got %s", update2.Content.State)
	}
	if update2.Content.Severity != "critical" {
		t.Errorf("expected worst severity (critical) for 2-instance update, got %s", update2.Content.Severity)
	}
}

func TestPartialResolve_ActivityContinues(t *testing.T) {
	store, handler, calls, mu := setup(t)

	// Pre-seed 2 firing instances of the same alert
	seedInstance(t, store, "NodeDown", "fp-aaa", instanceInfo{
		Severity: "critical", Summary: "node-1 down", Subtitle: "Grafana \u00b7 node-1:9100",
	})
	seedInstance(t, store, "NodeDown", "fp-bbb", instanceInfo{
		Severity: "warning", Summary: "node-2 down", Subtitle: "Grafana \u00b7 node-2:9100",
	})

	// Resolve one instance
	resolvePayload := `{
		"status": "resolved",
		"alerts": [{
			"status": "resolved",
			"labels": {"alertname": "NodeDown", "severity": "critical", "instance": "node-1:9100"},
			"annotations": {"summary": "node-1 recovered"},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "2026-02-18T12:30:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "fp-aaa"
		}]
	}`

	w := sendWebhook(t, handler, resolvePayload)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	// Partial resolve -> ONGOING update with remaining instance (no ENDED)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 API call (ONGOING update), got %d", len(recorded))
	}
	var updateReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &updateReq)
	if updateReq.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING after partial resolve, got %s", updateReq.State)
	}
	// Remaining instance is "fp-bbb" (warning)
	if updateReq.Content.State != "node-2 down" {
		t.Errorf("expected remaining instance summary, got %s", updateReq.Content.State)
	}

	// Verify the store still has 1 instance
	group, _ := store.GetGroup(context.Background(), "grafana", testKey, "NodeDown")
	if len(group) != 1 {
		t.Errorf("expected 1 remaining instance, got %d", len(group))
	}
}

func TestResolvedForUntrackedAlert_Skipped(t *testing.T) {
	_, handler, calls, mu := setup(t)

	// Resolve an alert that was never tracked (e.g. bridge restarted)
	payload := `{
		"status": "resolved",
		"alerts": [{
			"status": "resolved",
			"labels": {"alertname": "UnknownAlert", "severity": "warning"},
			"annotations": {"summary": "recovered"},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "2026-02-18T12:05:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "unknown00001"
		}]
	}`

	w := sendWebhook(t, handler, payload)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Errorf("expected 0 API calls for untracked resolved alert, got %d", len(recorded))
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
			expectedColor: pushward.ColorRed,
		},
		{
			name:          "warning",
			severity:      "warning",
			expectedIcon:  "exclamationmark.triangle.fill",
			expectedColor: pushward.ColorOrange,
		},
		{
			name:          "info",
			severity:      "info",
			expectedIcon:  "info.circle.fill",
			expectedColor: pushward.ColorBlue,
		},
		{
			name:          "unknown severity falls back to default",
			severity:      "low",
			expectedIcon:  "exclamationmark.triangle.fill",
			expectedColor: pushward.ColorOrange,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, handler, calls, mu := setup(t)

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

			w := sendWebhook(t, handler, payload)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}

			recorded := testutil.GetCalls(calls, mu)
			if len(recorded) < 2 {
				t.Fatalf("expected at least 2 calls, got %d", len(recorded))
			}

			var updateReq pushward.UpdateRequest
			testutil.UnmarshalBody(t, recorded[1].Body, &updateReq)
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
	_, handler, calls, mu := setup(t)

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

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(recorded))
	}

	var updateReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &updateReq)

	// Default severity is "warning" -> icon = default icon, color = #FF9500
	if updateReq.Content.Severity != "warning" {
		t.Errorf("expected default severity 'warning', got %s", updateReq.Content.Severity)
	}
	if updateReq.Content.Icon != "exclamationmark.triangle.fill" {
		t.Errorf("expected default icon, got %s", updateReq.Content.Icon)
	}
}

func TestMissingAlertname_FallbackDefault(t *testing.T) {
	_, handler, calls, mu := setup(t)

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

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 1 {
		t.Fatalf("expected at least 1 call, got %d", len(recorded))
	}

	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "Grafana Alert" {
		t.Errorf("expected fallback name 'Grafana Alert', got %s", createReq.Name)
	}
}

func TestNoInstanceLabel_SubtitleIsPlainGrafana(t *testing.T) {
	_, handler, calls, mu := setup(t)

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

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(recorded))
	}

	var updateReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &updateReq)
	if updateReq.Content.Subtitle != "Grafana" {
		t.Errorf("expected plain 'Grafana' subtitle without instance, got %s", updateReq.Content.Subtitle)
	}
}

func TestURLFallback_PanelURL_Over_DashboardURL(t *testing.T) {
	_, handler, calls, mu := setup(t)

	// Has both panelURL and dashboardURL -> panelURL wins
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

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	var updateReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &updateReq)

	if updateReq.Content.URL != "https://grafana.example.com/alerting/1/view" {
		t.Errorf("expected generatorURL as URL, got %s", updateReq.Content.URL)
	}
	if updateReq.Content.SecondaryURL != "https://grafana.example.com/d/dash/1?viewPanel=5" {
		t.Errorf("expected panelURL as secondaryURL, got %s", updateReq.Content.SecondaryURL)
	}
}

func TestURLFallback_DashboardURL_WhenNoPanelURL(t *testing.T) {
	_, handler, calls, mu := setup(t)

	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "Test2", "severity": "info"},
			"annotations": {"summary": "test"},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "https://grafana.example.com/alerting/2/view",
			"dashboardURL": "https://grafana.example.com/d/dash/2",
			"panelURL": "",
			"fingerprint": "urlfallback02"
		}]
	}`

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	var updateReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &updateReq)

	if updateReq.Content.SecondaryURL != "https://grafana.example.com/d/dash/2" {
		t.Errorf("expected dashboardURL fallback, got %s", updateReq.Content.SecondaryURL)
	}
}

func TestNoURLs(t *testing.T) {
	_, handler, calls, mu := setup(t)

	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "Test3", "severity": "info"},
			"annotations": {"summary": "test"},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "nourls123456"
		}]
	}`

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	var updateReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &updateReq)

	if updateReq.Content.URL != "" {
		t.Errorf("expected empty URL, got %s", updateReq.Content.URL)
	}
	if updateReq.Content.SecondaryURL != "" {
		t.Errorf("expected empty secondaryURL, got %s", updateReq.Content.SecondaryURL)
	}
}

func TestCustomSeverityLabel(t *testing.T) {
	cfg := testConfig()
	cfg.SeverityLabel = "priority" // Custom label
	_, handler, calls, mu := setupWithConfig(t, cfg)

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

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	var updateReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &updateReq)

	// Should use "priority" label (critical) not "severity" label (info)
	if updateReq.Content.Severity != "critical" {
		t.Errorf("expected severity from custom label 'priority'=critical, got %s", updateReq.Content.Severity)
	}
	if updateReq.Content.Icon != "exclamationmark.octagon.fill" {
		t.Errorf("expected critical icon, got %s", updateReq.Content.Icon)
	}
}

func TestInvalidJSON(t *testing.T) {
	_, handler, _, _ := setup(t)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("not json"))
	req.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestEmptyAlertsArray(t *testing.T) {
	_, handler, calls, mu := setup(t)

	w := sendWebhook(t, handler, `{"status": "firing", "alerts": []}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Errorf("expected 0 API calls for empty alerts, got %d", len(recorded))
	}
}

func TestResolvedRemovesFromStore(t *testing.T) {
	store, handler, calls, mu := setup(t)

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

	sendWebhook(t, handler, firingPayload)

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

	sendWebhook(t, handler, resolvedPayload)

	// Alert group should be removed from store immediately after ENDED
	group, _ := store.GetGroup(context.Background(), "grafana", testKey, "TestCleanup")
	if len(group) != 0 {
		t.Error("expected alert group to be removed from store immediately after ENDED")
	}

	// Verify no DELETE calls -- server handles cleanup via ended_ttl
	recorded := testutil.GetCalls(calls, mu)
	for _, c := range recorded {
		if c.Method == "DELETE" {
			t.Error("unexpected DELETE call -- server handles cleanup via ended_ttl")
		}
	}
}

func TestCreateActivity_IncludesTTLValues(t *testing.T) {
	_, handler, calls, mu := setup(t)

	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "TTLAlert", "severity": "info"},
			"annotations": {"summary": "Test TTL values"},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "ttl123456789"
		}]
	}`

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) < 1 {
		t.Fatalf("expected at least 1 call, got %d", len(recorded))
	}

	cfg := testConfig()
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)

	expectedEndedTTL := int(cfg.CleanupDelay.Seconds())
	expectedStaleTTL := int(cfg.StaleTimeout.Seconds())

	if createReq.EndedTTL != expectedEndedTTL {
		t.Errorf("expected ended_ttl %d, got %d", expectedEndedTTL, createReq.EndedTTL)
	}
	if createReq.StaleTTL != expectedStaleTTL {
		t.Errorf("expected stale_ttl %d, got %d", expectedStaleTTL, createReq.StaleTTL)
	}
}

func TestRealisticGrafanaPayload_PrometheusAlertmanager(t *testing.T) {
	_, handler, calls, mu := setup(t)

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
					"namespace": "apps",
					"pod": "app-server-6f7d8e9a1b-x2k4m",
					"severity": "critical"
				},
				"annotations": {
					"description": "Pod app-server-6f7d8e9a1b-x2k4m in namespace apps has been in a non-ready state for longer than 15 minutes.",
					"runbook_url": "https://runbooks.prometheus-operator.dev/runbooks/kubernetes/kubepodnotready",
					"summary": "Pod has been in a non-ready state for more than 15 minutes."
				},
				"startsAt": "2026-02-18T08:15:30.123456789Z",
				"endsAt": "0001-01-01T00:00:00Z",
				"generatorURL": "https://grafana.example.local/alerting/grafana/a1b2c3d4/view",
				"fingerprint": "e4f5a6b7c8d9",
				"silenceURL": "https://grafana.example.local/alerting/silence/new?alertmanager=grafana&matcher=alertname%3DKubePodNotReady",
				"dashboardURL": "https://grafana.example.local/d/kubernetes-pods/kubernetes-pod-overview?orgId=1",
				"panelURL": "https://grafana.example.local/d/kubernetes-pods/kubernetes-pod-overview?orgId=1&viewPanel=4"
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
		"externalURL": "https://grafana.example.local/",
		"version": "1",
		"groupKey": "{}/{grafana_folder=\"Kubernetes\"}:{alertname=\"KubePodNotReady\"}",
		"truncatedAlerts": 0,
		"orgId": 1,
		"title": "[FIRING:1] KubePodNotReady Kubernetes (apps app-server-6f7d8e9a1b-x2k4m critical)"
	}`

	w := sendWebhook(t, handler, payload)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}

	expectedSlug := slugForAlertname("KubePodNotReady")
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Slug != expectedSlug {
		t.Errorf("slug: expected %s, got %s", expectedSlug, createReq.Slug)
	}
	if createReq.Name != "KubePodNotReady" {
		t.Errorf("name: expected KubePodNotReady, got %s", createReq.Name)
	}

	var updateReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &updateReq)
	if updateReq.Content.Subtitle != "Grafana \u00b7 10.0.1.5:8080" {
		t.Errorf("subtitle: expected 'Grafana \u00b7 10.0.1.5:8080', got %s", updateReq.Content.Subtitle)
	}
	if updateReq.Content.State != "Pod has been in a non-ready state for more than 15 minutes." {
		t.Errorf("state (summary): unexpected value: %s", updateReq.Content.State)
	}
	if updateReq.Content.URL != "https://grafana.example.local/alerting/grafana/a1b2c3d4/view" {
		t.Errorf("URL: unexpected value: %s", updateReq.Content.URL)
	}
	if updateReq.Content.SecondaryURL != "https://grafana.example.local/d/kubernetes-pods/kubernetes-pod-overview?orgId=1&viewPanel=4" {
		t.Errorf("secondaryURL: unexpected value: %s", updateReq.Content.SecondaryURL)
	}
}

func TestRealisticGrafanaPayload_MultiAlertGroup(t *testing.T) {
	store, handler, calls, mu := setup(t)

	// Pre-seed a tracked alert instance that will be resolved in this webhook
	seedInstance(t, store, "TargetDown", "resolved0000", instanceInfo{
		Severity: "warning",
		Summary:  "node-exporter target was down",
		Subtitle: "Grafana \u00b7 node-exporter:9100",
		FiredAt:  time.Date(2026, 2, 18, 7, 0, 0, 0, time.UTC).Unix(),
	})

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
				"generatorURL": "https://grafana.example.local/alerting/grafana/uid1/view",
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
				"generatorURL": "https://grafana.example.local/alerting/grafana/uid2/view",
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
				"generatorURL": "https://grafana.example.local/alerting/grafana/uid3/view",
				"dashboardURL": "",
				"panelURL": "",
				"fingerprint": "resolved0000"
			}
		],
		"groupLabels": {"alertname": "TargetDown"},
		"commonLabels": {"alertname": "TargetDown"},
		"externalURL": "https://grafana.example.local/",
		"version": "1",
		"title": "[FIRING:2] TargetDown"
	}`

	w := sendWebhook(t, handler, payload)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 3 {
		t.Fatalf("expected 3 API calls, got %d", len(recorded))
	}

	// All calls should be PATCH (no POST since group was pre-seeded)
	for i, c := range recorded {
		if c.Method != "PATCH" {
			t.Errorf("call %d: expected PATCH, got %s", i, c.Method)
		}
	}

	// Last call: partial resolve -> still ONGOING with 2 remaining instances
	var lastReq pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &lastReq)
	if lastReq.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING after partial resolve, got %s", lastReq.State)
	}
	if lastReq.Content.State != "2 instances firing" {
		t.Errorf("expected '2 instances firing', got %s", lastReq.Content.State)
	}
}

func TestUnknownAlertStatus_Ignored(t *testing.T) {
	_, handler, calls, mu := setup(t)

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

	w := sendWebhook(t, handler, payload)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 0 {
		t.Errorf("expected 0 API calls for unknown status, got %d", len(recorded))
	}
}

func TestMaxBytesReader_OversizedBody(t *testing.T) {
	_, handler, _, _ := setup(t)

	// 1<<20 = 1MB limit; send just over
	oversized := strings.Repeat("x", 1<<20+1)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(oversized))
	req.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for oversized body, got %d", w.Code)
	}
}

func TestCreateActivityFailure_CleansUpStore(t *testing.T) {
	// Server returns 400 on POST /activities to simulate create failure
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/activities" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	store := state.NewMemoryStore()
	cfg := testConfig()
	pool := client.NewPool(srv.URL)
	h := NewHandler(store, pool, cfg)
	handler := auth.Middleware(h)

	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "FailCreate", "severity": "warning"},
			"annotations": {"summary": "test"},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "failcreate01"
		}]
	}`

	w := sendWebhook(t, handler, payload)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (handler still returns OK), got %d", w.Code)
	}

	// Store should be cleaned up after create failure
	group, _ := store.GetGroup(context.Background(), "grafana", testKey, "FailCreate")
	if len(group) != 0 {
		t.Error("expected store to be cleaned up after create failure")
	}
}

func TestUpdateActivityFailure_OnFiring(t *testing.T) {
	// Server returns 200 on create, 422 on update (4xx = no retry)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	store := state.NewMemoryStore()
	cfg := testConfig()
	pool := client.NewPool(srv.URL)
	h := NewHandler(store, pool, cfg)
	handler := auth.Middleware(h)

	payload := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "FailUpdate", "severity": "warning"},
			"annotations": {"summary": "test"},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "0001-01-01T00:00:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "failupdate01"
		}]
	}`

	w := sendWebhook(t, handler, payload)

	// Handler still returns OK (errors are logged, not surfaced to webhook sender)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Instance should still exist in store (only create failure removes it)
	group, _ := store.GetGroup(context.Background(), "grafana", testKey, "FailUpdate")
	if len(group) == 0 {
		t.Error("expected instances to still exist in store after update failure")
	}
}

func TestUpdateActivityFailure_OnResolve(t *testing.T) {
	// Server returns 422 on PATCH to simulate update failure (4xx = no retry)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	store := state.NewMemoryStore()
	cfg := testConfig()
	pool := client.NewPool(srv.URL)
	h := NewHandler(store, pool, cfg)
	handler := auth.Middleware(h)

	// Pre-populate with an active instance
	seedInstance(t, store, "FailResolve", "failresolv01", instanceInfo{
		Severity: "warning", Summary: "test", Subtitle: "Grafana",
	})

	payload := `{
		"status": "resolved",
		"alerts": [{
			"status": "resolved",
			"labels": {"alertname": "FailResolve", "severity": "warning"},
			"annotations": {"summary": "recovered"},
			"startsAt": "2026-02-18T12:00:00Z",
			"endsAt": "2026-02-18T12:05:00Z",
			"generatorURL": "",
			"dashboardURL": "",
			"panelURL": "",
			"fingerprint": "failresolv01"
		}]
	}`

	w := sendWebhook(t, handler, payload)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestMissingAuthKey_Unauthorized(t *testing.T) {
	_, handler, _, _ := setup(t)

	payload := `{"status": "firing", "alerts": []}`

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	// No Authorization header
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth key, got %d", w.Code)
	}
}

func TestZeroStartsAt_FiredAtOmitted(t *testing.T) {
	_, handler, calls, mu := setup(t)

	// startsAt is "0001-01-01T00:00:00Z" (Go zero time) — common in Grafana test notifications
	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "TestAlert", "severity": "warning"},
				"annotations": {"summary": "Test alert"},
				"startsAt": "0001-01-01T00:00:00Z",
				"fingerprint": "test000"
			}
		]
	}`

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.FiredAt != nil {
		t.Errorf("expected FiredAt to be nil for zero startsAt, got %d", *update.Content.FiredAt)
	}
}

func TestEmptyStartsAt_FiredAtOmitted(t *testing.T) {
	_, handler, calls, mu := setup(t)

	// startsAt is empty — unparseable
	payload := `{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "TestAlert2", "severity": "warning"},
				"annotations": {"summary": "Test alert"},
				"startsAt": "",
				"fingerprint": "test001"
			}
		]
	}`

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.FiredAt != nil {
		t.Errorf("expected FiredAt to be nil for empty startsAt, got %d", *update.Content.FiredAt)
	}
}

func TestResolvedAlert_ZeroStartsAt_FiredAtOmitted(t *testing.T) {
	store, handler, calls, mu := setup(t)

	// Seed an instance with FiredAt=0 (simulating zero time parse)
	seedInstance(t, store, "TestResolve", "fp-zero", instanceInfo{
		Severity: "warning",
		FiredAt:  0,
		Subtitle: "Grafana",
	})

	payload := `{
		"alerts": [
			{
				"status": "resolved",
				"labels": {"alertname": "TestResolve", "severity": "warning"},
				"annotations": {"summary": "Resolved"},
				"startsAt": "0001-01-01T00:00:00Z",
				"fingerprint": "fp-zero"
			}
		]
	}`

	w := sendWebhook(t, handler, payload)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (end update), got %d", len(recorded))
	}

	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &update)
	if update.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", update.State)
	}
	if update.Content.FiredAt != nil {
		t.Errorf("expected FiredAt to be nil for zero FiredAt, got %d", *update.Content.FiredAt)
	}
}
