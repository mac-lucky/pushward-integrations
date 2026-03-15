package lifecycle

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// EndConfig holds timing configuration for two-phase activity end.
type EndConfig struct {
	EndDelay       time.Duration
	EndDisplayTime time.Duration
}

// Ender manages two-phase activity end scheduling.
// Phase 1 sends an ONGOING update with final content (visible on Dynamic Island).
// Phase 2 sends an ENDED update after a display delay (dismisses the Live Activity).
type Ender struct {
	clients  *client.Pool
	store    state.Store // nil when no state cleanup is needed
	provider string
	config   EndConfig
	mu       sync.Mutex
	timers   map[string]*time.Timer
}

// NewEnder creates a new Ender. Pass nil for store if no state cleanup is needed.
func NewEnder(clients *client.Pool, store state.Store, provider string, cfg EndConfig) *Ender {
	return &Ender{
		clients:  clients,
		store:    store,
		provider: provider,
		config:   cfg,
		timers:   make(map[string]*time.Timer),
	}
}

// ScheduleEnd schedules a two-phase end for an activity:
//   - Phase 1 (after EndDelay): ONGOING update with final content
//   - Phase 2 (EndDisplayTime later): ENDED with same content
//
// The optional onComplete callback runs after the activity is ended and state
// is cleaned up. It is called outside the Ender's lock.
func (e *Ender) ScheduleEnd(userKey, mapKey, slug string, content pushward.Content, onComplete ...func()) {
	timerKey := userKey + ":" + mapKey
	cl := e.clients.Get(userKey)

	e.mu.Lock()
	if existing, ok := e.timers[timerKey]; ok {
		existing.Stop()
	}
	e.timers[timerKey] = time.AfterFunc(e.config.EndDelay, func() {
		// Phase 1: ONGOING with final content
		ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
		ongoingReq := pushward.UpdateRequest{
			State:   "ONGOING",
			Content: content,
		}
		if err := cl.UpdateActivity(ctx1, slug, ongoingReq); err != nil {
			slog.Error("failed to update activity (end phase 1)", "slug", slug, "error", err)
		} else {
			slog.Info("updated activity", "slug", slug, "state", content.State)
		}
		cancel1()

		time.Sleep(e.config.EndDisplayTime)

		// Phase 2: ENDED with same content
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel2()
		endedReq := pushward.UpdateRequest{
			State:   "ENDED",
			Content: content,
		}
		if err := cl.UpdateActivity(ctx2, slug, endedReq); err != nil {
			slog.Error("failed to end activity (end phase 2)", "slug", slug, "error", err)
		} else {
			slog.Info("ended activity", "slug", slug, "state", content.State)
		}

		// Clean up state store
		if e.store != nil {
			delCtx, delCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer delCancel()
			_ = e.store.Delete(delCtx, e.provider, userKey, mapKey, "")
		}

		// Clean up timer
		e.mu.Lock()
		delete(e.timers, timerKey)
		e.mu.Unlock()

		// Run optional post-cleanup callback
		for _, fn := range onComplete {
			fn()
		}
	})
	e.mu.Unlock()
}

// StopTimer cancels a pending end timer if one exists for the given key.
func (e *Ender) StopTimer(userKey, mapKey string) {
	timerKey := userKey + ":" + mapKey
	e.mu.Lock()
	if t, ok := e.timers[timerKey]; ok {
		t.Stop()
		delete(e.timers, timerKey)
	}
	e.mu.Unlock()
}
