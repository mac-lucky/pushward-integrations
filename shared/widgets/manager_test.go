package widgets

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
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

	// Initial poll runs synchronously in Start → exactly 1 create with value 1.
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

	// CreateWidget still happens (with no value), but no PATCHes for NaN.
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
	// Values unchanged across ticks → no PATCH.
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

