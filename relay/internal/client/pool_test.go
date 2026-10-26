package client

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func send(p *Pool, key string) error {
	return p.SendNotification(context.Background(), key, discardLogger(),
		pushward.SendNotificationRequest{Title: "t", Body: "b"})
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

func TestNewTransport_RaisesIdleConnsKeepsTimeouts(t *testing.T) {
	def := http.DefaultTransport.(*http.Transport)
	tr := NewTransport()

	if tr.MaxIdleConnsPerHost != maxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", tr.MaxIdleConnsPerHost, maxIdleConnsPerHost)
	}
	if tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
		t.Errorf("MaxIdleConns %d caps the per-host %d", tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	if tr.IdleConnTimeout != def.IdleConnTimeout || tr.TLSHandshakeTimeout != def.TLSHandshakeTimeout ||
		tr.ExpectContinueTimeout != def.ExpectContinueTimeout || tr.Proxy == nil || tr.DialContext == nil {
		t.Error("NewTransport dropped one of http.DefaultTransport's settings")
	}
	if def.MaxIdleConnsPerHost != 0 {
		t.Error("NewTransport changed http.DefaultTransport")
	}
}

// Twenty tenants send at once, twice. The first wave needs twenty
// connections; the second must find all of them idle. With
// http.DefaultTransport's two idle connections per host it dialed eighteen
// more.
func TestPool_TenantsReuseIdleConnections(t *testing.T) {
	const n = 20
	type wave struct {
		arrived atomic.Int32
		all     chan struct{}
	}
	var current atomic.Pointer[wave]
	var dials atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Hold every request until the whole wave is in flight, so the
		// wave cannot get by on fewer connections.
		wv := current.Load()
		if wv.arrived.Add(1) == n {
			close(wv.all)
		}
		select {
		case <-wv.all:
		case <-time.After(5 * time.Second):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			dials.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	p := NewPool(srv.URL, nil)
	for range 2 {
		current.Store(&wave{all: make(chan struct{})})
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				if err := send(p, fmt.Sprintf("hlk_tenant%d", i)); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
	}
	if got := dials.Load(); got != n {
		t.Errorf("%d connections for two waves of %d, want %d", got, n, n)
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
