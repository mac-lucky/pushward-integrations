package poller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/grafana/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// UpdateCallback is invoked after each successful poll with the values sent.
// Used by the handler to track the last series keys and values per slug so
// they can be reused on alert resolve (keeping keys stable prevents the
// server's AccumulateHistory from pruning prior series).
type UpdateCallback func(slug string, values map[string]float64)

// Poller manages per-alert polling goroutines that query Prometheus/VM
// and push timeline updates to PushWard.
type Poller struct {
	metricsClient *metrics.Client
	pwClient      *pushward.Client
	interval      time.Duration

	mu       sync.Mutex
	active   map[string]context.CancelFunc
	callback UpdateCallback
	wg       sync.WaitGroup
}

// New creates a new Poller.
func New(metricsClient *metrics.Client, pwClient *pushward.Client, interval time.Duration) *Poller {
	return &Poller{
		metricsClient: metricsClient,
		pwClient:      pwClient,
		interval:      interval,
		active:        make(map[string]context.CancelFunc),
	}
}

// Start begins polling for the given slug and PromQL expression.
// seriesLabel is the preferred metric label to use as series key (can be empty for auto-detect).
// No-op if already polling for this slug. Use Start when the firing webhook has
// already seeded the activity's timeline template/styling.
func (p *Poller) Start(slug, expr, seriesLabel string) {
	p.StartWithSeed(slug, expr, seriesLabel, nil)
}

// StartWithSeed is like Start but, when seed is non-nil, the poller sends a full
// UpdateActivity establishing the timeline template/styling on its first
// successful tick (then value-only patches thereafter). Used when the firing
// webhook had no values to seed with, so the activity would otherwise be left on
// the generic template while ONGOING.
func (p *Poller) StartWithSeed(slug, expr, seriesLabel string, seed *pushward.Content) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.active[slug]; ok {
		return
	}

	ctx, cancel := context.WithCancel(context.Background()) // #nosec G118 -- cancel is stored in p.active and called in Stop/StopAll
	p.active[slug] = cancel
	p.wg.Add(1)
	go p.run(ctx, slug, expr, seriesLabel, seed)
}

// Stop cancels the polling goroutine for the given slug.
func (p *Poller) Stop(slug string) {
	p.mu.Lock()
	cancel, ok := p.active[slug]
	if ok {
		delete(p.active, slug)
	}
	p.mu.Unlock()

	if ok {
		cancel()
	}
}

// Wait blocks until all polling goroutines have exited.
func (p *Poller) Wait() {
	p.wg.Wait()
}

// StopAll cancels all active polling goroutines.
func (p *Poller) StopAll() {
	p.mu.Lock()
	for slug, cancel := range p.active {
		cancel()
		delete(p.active, slug)
	}
	p.mu.Unlock()
}

// ActiveCount returns the number of active polling goroutines.
func (p *Poller) ActiveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.active)
}

// SetUpdateCallback registers a callback invoked after each successful poll.
// Safe to call concurrently with active polls; the callback is read under
// p.mu on each tick.
func (p *Poller) SetUpdateCallback(cb UpdateCallback) {
	p.mu.Lock()
	p.callback = cb
	p.mu.Unlock()
}

func (p *Poller) run(ctx context.Context, slug, expr, seriesLabel string, seed *pushward.Content) {
	defer p.wg.Done()

	logger := slog.With("slug", slug)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	// seeded is false only when the firing webhook couldn't seed the activity
	// (no values); the first successful tick then sends the full timeline seed.
	seeded := seed == nil

	for {
		select {
		case <-ctx.Done():
			logger.Info("poller stopped")
			return
		case <-ticker.C:
			seeded = p.poll(ctx, logger, slug, expr, seriesLabel, seed, seeded)
		}
	}
}

// poll queries the metric and pushes an update, returning the (possibly updated)
// seeded state.
func (p *Poller) poll(ctx context.Context, logger *slog.Logger, slug, expr, seriesLabel string, seed *pushward.Content, seeded bool) bool {
	points, err := p.metricsClient.QueryInstantAll(ctx, expr, time.Now())
	if err != nil {
		if ctx.Err() != nil {
			return seeded
		}
		logger.Warn("poll failed", "error", err)
		return seeded
	}

	if len(points) == 0 {
		return seeded
	}

	values := make(map[string]float64, len(points))
	for _, lp := range points {
		key := metrics.SeriesKey(lp.Labels, seriesLabel)
		values[key] = lp.Point.Value
	}

	if !seeded {
		// The firing webhook had no values, so establish the timeline
		// template/styling now via a full UpdateActivity before switching to
		// value-only patches. Leave seeded=false on failure so we retry.
		content := *seed
		content.Value = values
		if err := p.pwClient.UpdateActivity(ctx, slug, pushward.UpdateRequest{
			State:   pushward.StateOngoing,
			Content: content,
		}); err != nil {
			if ctx.Err() != nil {
				return seeded
			}
			logger.Warn("poll seed update failed", "error", err)
			return seeded
		}
		logger.Info("activity seeded by poller on first values")
		seeded = true
	} else {
		// Merge-patch with just the new sample. Template/units/accent/display
		// config were seeded already and are preserved server-side.
		if err := p.pwClient.PatchActivity(ctx, slug, pushward.PatchRequest{
			State:   pushward.StateOngoing,
			Content: &pushward.ContentPatch{Value: values},
		}); err != nil {
			if ctx.Err() != nil {
				return seeded
			}
			logger.Warn("poll update failed", "error", err)
			return seeded
		}
	}

	p.mu.Lock()
	cb := p.callback
	p.mu.Unlock()
	if cb != nil {
		cb(slug, values)
	}
	return seeded
}
