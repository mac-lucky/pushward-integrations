package widgets

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// --- valueChanged ---

func TestValueChanged_ExactCompare(t *testing.T) {
	if valueChanged(1.0, 1.0, 0) {
		t.Error("same value should not be considered changed")
	}
	if !valueChanged(1.0, 2.0, 0) {
		t.Error("different value should be considered changed")
	}
}

func TestValueChanged_Epsilon(t *testing.T) {
	if valueChanged(1.0, 1.4, 0.5) {
		t.Error("change within epsilon should be suppressed")
	}
	if !valueChanged(1.0, 1.6, 0.5) {
		t.Error("change beyond epsilon should be reported")
	}
}

func TestValueChanged_NaNNoChange(t *testing.T) {
	if valueChanged(math.NaN(), math.NaN(), 0) {
		t.Error("NaN -> NaN should be no change")
	}
}

// --- helpers ---

type stubServer struct {
	t         *testing.T
	creates   atomic.Int64
	updates   atomic.Int64
	deletes   atomic.Int64
	mu        sync.Mutex
	gotPatch  []pushward.UpdateWidgetRequest // captured PATCH bodies
	gotCreate []pushward.CreateWidgetRequest
	gotDelete []string
}

func newStubServer(t *testing.T) (*stubServer, *pushward.Client, func()) {
	t.Helper()
	s := &stubServer{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/widgets":
			s.creates.Add(1)
			var req pushward.CreateWidgetRequest
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)
			s.mu.Lock()
			s.gotCreate = append(s.gotCreate, req)
			s.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/widgets/"):
			s.updates.Add(1)
			if ct := r.Header.Get("Content-Type"); ct != "application/merge-patch+json" {
				s.t.Errorf("PATCH Content-Type = %q, want application/merge-patch+json", ct)
			}
			var req pushward.UpdateWidgetRequest
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &req)
			s.mu.Lock()
			s.gotPatch = append(s.gotPatch, req)
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/widgets/"):
			s.deletes.Add(1)
			slug := strings.TrimPrefix(r.URL.Path, "/widgets/")
			s.mu.Lock()
			s.gotDelete = append(s.gotDelete, slug)
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	client := pushward.NewClient(srv.URL, "hlk_test")
	return s, client, srv.Close
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- lifecycle ---

func TestManager_LifecycleNoWidgets(t *testing.T) {
	_, client, closeSrv := newStubServer(t)
	defer closeSrv()
	m, err := New(client, nil, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	m.Wait()
}

func TestManager_ScalarCreateAndUpdate(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	var i atomic.Int64
	src := ValueSourceFunc(func(_ context.Context) (float64, error) {
		return float64(i.Add(1)), nil
	})

	m, err := New(client, []Spec{{
		Slug:     "test-widget",
		Name:     "Test",
		Source:   src,
		Interval: 20 * time.Millisecond,
	}}, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Initial poll runs synchronously in Start -> exactly 1 create with value 1.
	if got := stub.creates.Load(); got != 1 {
		t.Fatalf("creates = %d, want 1", got)
	}
	stub.mu.Lock()
	if v := stub.gotCreate[0].Content.Value; v == nil || *v != 1 {
		t.Errorf("initial create value = %v, want 1", v)
	}
	stub.mu.Unlock()

	// Wait for the source to be called several times.
	waitFor(t, 500*time.Millisecond, func() bool { return i.Load() >= 4 })
	cancel()
	m.Wait()

	if updates := stub.updates.Load(); updates < 1 {
		t.Errorf("expected at least 1 update PATCH, got %d", updates)
	}
}

// Both per-widget tuning knobs ride the create body. Neither is re-sent on a
// PATCH, so a create that drops one leaves the widget untuned for its lifetime.
func TestManager_CreateCarriesThrottleAndStaleAfter(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := ValueSourceFunc(func(_ context.Context) (float64, error) { return 3.0, nil })
	m, err := New(client, []Spec{{
		Slug:         "tuned",
		Name:         "Tuned",
		Source:       src,
		Interval:     time.Hour, // no tick fires; only the synchronous create runs
		PushThrottle: pushward.IntPtr(30),
		StaleAfter:   pushward.IntPtr(300),
	}}, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	m.Wait()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.gotCreate) != 1 {
		t.Fatalf("creates = %d, want 1", len(stub.gotCreate))
	}
	got := stub.gotCreate[0]
	if got.PushThrottle == nil || *got.PushThrottle != 30 {
		t.Errorf("push_throttle = %v, want 30", got.PushThrottle)
	}
	if got.StaleAfter == nil || *got.StaleAfter != 300 {
		t.Errorf("stale_after = %v, want 300", got.StaleAfter)
	}
}

func TestManager_ScalarChangeSuppression(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := ValueSourceFunc(func(_ context.Context) (float64, error) { return 42.0, nil })

	m, err := New(client, []Spec{{
		Slug:     "static",
		Name:     "Static",
		Source:   src,
		Interval: 15 * time.Millisecond,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = m.Start(ctx)

	time.Sleep(120 * time.Millisecond)
	cancel()
	m.Wait()

	if stub.updates.Load() != 0 {
		t.Errorf("expected 0 PATCHes for static value, got %d", stub.updates.Load())
	}
	if stub.creates.Load() != 1 {
		t.Errorf("expected 1 create, got %d", stub.creates.Load())
	}
}

func TestManager_ScalarAlwaysMode(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := ValueSourceFunc(func(_ context.Context) (float64, error) { return 7.0, nil })

	m, err := New(client, []Spec{{
		Slug:       "always",
		Name:       "Always",
		Source:     src,
		Interval:   15 * time.Millisecond,
		UpdateMode: UpdateAlways,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = m.Start(ctx)

	waitFor(t, 500*time.Millisecond, func() bool { return stub.updates.Load() >= 2 })
	cancel()
	m.Wait()
}

func TestManager_ScalarSkipsNaN(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := ValueSourceFunc(func(_ context.Context) (float64, error) {
		return math.NaN(), nil
	})

	m, err := New(client, []Spec{{
		Slug:     "bad",
		Name:     "Bad",
		Source:   src,
		Interval: 15 * time.Millisecond,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = m.Start(ctx)

	time.Sleep(100 * time.Millisecond)
	cancel()
	m.Wait()

	// value-template scalar widget tolerates a nil initial Value, so the
	// create still happens (without Value) and no PATCHes are issued.
	if stub.updates.Load() != 0 {
		t.Errorf("PATCH happened for NaN, count=%d", stub.updates.Load())
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.gotCreate) != 1 {
		t.Fatalf("creates=%d", len(stub.gotCreate))
	}
	if stub.gotCreate[0].Content.Value != nil {
		t.Errorf("initial value should be nil for NaN, got %v", *stub.gotCreate[0].Content.Value)
	}
}

func TestManager_GaugeDefersCreateUntilFirstValue(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	minV, maxV := 0.0, 100.0
	var i atomic.Int64
	src := ValueSourceFunc(func(_ context.Context) (float64, error) {
		// First call returns ErrNoData (e.g. metric missing); subsequent
		// calls return a real value so the deferred create can fire.
		if i.Add(1) == 1 {
			return 0, ErrNoData
		}
		return 42.0, nil
	})

	m, err := New(client, []Spec{{
		Slug:     "gauge",
		Name:     "Gauge",
		Template: pushward.WidgetTemplateGauge,
		Source:   src,
		Interval: 15 * time.Millisecond,
		Content:  pushward.WidgetContent{MinValue: &minV, MaxValue: &maxV},
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// No create yet - initial poll returned ErrNoData.
	if got := stub.creates.Load(); got != 0 {
		t.Fatalf("creates after Start = %d, want 0 (deferred)", got)
	}

	waitFor(t, 500*time.Millisecond, func() bool { return stub.creates.Load() >= 1 })
	cancel()
	m.Wait()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.gotCreate) != 1 {
		t.Fatalf("creates=%d, want 1", len(stub.gotCreate))
	}
	if v := stub.gotCreate[0].Content.Value; v == nil || *v != 42.0 {
		t.Errorf("deferred create value = %v, want 42", v)
	}
	if got := stub.gotCreate[0].Content.Template; got != pushward.WidgetTemplateGauge {
		t.Errorf("template = %q, want gauge", got)
	}
}

func TestManager_ScalarSkipsErrNoData(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := ValueSourceFunc(func(_ context.Context) (float64, error) {
		return 0, ErrNoData
	})

	m, err := New(client, []Spec{{
		Slug:     "nodata",
		Name:     "NoData",
		Source:   src,
		Interval: 15 * time.Millisecond,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = m.Start(ctx)

	time.Sleep(80 * time.Millisecond)
	cancel()
	m.Wait()

	if stub.updates.Load() != 0 {
		t.Errorf("expected 0 PATCH for ErrNoData, got %d", stub.updates.Load())
	}
}

func TestManager_MultiFanOut(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	calls := atomic.Int64{}
	src := MultiValueSourceFunc(func(_ context.Context) ([]LabeledValue, error) {
		n := calls.Add(1)
		// First call: 3 series. Second and beyond: same 3 series, same values.
		_ = n
		return []LabeledValue{
			{Labels: map[string]string{"instance": "a"}, Value: 1},
			{Labels: map[string]string{"instance": "b"}, Value: 2},
			{Labels: map[string]string{"instance": "c"}, Value: 3},
		}, nil
	})

	m, err := New(client, []Spec{{
		Slug:         "group",
		MultiSource:  src,
		SlugTemplate: "g-{{.instance}}",
		Interval:     30 * time.Millisecond,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = m.Start(ctx)

	waitFor(t, 500*time.Millisecond, func() bool { return stub.creates.Load() >= 3 })
	time.Sleep(120 * time.Millisecond) // give the supervisor a few extra ticks
	cancel()
	m.Wait()

	if stub.creates.Load() != 3 {
		t.Errorf("expected 3 creates, got %d", stub.creates.Load())
	}
	// Values unchanged across ticks -> no PATCH.
	if stub.updates.Load() != 0 {
		t.Errorf("expected 0 PATCH for unchanged multi values, got %d", stub.updates.Load())
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	seenSlugs := map[string]bool{}
	for _, c := range stub.gotCreate {
		seenSlugs[c.Slug] = true
	}
	for _, want := range []string{"g-a", "g-b", "g-c"} {
		if !seenSlugs[want] {
			t.Errorf("missing create for slug %q (got %v)", want, seenSlugs)
		}
	}
}

func TestManager_MultiCardinalityCap(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := MultiValueSourceFunc(func(_ context.Context) ([]LabeledValue, error) {
		out := make([]LabeledValue, 10)
		for i := range out {
			out[i] = LabeledValue{
				Labels: map[string]string{"instance": string(rune('a' + i))},
				Value:  float64(i),
			}
		}
		return out, nil
	})

	m, err := New(client, []Spec{{
		Slug:         "capped",
		MultiSource:  src,
		SlugTemplate: "c-{{.instance}}",
		Interval:     30 * time.Millisecond,
		MaxSeries:    3,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = m.Start(ctx)

	waitFor(t, 300*time.Millisecond, func() bool { return stub.creates.Load() >= 3 })
	cancel()
	m.Wait()

	if got := stub.creates.Load(); got != 3 {
		t.Errorf("creates = %d, want 3 (cap)", got)
	}
}

func TestManager_MultiCleanupMissing(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	calls := atomic.Int64{}
	src := MultiValueSourceFunc(func(_ context.Context) ([]LabeledValue, error) {
		if calls.Add(1) == 1 {
			return []LabeledValue{
				{Labels: map[string]string{"id": "x"}, Value: 1},
				{Labels: map[string]string{"id": "y"}, Value: 2},
			}, nil
		}
		return []LabeledValue{
			{Labels: map[string]string{"id": "x"}, Value: 1},
		}, nil
	})

	m, err := New(client, []Spec{{
		Slug:           "cleanup",
		MultiSource:    src,
		SlugTemplate:   "k-{{.id}}",
		Interval:       30 * time.Millisecond,
		CleanupMissing: true,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = m.Start(ctx)

	waitFor(t, 500*time.Millisecond, func() bool { return stub.deletes.Load() >= 1 })
	cancel()
	m.Wait()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.gotDelete) == 0 || stub.gotDelete[0] != "k-y" {
		t.Errorf("expected DELETE of k-y, got %v", stub.gotDelete)
	}
}

// TestManager_MultiCleanupGraceAbsorbsSingleMiss pins the MissGrace debounce:
// a series that disappears for exactly one tick and then returns must NOT be
// pruned or DELETEd. MissGrace=2 tolerates one missing tick (missCount reaches
// 1 < 2, then resets on recovery). Unlike TestManager_MultiCleanupMissing -
// where the series stays gone forever and so is eventually deleted regardless -
// this exercises the grace window itself: without it, the first miss DELETEs.
func TestManager_MultiCleanupGraceAbsorbsSingleMiss(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	var calls atomic.Int64
	src := MultiValueSourceFunc(func(_ context.Context) ([]LabeledValue, error) {
		// Call 1 is the synchronous initial poll (creates k-x, k-y). Call 2 (the
		// first tick) drops "y" for a single tick; call 3+ restore it.
		if calls.Add(1) == 2 {
			return []LabeledValue{
				{Labels: map[string]string{"id": "x"}, Value: 1},
			}, nil
		}
		return []LabeledValue{
			{Labels: map[string]string{"id": "x"}, Value: 1},
			{Labels: map[string]string{"id": "y"}, Value: 2},
		}, nil
	})

	m, err := New(client, []Spec{{
		Slug:           "cleanup",
		MultiSource:    src,
		SlugTemplate:   "k-{{.id}}",
		Interval:       20 * time.Millisecond,
		CleanupMissing: true,
		MissGrace:      2, // tolerate exactly one missing tick
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = m.Start(ctx)

	// Drive enough ticks that the miss (call 2) and several recoveries elapse.
	waitFor(t, 1*time.Second, func() bool { return calls.Load() >= 5 })
	cancel()
	m.Wait()

	if got := stub.deletes.Load(); got != 0 {
		t.Errorf("expected 0 DELETE for a single-tick gap within MissGrace, got %d", got)
	}
}

// --- validation ---

func TestNew_RejectsMissingSource(t *testing.T) {
	_, client, closeSrv := newStubServer(t)
	defer closeSrv()
	_, err := New(client, []Spec{{Slug: "x"}}, quietLogger())
	if err == nil {
		t.Fatal("expected error for spec with no source")
	}
}

func TestNew_RejectsBothSources(t *testing.T) {
	_, client, closeSrv := newStubServer(t)
	defer closeSrv()
	src := ValueSourceFunc(func(_ context.Context) (float64, error) { return 0, nil })
	multi := MultiValueSourceFunc(func(_ context.Context) ([]LabeledValue, error) { return nil, nil })
	_, err := New(client, []Spec{{Slug: "x", Source: src, MultiSource: multi}}, quietLogger())
	if err == nil {
		t.Fatal("expected error when both Source and MultiSource set")
	}
}

func TestNew_RejectsBadSlugTemplate(t *testing.T) {
	_, client, closeSrv := newStubServer(t)
	defer closeSrv()
	multi := MultiValueSourceFunc(func(_ context.Context) ([]LabeledValue, error) { return nil, nil })
	_, err := New(client, []Spec{{Slug: "x", MultiSource: multi, SlugTemplate: "no-template-vars"}}, quietLogger())
	if err == nil {
		t.Fatal("expected error for slug_template missing label reference")
	}
}

func TestNew_RejectsBadLabelTemplate(t *testing.T) {
	_, client, closeSrv := newStubServer(t)
	defer closeSrv()
	src := ValueSourceFunc(func(_ context.Context) (float64, error) { return 0, nil })
	_, err := New(client, []Spec{{Slug: "x", Source: src, LabelTemplate: "{{.Value"}}, quietLogger())
	if err == nil {
		t.Fatal("expected error for unparseable label template")
	}
}

// TestNew_AcceptsAllFalseStatChangeMask pins the removal of the old
// construction-time rejection of all-false masks. An all-false mask is a valid
// "create once, never PATCH" static widget (statRowsEqualMasked treats rows past
// the mask as triggers, and the frozen case can't be proven at New() time since
// row count is unknown). New() must accept it even under UpdateOnChange.
func TestNew_AcceptsAllFalseStatChangeMask(t *testing.T) {
	_, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := StatListSourceFunc(func(_ context.Context) ([]pushward.StatRow, error) {
		return []pushward.StatRow{{Label: "x", Value: "1"}}, nil
	})
	_, err := New(client, []Spec{{
		Slug:           "static",
		Name:           "Static",
		StatListSource: src,
		UpdateMode:     UpdateOnChange,
		StatChangeMask: []bool{false, false},
	}}, quietLogger())
	if err != nil {
		t.Fatalf("all-false StatChangeMask must be accepted (create-once static widget), got %v", err)
	}
}

func TestStart_FailsFastOnWidgetLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"widget.limit_exceeded","title":"limit","status":409}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client := pushward.NewClient(srv.URL, "hlk_x")

	src := ValueSourceFunc(func(_ context.Context) (float64, error) { return 1, nil })
	m, err := New(client, []Spec{{Slug: "x", Source: src, Interval: time.Hour}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	err = m.Start(context.Background())
	if err == nil {
		t.Fatal("expected widget limit error")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error should mention cap, got %v", err)
	}
}

// A flat metric under on_change would otherwise never PATCH again, so a widget
// carrying stale_after ages out on the device. The heartbeat re-sends the same
// content; the server turns that into a touch.
func TestManager_HeartbeatResendsUnchangedValue(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := ValueSourceFunc(func(_ context.Context) (float64, error) { return 42.0, nil })
	m, err := New(client, []Spec{{
		Slug:       "flat",
		Name:       "Flat",
		Source:     src,
		Interval:   15 * time.Millisecond,
		StaleAfter: pushward.IntPtr(120),
		Heartbeat:  40 * time.Millisecond,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = m.Start(ctx)

	waitFor(t, time.Second, func() bool { return stub.updates.Load() >= 2 })
	cancel()
	m.Wait()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if got := stub.gotPatch[0].Content; got == nil || got.Value == nil || *got.Value != 42 {
		t.Errorf("heartbeat PATCH should carry the unchanged value, got %+v", got)
	}
}

func TestManager_NoHeartbeatWhenUnset(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := ValueSourceFunc(func(_ context.Context) (float64, error) { return 42.0, nil })
	m, err := New(client, []Spec{{
		Slug:     "flat",
		Name:     "Flat",
		Source:   src,
		Interval: 10 * time.Millisecond,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = m.Start(ctx)
	time.Sleep(120 * time.Millisecond)
	cancel()
	m.Wait()

	if got := stub.updates.Load(); got != 0 {
		t.Errorf("PATCHes without a heartbeat = %d, want 0", got)
	}
}

// pointSourceStub is a ValueSource that also keeps a sparkline buffer, the
// shape a trend widget needs.
type pointSourceStub struct{ pts []float64 }

func (p *pointSourceStub) Value(_ context.Context) (float64, error) {
	return p.pts[len(p.pts)-1], nil
}
func (p *pointSourceStub) Points() []float64 { return p.pts }

// growingPointSource models the trap: a buffer that grows on every poll even
// though the reading never moves.
type growingPointSource struct{ pts []float64 }

func (p *growingPointSource) Value(_ context.Context) (float64, error) {
	p.pts = append(p.pts, 7)
	return 7, nil
}
func (p *growingPointSource) Points() []float64 { return slices.Clone(p.pts) }

// The heartbeat exists to re-stamp updated_at, and the server only skips the
// APNs push when the merged content equals what it stored. A PointSource whose
// buffer moves every poll would break that on every single beat - a real push
// and a quota slot each time - unless the heartbeat re-sends stored content.
func TestManager_HeartbeatPayloadMatchesStoredContent(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := &growingPointSource{pts: []float64{1, 2}} // older, differing samples
	m, err := New(client, []Spec{{
		Slug:       "trend",
		Name:       "Trend",
		Template:   pushward.WidgetTemplateTrend,
		Source:     src,
		Interval:   15 * time.Millisecond,
		StaleAfter: pushward.IntPtr(120),
		Heartbeat:  20 * time.Millisecond,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, time.Second, func() bool { return stub.updates.Load() >= 2 })
	cancel()
	m.Wait()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	stored := stub.gotCreate[0].Content
	for i, patch := range stub.gotPatch {
		if patch.Content == nil {
			t.Fatalf("patch %d carried no content", i)
		}
		// Points is the field that matters most: merge-patch replaces an array
		// wholesale, so a grown buffer can never compare equal server-side.
		if !slices.Equal(patch.Content.Points, stored.Points) {
			t.Errorf("heartbeat %d points = %v, want the stored %v; the server would push instead of touching",
				i, patch.Content.Points, stored.Points)
		}
		// And the whole payload, judged the way the server judges it: apply the
		// patch to the stored content and check nothing moved. Modelling the
		// merge matters because absent keys are preserved, so a struct-to-struct
		// comparison would flag fields the server never even looks at.
		body, err := json.Marshal(patch.Content)
		if err != nil {
			t.Fatalf("marshal patch %d: %v", i, err)
		}
		merged := stored
		if err := json.Unmarshal(body, &merged); err != nil {
			t.Fatalf("merge patch %d: %v", i, err)
		}
		if !reflect.DeepEqual(merged, stored) {
			t.Errorf("heartbeat %d changes stored content:\n merged %+v\n stored %+v", i, merged, stored)
		}
	}
}

// A real move must still publish the current buffer, heartbeat or not.
func TestManager_ChangedValueSendsFreshPoints(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	var n atomic.Int64
	src := &countingPointSource{next: func() float64 { return float64(n.Add(1)) }}
	src.pts = []float64{0}
	m, err := New(client, []Spec{{
		Slug: "trend", Name: "Trend", Template: pushward.WidgetTemplateTrend,
		Source: src, Interval: 15 * time.Millisecond,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, time.Second, func() bool { return stub.updates.Load() >= 1 })
	cancel()
	m.Wait()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	got := stub.gotPatch[0].Content
	if got == nil || len(got.Points) < 2 {
		t.Fatalf("changed value should publish the live buffer, got %+v", got)
	}
}

type countingPointSource struct {
	pts  []float64
	next func() float64
}

func (p *countingPointSource) Value(_ context.Context) (float64, error) {
	v := p.next()
	p.pts = append(p.pts, v)
	return v, nil
}
func (p *countingPointSource) Points() []float64 { return slices.Clone(p.pts) }

// A trend spec whose points can never be filled must fail at construction, not
// with a 422 on the first create that takes the whole manager down.
func TestNew_RejectsTrendWithoutPointSource(t *testing.T) {
	_, client, closeSrv := newStubServer(t)
	defer closeSrv()

	multi := MultiValueSourceFunc(func(_ context.Context) ([]LabeledValue, error) { return nil, nil })
	_, err := New(client, []Spec{{
		Slug: "t", Template: pushward.WidgetTemplateTrend,
		MultiSource: multi, SlugTemplate: "t-{{.instance}}",
	}}, quietLogger())
	if err == nil {
		t.Fatal("expected trend + MultiSource to be rejected")
	}

	plain := ValueSourceFunc(func(_ context.Context) (float64, error) { return 1, nil })
	if _, err := New(client, []Spec{{
		Slug: "t", Template: pushward.WidgetTemplateTrend, Source: plain,
	}}, quietLogger()); err == nil {
		t.Fatal("expected trend + plain ValueSource with no seeded points to be rejected")
	}

	// Pre-seeded static points are the documented escape hatch.
	if _, err := New(client, []Spec{{
		Slug: "t", Template: pushward.WidgetTemplateTrend, Source: plain,
		Content: pushward.WidgetContent{Points: []float64{1, 2}},
	}}, quietLogger()); err != nil {
		t.Errorf("seeded Content.Points should be accepted: %v", err)
	}
}

func TestHeartbeatFor(t *testing.T) {
	if got := HeartbeatFor(nil); got != 0 {
		t.Errorf("HeartbeatFor(nil) = %v, want 0", got)
	}
	if got := HeartbeatFor(pushward.IntPtr(60)); got != 30*time.Second {
		t.Errorf("HeartbeatFor(60) = %v, want the 30s floor", got)
	}
	if got := HeartbeatFor(pushward.IntPtr(3600)); got != 30*time.Minute {
		t.Errorf("HeartbeatFor(3600) = %v, want half the window", got)
	}
}

func TestManager_PointSourceFeedsTrendContent(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := &pointSourceStub{pts: []float64{1, 2, 3}}
	m, err := New(client, []Spec{{
		Slug:     "trend",
		Name:     "Trend",
		Template: pushward.WidgetTemplateTrend,
		Source:   src,
		Interval: time.Hour,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	m.Wait()

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.gotCreate) != 1 {
		t.Fatalf("creates = %d, want 1", len(stub.gotCreate))
	}
	if got := stub.gotCreate[0].Content.Points; len(got) != 3 || got[2] != 3 {
		t.Errorf("create points = %v, want [1 2 3]", got)
	}
}

// A source that has not buffered enough samples returns ErrNoData, which must
// keep the trend widget's creation deferred rather than posting 1 point.
func TestManager_TrendCreateDeferredWithoutPoints(t *testing.T) {
	stub, client, closeSrv := newStubServer(t)
	defer closeSrv()

	src := &emptyPointSource{}
	m, err := New(client, []Spec{{
		Slug:     "trend",
		Name:     "Trend",
		Template: pushward.WidgetTemplateTrend,
		Source:   src,
		Interval: time.Hour,
	}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	m.Wait()

	if got := stub.creates.Load(); got != 0 {
		t.Errorf("creates = %d, want 0 while the source has no points yet", got)
	}
}

// emptyPointSource is a PointSource still filling its buffer: it withholds a
// value until it has enough samples to draw a line.
type emptyPointSource struct{}

func (emptyPointSource) Value(_ context.Context) (float64, error) { return 0, ErrNoData }
func (emptyPointSource) Points() []float64                        { return nil }

// --- utilities ---

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}
