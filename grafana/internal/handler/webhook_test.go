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
			json.NewDecoder(r.Body).Decode(&req)
			m.mu.Lock()
			m.creates = append(m.creates, req)
			m.mu.Unlock()
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/activity/"):
			var req pushward.UpdateRequest
			json.NewDecoder(r.Body).Decode(&req)
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
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1700000000,"10"],[1700000015,"20"]]}]}}`))
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
				"pushward_threshold": "80"
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
	if up.Content.History == nil || len(up.Content.History["HighCPU"]) != 2 {
		t.Errorf("expected 2 history points keyed by alertname, got %v", up.Content.History)
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
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1700000000,"10"]]}]}}`))
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
