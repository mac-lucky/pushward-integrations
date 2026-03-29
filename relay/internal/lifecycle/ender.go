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

// EnderProvider is implemented by handlers that own a lifecycle.Ender.
type EnderProvider interface {
	Ender() *Ender
}

// EndConfig holds timing configuration for two-phase activity end.
type EndConfig struct {
	EndDelay       time.Duration
	EndDisplayTime time.Duration
}

// timerPair holds both phase-1 and phase-2 timers so StopTimer can cancel both.
type timerPair struct {
	phase1 *time.Timer
	phase2 *time.Timer
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
	timers   map[string]*timerPair
	wg       sync.WaitGroup
}

// NewEnder creates a new Ender. Pass nil for store if no state cleanup is needed.
func NewEnder(clients *client.Pool, store state.Store, provider string, cfg EndConfig) *Ender {
	return &Ender{
		clients:  clients,
		store:    store,
		provider: provider,
		config:   cfg,
		timers:   make(map[string]*timerPair),
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
		if existing.phase1.Stop() {
			e.wg.Done()
		}
		if existing.phase2 != nil && existing.phase2.Stop() {
			e.wg.Done()
		}
	}
	tp := &timerPair{}
	e.wg.Add(1)
	tp.phase1 = time.AfterFunc(e.config.EndDelay, func() {
		defer e.wg.Done()
		// Phase 1: ONGOING with final content
		ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel1()
		ongoingReq := pushward.UpdateRequest{
			State:   pushward.StateOngoing,
			Content: content,
		}
		if err := cl.UpdateActivity(ctx1, slug, ongoingReq); err != nil {
			slog.Error("failed to update activity (end phase 1)", "slug", slug, "error", err)
		} else {
			slog.Info("updated activity (end phase 1)", "slug", slug, "state", pushward.StateOngoing)
		}

		// Phase 2: schedule ENDED after display time
		e.mu.Lock()
		if _, ok := e.timers[timerKey]; !ok {
			e.mu.Unlock()
			return // StopTimer already cancelled this end sequence
		}
		e.wg.Add(1)
		tp.phase2 = time.AfterFunc(e.config.EndDisplayTime, func() {
			defer e.wg.Done()
			ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel2()
			endedReq := pushward.UpdateRequest{
				State:   pushward.StateEnded,
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
				if err := e.store.Delete(delCtx, e.provider, userKey, mapKey, ""); err != nil {
					slog.Warn("state store delete failed", "error", err, "provider", e.provider)
				}
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
	})
	e.timers[timerKey] = tp
	e.mu.Unlock()
}

// StopTimer cancels a pending end timer if one exists for the given key.
func (e *Ender) StopTimer(userKey, mapKey string) {
	timerKey := userKey + ":" + mapKey
	e.mu.Lock()
	if tp, ok := e.timers[timerKey]; ok {
		if tp.phase1.Stop() {
			e.wg.Done()
		}
		if tp.phase2 != nil && tp.phase2.Stop() {
			e.wg.Done()
		}
		delete(e.timers, timerKey)
	}
	e.mu.Unlock()
}

// StopAll cancels all pending ender timers.
func (e *Ender) StopAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, tp := range e.timers {
		if tp.phase1.Stop() {
			e.wg.Done() // callback will never run
		}
		if tp.phase2 != nil && tp.phase2.Stop() {
			e.wg.Done() // callback will never run
		}
		delete(e.timers, key)
	}
}

// Wait blocks until all in-flight ender callbacks complete.
func (e *Ender) Wait() {
	e.wg.Wait()
}
