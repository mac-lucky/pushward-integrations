package lifecycle

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

func init() {
	SetRetryDelay(10 * time.Millisecond)
}

func newTestEnder(pool *client.Pool, store state.Store) *Ender {
	return NewEnder(pool, store, "test", EndConfig{
		EndDelay:       1 * time.Hour,
		EndDisplayTime: 1 * time.Hour,
	})
}

func TestUpdateWithRetry_SucceedsFirstAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pool := client.NewPool(srv.URL, nil)
	e := newTestEnder(pool, nil)
	cl := pushward.NewClient(srv.URL, "test-key")
	req := pushward.UpdateRequest{State: pushward.StateOngoing}

	err := e.updateWithRetry(cl, "slug-1", req, 5*time.Second)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 HTTP call, got %d", got)
	}
}

func TestUpdateWithRetry_RetriesOnFailure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest) // 400 = non-retryable inside doWithRetry
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pool := client.NewPool(srv.URL, nil)
	e := newTestEnder(pool, nil)
	cl := pushward.NewClient(srv.URL, "test-key")
	req := pushward.UpdateRequest{State: pushward.StateEnded}

	err := e.updateWithRetry(cl, "slug-2", req, 5*time.Second)
	if err != nil {
		t.Fatalf("expected success on retry, got %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 HTTP calls, got %d", got)
	}
}

func TestUpdateWithRetry_FailsBothAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	pool := client.NewPool(srv.URL, nil)
	e := newTestEnder(pool, nil)
	cl := pushward.NewClient(srv.URL, "test-key")
	req := pushward.UpdateRequest{State: pushward.StateOngoing}

	err := e.updateWithRetry(cl, "slug-3", req, 5*time.Second)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 HTTP calls, got %d", got)
	}
}

func TestFlushAll_SendsEndedPush(t *testing.T) {
	var endedCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"`+pushward.StateEnded+`"`) {
			endedCalls.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pool := client.NewPool(srv.URL, nil)
	e := NewEnder(pool, nil, "test", EndConfig{
		EndDelay:       1 * time.Hour, // long delay so timer won't fire
		EndDisplayTime: 1 * time.Hour,
	})

	e.ScheduleEnd("user1", "key1", "slug-a", pushward.Content{State: "done"})
	e.ScheduleEnd("user1", "key2", "slug-b", pushward.Content{State: "done"})

	e.FlushAll()
	e.Wait()

	if got := endedCalls.Load(); got != 2 {
		t.Fatalf("expected 2 ENDED calls, got %d", got)
	}
}

func TestFlushAll_CleansStateStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL, nil)
	e := NewEnder(pool, store, "test-provider", EndConfig{
		EndDelay:       1 * time.Hour,
		EndDisplayTime: 1 * time.Hour,
	})

	// Pre-seed state store entries.
	_ = store.Set(context.Background(), "test-provider", "user1", "key1", "", json.RawMessage(`{}`), 0)
	_ = store.Set(context.Background(), "test-provider", "user1", "key2", "", json.RawMessage(`{}`), 0)

	e.ScheduleEnd("user1", "key1", "slug-a", pushward.Content{State: "done"})
	e.ScheduleEnd("user1", "key2", "slug-b", pushward.Content{State: "done"})

	e.FlushAll()
	e.Wait()

	// Verify state store entries were deleted.
	v1, _ := store.Get(context.Background(), "test-provider", "user1", "key1", "")
	v2, _ := store.Get(context.Background(), "test-provider", "user1", "key2", "")
	if v1 != nil {
		t.Fatal("expected key1 state to be deleted")
	}
	if v2 != nil {
		t.Fatal("expected key2 state to be deleted")
	}
}

// A redelivered terminal webhook must not postpone the end it asked for.
// ScheduleEnd cancels and re-arms, so before EndIfTracked stopped short of it a
// sender retrying faster than EndDelay could hold a finished activity on the
// lock screen forever.
func TestEndIfTrackedDoesNotRearmAPendingEnd(t *testing.T) {
	var patches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const endDelay = 150 * time.Millisecond
	store := state.NewMemoryStore()
	e := NewEnder(client.NewPool(srv.URL, nil), store, "test", EndConfig{
		EndDelay:       endDelay,
		EndDisplayTime: 1 * time.Hour,
	})
	defer e.StopAll()

	ctx := context.Background()
	if err := store.Set(ctx, "test", "hlk_test", "map-1", "", json.RawMessage(`{}`), time.Hour); err != nil {
		t.Fatalf("seeding state: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	content := pushward.Content{Template: pushward.TemplateGeneric, State: "Done"}

	start := time.Now()
	if !e.EndIfTracked(ctx, log, "hlk_test", "map-1", "slug-1", content) {
		t.Fatal("first EndIfTracked should report an end in flight")
	}

	// Redeliver well inside the window the first call armed.
	time.Sleep(endDelay / 2)
	if !e.EndIfTracked(ctx, log, "hlk_test", "map-1", "slug-1", content) {
		t.Fatal("redelivery should still report an end in flight")
	}

	// Past the ORIGINAL deadline but before a re-armed one would fire.
	time.Sleep(endDelay/2 + 60*time.Millisecond)
	if got := patches.Load(); got != 1 {
		t.Fatalf("expected phase 1 to have fired once on the original schedule after %v, got %d PATCHes",
			time.Since(start), got)
	}
}
