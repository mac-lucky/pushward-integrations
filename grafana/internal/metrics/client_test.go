package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQueryRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("query") != "up" {
			t.Errorf("query = %q, want %q", q.Get("query"), "up")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"status": "success",
			"data": {
				"resultType": "matrix",
				"result": [{
					"metric": {"__name__": "up"},
					"values": [
						[1700000000, "1"],
						[1700000015, "0.5"],
						[1700000030, "1"]
					]
				}]
			}
		}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	points, err := c.QueryRange(context.Background(), "up", time.Unix(1700000000, 0), time.Unix(1700000030, 0), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 {
		t.Fatalf("got %d points, want 3", len(points))
	}
	if points[0].T != 1700000000 || points[0].V != 1.0 {
		t.Errorf("points[0] = %+v, want {T:1700000000 V:1}", points[0])
	}
	if points[1].V != 0.5 {
		t.Errorf("points[1].V = %v, want 0.5", points[1].V)
	}
}

func TestQueryRange_SkipsNaN(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"status": "success",
			"data": {
				"resultType": "matrix",
				"result": [{
					"metric": {},
					"values": [
						[1700000000, "1"],
						[1700000015, "NaN"],
						[1700000030, "+Inf"],
						[1700000045, "2"]
					]
				}]
			}
		}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	points, err := c.QueryRange(context.Background(), "up", time.Unix(1700000000, 0), time.Unix(1700000045, 0), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 {
		t.Fatalf("got %d points, want 2 (NaN and Inf skipped)", len(points))
	}
}

func TestQueryRange_EmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	points, err := c.QueryRange(context.Background(), "nonexistent", time.Unix(1700000000, 0), time.Unix(1700000030, 0), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if points != nil {
		t.Errorf("expected nil for empty result, got %v", points)
	}
}

func TestQueryRange_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.QueryRange(context.Background(), "bad{", time.Unix(1700000000, 0), time.Unix(1700000030, 0), 15*time.Second)
	if err == nil {
		t.Fatal("expected error for error response")
	}
}

func TestQueryRange_BasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "admin" || pass != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1700000000,"1"]]}]}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithBasicAuth("admin", "secret"))
	points, err := c.QueryRange(context.Background(), "up", time.Unix(1700000000, 0), time.Unix(1700000000, 0), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 {
		t.Fatalf("got %d points, want 1", len(points))
	}
}

func TestQueryInstant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("query") != "up" {
			t.Errorf("query = %q, want %q", q.Get("query"), "up")
		}
		if q.Get("time") == "" {
			t.Error("time parameter missing")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"status": "success",
			"data": {
				"resultType": "vector",
				"result": [{
					"metric": {"__name__": "up"},
					"value": [1700000000, "0.75"]
				}]
			}
		}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	point, err := c.QueryInstant(context.Background(), "up", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if point == nil {
		t.Fatal("expected non-nil point")
	}
	if point.T != 1700000000 || point.V != 0.75 {
		t.Errorf("point = %+v, want {T:1700000000 V:0.75}", point)
	}
}

func TestQueryInstant_EmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	point, err := c.QueryInstant(context.Background(), "nonexistent", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if point != nil {
		t.Errorf("expected nil for empty result, got %+v", point)
	}
}

func TestQueryInstant_NaN(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"NaN"]}]}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	point, err := c.QueryInstant(context.Background(), "up", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if point != nil {
		t.Errorf("expected nil for NaN, got %+v", point)
	}
}
