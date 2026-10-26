package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/lrumap"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// breakerPool returns a pool with breakers whose clients make one attempt
// per call: a 1ns retry budget stops before the first backoff, so a failing
// call is one request and one fault instead of 15s of retries.
func breakerPool(url string, threshold int, cooldown time.Duration) *Pool {
	return NewPool(url, nil,
		WithClientOptions(pushward.WithRetryBudget(time.Nanosecond)),
		WithBreakers(threshold, cooldown),
	)
}

// hitsByKey counts requests per bearer key and answers status(key).
func hitsByKey(t *testing.T, status func(key string) int) (*httptest.Server, map[string]*atomic.Int32) {
	t.Helper()
	hits := map[string]*atomic.Int32{}
	for _, k := range []string{"hlk_a", "hlk_b", "hlk_c", "hlk_d", "hlk_e"} {
		hits[k] = new(atomic.Int32)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if h, ok := hits[key]; ok {
			h.Add(1)
		}
		w.WriteHeader(status(key))
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func TestBreakers_TenantFailuresLeaveOtherTenantsDelivering(t *testing.T) {
	srv, hits := hitsByKey(t, func(key string) int {
		if key == "hlk_a" {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	p := breakerPool(srv.URL, 5, time.Minute)

	for i := range 5 {
		if err := send(p, "hlk_a"); err == nil || errors.Is(err, pushward.ErrCircuitOpen) {
			t.Fatalf("call %d: want a 500 error, got %v", i, err)
		}
	}
	if err := send(p, "hlk_a"); !errors.Is(err, pushward.ErrCircuitOpen) {
		t.Fatalf("6th call for the failing tenant: want ErrCircuitOpen, got %v", err)
	}
	if got := hits["hlk_a"].Load(); got != 5 {
		t.Errorf("failing tenant reached the server %d times, want 5", got)
	}
	if p.ServerBreakerOpen() {
		t.Fatal("one tenant's failures opened the server breaker")
	}

	for _, k := range []string{"hlk_b", "hlk_c"} {
		if err := send(p, k); err != nil {
			t.Errorf("%s: want delivery, got %v", k, err)
		}
	}
	if got := p.OpenTenantBreakers(); got != 1 {
		t.Errorf("OpenTenantBreakers = %d, want 1", got)
	}
}

func TestBreakers_ServerWideFailureOpensForEveryTenant(t *testing.T) {
	down := func(t *testing.T) string {
		srv, _ := hitsByKey(t, func(string) int { return http.StatusServiceUnavailable })
		return srv.URL
	}
	refused := func(t *testing.T) string {
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close()
		return srv.URL
	}
	for name, start := range map[string]func(*testing.T) string{"503 for everyone": down, "connection refused": refused} {
		t.Run(name, func(t *testing.T) {
			p := breakerPool(start(t), 5, time.Minute)

			// Five faults from three tenants, none of them enough to open a
			// tenant's own breaker.
			for _, k := range []string{"hlk_a", "hlk_b", "hlk_c", "hlk_a", "hlk_b"} {
				if err := send(p, k); err == nil || errors.Is(err, pushward.ErrCircuitOpen) {
					t.Fatalf("%s: want a server error, got %v", k, err)
				}
			}
			if !p.ServerBreakerOpen() {
				t.Fatal("server breaker still closed after faults from 3 tenants")
			}
			if got := p.OpenTenantBreakers(); got != 0 {
				t.Errorf("OpenTenantBreakers = %d, want 0", got)
			}
			// A tenant that never failed is stopped too.
			if err := send(p, "hlk_d"); !errors.Is(err, pushward.ErrCircuitOpen) {
				t.Fatalf("fresh tenant: want ErrCircuitOpen, got %v", err)
			}
		})
	}
}

func TestBreakers_ServerBreakerRecoversOnProbe(t *testing.T) {
	var healthy atomic.Bool
	srv, hits := hitsByKey(t, func(string) int {
		if healthy.Load() {
			return http.StatusOK
		}
		return http.StatusBadGateway
	})
	p := breakerPool(srv.URL, 3, 50*time.Millisecond)

	for _, k := range []string{"hlk_a", "hlk_b", "hlk_c"} {
		_ = send(p, k)
	}
	if !p.ServerBreakerOpen() {
		t.Fatal("server breaker did not open")
	}
	healthy.Store(true)
	time.Sleep(80 * time.Millisecond)

	if err := send(p, "hlk_d"); err != nil {
		t.Fatalf("probe after cooldown: %v", err)
	}
	if p.ServerBreakerOpen() {
		t.Fatal("a successful probe left the server breaker open")
	}
	if err := send(p, "hlk_e"); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if got := hits["hlk_e"].Load(); got != 1 {
		t.Errorf("hlk_e reached the server %d times, want 1", got)
	}
}

// A 4xx or a sustained 429 proves the server is up: neither may open the
// tenant's breaker or the server's.
func TestBreakers_ClientErrorsAndThrottlingDoNotTrip(t *testing.T) {
	srv, _ := hitsByKey(t, func(key string) int {
		if key == "hlk_a" {
			return http.StatusUnprocessableEntity
		}
		return http.StatusTooManyRequests
	})
	p := breakerPool(srv.URL, 2, time.Minute)

	for range 6 {
		for _, k := range []string{"hlk_a", "hlk_b", "hlk_c"} {
			if err := send(p, k); errors.Is(err, pushward.ErrCircuitOpen) {
				t.Fatalf("%s: breaker opened on a reachable server", k)
			}
		}
	}
	if p.ServerBreakerOpen() || p.OpenTenantBreakers() != 0 {
		t.Fatalf("server open %v, tenants open %d: want neither", p.ServerBreakerOpen(), p.OpenTenantBreakers())
	}
}

// A tenant's breaker lives in its pool entry, so eviction drops it and a
// returning tenant starts closed.
func TestBreakers_EvictionDropsTenantBreaker(t *testing.T) {
	srv, hits := hitsByKey(t, func(key string) int {
		if key == "hlk_a" {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	p := breakerPool(srv.URL, 2, time.Minute)
	// Two entries: below lrumap's sample size, so eviction is exact LRU.
	p.clients = lrumap.New[*pooled](2)

	_ = send(p, "hlk_a")
	_ = send(p, "hlk_a")
	if got := p.OpenTenantBreakers(); got != 1 {
		t.Fatalf("OpenTenantBreakers = %d, want 1", got)
	}
	time.Sleep(time.Millisecond)
	_ = send(p, "hlk_b")
	time.Sleep(time.Millisecond)
	_ = send(p, "hlk_c") // evicts hlk_a

	if got := p.OpenTenantBreakers(); got != 0 {
		t.Errorf("OpenTenantBreakers after eviction = %d, want 0", got)
	}
	if err := send(p, "hlk_a"); errors.Is(err, pushward.ErrCircuitOpen) {
		t.Fatal("evicted tenant came back with its old open breaker")
	}
	if got := hits["hlk_a"].Load(); got != 3 {
		t.Errorf("hlk_a reached the server %d times, want 3", got)
	}
}

func TestServerBreaker_NeedsFaultsFromSeveralTenants(t *testing.T) {
	s := newServerBreaker(5, time.Minute)

	for range 20 {
		s.recordFailure("a")
	}
	if s.blocking() {
		t.Fatal("opened on one tenant's faults")
	}
	if len(s.tenants) != 1 {
		t.Errorf("tracking %d tenants, want 1", len(s.tenants))
	}
	s.recordFailure("b")
	if s.blocking() {
		t.Fatal("opened on two tenants' faults")
	}
	s.recordFailure("c")
	if !s.blocking() {
		t.Fatal("did not open on faults from three tenants")
	}
	if s.streak != 0 || len(s.tenants) != 0 {
		t.Errorf("streak %d, %d tenants kept after opening: want both cleared", s.streak, len(s.tenants))
	}
}

func TestServerBreaker_SuccessEndsStreakReachableDoesNot(t *testing.T) {
	s := newServerBreaker(5, time.Minute)

	for _, k := range []string{"a", "b", "c", "a"} {
		s.recordFailure(k)
	}
	s.recordSuccess()
	for _, k := range []string{"a", "b", "c", "a"} {
		s.recordFailure(k)
	}
	if s.blocking() {
		t.Fatal("a success did not end the streak")
	}

	s.recordReachable()
	s.recordFailure("b")
	if !s.blocking() {
		t.Fatal("a reachable 4xx/429 reset the streak")
	}
}

// The tenant's own breaker is past its cooldown, so Allow admits the call as
// its half-open probe, but the server breaker refuses it. The call never runs,
// so own must be re-armed: left half-open it refuses the tenant for good.
func TestBreakers_OwnProbeRefusedByServerIsRearmed(t *testing.T) {
	const cooldown = 100 * time.Millisecond
	var healthy atomic.Bool
	srv, _ := hitsByKey(t, func(string) int {
		if healthy.Load() {
			return http.StatusOK
		}
		return http.StatusInternalServerError
	})
	p := breakerPool(srv.URL, 5, cooldown)

	for range 5 {
		_ = send(p, "hlk_a")
	}
	time.Sleep(cooldown * 3 / 2) // own(a) may probe now
	// Open the server breaker directly: faults sent through tenants would
	// first have to stay inside the server streak's window.
	for _, k := range []string{"x", "y", "z", "x", "y"} {
		p.server.recordFailure(k)
	}
	if !p.ServerBreakerOpen() {
		t.Fatal("server breaker did not open")
	}
	if err := send(p, "hlk_a"); !errors.Is(err, pushward.ErrCircuitOpen) {
		t.Fatalf("want ErrCircuitOpen while the server breaker is open, got %v", err)
	}

	healthy.Store(true)
	time.Sleep(cooldown * 3 / 2)
	if err := send(p, "hlk_b"); err != nil {
		t.Fatalf("server probe: %v", err)
	}
	time.Sleep(cooldown * 3 / 2)
	if err := send(p, "hlk_a"); err != nil {
		t.Fatalf("tenant a after recovery: %v (own breaker wedged half-open?)", err)
	}
}

// A server probe abandoned during retry backoff never reports an outcome, so
// Abort must re-arm the server breaker: left half-open it refuses every tenant.
func TestBreakers_AbandonedServerProbeIsRearmed(t *testing.T) {
	const cooldown = 50 * time.Millisecond
	var healthy atomic.Bool
	srv, _ := hitsByKey(t, func(string) int {
		if healthy.Load() {
			return http.StatusOK
		}
		return http.StatusServiceUnavailable
	})
	p := NewPool(srv.URL, nil,
		WithClientOptions(pushward.WithRetryBudget(10*time.Second)),
		WithBreakers(3, cooldown),
	)
	for _, k := range []string{"a", "b", "c"} {
		p.server.recordFailure(k)
	}
	if !p.ServerBreakerOpen() {
		t.Fatal("server breaker did not open")
	}
	time.Sleep(cooldown * 3 / 2)

	// The first backoff is at least 500ms, so the deadline cuts it short.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := p.SendNotification(ctx, "hlk_d", discardLogger(), pushward.SendNotificationRequest{Title: "t", Body: "b"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the deadline to end the backoff, got %v", err)
	}

	healthy.Store(true)
	time.Sleep(cooldown * 3 / 2)
	if err := send(p, "hlk_e"); err != nil {
		t.Fatalf("after the abandoned probe: %v (server breaker wedged half-open?)", err)
	}
}

// A breaker past its cooldown no longer refuses anything, even though nobody
// has called Allow since. The gauges must drop back to zero for a tenant that
// tripped its breaker and then went quiet, since it stays pooled until
// eviction.
func TestBreakers_GaugesClearWhenCooldownEnds(t *testing.T) {
	const cooldown = 50 * time.Millisecond
	srv, _ := hitsByKey(t, func(string) int { return http.StatusInternalServerError })
	p := breakerPool(srv.URL, 2, cooldown)

	_ = send(p, "hlk_a")
	_ = send(p, "hlk_a")
	for _, k := range []string{"x", "y", "z"} {
		p.server.recordFailure(k)
	}
	if got := p.OpenTenantBreakers(); got != 1 {
		t.Fatalf("OpenTenantBreakers = %d, want 1", got)
	}
	if !p.ServerBreakerOpen() {
		t.Fatal("server breaker did not open")
	}

	time.Sleep(cooldown * 3 / 2)
	if got := p.OpenTenantBreakers(); got != 0 {
		t.Errorf("OpenTenantBreakers after the cooldown = %d, want 0", got)
	}
	if p.ServerBreakerOpen() {
		t.Error("ServerBreakerOpen still true after the cooldown")
	}
}
