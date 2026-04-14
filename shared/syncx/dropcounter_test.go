package syncx

import (
	"sync"
	"testing"
)

func TestDropCounter(t *testing.T) {
	tests := []struct {
		name     string
		logEvery uint64
		drops    int
		wantLogs []uint64 // totals at which shouldLog was true
	}{
		{"first drop logs", 100, 1, []uint64{1}},
		{"second drop does not log", 100, 2, []uint64{1}},
		{"logs at Nth drop", 10, 10, []uint64{1, 10}},
		{"multiple cycles", 5, 12, []uint64{1, 5, 10}},
		{"logEvery=1 logs every drop", 1, 4, []uint64{1, 2, 3, 4}},
		{"zero drops logs nothing", 100, 0, nil},
		{"logEvery=2 logs odd first", 2, 6, []uint64{1, 2, 4, 6}},
		{"logEvery large", 1000, 5, []uint64{1}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewDropCounter(tc.logEvery)
			var got []uint64
			for i := 0; i < tc.drops; i++ {
				total, shouldLog := c.Drop()
				if shouldLog {
					got = append(got, total)
				}
			}
			if !equalU64(got, tc.wantLogs) {
				t.Errorf("got logs at %v, want %v", got, tc.wantLogs)
			}
		})
	}
}

func TestDropCounter_ZeroLogEveryPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for logEvery=0")
		}
	}()
	NewDropCounter(0)
}

func TestDropCounter_ConcurrentIncrement(t *testing.T) {
	c := NewDropCounter(100)
	const goroutines = 50
	const perGoroutine = 20
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				c.Drop()
			}
		}()
	}
	wg.Wait()
	total, _ := c.Drop()
	if total != goroutines*perGoroutine+1 {
		t.Errorf("total=%d, want %d", total, goroutines*perGoroutine+1)
	}
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
