package poller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/grafana/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

func TestStartStop(t *testing.T) {
	// Verify start/stop lifecycle without real HTTP calls.
	// metricsClient and pwClient are nil — the goroutine will tick but
	// poll() will panic if called, so we stop before the first tick.
	p := New(nil, nil, 1*time.Hour) // long interval so it won't tick
	p.Start("test-slug", "up", "TestAlert")

	if p.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d, want 1", p.ActiveCount())
	}

	// Start again — should be a no-op
	p.Start("test-slug", "up", "TestAlert")
	if p.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d after duplicate start, want 1", p.ActiveCount())
	}

	p.Stop("test-slug")
	p.Wait()

	if p.ActiveCount() != 0 {
		t.Fatalf("ActiveCount = %d after stop, want 0", p.ActiveCount())
	}
}

func TestStopAll(t *testing.T) {
	p := New(nil, nil, 1*time.Hour)
	p.Start("slug-1", "up", "Alert1")
	p.Start("slug-2", "up", "Alert2")

	if p.ActiveCount() != 2 {
		t.Fatalf("ActiveCount = %d, want 2", p.ActiveCount())
	}

	p.StopAll()
	p.Wait()

	if p.ActiveCount() != 0 {
		t.Fatalf("ActiveCount = %d after StopAll, want 0", p.ActiveCount())
	}
}

func TestStopNonExistent(t *testing.T) {
	p := New(nil, nil, 1*time.Hour)
	p.Stop("does-not-exist") // should not panic
}

// pwCall records one request made to the mock PushWard server.
type pwCall struct {
	method string
	path   string
	body   []byte
}

// pwRecorder is a minimal PushWard mock that records method+path+body and
// replies with a fixed status. The poller never calls CreateActivity, so the
// contract-validating testutil.MockPushWardServer (which 404s a PATCH for an
// uncreated slug) is unsuitable here — we only need to observe the raw calls.
type pwRecorder struct {
	server *httptest.Server
	status int

	mu    sync.Mutex
	calls []pwCall
}

func newPWRecorder(t *testing.T, status int) *pwRecorder {
	t.Helper()
	r := &pwRecorder{status: status}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.calls = append(r.calls, pwCall{method: req.Method, path: req.URL.Path, body: body})
		r.mu.Unlock()
		w.WriteHeader(r.status)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *pwRecorder) getCalls() []pwCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]pwCall{}, r.calls...)
}

// newInstantMetricsClient returns a metrics.Client backed by an httptest server
// that answers instant queries with a single labeled point.
func newInstantMetricsClient(t *testing.T, body string) *metrics.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return metrics.NewClient(srv.URL)
}

// decodeContent extracts the "content" object from a recorded request body as a
// generic map so tests can assert which keys are present on the wire.
func decodeContent(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var req struct {
		Content map[string]any `json:"content"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decoding body %s: %v", body, err)
	}
	return req.Content
}

func assertValueKey(t *testing.T, content map[string]any, key string, want float64) {
	t.Helper()
	vm, ok := content["value"].(map[string]any)
	if !ok {
		t.Fatalf("content.value is not a map: %T (%v)", content["value"], content["value"])
	}
	v, ok := vm[key].(float64)
	if !ok || v != want {
		t.Errorf("content.value[%q] = %v, want %v", key, vm[key], want)
	}
}

const instantOnePoint = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"node-1"},"value":[1700000000,"42.5"]}]}}`

// TestPoll_SeedThenPatch pins the StartWithSeed state machine: the first poll
// with a non-nil seed and seeded=false sends a FULL UpdateActivity (carrying the
// timeline template) and flips seeded→true; the next poll (seeded=true) sends a
// value-only merge-patch that omits the template. Both land on
// PATCH /activities/{slug}; the template's presence/absence in the body is the
// discriminator. If the seed branch were removed, the first body would lack the
// template (or never flip seeded), failing this test.
func TestPoll_SeedThenPatch(t *testing.T) {
	mc := newInstantMetricsClient(t, instantOnePoint)
	pw := newPWRecorder(t, http.StatusOK)
	pwClient := pushward.NewClient(pw.server.URL, "test-key")

	p := New(mc, pwClient, 1*time.Hour)
	logger := slog.Default()
	seed := &pushward.Content{
		Template:    pushward.TemplateTimeline,
		Subtitle:    "Grafana",
		AccentColor: pushward.ColorRed,
		Icon:        "exclamationmark.triangle.fill",
	}
	ctx := context.Background()

	// First tick: seeded=false → full UpdateActivity establishing the template.
	seeded := p.poll(ctx, logger, "grafana-abc", "up", "instance", seed, false)
	if !seeded {
		t.Fatal("first poll should return seeded=true after a successful seed")
	}

	calls := pw.getCalls()
	if len(calls) != 1 {
		t.Fatalf("calls after first poll = %d, want 1", len(calls))
	}
	if calls[0].method != http.MethodPatch || calls[0].path != "/activities/grafana-abc" {
		t.Errorf("first call = %s %s, want PATCH /activities/grafana-abc", calls[0].method, calls[0].path)
	}
	seedContent := decodeContent(t, calls[0].body)
	tmpl, hasTemplate := seedContent["template"]
	if !hasTemplate {
		t.Errorf("first poll body must include content.template (full seed), got %s", calls[0].body)
	}
	if tmpl != pushward.TemplateTimeline {
		t.Errorf("seed template = %v, want %q", tmpl, pushward.TemplateTimeline)
	}
	assertValueKey(t, seedContent, "node-1", 42.5)

	// Second tick: seeded=true → value-only merge-patch (template NOT re-sent).
	seeded = p.poll(ctx, logger, "grafana-abc", "up", "instance", seed, true)
	if !seeded {
		t.Fatal("second poll should keep seeded=true")
	}

	calls = pw.getCalls()
	if len(calls) != 2 {
		t.Fatalf("calls after second poll = %d, want 2", len(calls))
	}
	if calls[1].method != http.MethodPatch || calls[1].path != "/activities/grafana-abc" {
		t.Errorf("second call = %s %s, want PATCH /activities/grafana-abc", calls[1].method, calls[1].path)
	}
	patchContent := decodeContent(t, calls[1].body)
	if _, ok := patchContent["template"]; ok {
		t.Errorf("second poll body must NOT include content.template (value-only patch), got %s", calls[1].body)
	}
	assertValueKey(t, patchContent, "node-1", 42.5)
}

// TestPoll_SeedFailureKeepsSeededFalse pins that a failed seed UpdateActivity
// leaves seeded=false so the next tick retries the seed. A 400 is a
// non-retryable 4xx, so UpdateActivity fails fast (no backoff) and poll returns
// the unchanged seeded state.
func TestPoll_SeedFailureKeepsSeededFalse(t *testing.T) {
	mc := newInstantMetricsClient(t, instantOnePoint)
	pw := newPWRecorder(t, http.StatusBadRequest)
	pwClient := pushward.NewClient(pw.server.URL, "test-key")

	p := New(mc, pwClient, 1*time.Hour)
	seed := &pushward.Content{Template: pushward.TemplateTimeline, Subtitle: "Grafana"}

	seeded := p.poll(context.Background(), slog.Default(), "grafana-abc", "up", "instance", seed, false)
	if seeded {
		t.Fatal("poll must leave seeded=false when the seed UpdateActivity errors")
	}

	calls := pw.getCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1 (one seed attempt, 4xx is not retried)", len(calls))
	}
	if calls[0].method != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", calls[0].method)
	}
}
