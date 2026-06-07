package ratelimit

import "testing"

func TestAllowKey_BurstExhaustion(t *testing.T) {
	saved := keyLimiters
	keyLimiters = newLimiterMap(1, 10, 10_000)
	t.Cleanup(func() { keyLimiters = saved })

	// Exhaust the burst (10 tokens).
	for i := 0; i < 10; i++ {
		if !AllowKey("POST /jellyfin", "hlk_testkey") {
			t.Fatalf("request %d: expected allowed within burst", i+1)
		}
	}
	// Next request exceeds the burst.
	if AllowKey("POST /jellyfin", "hlk_testkey") {
		t.Error("expected rate limit after burst exhaustion")
	}
}

func TestAllowKey_PerRouteIsolation(t *testing.T) {
	saved := keyLimiters
	keyLimiters = newLimiterMap(1, 10, 10_000)
	t.Cleanup(func() { keyLimiters = saved })

	// Exhaust route A.
	for i := 0; i < 10; i++ {
		AllowKey("POST /jellyfin", "hlk_testkey")
	}
	if AllowKey("POST /jellyfin", "hlk_testkey") {
		t.Error("route A: expected rate limit")
	}
	// Route B with the same key has its own bucket.
	if !AllowKey("POST /argocd", "hlk_testkey") {
		t.Error("route B: expected allowed (separate bucket)")
	}
}
