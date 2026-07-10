package client

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Pooled clients must carry a retry budget. The relay calls PushWard from a
// webhook handler, so a server answering 429 with a large Retry-After would
// otherwise park the handler goroutine (and hold its inbound connection)
// through every remaining attempt.
func TestPool_ClientsCarryRetryBudget(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusTooManyRequests)
		// 60s: below maxRetryAfter, so only the budget can stop the sleep.
		_, _ = w.Write([]byte(`{"code":"rate_limit.exceeded","retry_after_ms":60000}`))
	}))
	defer srv.Close()

	p := NewPool(srv.URL, nil)

	start := time.Now()
	err := p.SendNotification(context.Background(), "hlk_test", discardLogger(),
		pushward.SendNotificationRequest{Title: "t", Body: "b"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when the server keeps throttling")
	}
	// The first backoff alone (60s) overshoots the budget, so it never sleeps.
	if got := count.Load(); got != 1 {
		t.Errorf("expected 1 attempt, got %d", got)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("pooled client slept past its retry budget, took %s", elapsed)
	}
	if !strings.Contains(err.Error(), "retry budget") {
		t.Errorf("expected a retry-budget error, got %q", err.Error())
	}
}

// Get is an LRU cache keyed by the hlk_ key: the same key reuses one client,
// different keys get their own.
func TestPool_GetCachesPerKey(t *testing.T) {
	p := NewPool("https://api.example", nil)

	a1 := p.Get("hlk_a")
	a2 := p.Get("hlk_a")
	b := p.Get("hlk_b")

	if a1 != a2 {
		t.Error("expected the same client instance for the same key")
	}
	if a1 == b {
		t.Error("expected a distinct client instance per key")
	}
}
