package grafana

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtractRuleUID(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://grafana.example.com/alerting/abc123/edit", "abc123"},
		{"https://grafana.example.com/alerting/1afz29v7z/edit", "1afz29v7z"},
		{"https://grafana.example.com/alerting/rule-uid/view", "rule-uid"},
		{"", ""},
		{"https://grafana.example.com/dashboards", ""},
		{"https://grafana.example.com/alerting//edit", ""},
		{"https://grafana.example.com/alerting/../etc/passwd/edit", ""},
		{"https://grafana.example.com/alerting/uid%2F/edit", ""},
	}

	for _, tt := range tests {
		got := ExtractRuleUID(tt.url)
		if got != tt.want {
			t.Errorf("ExtractRuleUID(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestGetRuleQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Path != "/api/v1/provisioning/alert-rules/abc123" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"data": [
				{
					"refId": "A",
					"datasourceUid": "prometheus-uid",
					"model": {"expr": "rate(cpu[5m])"}
				},
				{
					"refId": "B",
					"datasourceUid": "-100",
					"model": {"type": "classic_conditions"}
				}
			]
		}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-token")
	rq, err := c.GetRuleQuery(context.Background(), "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if rq.Expr != "rate(cpu[5m])" {
		t.Errorf("Expr = %q, want %q", rq.Expr, "rate(cpu[5m])")
	}
	if rq.DatasourceUID != "prometheus-uid" {
		t.Errorf("DatasourceUID = %q, want %q", rq.DatasourceUID, "prometheus-uid")
	}
	if rq.RefID != "A" {
		t.Errorf("RefID = %q, want %q", rq.RefID, "A")
	}
}

func TestGetRuleQuery_Cached(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"refId":"A","datasourceUid":"prom","model":{"expr":"up"}}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token")
	_, _ = c.GetRuleQuery(context.Background(), "uid1")
	_, _ = c.GetRuleQuery(context.Background(), "uid1")

	if calls != 1 {
		t.Errorf("expected 1 API call (cached), got %d", calls)
	}
}

func TestGetRuleQuery_ForbiddenWithoutEditorRole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "viewer-token")
	_, err := c.GetRuleQuery(context.Background(), "abc123")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestGetRuleQuery_NoDataQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Only expression nodes, no real datasource query
		w.Write([]byte(`{"data":[{"refId":"B","datasourceUid":"-100","model":{"type":"classic_conditions"}}]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token")
	_, err := c.GetRuleQuery(context.Background(), "uid1")
	if err == nil {
		t.Fatal("expected error when no datasource query found")
	}
}
