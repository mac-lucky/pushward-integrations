package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/grafana/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

func TestStartStop(t *testing.T) {
	// Verify start/stop lifecycle without real HTTP calls.
	// metricsClient and pwClient are nil - the goroutine will tick but
	// poll() will panic if called, so we stop before the first tick.
	p := New(nil, nil, 1*time.Hour) // long interval so it won't tick
	p.Start("test-slug", "up", "TestAlert", nil)

	if p.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d, want 1", p.ActiveCount())
	}

	// Start again - should be a no-op
	p.Start("test-slug", "up", "TestAlert", nil)
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
	p.Start("slug-1", "up", "Alert1", nil)
	p.Start("slug-2", "up", "Alert2", nil)

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
// uncreated slug) is unsuitable here - we only need to observe the raw calls.
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
// timeline template) and flips seeded->true; the next poll (seeded=true) sends a
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

	// First tick: seeded=false -> full UpdateActivity establishing the template.
	seeded := p.poll(ctx, logger, "grafana-abc", "up", "instance", seed, false, &seriesKeys{})
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

	// Second tick: seeded=true -> value-only merge-patch (template NOT re-sent).
	seeded = p.poll(ctx, logger, "grafana-abc", "up", "instance", seed, true, &seriesKeys{})
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

	seeded := p.poll(context.Background(), slog.Default(), "grafana-abc", "up", "instance", seed, false, &seriesKeys{})
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

// A series missing from a tick keeps its place (and history) on the card for
// missGrace successful ticks, then goes out as a null so merge-patch deletes it.
func TestSeriesKeys_NullsASeriesOnlyAfterTheGrace(t *testing.T) {
	keys := seriesKeys{}
	_, patch, keys := keys.next(map[string]float64{"a": 1, "b": 2})
	if len(patch) != 2 {
		t.Fatalf("first patch = %v, want both series", patch)
	}
	for tick := 1; tick < missGrace; tick++ {
		_, patch, keys = keys.next(map[string]float64{"a": 1})
		if _, ok := patch["b"]; ok {
			t.Fatalf("tick %d: b sent as %v before the grace ran out", tick, patch["b"])
		}
	}
	_, patch, keys = keys.next(map[string]float64{"a": 1})
	if v, ok := patch["b"]; !ok || v != nil {
		t.Fatalf("after %d misses patch[b] = %v (present %v), want null", missGrace, v, ok)
	}
	if _, ok := keys["b"]; ok {
		t.Fatal("b still tracked after its null")
	}
	_, patch, _ = keys.next(map[string]float64{"a": 1, "b": 3})
	if patch["b"] != 3.0 {
		t.Fatalf("a returning series = %v, want re-added", patch["b"])
	}
}

// A churning query never leaves more than maxSeries keys on the card: a new
// key waits for a slot, and the slots go to keys in sorted order.
func TestSeriesKeys_CapsTheCardAtMaxSeries(t *testing.T) {
	values := map[string]float64{}
	for i := range maxSeries + 2 {
		values[fmt.Sprintf("s%02d", i)] = float64(i)
	}
	sent, _, keys := seriesKeys{}.next(values)
	if len(sent) != maxSeries || len(keys) != maxSeries {
		t.Fatalf("sent %d series, tracking %d, want %d", len(sent), len(keys), maxSeries)
	}
	if _, ok := sent[fmt.Sprintf("s%02d", maxSeries)]; ok {
		t.Fatal("slots should go to the lowest keys first")
	}

	// The whole set churns: old keys hold their slots through the grace.
	churned := map[string]float64{"z1": 1, "z2": 2}
	for range missGrace - 1 {
		sent, _, keys = keys.next(churned)
		if len(sent) != 0 || len(keys) > maxSeries {
			t.Fatalf("during the grace sent %v with %d keys tracked", sent, len(keys))
		}
	}
	sent, _, keys = keys.next(churned)
	if len(sent) != 2 || len(keys) != 2 {
		t.Fatalf("after the grace sent %v, tracking %d keys, want the two new ones", sent, len(keys))
	}
}

// A failed send moves the miss counters without adding or dropping a key, so
// a series past the grace goes out as a null on the next tick.
func TestSeriesKeys_MissedMovesTheCountersOnly(t *testing.T) {
	keys := seriesKeys{"a": 0, "b": missGrace - 1}.missed(map[string]float64{"a": 1, "c": 3})
	if len(keys) != 2 || keys["a"] != 0 || keys["b"] != missGrace {
		t.Fatalf("after a failed send keys = %v, want a at 0 and b at %d, no c", keys, missGrace)
	}
	if _, patch, _ := keys.next(map[string]float64{"a": 1}); patch["b"] != nil {
		t.Fatalf("next patch = %v, want b nulled", patch)
	} else if _, ok := patch["b"]; !ok {
		t.Fatalf("next patch = %v, want b sent as null", patch)
	}
}

// mergeCard is a PushWard mock that applies each PATCH's content.value as a
// merge patch to one card and refuses, like the server, a result with more
// than maxSeries keys.
type mergeCard struct {
	mu      sync.Mutex
	keys    map[string]bool
	refused int
}

func (c *mergeCard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Content struct {
			Value map[string]*float64 `json:"value"`
		} `json:"content"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]bool, len(c.keys))
	for k := range c.keys {
		next[k] = true
	}
	for k, v := range req.Content.Value {
		if v == nil {
			delete(next, k)
		} else {
			next[k] = true
		}
	}
	if len(next) > maxSeries {
		c.refused++
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	c.keys = next
	w.WriteHeader(http.StatusOK)
}

func (c *mergeCard) snapshot() ([]string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.keys))
	for k := range c.keys {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys, c.refused
}

// The firing update left nine series on the card, three of them only in the
// history backfill. A rollout then swaps two pods. The poller must count the
// keys the firing update set, so the card never goes over the server's cap,
// and retire the dead ones through the grace.
func TestPoller_CountsTheKeysTheFiringUpdateSet(t *testing.T) {
	onCard := map[string]float64{}
	card := &mergeCard{keys: map[string]bool{}}
	for i := range 9 {
		k := fmt.Sprintf("p%d", i)
		onCard[k] = 1
		card.keys[k] = true
	}
	live := []string{"p2", "p3", "p4", "p5", "q0", "q1"}
	var result []string
	for _, k := range live {
		result = append(result, fmt.Sprintf(`{"metric":{"pod":%q},"value":[1700000000,"1"]}`, k))
	}
	mc := newInstantMetricsClient(t, `{"status":"success","data":{"resultType":"vector","result":[`+strings.Join(result, ",")+`]}}`)
	srv := httptest.NewServer(card)
	t.Cleanup(srv.Close)

	p := New(mc, pushward.NewClient(srv.URL, "test-key"), 5*time.Millisecond)
	p.Start("grafana-abc", "up", "pod", onCard)
	deadline := time.Now().Add(5 * time.Second)
	for {
		keys, _ := card.snapshot()
		if slices.Equal(keys, live) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	p.StopAll()
	p.Wait()

	keys, refused := card.snapshot()
	if refused != 0 {
		t.Errorf("the server refused %d patches over the series cap", refused)
	}
	if !slices.Equal(keys, live) {
		t.Errorf("card keys = %v, want the live series %v", keys, live)
	}
}
