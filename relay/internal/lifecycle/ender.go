package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"hash/maphash"
	"log/slog"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

const defaultRetryDelay = 5 * time.Second

// endTokenSubKey is the state row a pending two-phase end keeps next to the
// provider's own row (sub_key ""). The timers live on one replica; the token
// lets every replica see and cancel the end: StopTimer deletes it, a newer
// ScheduleEnd overwrites it, and each phase checks it is still its own.
const endTokenSubKey = "end"

// endTokenSlack bounds the token row past the two phase delays, covering the
// retries of both phases.
const endTokenSlack = 10 * time.Minute

// tokenStripes is how many locks order the end token store calls. A key always
// takes the same one, so calls for one card stay ordered, while a slow store
// call holds up only the keys that share its stripe, not every tenant.
const tokenStripes = 64

var tokenSeed = maphash.MakeSeed()

var (
	retryDelayMu      sync.RWMutex
	packageRetryDelay = defaultRetryDelay
)

// SetRetryDelay overrides the default pause between outer retry attempts for
// all subsequently created Enders. Intended for use in tests only to keep
// execution fast. Safe to call concurrently.
func SetRetryDelay(d time.Duration) {
	retryDelayMu.Lock()
	packageRetryDelay = d
	retryDelayMu.Unlock()
}

// EnderProvider is implemented by handlers that own a lifecycle.Ender.
type EnderProvider interface {
	Ender() *Ender
}

// EndConfig holds timing configuration for two-phase activity end.
type EndConfig struct {
	EndDelay       time.Duration
	EndDisplayTime time.Duration
}

// EnderOption configures an Ender at construction time.
type EnderOption func(*Ender)

// WithRetryDelay sets the pause between the two outer attempts in updateWithRetry.
// Primarily used in tests to keep execution fast.
func WithRetryDelay(d time.Duration) EnderOption {
	return func(e *Ender) { e.retryDelay = d }
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
	token   string // end token this sequence wrote; "" when the write failed
}

// Ender manages two-phase activity end scheduling.
// Phase 1 sends an ONGOING update with final content (visible on Dynamic Island).
// Phase 2 sends an ENDED update after a display delay (dismisses the Live Activity).
type Ender struct {
	clients        *client.Pool
	store          state.Store
	provider       string
	config         EndConfig
	retryDelay     time.Duration
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	mu             sync.Mutex
	tokenMu        [tokenStripes]sync.Mutex // orders this replica's end token store calls per key (tokenLock); taken before mu
	timers         map[string]*timerPair
	nextGen        uint64
	wg             sync.WaitGroup
}

// tokenLock returns the lock that orders the end token store calls for timerKey.
func (e *Ender) tokenLock(timerKey string) *sync.Mutex {
	return &e.tokenMu[maphash.String(tokenSeed, timerKey)%tokenStripes]
}

// NewEnder creates a new Ender. Pass nil for store if no state cleanup is needed.
func NewEnder(clients *client.Pool, store state.Store, provider string, cfg EndConfig, opts ...EnderOption) *Ender {
	retryDelayMu.RLock()
	rd := packageRetryDelay
	retryDelayMu.RUnlock()

	ctx, cancel := context.WithCancel(context.Background())
	e := &Ender{
		clients:        clients,
		store:          store,
		provider:       provider,
		config:         cfg,
		retryDelay:     rd,
		shutdownCtx:    ctx,
		shutdownCancel: cancel,
		timers:         make(map[string]*timerPair),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// EndIfTracked ends an activity a previous request opened, if one is tracked,
// and reports whether an end is in flight for it. Use it where this request may
// not touch the activity surface (channels=notification) but an earlier one
// already opened an activity, which would otherwise hang on the lock screen
// until its stale TTL expires.
//
// It is idempotent: calling it again while an end is already pending, on this
// replica or another, is a no-op that still reports true, so a redelivered
// webhook cannot postpone the end it asked for.
//
// Only sound where the provider writes its state row below its own
// AllowsActivity gate, so a row implies the activity exists. Where the row
// doubles as a notification-dedup marker written above the gate, this would end
// an activity the server never created. A nil store means no state at all.
func (e *Ender) EndIfTracked(ctx context.Context, log *slog.Logger, userKey, mapKey, slug string, content pushward.Content, onComplete ...func()) bool {
	if e.store == nil {
		return false
	}
	tracked, err := e.store.Exists(ctx, e.provider, userKey, mapKey, "")
	if err != nil {
		log.Warn("state store read failed", "error", err, "provider", e.provider, "slug", slug)
		return false
	}
	if !tracked {
		return false
	}
	// Deliberately not ScheduleEnd: that cancels and re-arms, so a redelivery of
	// the same terminal webhook would push the end out by another EndDelay, and
	// a sender retrying faster than that keeps the card on the lock screen
	// indefinitely. An end already in flight is the outcome this call wants, so
	// leaving it alone is the correct no-op. Where two DIFFERENT terminal events
	// land on one key the first one's content wins - for every caller of this
	// method those are mutually exclusive outcomes for one run, so there is no
	// real sequence where the later content is the one to show.
	e.scheduleEnd(userKey, mapKey, slug, content, true, onComplete...)
	return true
}

// ScheduleEnd schedules a two-phase end for an activity:
//   - Phase 1 (after EndDelay): ONGOING update with final content
//   - Phase 2 (EndDisplayTime later): ENDED with same content
//
// The optional onComplete callback runs after the activity is ended and state
// is cleaned up. It is called outside the Ender's lock.
func (e *Ender) ScheduleEnd(userKey, mapKey, slug string, content pushward.Content, onComplete ...func()) {
	e.scheduleEnd(userKey, mapKey, slug, content, false, onComplete...)
}

// scheduleEnd is ScheduleEnd with the supersede rule made explicit. With
// skipIfPending set it leaves an in-flight two-phase end alone instead of
// cancelling and re-arming it; the check and the arm share one critical section
// so concurrent callers cannot both decide nothing is pending.
func (e *Ender) scheduleEnd(userKey, mapKey, slug string, content pushward.Content, skipIfPending bool, onComplete ...func()) {
	timerKey := auth.MapKeyPrefix(userKey) + ":" + mapKey
	cl := e.clients.Get(userKey)

	// Only scheduleEnd adds timers, under the key's token lock, so the pending
	// check holds until the arm below and a skipped call never overwrites the
	// token of the end it leaves alone. The check takes in other replicas'
	// ends too: overwriting their token would cancel that end and start the
	// wait over.
	tokenMu := e.tokenLock(timerKey)
	tokenMu.Lock()
	defer tokenMu.Unlock()
	if skipIfPending && e.Ending(context.Background(), userKey, mapKey) {
		return
	}
	token := e.writeToken(userKey, mapKey)

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
		token:   token,
	}
	myGen := tp.gen
	e.wg.Add(1)
	tp.phase1 = time.AfterFunc(e.config.EndDelay, func() {
		defer e.wg.Done()
		if !e.ownsEnd(context.Background(), tp) {
			e.dropCancelled(timerKey, myGen, onComplete)
			return
		}
		// Phase 1: ONGOING with final content
		ongoingReq := pushward.UpdateRequest{
			State:   pushward.StateOngoing,
			Content: content,
		}
		if err := e.updateWithRetry(cl, slug, ongoingReq, 30*time.Second); err != nil {
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
			if !e.ownsEnd(context.Background(), tp) {
				e.dropCancelled(timerKey, myGen, onComplete)
				return
			}
			endedReq := pushward.UpdateRequest{
				State:   pushward.StateEnded,
				Content: content,
			}
			if err := e.updateWithRetry(cl, slug, endedReq, 30*time.Second); err != nil {
				slog.Error("failed to end activity (end phase 2)", "slug", slug, "error", err)
			} else {
				slog.Info("ended activity", "slug", slug, "state", content.State)
			}

			// Clean up the state store, but only while the end is still
			// OURS: a StopTimer (here or on another replica) followed by a
			// new alert has written a row this sequence no longer owns.
			// Detached from any request ctx on purpose: the phase-2 timer
			// fires EndDisplayTime after phase-1 completes, which is well
			// beyond any sensible request/trace lifetime.
			if e.store != nil {
				tokenMu.Lock()
				e.mu.Lock()
				cur, ok := e.timers[timerKey]
				mine := ok && cur.gen == myGen
				e.mu.Unlock()
				if mine && e.ownsEnd(context.Background(), tp) {
					delCtx, delCancel := context.WithTimeout(context.Background(), 5*time.Second)
					if err := e.store.Delete(delCtx, e.provider, userKey, mapKey, ""); err != nil {
						slog.Warn("state store delete failed", "error", err, "provider", e.provider)
					}
					e.deleteToken(delCtx, userKey, mapKey)
					delCancel()
				}
				tokenMu.Unlock()
			}

			// Clean up timer, but only if it is still OURS. A superseding
			// ScheduleEnd for the same key may have installed a newer timerPair
			// while this phase-2 was running its ENDED update; deleting
			// unconditionally would clobber that newer pair, leaving the newer
			// sequence's phase-1 unable to arm phase-2 (Live Activity stuck).
			e.mu.Lock()
			if cur, ok := e.timers[timerKey]; ok && cur.gen == myGen {
				delete(e.timers, timerKey)
			}
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

// writeToken stores a fresh end token for the key and returns it, or "" when
// there is no store or the write failed: a sequence without a token never
// checks one, so a DB blip cannot leave a card stuck ONGOING.
func (e *Ender) writeToken(userKey, mapKey string) string {
	if e.store == nil {
		return ""
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	data, _ := json.Marshal(token)
	ttl := e.config.EndDelay + e.config.EndDisplayTime + endTokenSlack
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.store.Set(ctx, e.provider, userKey, mapKey, endTokenSubKey, data, ttl); err != nil {
		slog.Warn("end token write failed", "error", err, "provider", e.provider)
		return ""
	}
	return token
}

// ownsEnd reports whether the store still holds tp's end token. It fails open:
// with no token or on a read error the sequence goes ahead as it always did.
func (e *Ender) ownsEnd(ctx context.Context, tp *timerPair) bool {
	if e.store == nil || tp.token == "" {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := e.store.Get(ctx, e.provider, tp.userKey, tp.mapKey, endTokenSubKey)
	if err != nil {
		slog.Warn("end token read failed, ending anyway", "error", err, "provider", e.provider, "slug", tp.slug)
		return true
	}
	var cur string
	if raw == nil || json.Unmarshal(raw, &cur) != nil || cur != tp.token {
		slog.Info("end cancelled or superseded elsewhere", "provider", e.provider, "slug", tp.slug)
		return false
	}
	return true
}

func (e *Ender) deleteToken(ctx context.Context, userKey, mapKey string) {
	if err := e.store.Delete(ctx, e.provider, userKey, mapKey, endTokenSubKey); err != nil {
		slog.Warn("end token delete failed", "error", err, "provider", e.provider)
	}
}

// dropCancelled forgets a sequence whose token another replica took away.
// The activity and its row now belong to whoever did that, so only the local
// timer entry and the local onComplete cleanup run.
func (e *Ender) dropCancelled(timerKey string, gen uint64, onComplete []func()) {
	e.mu.Lock()
	if cur, ok := e.timers[timerKey]; ok && cur.gen == gen {
		delete(e.timers, timerKey)
	}
	e.mu.Unlock()
	for _, fn := range onComplete {
		fn()
	}
}

// Pending reports whether an end is scheduled for the given key and has not
// finished. It holds only on this replica: another one may be ending the
// same activity (Ending sees those too).
func (e *Ender) Pending(userKey, mapKey string) bool {
	timerKey := auth.MapKeyPrefix(userKey) + ":" + mapKey
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.timers[timerKey]
	return ok
}

// Ending reports whether an end is in flight for the key on this replica or,
// through the end token, on another one. A failed token read counts only this
// replica, as Pending does.
func (e *Ender) Ending(ctx context.Context, userKey, mapKey string) bool {
	if e.Pending(userKey, mapKey) {
		return true
	}
	if e.store == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := e.store.Get(ctx, e.provider, userKey, mapKey, endTokenSubKey)
	if err != nil {
		slog.Warn("end token read failed", "error", err, "provider", e.provider)
		return false
	}
	return raw != nil
}

// StopTimer cancels a pending end for the given key, on this replica and,
// through the end token, on any other. It reports whether an end was pending
// anywhere, so an alert arriving inside the end window can treat the card as
// new. A store error counts only the local timer.
func (e *Ender) StopTimer(userKey, mapKey string) bool {
	timerKey := auth.MapKeyPrefix(userKey) + ":" + mapKey
	tokenMu := e.tokenLock(timerKey)
	tokenMu.Lock()
	defer tokenMu.Unlock()
	e.mu.Lock()
	tp, pending := e.timers[timerKey]
	if pending {
		if tp.phase1.Stop() {
			e.wg.Done()
		}
		if tp.phase2 != nil && tp.phase2.Stop() {
			e.wg.Done()
		}
		delete(e.timers, timerKey)
	}
	e.mu.Unlock()

	if e.store == nil {
		return pending
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := e.store.Get(ctx, e.provider, userKey, mapKey, endTokenSubKey)
	if err != nil {
		slog.Warn("end token read failed", "error", err, "provider", e.provider)
		return pending
	}
	if raw != nil {
		e.deleteToken(ctx, userKey, mapKey)
	}
	return pending || raw != nil
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
// update for each, within ctx. Use this during graceful shutdown instead of
// StopAll to ensure activities are properly ended before the process exits.
func (e *Ender) FlushAll(ctx context.Context) {
	e.shutdownCancel() // signal in-flight retry sleeps to bail early

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

	for i, tp := range pending {
		if ctx.Err() != nil {
			slog.Warn("flush timed out, dropping remaining enders", "remaining", len(pending)-i)
			break
		}
		if !e.ownsEnd(ctx, tp) {
			continue // another replica cancelled or took over this end
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
			if tp.token != "" {
				e.deleteToken(ctx, tp.userKey, tp.mapKey)
			}
		}
	}
}

// Wait blocks until all in-flight ender callbacks complete.
func (e *Ender) Wait() {
	e.wg.Wait()
}

// updateWithRetry attempts an UpdateActivity call and retries once after
// e.retryDelay on failure. Each UpdateActivity call already does up to 5
// internal retries, so worst case is:
// attempt (5 retries) -> retryDelay wait -> attempt (5 retries).
// The retry sleep is interruptible via the ender's shutdown context.
func (e *Ender) updateWithRetry(cl *pushward.Client, slug string, req pushward.UpdateRequest, perAttemptTimeout time.Duration) error {
	// Derive per-attempt contexts from shutdownCtx so FlushAll's shutdownCancel
	// aborts any in-flight UpdateActivity immediately - otherwise an attempt
	// against an unresponsive server could hold the WaitGroup for the full
	// per-attempt timeout (up to ~65s across retry), overrunning the k8s
	// termination grace period. In normal operation shutdownCtx is live, so
	// behavior is unchanged.
	ctx1, cancel1 := context.WithTimeout(e.shutdownCtx, perAttemptTimeout)
	err := cl.UpdateActivity(ctx1, slug, req)
	cancel1()
	if err == nil {
		return nil
	}
	slog.Warn("ender update failed, retrying", "slug", slug, "delay", e.retryDelay, "error", err)

	timer := time.NewTimer(e.retryDelay)
	select {
	case <-timer.C:
	case <-e.shutdownCtx.Done():
		timer.Stop()
		return err
	}

	ctx2, cancel2 := context.WithTimeout(e.shutdownCtx, perAttemptTimeout)
	defer cancel2()
	return cl.UpdateActivity(ctx2, slug, req)
}
