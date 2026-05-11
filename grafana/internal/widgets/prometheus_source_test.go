package widgets

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mac-lucky/pushward-integrations/grafana/internal/metrics"
	sharedwidgets "github.com/mac-lucky/pushward-integrations/shared/widgets"
)

const instantPayload = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"users","instance":"node-a"},"value":[1700000000,"42"]}]}}`

const instantEmptyPayload = `{"status":"success","data":{"resultType":"vector","result":[]}}`

func newPromStub(body string) (*metrics.Client, func()) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	c := metrics.NewClient(srv.URL)
	return c, srv.Close
}

func TestScalarSource_Value(t *testing.T) {
	mc, close := newPromStub(instantPayload)
	defer close()
	src := &ScalarSource{Client: mc, Expr: "users"}
	v, err := src.Value(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != 42 {
		t.Errorf("value = %v, want 42", v)
	}
}

func TestScalarSource_NoData(t *testing.T) {
	mc, close := newPromStub(instantEmptyPayload)
	defer close()
	src := &ScalarSource{Client: mc, Expr: "users"}
	_, err := src.Value(context.Background())
	if !errors.Is(err, sharedwidgets.ErrNoData) {
		t.Errorf("expected ErrNoData, got %v", err)
	}
}

func TestMultiSource_Values(t *testing.T) {
	mc, close := newPromStub(instantPayload)
	defer close()
	src := &MultiSource{Client: mc, Expr: "users"}
	vals, err := src.Values(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 1 {
		t.Fatalf("len(vals) = %d, want 1", len(vals))
	}
	if vals[0].Value != 42 {
		t.Errorf("value = %v, want 42", vals[0].Value)
	}
	if vals[0].Labels["instance"] != "node-a" {
		t.Errorf("instance label = %q, want node-a", vals[0].Labels["instance"])
	}
}
