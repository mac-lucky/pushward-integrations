package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/grafana/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/grafana/internal/poller"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

type mockPWServer struct {
	server *httptest.Server

	mu      sync.Mutex
	creates []pushward.CreateActivityRequest
	updates []pushward.UpdateRequest
}

func newMockPWServer() *mockPWServer {
	m := &mockPWServer{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/activities":
			var req pushward.CreateActivityRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			m.mu.Lock()
			m.creates = append(m.creates, req)
			m.mu.Unlock()
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/activity/"):
			var req pushward.UpdateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			m.mu.Lock()
			m.updates = append(m.updates, req)
			m.mu.Unlock()
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return m
}

func (m *mockPWServer) close() { m.server.Close() }

func (m *mockPWServer) getCreates() []pushward.CreateActivityRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]pushward.CreateActivityRequest{}, m.creates...)
}

func (m *mockPWServer) getUpdates() []pushward.UpdateRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]pushward.UpdateRequest{}, m.updates...)
}

func fireWebhook(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.WaitIdle()
	return rec
}

func TestWebhook_FiringWithAnnotation(t *testing.T) {
	pw := newMockPWServer()
	defer pw.close()

	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"instance":"10.0.0.1:9100"},"values":[[1700000000,"10"],[1700000015,"20"]]}]}}`))
	}))
	defer promSrv.Close()

	pwClient := pushward.NewClient(pw.server.URL, "test-key")
	mc := metrics.NewClient(promSrv.URL)
	p := poller.New(mc, pwClient, 1*time.Hour)
	defer p.StopAll()

	h := NewHandler(pwClient, mc, nil, p, Config{Priority: 7})

	body := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "HighCPU", "severity": "critical"},
			"annotations": {
				"summary": "CPU over 80%",
				"pushward_query": "rate(cpu[5m])",
				"pushward_unit": "%",
				"pushward_threshold": "80",
				"pushward_series_label": "instance"
			},
			"values": {"B": 87.3},
			"startsAt": "2026-04-05T10:00:00Z",
			"generatorURL": "https://grafana.example.com/alerting/abc123/edit",
			"fingerprint": "abc123"
		}]
	}`

	rec := fireWebhook(t, h, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if len(pw.getCreates()) != 1 {
		t.Fatalf("creates = %d, want 1", len(pw.getCreates()))
	}
	if pw.getCreates()[0].Priority != 7 {
		t.Errorf("priority = %d, want 7", pw.getCreates()[0].Priority)
	}

	if len(pw.getUpdates()) != 1 {
		t.Fatalf("updates = %d, want 1", len(pw.getUpdates()))
	}
	up := pw.getUpdates()[0]
	if up.State != pushward.StateOngoing {
		t.Errorf("state = %q, want %q", up.State, pushward.StateOngoing)
	}
	if up.Content.Template != templateTimeline {
		t.Errorf("template = %q, want %q", up.Content.Template, templateTimeline)
	}
	if up.Content.Unit != "%" {
		t.Errorf("unit = %q, want %%", up.Content.Unit)
	}
	if len(up.Content.Thresholds) != 1 || up.Content.Thresholds[0].Value != 80 {
		t.Errorf("thresholds = %+v, want [{Value:80}]", up.Content.Thresholds)
	}
	if up.Content.History == nil || len(up.Content.History["10.0.0.1:9100"]) != 2 {
		t.Errorf("expected 2 history points keyed by instance label, got %v", up.Content.History)
	}
	if up.Content.AccentColor != pushward.ColorRed {
		t.Errorf("accent_color = %q, want critical red", up.Content.AccentColor)
	}

	if p.ActiveCount() != 1 {
		t.Errorf("poller active = %d, want 1", p.ActiveCount())
	}

	alerts := h.activeAlerts()
	if len(alerts) != 1 || alerts[0] != "HighCPU" {
		t.Errorf("active alerts = %v, want [HighCPU]", alerts)
	}
}

func TestWebhook_Resolved(t *testing.T) {
	pw := newMockPWServer()
	defer pw.close()

	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1700000000,"10"]]}]}}`))
	}))
	defer promSrv.Close()

	pwClient := pushward.NewClient(pw.server.URL, "test-key")
	mc := metrics.NewClient(promSrv.URL)
	p := poller.New(mc, pwClient, 1*time.Hour)
	defer p.StopAll()

	h := NewHandler(pwClient, mc, nil, p, Config{})

	fire := `{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"TestAlert"},"annotations":{"pushward_query":"up"},"values":{"A":1},"startsAt":"2026-04-05T10:00:00Z","generatorURL":"","fingerprint":"f1"}]}`
	fireWebhook(t, h, fire)

	resolve := `{"status":"resolved","alerts":[{"status":"resolved","labels":{"alertname":"TestAlert"},"annotations":{},"values":{"A":0},"startsAt":"2026-04-05T10:00:00Z","generatorURL":"","fingerprint":"f1"}]}`
	rec := fireWebhook(t, h, resolve)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var foundEnd bool
	for _, u := range pw.getUpdates() {
		if u.State == pushward.StateEnded {
			foundEnd = true
			if u.Content.Icon != resolvedIcon {
				t.Errorf("end icon = %q, want checkmark", u.Content.Icon)
			}
		}
	}
	if !foundEnd {
		t.Error("no ENDED update found")
	}

	if p.ActiveCount() != 0 {
		t.Errorf("poller active = %d, want 0", p.ActiveCount())
	}

	if len(h.activeAlerts()) != 0 {
		t.Errorf("active alerts = %v, want empty", h.activeAlerts())
	}
}

func TestWebhook_NoQueryFallback(t *testing.T) {
	pw := newMockPWServer()
	defer pw.close()

	pwClient := pushward.NewClient(pw.server.URL, "test-key")
	mc := metrics.NewClient("http://unused:9090")
	p := poller.New(mc, pwClient, 1*time.Hour)
	defer p.StopAll()

	h := NewHandler(pwClient, mc, nil, p, Config{})

	body := `{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"NoQuery"},"annotations":{},"values":{"A":42},"startsAt":"2026-04-05T10:00:00Z","generatorURL":"","fingerprint":"f2"}]}`
	rec := fireWebhook(t, h, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if len(pw.getCreates()) != 1 {
		t.Fatalf("creates = %d, want 1", len(pw.getCreates()))
	}
	if len(pw.getUpdates()) != 1 {
		t.Fatalf("updates = %d, want 1", len(pw.getUpdates()))
	}
	if pw.getUpdates()[0].Content.History != nil {
		t.Errorf("expected no history for fallback, got %v", pw.getUpdates()[0].Content.History)
	}

	if p.ActiveCount() != 0 {
		t.Errorf("poller active = %d, want 0 (no query)", p.ActiveCount())
	}
}

func TestWebhook_MethodNotAllowed(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, Config{})
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestWebhook_MultiSeriesHistory(t *testing.T) {
	pw := newMockPWServer()
	defer pw.close()

	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status": "success",
			"data": {
				"resultType": "matrix",
				"result": [
					{"metric": {"instance": "10.0.0.1:9100"}, "values": [[1700000000,"10"],[1700000015,"20"]]},
					{"metric": {"instance": "10.0.0.2:9100"}, "values": [[1700000000,"30"],[1700000015,"40"]]}
				]
			}
		}`))
	}))
	defer promSrv.Close()

	pwClient := pushward.NewClient(pw.server.URL, "test-key")
	mc := metrics.NewClient(promSrv.URL)
	p := poller.New(mc, pwClient, 1*time.Hour)
	defer p.StopAll()

	h := NewHandler(pwClient, mc, nil, p, Config{Priority: 5})

	body := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "MultiCPU", "severity": "warning"},
			"annotations": {
				"pushward_query": "rate(cpu[5m])",
				"pushward_series_label": "instance"
			},
			"values": {"B": 10},
			"startsAt": "2026-04-05T10:00:00Z",
			"generatorURL": "",
			"fingerprint": "multi1"
		}]
	}`

	rec := fireWebhook(t, h, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	updates := pw.getUpdates()
	if len(updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(updates))
	}

	hist := updates[0].Content.History
	if hist == nil {
		t.Fatal("expected non-nil history")
	}
	if len(hist["10.0.0.1:9100"]) != 2 {
		t.Errorf("expected 2 points for 10.0.0.1:9100, got %d", len(hist["10.0.0.1:9100"]))
	}
	if len(hist["10.0.0.2:9100"]) != 2 {
		t.Errorf("expected 2 points for 10.0.0.2:9100, got %d", len(hist["10.0.0.2:9100"]))
	}
}

// TestWebhook_ValuesUseMetricLabels verifies that the initial update uses
// Prometheus metric labels (e.g. "10.0.0.1:9100") instead of raw Grafana
// expression ref IDs (B, C) for the value keys.
func TestWebhook_ValuesUseMetricLabels(t *testing.T) {
	pw := newMockPWServer()
	defer pw.close()

	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status": "success",
			"data": {
				"resultType": "matrix",
				"result": [
					{"metric": {"instance": "home-server"}, "values": [[1700000000,"25"],[1700000015,"21.2"]]}
				]
			}
		}`))
	}))
	defer promSrv.Close()

	pwClient := pushward.NewClient(pw.server.URL, "test-key")
	mc := metrics.NewClient(promSrv.URL)
	p := poller.New(mc, pwClient, 1*time.Hour)
	defer p.StopAll()

	h := NewHandler(pwClient, mc, nil, p, Config{Priority: 5})

	// Simulate a Grafana webhook with expression ref IDs B and C as value keys.
	// B is the actual metric value, C is the threshold condition result.
	body := `{
		"status": "firing",
		"alerts": [{
			"status": "firing",
			"labels": {"alertname": "HomeServerCPULow"},
			"annotations": {
				"pushward_query": "100 - rate(cpu_idle[5m])",
				"pushward_series_label": "instance"
			},
			"values": {"B": 21.2, "C": 1.0},
			"startsAt": "2026-04-05T10:00:00Z",
			"generatorURL": "",
			"fingerprint": "cpu1"
		}]
	}`

	fireWebhook(t, h, body)

	updates := pw.getUpdates()
	if len(updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(updates))
	}

	// Value should use Prometheus metric label, not Grafana ref IDs.
	valMap, ok := updates[0].Content.Value.(map[string]interface{})
	if !ok {
		t.Fatalf("value is not a map: %T = %v", updates[0].Content.Value, updates[0].Content.Value)
	}

	if _, ok := valMap["B"]; ok {
		t.Errorf("value map should not use ref ID 'B', got %v", valMap)
	}
	if _, ok := valMap["C"]; ok {
		t.Errorf("value map should not use ref ID 'C', got %v", valMap)
	}
	if v, ok := valMap["home-server"]; !ok || v != 21.2 {
		t.Errorf("expected value keyed by 'home-server' = 21.2, got %v", valMap)
	}
}

func TestWebhook_MultiInstancePartialResolve(t *testing.T) {
	pw := newMockPWServer()
	defer pw.close()

	pwClient := pushward.NewClient(pw.server.URL, "test-key")
	mc := metrics.NewClient("http://unused:9090")
	p := poller.New(mc, pwClient, 1*time.Hour)
	defer p.StopAll()

	h := NewHandler(pwClient, mc, nil, p, Config{})

	// Fire two instances of the same alert.
	fire1 := `{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"CPUHigh"},"annotations":{},"values":{"A":90},"startsAt":"2026-04-05T10:00:00Z","generatorURL":"","fingerprint":"inst1"}]}`
	fireWebhook(t, h, fire1)

	fire2 := `{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"CPUHigh"},"annotations":{},"values":{"A":85},"startsAt":"2026-04-05T10:00:00Z","generatorURL":"","fingerprint":"inst2"}]}`
	fireWebhook(t, h, fire2)

	// Resolve one instance — activity should stay active.
	resolve1 := `{"status":"resolved","alerts":[{"status":"resolved","labels":{"alertname":"CPUHigh"},"annotations":{},"values":{"A":5},"startsAt":"2026-04-05T10:00:00Z","generatorURL":"","fingerprint":"inst1"}]}`
	fireWebhook(t, h, resolve1)

	if len(h.activeAlerts()) != 1 {
		t.Errorf("expected 1 active alert after partial resolve, got %v", h.activeAlerts())
	}

	// Check no ENDED update was sent.
	for _, u := range pw.getUpdates() {
		if u.State == pushward.StateEnded {
			t.Error("should not have sent ENDED when other instances are still firing")
		}
	}

	// Resolve the second instance — now the activity should end.
	resolve2 := `{"status":"resolved","alerts":[{"status":"resolved","labels":{"alertname":"CPUHigh"},"annotations":{},"values":{"A":3},"startsAt":"2026-04-05T10:00:00Z","generatorURL":"","fingerprint":"inst2"}]}`
	fireWebhook(t, h, resolve2)

	if len(h.activeAlerts()) != 0 {
		t.Errorf("expected 0 active alerts after full resolve, got %v", h.activeAlerts())
	}

	var foundEnd bool
	for _, u := range pw.getUpdates() {
		if u.State == pushward.StateEnded {
			foundEnd = true
		}
	}
	if !foundEnd {
		t.Error("expected ENDED update after all instances resolved")
	}
}

func TestWebhook_SamePayloadFireAndResolve(t *testing.T) {
	pw := newMockPWServer()
	defer pw.close()

	pwClient := pushward.NewClient(pw.server.URL, "test-key")
	mc := metrics.NewClient("http://unused:9090")
	p := poller.New(mc, pwClient, 1*time.Hour)
	defer p.StopAll()

	h := NewHandler(pwClient, mc, nil, p, Config{})

	// Grafana sends both firing (inst1) and resolved (inst2) in the same payload.
	// inst2 should NOT kill the activity created by inst1.
	body := `{"status":"firing","alerts":[
		{"status":"firing","labels":{"alertname":"MixedAlert"},"annotations":{},"values":{"A":50},"startsAt":"2026-04-05T10:00:00Z","generatorURL":"","fingerprint":"inst1"},
		{"status":"resolved","labels":{"alertname":"MixedAlert"},"annotations":{},"values":{"A":5},"startsAt":"2026-04-05T10:00:00Z","generatorURL":"","fingerprint":"inst2"}
	]}`
	fireWebhook(t, h, body)

	if len(h.activeAlerts()) != 1 {
		t.Errorf("expected 1 active alert, got %v", h.activeAlerts())
	}

	// No ENDED update should have been sent.
	for _, u := range pw.getUpdates() {
		if u.State == pushward.StateEnded {
			t.Error("should not have ended activity when firing instance still exists")
		}
	}
}

func TestMakeSlug(t *testing.T) {
	s1 := makeSlug("HighCPU")
	s2 := makeSlug("HighCPU")
	s3 := makeSlug("HighMemory")

	if s1 != s2 {
		t.Errorf("same alertname should produce same slug: %q != %q", s1, s2)
	}
	if s1 == s3 {
		t.Errorf("different alertnames should produce different slugs: %q == %q", s1, s3)
	}
	if !strings.HasPrefix(s1, "grafana-") {
		t.Errorf("slug should start with grafana-: %q", s1)
	}
}
