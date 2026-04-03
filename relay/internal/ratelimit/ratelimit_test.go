package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
)

func withAuth(h http.Handler) http.Handler {
	return auth.Middleware(h)
}

func TestKeyMiddleware_BurstExhaustion(t *testing.T) {
	saved := keyLimiters
	keyLimiters = newLimiterMap(1, 10, 10_000)
	t.Cleanup(func() { keyLimiters = saved })

	handler := withAuth(Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	// Exhaust burst (10 requests)
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest("POST", "/jellyfin", nil)
		r.Pattern = "POST /jellyfin"
		r.Header.Set("Authorization", "Bearer hlk_testkey")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, w.Code)
		}
	}

	// Next request should be rate limited
	r := httptest.NewRequest("POST", "/jellyfin", nil)
	r.Pattern = "POST /jellyfin"
	r.Header.Set("Authorization", "Bearer hlk_testkey")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after burst exhaustion, got %d", w.Code)
	}
}

func TestKeyMiddleware_PerRouteIsolation(t *testing.T) {
	saved := keyLimiters
	keyLimiters = newLimiterMap(1, 10, 10_000)
	t.Cleanup(func() { keyLimiters = saved })

	handler := withAuth(Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	// Exhaust burst on route A (jellyfin)
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest("POST", "/jellyfin", nil)
		r.Pattern = "POST /jellyfin"
		r.Header.Set("Authorization", "Bearer hlk_testkey")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
	}

	// Route A should be limited
	r := httptest.NewRequest("POST", "/jellyfin", nil)
	r.Pattern = "POST /jellyfin"
	r.Header.Set("Authorization", "Bearer hlk_testkey")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("route A: expected 429, got %d", w.Code)
	}

	// Route B (argocd) should still be allowed with the same key
	r = httptest.NewRequest("POST", "/argocd", nil)
	r.Pattern = "POST /argocd"
	r.Header.Set("Authorization", "Bearer hlk_testkey")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("route B: expected 200, got %d", w.Code)
	}
}

func TestKeyMiddleware_NoKey_PassThrough(t *testing.T) {
	saved := keyLimiters
	keyLimiters = newLimiterMap(1, 1, 10_000) // burst=1 to catch any unexpected limiting
	t.Cleanup(func() { keyLimiters = saved })

	var called bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	// Skip auth.Middleware — call Middleware directly with no key in context.
	handler := Middleware(inner)

	r := httptest.NewRequest("POST", "/jellyfin", nil)
	r.Pattern = "POST /jellyfin"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if !called {
		t.Error("expected handler to be called when no key is present")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 pass-through, got %d", w.Code)
	}
}
