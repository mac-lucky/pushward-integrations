package lrumap

import (
	"strconv"
	"testing"
	"time"
)

func TestSweep_RemovesOldEntries(t *testing.T) {
	m := New[int](100)

	// Insert entries with artificially old timestamps.
	m.GetOrCreate("old1", func() int { return 1 })
	m.GetOrCreate("old2", func() int { return 2 })

	// Backdate lastAccess to 10 minutes ago.
	past := time.Now().Add(-10 * time.Minute).UnixNano()
	m.mu.RLock()
	for _, e := range m.entries {
		e.lastAccess.Store(past)
	}
	m.mu.RUnlock()

	// Insert a fresh entry.
	m.GetOrCreate("fresh", func() int { return 3 })

	if m.Len() != 3 {
		t.Fatalf("expected 3 entries before sweep, got %d", m.Len())
	}

	removed := m.Sweep(5 * time.Minute)
	if removed != 2 {
		t.Fatalf("expected 2 removed, got %d", removed)
	}
	if m.Len() != 1 {
		t.Fatalf("expected 1 entry after sweep, got %d", m.Len())
	}

	// The fresh entry should still be accessible.
	v := m.GetOrCreate("fresh", func() int { return 99 })
	if v != 3 {
		t.Fatalf("expected fresh entry value 3, got %d", v)
	}
}

func TestSweep_KeepsRecentEntries(t *testing.T) {
	m := New[string](100)
	m.GetOrCreate("a", func() string { return "alpha" })
	m.GetOrCreate("b", func() string { return "beta" })

	removed := m.Sweep(5 * time.Minute)
	if removed != 0 {
		t.Fatalf("expected 0 removed for recent entries, got %d", removed)
	}
	if m.Len() != 2 {
		t.Fatalf("expected 2 entries still present, got %d", m.Len())
	}
}

func TestGetOrCreate_EvictsLRU(t *testing.T) {
	m := New[int](3)

	// Small sleeps ensure each access has a strictly monotonic UnixNano
	// timestamp even on systems with coarse clock resolution.
	m.GetOrCreate("a", func() int { return 1 })
	time.Sleep(time.Millisecond)
	m.GetOrCreate("b", func() int { return 2 })
	time.Sleep(time.Millisecond)
	m.GetOrCreate("c", func() int { return 3 })
	time.Sleep(time.Millisecond)

	// Access "a" to make it recently used; "b" becomes LRU.
	m.GetOrCreate("a", func() int { return 99 })
	time.Sleep(time.Millisecond)

	// Insert "d" — should evict "b" (least recently used).
	var evictedKey string
	m.SetOnEvict(func(key string, _ int) { evictedKey = key })
	m.GetOrCreate("d", func() int { return 4 })

	if evictedKey != "b" {
		t.Fatalf("expected eviction of 'b', got '%s'", evictedKey)
	}
	if m.Len() != 3 {
		t.Fatalf("expected 3 entries after eviction, got %d", m.Len())
	}
}

// TestGetOrCreate_EvictsViaSampling exercises the approximate-LRU sampling path:
// the existing eviction test uses maxSize=3 (< evictionSampleSize=8) so it never
// reaches the sample-and-break loop. Here maxSize > evictionSampleSize and more
// than maxSize distinct keys are inserted, so every insertion past capacity must
// trigger one eviction while Len() stays pinned at capacity. The victim is
// pseudo-random (Go randomizes map iteration), so we assert observable counts,
// not a specific victim.
func TestGetOrCreate_EvictsViaSampling(t *testing.T) {
	const (
		maxSize = 10 // > evictionSampleSize (8) so the sampling break path runs.
		inserts = 25
	)
	if maxSize <= evictionSampleSize {
		t.Fatalf("test precondition: maxSize (%d) must exceed evictionSampleSize (%d)", maxSize, evictionSampleSize)
	}

	m := New[int](maxSize)
	evictions := 0
	m.SetOnEvict(func(string, int) { evictions++ })

	for i := range inserts {
		m.GetOrCreate("k"+strconv.Itoa(i), func() int { return i })
	}

	// Capacity must never be exceeded.
	if got := m.Len(); got != maxSize {
		t.Fatalf("expected Len() pinned at capacity %d, got %d", maxSize, got)
	}

	// Each of the inserts beyond capacity must have evicted exactly one entry.
	if want := inserts - maxSize; evictions != want {
		t.Fatalf("expected %d evictions for %d inserts at capacity %d, got %d",
			want, inserts, maxSize, evictions)
	}
}
