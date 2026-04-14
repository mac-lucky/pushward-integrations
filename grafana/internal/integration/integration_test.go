//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	grafanaapi "github.com/mac-lucky/pushward-integrations/grafana/internal/grafana"
	"github.com/mac-lucky/pushward-integrations/grafana/internal/handler"
	"github.com/mac-lucky/pushward-integrations/grafana/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/grafana/internal/poller"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// Shared containers across all tests.
var sharedEnv testEnv

func TestMain(m *testing.M) {
	ctx := context.Background()

	networkName := "pushward-integration-test"
	net, err := testcontainers.GenericNetwork(ctx, testcontainers.GenericNetworkRequest{
		NetworkRequest: testcontainers.NetworkRequest{Name: networkName},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create network: %v\n", err)
		os.Exit(1)
	}

	vmCtr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "victoriametrics/victoria-metrics:v1.117.1",
			ExposedPorts: []string{"8428/tcp"},
			Cmd:          []string{"-search.latencyOffset=1s", "-retentionPeriod=1d"},
			Networks:     []string{networkName},
			NetworkAliases: map[string][]string{
				networkName: {"victoriametrics"},
			},
			WaitingFor: wait.ForHTTP("/health").WithPort("8428/tcp").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "start VictoriaMetrics: %v\n", err)
		os.Exit(1)
	}

	grafanaCtr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "grafana/grafana:11.6.0",
			ExposedPorts: []string{"3000/tcp"},
			Networks:     []string{networkName},
			Env: map[string]string{
				"GF_AUTH_ANONYMOUS_ENABLED":  "true",
				"GF_AUTH_ANONYMOUS_ORG_ROLE": "Admin",
			},
			WaitingFor: wait.ForHTTP("/api/health").WithPort("3000/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "start Grafana: %v\n", err)
		os.Exit(1)
	}

	vmHost, _ := vmCtr.Host(ctx)
	vmPort, _ := vmCtr.MappedPort(ctx, "8428/tcp")
	grafanaHost, _ := grafanaCtr.Host(ctx)
	grafanaPort, _ := grafanaCtr.MappedPort(ctx, "3000/tcp")

	sharedEnv = testEnv{
		grafanaURL: fmt.Sprintf("http://%s:%s", grafanaHost, grafanaPort.Port()),
		vmURL:      fmt.Sprintf("http://%s:%s", vmHost, vmPort.Port()),
		vmInternal: "http://victoriametrics:8428",
	}

	code := m.Run()

	// Cleanup
	grafanaCtr.Terminate(ctx)
	vmCtr.Terminate(ctx)
	net.Remove(ctx)
	os.Exit(code)
}

// testEnv holds URLs for the test containers.
type testEnv struct {
	grafanaURL string
	vmURL      string
	vmInternal string
}

// apiCall records a PushWard API call for assertions.
type apiCall struct {
	Method string
	Path   string
	Body   json.RawMessage
}

func mockPWServer(t *testing.T) (*httptest.Server, *[]apiCall, *sync.Mutex) {
	t.Helper()
	var calls []apiCall
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, apiCall{Method: r.Method, Path: r.URL.Path, Body: body})
		mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/activities":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/activity/"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &mu
}

func getCalls(calls *[]apiCall, mu *sync.Mutex) []apiCall {
	mu.Lock()
	defer mu.Unlock()
	return append([]apiCall{}, *calls...)
}

func writeMetric(t *testing.T, vmURL, line string) {
	t.Helper()
	resp, err := http.Post(vmURL+"/api/v1/import/prometheus", "text/plain", strings.NewReader(line+"\n"))
	if err != nil {
		t.Fatal("write metric:", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("write metric: status %d", resp.StatusCode)
	}
}

func waitForMetric(t *testing.T, vmURL, expr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("%s/api/v1/query?query=%s", vmURL, expr))
		if err == nil {
			var result struct {
				Data struct {
					Result []json.RawMessage `json:"result"`
				} `json:"data"`
			}
			json.NewDecoder(resp.Body).Decode(&result)
			resp.Body.Close()
			if len(result.Data.Result) > 0 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("metric %q not available after %s", expr, timeout)
}

// hostWebhookURL rewrites 127.0.0.1 to host.docker.internal for container access.
func hostWebhookURL(serverURL, path string) string {
	return strings.Replace(serverURL, "127.0.0.1", "host.docker.internal", 1) + path
}

type grafanaAPI struct{ url string }

func (g *grafanaAPI) post(path string, body any) (json.RawMessage, error) {
	return g.do(http.MethodPost, path, body)
}

func (g *grafanaAPI) put(path string, body any) (json.RawMessage, error) {
	return g.do(http.MethodPut, path, body)
}

func (g *grafanaAPI) do(method, path string, body any) (json.RawMessage, error) {
	var bodyReader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, g.url+path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Disable-Provenance", "true")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("grafana %s %s: %d: %s", method, path, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// setupGrafana creates datasource, contact point, and notification policy.
// Each test gets a unique datasource name and contact point to avoid conflicts.
func setupGrafana(t *testing.T, env testEnv, webhookURL, dsName string) string {
	t.Helper()
	g := &grafanaAPI{url: env.grafanaURL}

	dsUID := strings.ToLower(strings.ReplaceAll(dsName, " ", "-"))
	_, err := g.post("/api/datasources", map[string]any{
		"name":   dsName,
		"uid":    dsUID,
		"type":   "prometheus",
		"url":    env.vmInternal,
		"access": "proxy",
	})
	if err != nil {
		t.Fatal("create datasource:", err)
	}

	// Create folder (ignore error if already exists)
	g.post("/api/folders", map[string]any{
		"uid":   "test-alerts",
		"title": "Test Alerts",
	})

	cpName := "webhook-" + dsName
	_, err = g.post("/api/v1/provisioning/contact-points", map[string]any{
		"name": cpName,
		"type": "webhook",
		"settings": map[string]any{
			"url":        webhookURL,
			"httpMethod": "POST",
		},
	})
	if err != nil {
		t.Fatal("create contact point:", err)
	}

	_, err = g.put("/api/v1/provisioning/policies", map[string]any{
		"receiver":        cpName,
		"group_by":        []string{"grafana_folder", "alertname"},
		"group_wait":      "0s",
		"group_interval":  "1s",
		"repeat_interval": "1h",
		"routes":          []any{},
	})
	if err != nil {
		t.Fatal("set notification policy:", err)
	}

	return dsUID
}

func createAlertRule(t *testing.T, grafanaURL, dsUID, expr, title string, annotations map[string]string) {
	t.Helper()
	g := &grafanaAPI{url: grafanaURL}

	rule := map[string]any{
		"title":        title,
		"ruleGroup":    "test-group-" + title,
		"folderUID":    "test-alerts",
		"noDataState":  "OK",
		"execErrState": "OK",
		"for":          "0s",
		"orgId":        1,
		"condition":    "B",
		"annotations":  annotations,
		"data": []map[string]any{
			{
				"refId":             "A",
				"queryType":         "",
				"relativeTimeRange": map[string]int{"from": 600, "to": 0},
				"datasourceUid":     dsUID,
				"model": map[string]any{
					"expr":          expr,
					"hide":          false,
					"intervalMs":    1000,
					"maxDataPoints": 43200,
					"refId":         "A",
				},
			},
			{
				"refId":             "B",
				"queryType":         "",
				"relativeTimeRange": map[string]int{"from": 0, "to": 0},
				"datasourceUid":     "-100",
				"model": map[string]any{
					"conditions": []map[string]any{
						{
							"evaluator": map[string]any{"params": []int{0}, "type": "gt"},
							"operator":  map[string]any{"type": "and"},
							"query":     map[string]any{"params": []string{"A"}},
							"reducer":   map[string]any{"params": []any{}, "type": "last"},
							"type":      "query",
						},
					},
					"datasource":    map[string]any{"type": "__expr__", "uid": "-100"},
					"hide":          false,
					"intervalMs":    1000,
					"maxDataPoints": 43200,
					"refId":         "B",
					"type":          "classic_conditions",
				},
			},
		},
	}

	_, err := g.post("/api/v1/provisioning/alert-rules", rule)
	if err != nil {
		t.Fatal("create alert rule:", err)
	}
}

func waitForCalls(t *testing.T, calls *[]apiCall, mu *sync.Mutex, minCalls int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(getCalls(calls, mu)) >= minCalls {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("expected at least %d PW API calls, got %d after %s", minCalls, len(getCalls(calls, mu)), timeout)
}

// findUpdate searches PW API calls for an UpdateActivity with the given state.
func findUpdate(calls []apiCall, state string) *pushward.UpdateRequest {
	for _, c := range calls {
		if c.Method == http.MethodPatch && strings.HasPrefix(c.Path, "/activity/") {
			var req pushward.UpdateRequest
			json.Unmarshal(c.Body, &req)
			if req.State == state {
				return &req
			}
		}
	}
	return nil
}

func TestIntegration_FiringWithAnnotation(t *testing.T) {
	pwSrv, calls, mu := mockPWServer(t)
	mc := metrics.NewClient(sharedEnv.vmURL)
	pwClient := pushward.NewClient(pwSrv.URL, "test-key")
	p := poller.New(mc, pwClient, 30*time.Second)
	defer func() { p.StopAll(); p.Wait() }()

	h := handler.NewHandler(pwClient, mc, nil, p, handler.Config{
		HistoryWindow: 30 * time.Minute,
		Priority:      7,
	})

	handlerSrv := httptest.NewServer(h)
	defer handlerSrv.Close()

	writeMetric(t, sharedEnv.vmURL, `integration_cpu{job="test"} 85`)
	waitForMetric(t, sharedEnv.vmURL, "integration_cpu", 10*time.Second)

	webhookURL := hostWebhookURL(handlerSrv.URL, "/webhook")
	dsUID := setupGrafana(t, sharedEnv, webhookURL, "VM-Firing")

	createAlertRule(t, sharedEnv.grafanaURL, dsUID, "integration_cpu", "HighCPU", map[string]string{
		"pushward_query":     "integration_cpu",
		"pushward_unit":      "%",
		"pushward_threshold": "80",
		"summary":            "CPU over 80%",
	})

	waitForCalls(t, calls, mu, 2, 90*time.Second)
	h.WaitIdle()

	allCalls := getCalls(calls, mu)

	// Verify CreateActivity
	var foundCreate bool
	for _, c := range allCalls {
		if c.Method == http.MethodPost && c.Path == "/activities" {
			foundCreate = true
			var req pushward.CreateActivityRequest
			json.Unmarshal(c.Body, &req)
			if req.Priority != 7 {
				t.Errorf("priority = %d, want 7", req.Priority)
			}
			if req.Name != "HighCPU" {
				t.Errorf("name = %q, want HighCPU", req.Name)
			}
		}
	}
	if !foundCreate {
		t.Fatal("no CreateActivity call found")
	}

	// Verify UpdateActivity with ONGOING state
	up := findUpdate(allCalls, pushward.StateOngoing)
	if up == nil {
		t.Fatal("no ONGOING UpdateActivity call found")
	}
	if up.Content.Template != pushward.TemplateTimeline {
		t.Errorf("template = %q, want %q", up.Content.Template, pushward.TemplateTimeline)
	}
	if up.Content.Unit != "%" {
		t.Errorf("unit = %q, want %%", up.Content.Unit)
	}
	if len(up.Content.Thresholds) == 0 || up.Content.Thresholds[0].Value != 80 {
		t.Errorf("thresholds = %+v, want [{Value:80 ...}]", up.Content.Thresholds)
	}
	if up.Content.History == nil {
		t.Error("expected seeded history from VictoriaMetrics, got nil")
	}

	t.Logf("received %d PW API calls", len(allCalls))
}

func TestIntegration_AutoExtract(t *testing.T) {
	pwSrv, calls, mu := mockPWServer(t)
	mc := metrics.NewClient(sharedEnv.vmURL)
	pwClient := pushward.NewClient(pwSrv.URL, "test-key")

	gc := grafanaapi.NewClient(sharedEnv.grafanaURL, "")
	p := poller.New(mc, pwClient, 30*time.Second)
	defer func() { p.StopAll(); p.Wait() }()

	h := handler.NewHandler(pwClient, mc, gc, p, handler.Config{
		HistoryWindow: 30 * time.Minute,
		Priority:      5,
	})

	handlerSrv := httptest.NewServer(h)
	defer handlerSrv.Close()

	writeMetric(t, sharedEnv.vmURL, `auto_extract_metric{job="test"} 42`)
	waitForMetric(t, sharedEnv.vmURL, "auto_extract_metric", 10*time.Second)

	webhookURL := hostWebhookURL(handlerSrv.URL, "/webhook")
	dsUID := setupGrafana(t, sharedEnv, webhookURL, "Prometheus-AutoExtract")

	// NO pushward_query annotation — handler must auto-extract from Grafana API
	createAlertRule(t, sharedEnv.grafanaURL, dsUID, "auto_extract_metric", "AutoExtractAlert", map[string]string{
		"summary": "Auto extracted query test",
	})

	waitForCalls(t, calls, mu, 2, 90*time.Second)
	h.WaitIdle()

	allCalls := getCalls(calls, mu)

	up := findUpdate(allCalls, pushward.StateOngoing)
	if up == nil {
		t.Fatal("no ONGOING UpdateActivity call found")
	}
	if up.Content.History == nil {
		t.Error("expected seeded history from auto-extracted query, got nil")
	}

	if p.ActiveCount() == 0 {
		t.Error("expected poller to be active (query was auto-extracted)")
	}
}

func TestIntegration_Resolved(t *testing.T) {
	pwSrv, calls, mu := mockPWServer(t)
	mc := metrics.NewClient(sharedEnv.vmURL)
	pwClient := pushward.NewClient(pwSrv.URL, "test-key")
	p := poller.New(mc, pwClient, 30*time.Second)
	defer func() { p.StopAll(); p.Wait() }()

	h := handler.NewHandler(pwClient, mc, nil, p, handler.Config{
		HistoryWindow: 30 * time.Minute,
		Priority:      5,
	})

	handlerSrv := httptest.NewServer(h)
	defer handlerSrv.Close()

	writeMetric(t, sharedEnv.vmURL, `resolve_metric{job="test"} 100`)
	waitForMetric(t, sharedEnv.vmURL, "resolve_metric", 30*time.Second)

	webhookURL := hostWebhookURL(handlerSrv.URL, "/webhook")
	dsUID := setupGrafana(t, sharedEnv, webhookURL, "VM-Resolve")

	createAlertRule(t, sharedEnv.grafanaURL, dsUID, "resolve_metric", "ResolveAlert", map[string]string{
		"pushward_query": "resolve_metric",
		"summary":        "Resolve test",
	})

	// Wait for firing
	waitForCalls(t, calls, mu, 2, 90*time.Second)
	h.WaitIdle()
	t.Log("alert fired, now resolving")

	// Write 0 to resolve (classic_conditions checks > 0)
	writeMetric(t, sharedEnv.vmURL, `resolve_metric{job="test"} 0`)

	// Wait for ENDED state specifically — Grafana needs multiple eval cycles
	deadline := time.Now().Add(120 * time.Second)
	var up *pushward.UpdateRequest
	for time.Now().Before(deadline) {
		up = findUpdate(getCalls(calls, mu), pushward.StateEnded)
		if up != nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	h.WaitIdle()

	if up == nil {
		t.Fatal("no ENDED UpdateActivity call found within 120s")
	}
	if up.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("resolved icon = %q, want checkmark.circle.fill", up.Content.Icon)
	}
}
