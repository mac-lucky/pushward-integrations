package ratelimit

import (
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

	if got := ClientIP(r.RemoteAddr, r.Header.Get); got != "1.2.3.4" {
		t.Errorf("expected CF-Connecting-IP 1.2.3.4, got %s", got)
	}
}

func TestClientIP_XRealIP(t *testing.T) {
	setupTrustedProxy(t)

	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Real-IP", "5.6.7.8")
	r.Header.Set("X-Forwarded-For", "9.10.11.12")

	if got := ClientIP(r.RemoteAddr, r.Header.Get); got != "5.6.7.8" {
		t.Errorf("expected X-Real-IP 5.6.7.8, got %s", got)
	}
}

func TestClientIP_XForwardedFor_RightmostUntrusted(t *testing.T) {
	setupTrustedProxy(t)

	// The rightmost entry (10.0.0.5) is a trusted proxy hop and must be skipped;
	// the next entry (9.10.11.12) is the closest untrusted client. The leftmost
	// is client-controlled and must NOT be trusted blindly.
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 9.10.11.12, 10.0.0.5")

	if got := ClientIP(r.RemoteAddr, r.Header.Get); got != "9.10.11.12" {
		t.Errorf("expected rightmost untrusted XFF entry 9.10.11.12, got %s", got)
	}
}

func TestClientIP_XForwardedFor_SingleEntry(t *testing.T) {
	setupTrustedProxy(t)

	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("X-Forwarded-For", "9.10.11.12")

	if got := ClientIP(r.RemoteAddr, r.Header.Get); got != "9.10.11.12" {
		t.Errorf("expected XFF 9.10.11.12, got %s", got)
	}
}

func TestClientIP_RemoteAddr_IPv4(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "192.168.1.1:12345"

	if got := ClientIP(r.RemoteAddr, r.Header.Get); got != "192.168.1.1" {
		t.Errorf("expected 192.168.1.1, got %s", got)
	}
}

func TestClientIP_RemoteAddr_IPv6(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "[::1]:12345"

	if got := ClientIP(r.RemoteAddr, r.Header.Get); got != "::1" {
		t.Errorf("expected ::1, got %s", got)
	}
}

func TestClientIP_RemoteAddr_NoPort(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "192.168.1.1"

	if got := ClientIP(r.RemoteAddr, r.Header.Get); got != "192.168.1.1" {
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

	if got := ClientIP(r.RemoteAddr, r.Header.Get); got != "203.0.113.1" {
		t.Errorf("expected RemoteAddr 203.0.113.1 (untrusted proxy), got %s", got)
	}
}

func TestAllowIP_BurstExhaustion(t *testing.T) {
	saved := ipLimiters
	ipLimiters = newLimiterMap(5, 20, 5_000)
	t.Cleanup(func() { ipLimiters = saved })

	// Exhaust the burst (20 tokens).
	for i := 0; i < 20; i++ {
		if !AllowIP("10.0.0.1") {
			t.Fatalf("request %d: expected allowed within burst", i+1)
		}
	}
	if AllowIP("10.0.0.1") {
		t.Error("expected rate limit after burst exhaustion")
	}
}

func TestAllowIP_IndependentBuckets(t *testing.T) {
	saved := ipLimiters
	ipLimiters = newLimiterMap(5, 20, 5_000)
	t.Cleanup(func() { ipLimiters = saved })

	for i := 0; i < 20; i++ {
		AllowIP("10.0.0.1")
	}
	if AllowIP("10.0.0.1") {
		t.Error("IP A: expected rate limit")
	}
	if !AllowIP("10.0.0.2") {
		t.Error("IP B: expected allowed (separate bucket)")
	}
}
