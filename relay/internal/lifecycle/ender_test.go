package lifecycle

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

func init() {
	enderRetryDelay = 10 * time.Millisecond
}

func TestUpdateWithRetry_SucceedsFirstAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cl := pushward.NewClient(srv.URL, "test-key")
	req := pushward.UpdateRequest{State: pushward.StateOngoing}

	err := updateWithRetry(cl, "slug-1", req, 5*time.Second)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", got)
	}
}

func TestUpdateWithRetry_RetriesOnFailure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest) // 400 = non-retryable inside doWithRetry
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cl := pushward.NewClient(srv.URL, "test-key")
	req := pushward.UpdateRequest{State: pushward.StateEnded}

	err := updateWithRetry(cl, "slug-2", req, 5*time.Second)
	if err != nil {
		t.Fatalf("expected success on retry, got %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 HTTP calls, got %d", got)
	}
}

func TestUpdateWithRetry_FailsBothAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	cl := pushward.NewClient(srv.URL, "test-key")
	req := pushward.UpdateRequest{State: pushward.StateOngoing}

	err := updateWithRetry(cl, "slug-3", req, 5*time.Second)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 HTTP calls, got %d", got)
	}
}
