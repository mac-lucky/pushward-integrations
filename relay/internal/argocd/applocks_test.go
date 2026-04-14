package argocd

import (
	"fmt"
	"sync"
	"testing"
)

func TestAppLocks_RefcountReclaimsEntries(t *testing.T) {
	tests := []struct {
		name       string
		goroutines int
		keys       int
		iterations int
	}{
		{"single key many goroutines", 64, 1, 200},
		{"many keys many goroutines", 64, 16, 200},
		{"mostly unique keys", 32, 1024, 10},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAppLocks()

			var wg sync.WaitGroup
			for g := 0; g < tc.goroutines; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					for i := 0; i < tc.iterations; i++ {
						key := fmt.Sprintf("k-%d", (g+i)%tc.keys)
						m := a.Acquire(key)
						m.Unlock()
						a.Release(key)
					}
				}(g)
			}
			wg.Wait()

			if got := a.size(); got != 0 {
				t.Fatalf("appLocks leaked entries: got %d, want 0", got)
			}
		})
	}
}

func TestAppLocks_MutualExclusion(t *testing.T) {
	a := newAppLocks()
	const goroutines = 32
	const iterations = 500

	var counter int
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				m := a.Acquire("shared")
				counter++
				m.Unlock()
				a.Release("shared")
			}
		}()
	}
	wg.Wait()

	if counter != goroutines*iterations {
		t.Fatalf("mutex did not serialize: counter=%d want=%d", counter, goroutines*iterations)
	}
	if got := a.size(); got != 0 {
		t.Fatalf("appLocks leaked entries: got %d, want 0", got)
	}
}

func TestAppLocks_ReacquireAfterDelete(t *testing.T) {
	a := newAppLocks()

	m1 := a.Acquire("k")
	m1.Unlock()
	a.Release("k")

	if got := a.size(); got != 0 {
		t.Fatalf("expected entry deleted after release, got size=%d", got)
	}

	m2 := a.Acquire("k")
	if got := a.size(); got != 1 {
		t.Fatalf("expected entry recreated on reacquire, got size=%d", got)
	}
	m2.Unlock()
	a.Release("k")

	if got := a.size(); got != 0 {
		t.Fatalf("expected entry deleted after second release, got size=%d", got)
	}
}
