package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func setupTrustedProxy(t *testing.T) {
	t.Helper()
	saved := trustedProxies
	// Trust the default httptest RemoteAddr range (192.0.2.0/24) and 10.0.0.0/8
	if err := SetTrustedProxyCIDRs([]string{"192.0.2.0/24", "10.0.0.0/8"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { trustedProxies = saved })
}

func TestClientIP_CFConnectingIP(t *testing.T) {
	setupTrustedProxy(t)

	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("CF-Connecting-IP", "1.2.3.4")
	r.Header.Set("X-Real-IP", "5.6.7.8")
	r.Header.Set("X-Forwarded-For", "9.10.11.12, 13.14.15.16")

	if got := clientIP(r); got != "1.2.3.4" {
		t.Errorf("expected CF-Connecting-IP 1.2.3.4, got %s", got)
	}
}

func TestClientIP_XRealIP(t *testing.T) {
	setupTrustedProxy(t)

	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Real-IP", "5.6.7.8")
	r.Header.Set("X-Forwarded-For", "9.10.11.12")

	if got := clientIP(r); got != "5.6.7.8" {
		t.Errorf("expected X-Real-IP 5.6.7.8, got %s", got)
	}
}

func TestClientIP_XForwardedFor_FirstEntry(t *testing.T) {
	setupTrustedProxy(t)

	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Forwarded-For", "9.10.11.12, 13.14.15.16")

	if got := clientIP(r); got != "9.10.11.12" {
		t.Errorf("expected first XFF entry 9.10.11.12, got %s", got)
	}
}

func TestClientIP_XForwardedFor_SingleEntry(t *testing.T) {
	setupTrustedProxy(t)

	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Forwarded-For", "9.10.11.12")

	if got := clientIP(r); got != "9.10.11.12" {
		t.Errorf("expected XFF 9.10.11.12, got %s", got)
	}
}

func TestClientIP_RemoteAddr_IPv4(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "192.168.1.1:12345"

	if got := clientIP(r); got != "192.168.1.1" {
		t.Errorf("expected 192.168.1.1, got %s", got)
	}
}

func TestClientIP_RemoteAddr_IPv6(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "[::1]:12345"

	if got := clientIP(r); got != "::1" {
		t.Errorf("expected ::1, got %s", got)
	}
}

func TestClientIP_RemoteAddr_NoPort(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "192.168.1.1"

	if got := clientIP(r); got != "192.168.1.1" {
		t.Errorf("expected 192.168.1.1, got %s", got)
	}
}

func TestClientIP_UntrustedProxy_IgnoresHeaders(t *testing.T) {
	// No trusted proxies configured
	saved := trustedProxies
	trustedProxies = nil
	t.Cleanup(func() { trustedProxies = saved })

	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "203.0.113.1:12345"
	r.Header.Set("CF-Connecting-IP", "1.2.3.4")
	r.Header.Set("X-Real-IP", "5.6.7.8")
	r.Header.Set("X-Forwarded-For", "9.10.11.12")

	if got := clientIP(r); got != "203.0.113.1" {
		t.Errorf("expected RemoteAddr 203.0.113.1 (untrusted proxy), got %s", got)
	}
}

func TestIPMiddleware_BurstExhaustion(t *testing.T) {
	// Use a fresh limiter map for this test
	saved := ipLimiters
	ipLimiters = newLimiterMap(5, 20, 5_000)
	t.Cleanup(func() { ipLimiters = saved })

	handler := IPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Exhaust burst (20 requests)
	for i := 0; i < 20; i++ {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, w.Code)
		}
	}

	// Next request should be rate limited
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after burst exhaustion, got %d", w.Code)
	}
}

func TestIPMiddleware_IndependentBuckets(t *testing.T) {
	saved := ipLimiters
	ipLimiters = newLimiterMap(5, 20, 5_000)
	t.Cleanup(func() { ipLimiters = saved })

	handler := IPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Exhaust burst for IP A
	for i := 0; i < 20; i++ {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
	}

	// IP A should be limited
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("IP A: expected 429, got %d", w.Code)
	}

	// IP B should still be allowed
	r = httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "10.0.0.2:1234"
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("IP B: expected 200, got %d", w.Code)
	}
}
