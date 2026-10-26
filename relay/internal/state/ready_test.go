package state

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/server"
)

func TestReadyCheck_SurvivesBlipFailsWhenSustained(t *testing.T) {
	// The handler runs on the server's goroutine, so the fake clock and
	// ping result sit behind a lock.
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var pingErr error
	r := &readiness{
		ping: func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			return pingErr
		},
		grace: 30 * time.Second,
		now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
	}
	srv := httptest.NewServer(server.NewMux(r.check))
	defer srv.Close()

	status := func(path string) int {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	step := func(d time.Duration, err error) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
		pingErr = err
	}
	down := errors.New("connection refused")

	steps := []struct {
		name  string
		after time.Duration
		err   error
		want  int
	}{
		{"healthy", 0, nil, http.StatusOK},
		{"blip starts", 10 * time.Second, down, http.StatusOK},
		{"blip at 20s", 20 * time.Second, down, http.StatusOK},
		{"back", 5 * time.Second, nil, http.StatusOK},
		// The success reset the clock: 29s into the next outage is still
		// inside the grace, 30s is not.
		{"outage starts", 10 * time.Second, down, http.StatusOK},
		{"outage at 29s", 29 * time.Second, down, http.StatusOK},
		{"outage at 30s", time.Second, down, http.StatusServiceUnavailable},
		{"outage at 60s", 30 * time.Second, down, http.StatusServiceUnavailable},
		{"recovered", 5 * time.Second, nil, http.StatusOK},
	}
	for _, s := range steps {
		step(s.after, s.err)
		if got := status("/ready"); got != s.want {
			t.Errorf("%s: /ready = %d, want %d", s.name, got, s.want)
		}
		if s.err != nil {
			if got := status("/health"); got != http.StatusOK {
				t.Errorf("%s: /health = %d, liveness must not follow the database", s.name, got)
			}
		}
	}
}
