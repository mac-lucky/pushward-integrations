package lifecycle

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// enderRetryDelay is the pause between the two outer attempts in updateWithRetry.
// Tests override this via SetRetryDelay to keep execution fast.
var enderRetryDelay = 5 * time.Second

// SetRetryDelay overrides the pause between outer retry attempts in
// updateWithRetry. Intended for use in tests only.
func SetRetryDelay(d time.Duration) { enderRetryDelay = d }

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
// The gen field is incremented on each ScheduleEnd so that a superseded phase-1
// goroutine can detect it has been replaced and avoid arming an orphaned phase-2.
type timerPair struct {
	phase1  *time.Timer
	phase2  *time.Timer
	gen     uint64
	userKey string
	mapKey  string
	slug    string
	content pushward.Content
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
	nextGen  uint64
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
	timerKey := auth.MapKeyPrefix(userKey) + ":" + mapKey
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
	e.nextGen++
	tp := &timerPair{
		gen:     e.nextGen,
		userKey: userKey,
		mapKey:  mapKey,
		slug:    slug,
		content: content,
	}
	myGen := tp.gen
	e.wg.Add(1)
	tp.phase1 = time.AfterFunc(e.config.EndDelay, func() {
		defer e.wg.Done()
		// Phase 1: ONGOING with final content
		ongoingReq := pushward.UpdateRequest{
			State:   pushward.StateOngoing,
			Content: content,
		}
		if err := updateWithRetry(cl, slug, ongoingReq, 30*time.Second); err != nil {
			slog.Error("failed to update activity (end phase 1)", "slug", slug, "error", err)
		} else {
			slog.Info("updated activity (end phase 1)", "slug", slug, "state", pushward.StateOngoing)
		}

		// Phase 2: schedule ENDED after display time
		e.mu.Lock()
		cur, ok := e.timers[timerKey]
		if !ok || cur.gen != myGen {
			e.mu.Unlock()
			return // StopTimer cancelled or ScheduleEnd superseded this sequence
		}
		e.wg.Add(1)
		tp.phase2 = time.AfterFunc(e.config.EndDisplayTime, func() {
			defer e.wg.Done()
			endedReq := pushward.UpdateRequest{
				State:   pushward.StateEnded,
				Content: content,
			}
			if err := updateWithRetry(cl, slug, endedReq, 30*time.Second); err != nil {
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
	timerKey := auth.MapKeyPrefix(userKey) + ":" + mapKey
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

// FlushAll stops all pending ender timers and immediately sends an ENDED
// update for each. Use this during graceful shutdown instead of StopAll to
// ensure activities are properly ended before the process exits.
func (e *Ender) FlushAll() {
	e.mu.Lock()
	pending := make([]*timerPair, 0, len(e.timers))
	for key, tp := range e.timers {
		if tp.phase1.Stop() {
			e.wg.Done()
		}
		if tp.phase2 != nil && tp.phase2.Stop() {
			e.wg.Done()
		}
		pending = append(pending, tp)
		delete(e.timers, key)
	}
	e.mu.Unlock()

	if len(pending) == 0 {
		return
	}

	// Wait for any in-flight callbacks that were already running when
	// Stop() returned false. This prevents a phase-1 ONGOING update from
	// landing after our ENDED update.
	e.wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i, tp := range pending {
		if ctx.Err() != nil {
			slog.Warn("flush timed out, dropping remaining enders", "remaining", len(pending)-i)
			break
		}
		cl := e.clients.Get(tp.userKey)
		endReq := pushward.UpdateRequest{
			State:   pushward.StateEnded,
			Content: tp.content,
		}
		if err := cl.UpdateActivity(ctx, tp.slug, endReq); err != nil {
			slog.Error("flush: failed to end activity", "slug", tp.slug, "error", err)
		} else {
			slog.Info("flush: ended activity", "slug", tp.slug)
		}
		if e.store != nil {
			if err := e.store.Delete(ctx, e.provider, tp.userKey, tp.mapKey, ""); err != nil {
				slog.Warn("flush: state store delete failed", "error", err, "provider", e.provider)
			}
		}
	}
}

// Wait blocks until all in-flight ender callbacks complete.
func (e *Ender) Wait() {
	e.wg.Wait()
}

// updateWithRetry attempts an UpdateActivity call and retries once after
// enderRetryDelay on failure. Each UpdateActivity call already does up to 5
// internal retries, so worst case is:
// attempt (5 retries) → 5s wait → attempt (5 retries).
func updateWithRetry(cl *pushward.Client, slug string, req pushward.UpdateRequest, perAttemptTimeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), perAttemptTimeout)
	defer cancel()

	err := cl.UpdateActivity(ctx, slug, req)
	if err == nil {
		return nil
	}
	slog.Warn("ender update failed, retrying in 5s", "slug", slug, "error", err)

	select {
	case <-time.After(enderRetryDelay):
	case <-ctx.Done():
		return err
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), perAttemptTimeout)
	defer cancel2()
	return cl.UpdateActivity(ctx2, slug, req)
}
