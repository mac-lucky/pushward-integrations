package poller

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/backrest/internal/backrest"
	"github.com/mac-lucky/pushward-integrations/backrest/internal/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/syncx"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// Client is the slice of the Backrest API this poller uses. Declared here
// rather than taking the concrete type so tests can drive the poller from a
// fake without standing up an HTTP server.
type Client interface {
	GetOperations(ctx context.Context, lastN int64) ([]backrest.Operation, error)
	GetLogs(ctx context.Context, ref string) (string, error)
}

const (
	// progressChangeFrac is how far the bar must move before a tick is worth a
	// push on its own.
	progressChangeFrac = 0.02

	// heartbeatFloor bounds how often the keep-alive can fire. The interval
	// itself is derived from stale_timeout (see Poller.heartbeat); the floor
	// only stops a very short stale_timeout turning the poll loop into a push
	// loop.
	heartbeatFloor = 30 * time.Second

	// logRefreshInterval is how often a running prune or check re-reads its
	// command output. The log only feeds a 20-line tail, so re-fetching it on
	// every 5s tick would be a lot of transfer for a view that barely changes.
	logRefreshInterval = 15 * time.Second

	// speedAlpha weights the newest sample in the transfer-rate average. Low
	// enough that one slow tick does not throw the ETA, high enough that a real
	// slowdown reaches it within a few polls.
	speedAlpha = 0.3

	// minLiveWindow is the shortest ETA worth animating. Below it the bar would
	// finish filling before the next poll and the countdown would read as a
	// glitch.
	minLiveWindow = 5 * time.Second

	// maxLiveWindow caps the anchor. A backup that stalls early produces an
	// arbitrarily large estimate, and an end date days out renders as a bar
	// that never visibly moves - worse than no animation.
	maxLiveWindow = 12 * time.Hour

	// reanchorFloor and reanchorFrac decide when a new estimate differs enough
	// to be worth re-sending. Without them the anchor moves on every tick and
	// iOS restarts its animation each time, which reads as a stutter.
	reanchorFloor = 15 * time.Second
	reanchorFrac  = 0.2

	// endPushTimeout bounds each of the two closing frames. They fire from
	// timers after the poll context is gone, so they carry their own deadline
	// rather than inheriting one.
	endPushTimeout = 30 * time.Second

	// maxSendFailures is how many consecutive rejections an operation gets
	// before the bridge stops trying.
	//
	// Without it a terminal operation whose activity the server refuses - a
	// revoked key, a plan limit - is never closed out, so nothing ever removes
	// its entry and interval() keeps the poll loop at its fast rate. It retries
	// every tick for as long as the row stays in the query window, which on a
	// nightly-backup instance is over a week.
	maxSendFailures = 5
)

// tracked is the poller's memory of one operation it has an open activity for.
//
// Its fields belong to the poll goroutine. p.mu guards p.tracked and p.done,
// which the end timers also touch; `ending` is read under it only because it is
// read while walking those maps.
type tracked struct {
	opID     int64
	slug     string
	subtitle string
	seeded   bool

	// Push throttle. lastPhase is the coarse step, not the rendered state line:
	// that line carries a byte count and a transfer rate which move on nearly
	// every tick, so comparing it would push on nearly every tick too.
	lastProgress float64
	lastPhase    string
	lastTemplate string
	lastPushAt   time.Time

	// Transfer-rate estimate.
	lastBytes    int64
	lastSampleAt time.Time
	speed        float64

	// Live-progress anchor: the unix window the bar is animating across, zero
	// when no anchor has been sent. liveStart is republished unchanged on every
	// push that keeps the anchor, because restamping it to now would restart
	// the client-side animation and undo the drift suppression in anchor().
	liveStart int64
	liveEnd   int64

	// Cached command-output tail for a running prune or check.
	logLines    []pushward.LogLine
	lastLogPoll time.Time

	// failures counts consecutive rejected sends, so an activity the server
	// will never accept is eventually abandoned instead of retried forever.
	failures int

	ending    bool
	endTimers *syncx.TimerGroup
}

type Poller struct {
	cfg *config.Config
	br  Client
	pw  *pushward.Client

	mu      sync.Mutex
	tracked map[int64]*tracked
	// done holds operations already closed out, so a terminal row that stays in
	// the query window is not turned into a second activity on the next tick.
	done map[int64]bool
	// primed is false until the first poll has established what was already
	// finished when the bridge started.
	primed bool

	// now is time.Now except in tests.
	now func() time.Time
}

func New(cfg *config.Config, br Client, pw *pushward.Client) *Poller {
	return &Poller{
		cfg:     cfg,
		br:      br,
		pw:      pw,
		tracked: make(map[int64]*tracked),
		done:    make(map[int64]bool),
		now:     time.Now,
	}
}

// Run polls until ctx is cancelled, then drains any end timers still in flight.
func (p *Poller) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			p.shutdown()
			return ctx.Err()
		case <-timer.C:
		}

		if err := p.poll(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("poll failed", "error", err)
		}
		timer.Reset(p.interval())
	}
}

// heartbeat is how long the bridge may stay silent before re-sending an
// unchanged frame.
//
// It is half of stale_timeout rather than a fixed interval, because the only
// job of the keep-alive is to beat the server's stale-dismissal clock. Pinning
// it at 30s instead would spend ~720 pushes on a six-hour backup parked on one
// frame where 24 do the same work, and would silently stop protecting the
// activity for anyone who configures a stale_timeout below 30s.
func (p *Poller) heartbeat() time.Duration {
	if h := p.cfg.PushWard.StaleTimeout / 2; h > heartbeatFloor {
		return h
	}
	return heartbeatFloor
}

// interval polls fast while something is running and slowly otherwise. An idle
// Backrest is the normal state; there is nothing to watch between nightly runs.
func (p *Poller) interval() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.tracked {
		if !t.ending {
			return p.cfg.Polling.Interval
		}
	}
	return p.cfg.Polling.IdleInterval
}

// shutdown stops the end timers re-arming and waits for the phases already
// running, so a final frame in flight is not cut off.
func (p *Poller) shutdown() {
	p.mu.Lock()
	groups := make([]*syncx.TimerGroup, 0, len(p.tracked))
	for _, t := range p.tracked {
		if t.endTimers != nil {
			groups = append(groups, t.endTimers)
		}
	}
	p.mu.Unlock()

	for _, g := range groups {
		g.Close()
	}
	for _, g := range groups {
		g.Wait()
	}
}

func (p *Poller) poll(ctx context.Context) error {
	ops, err := p.br.GetOperations(ctx, p.cfg.Polling.LastN)
	if err != nil {
		return err
	}

	// The first poll returns everything Backrest has done recently, all of it
	// finished. Announcing those would push a wall of activities for backups
	// that ran days ago, so the first pass only records them.
	p.mu.Lock()
	priming := !p.primed
	p.primed = true
	p.mu.Unlock()

	seen := make(map[int64]bool, len(ops))
	for i := range ops {
		op := &ops[i]
		if op.Kind() == backrest.KindOther {
			continue
		}
		id := op.ID.Int64()
		seen[id] = true

		switch {
		case priming:
			// A backup already running at startup is picked up normally on the
			// next tick; only the finished ones need suppressing.
			if op.Terminal() {
				p.markDone(id)
			}
		case op.Running():
			p.handleRunning(ctx, op)
		case op.Terminal():
			p.handleTerminal(ctx, op)
		}
	}

	p.reapVanished(ctx, seen)
	p.pruneDone(seen)
	return nil
}

func (p *Poller) markDone(id int64) {
	p.mu.Lock()
	p.done[id] = true
	p.mu.Unlock()
}

func (p *Poller) isDone(id int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done[id]
}

// pruneDone drops bookkeeping for operations that have scrolled out of the
// query window. They cannot come back, so keeping them would be a slow leak on
// a process meant to run for months.
func (p *Poller) pruneDone(seen map[int64]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.done {
		if !seen[id] {
			delete(p.done, id)
		}
	}
}

// reapVanished closes activities whose operation left the query window without
// ever being seen finished. Nothing more is coming for them, and leaving them
// open would show a backup running until the server's stale timeout expires.
func (p *Poller) reapVanished(ctx context.Context, seen map[int64]bool) {
	p.mu.Lock()
	var orphans []*tracked
	for id, t := range p.tracked {
		if !seen[id] && !t.ending {
			orphans = append(orphans, t)
		}
	}
	p.mu.Unlock()

	for _, t := range orphans {
		slog.Warn("operation left the query window, closing activity", "slug", t.slug, "op", t.opID)
		p.scheduleEnd(ctx, t, orphanContent(t.subtitle, t.lastProgress))
	}
}

// track returns the record for op, creating it on first sight. nil means the
// operation is already closed out, or its two-phase end is under way, and must
// not be reopened behind its own ENDED frame.
func (p *Poller) track(op *backrest.Operation) *tracked {
	id := op.ID.Int64()
	if p.isDone(id) {
		return nil
	}

	p.mu.Lock()
	t, ok := p.tracked[id]
	if !ok {
		t = &tracked{opID: id, slug: slugFor(op), subtitle: subtitle(op)}
		p.tracked[id] = t
	}
	p.mu.Unlock()

	// done[] is set alongside ending under one lock, so isDone above has
	// already covered this. Kept because the two are separate fields and the
	// read is free.
	if t.ending {
		return nil
	}
	return t
}

// push seeds or patches the activity, recording the frame only once it has
// landed so a failed send is simply re-evaluated on the next tick.
func (p *Poller) push(ctx context.Context, t *tracked, op *backrest.Operation, content pushward.Content, phase string, now time.Time) error {
	var err error
	if !t.seeded {
		err = p.create(ctx, t, op, content)
	} else {
		err = p.patch(ctx, t, content)
	}
	if err != nil {
		t.failures++
		if t.failures >= maxSendFailures {
			slog.Error("giving up on activity after repeated rejections",
				"slug", t.slug, "op", t.opID, "attempts", t.failures)
			p.abandon(t)
		}
		return err
	}
	t.failures = 0
	t.markPushed(content, phase, now)
	return nil
}

// abandon drops an operation the server will not accept.
//
// It is marked done rather than merely deleted so a terminal row still sitting
// in the query window is not picked straight back up on the next tick, and so
// interval() stops treating it as live work holding the fast poll rate.
func (p *Poller) abandon(t *tracked) {
	p.mu.Lock()
	defer p.mu.Unlock()
	t.ending = true
	p.done[t.opID] = true
	delete(p.tracked, t.opID)
}

// handleRunning creates the activity on first sight and pushes an update when
// the tick has something new to say.
func (p *Poller) handleRunning(ctx context.Context, op *backrest.Operation) {
	t := p.track(op)
	if t == nil {
		return
	}

	now := p.now()
	content, phase := p.frameFor(ctx, t, op, now)

	// The throttle applies only once there is a frame to compare against.
	if t.seeded && !t.shouldPush(content, phase, now, p.heartbeat()) {
		return
	}
	_ = p.push(ctx, t, op, content, phase, now)
}

// frameFor builds the frame for an operation still in flight: the rate sample,
// the live-progress anchor and the log tail are all poller state, so they are
// resolved here and handed to the renderer.
func (p *Poller) frameFor(ctx context.Context, t *tracked, op *backrest.Operation, now time.Time) (pushward.Content, string) {
	if op.Kind() != backrest.KindBackup {
		c := repoTaskContent(op, p.cachedLogLines(ctx, t, op, now))
		// Prune and check render a fixed state line, so it is a usable phase.
		return c, c.State
	}

	t.sample(op.BytesDone(), now)

	liveStart, liveEnd := t.liveStart, t.liveEnd
	switch {
	case !p.cfg.Render.LiveProgress:
		liveStart, liveEnd = 0, 0
	default:
		if end, ok := t.anchor(op, now); ok {
			liveStart, liveEnd = now.Unix(), end
		}
	}
	return runningContent(op, t.speed, liveStart, liveEnd)
}

// handleTerminal sends the completion frame and schedules the two-phase end.
func (p *Poller) handleTerminal(ctx context.Context, op *backrest.Operation) {
	t := p.track(op)
	if t == nil {
		return
	}

	now := p.now()
	lines := errorLines(op)
	if len(lines) == 0 {
		// Straight to the fetch, not the cache: on the final frame the last
		// lines of the log are the whole point.
		lines = p.fetchLogLines(ctx, t, op, now)
	}
	content := endContent(op, lines)

	if err := p.push(ctx, t, op, content, content.State, now); err != nil {
		return
	}

	slog.Info("operation finished", "slug", t.slug, "op", t.opID, "status", op.Status, "state", content.State)
	p.scheduleEnd(ctx, t, content)
}

// cachedLogLines returns the command-output tail, re-reading it only once the
// refresh interval has passed.
//
// A running prune is polled every few seconds while its log grows far more
// slowly, so re-fetching every tick would be a lot of transfer for a 20-line
// view that barely changes.
func (p *Poller) cachedLogLines(ctx context.Context, t *tracked, op *backrest.Operation, now time.Time) []pushward.LogLine {
	if !t.lastLogPoll.IsZero() && now.Sub(t.lastLogPoll) < logRefreshInterval {
		return t.logLines
	}
	return p.fetchLogLines(ctx, t, op, now)
}

// fetchLogLines reads the command output of a prune or check. Backups have no
// output log of their own: their task log is restic's raw JSON status stream,
// which is not something to put in front of a person.
//
// On failure the previous tail is kept rather than blanking the view, since a
// stale log is more use than none.
func (p *Poller) fetchLogLines(ctx context.Context, t *tracked, op *backrest.Operation, now time.Time) []pushward.LogLine {
	if !p.cfg.Render.Logs {
		return nil
	}
	ref := op.OutputLogref()
	if ref == "" {
		return nil
	}

	out, err := p.br.GetLogs(ctx, ref)
	if err != nil {
		slog.Warn("failed to fetch task log", "ref", ref, "error", err)
		return t.logLines
	}
	t.lastLogPoll = now
	t.logLines = outputLines(out)
	return t.logLines
}

func (p *Poller) create(ctx context.Context, t *tracked, op *backrest.Operation, content pushward.Content) error {
	endedTTL := int(p.cfg.PushWard.CleanupDelay.Seconds())
	staleTTL := int(p.cfg.PushWard.StaleTimeout.Seconds())

	if err := p.pw.CreateActivity(ctx, t.slug, activityName(op), p.cfg.PushWard.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "slug", t.slug, "error", err)
		return err
	}
	req := pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}
	if err := p.pw.UpdateActivity(ctx, t.slug, req); err != nil {
		slog.Error("failed to seed activity", "slug", t.slug, "error", err)
		return err
	}
	t.seeded = true
	slog.Info("tracking operation", "slug", t.slug, "op", t.opID, "kind", op.Kind())
	return nil
}

func (p *Poller) patch(ctx context.Context, t *tracked, content pushward.Content) error {
	req := pushward.PatchRequest{Content: contentPatch(content)}
	if err := p.pw.PatchActivity(ctx, t.slug, req); err != nil {
		slog.Error("failed to patch activity", "slug", t.slug, "error", err)
		return err
	}
	return nil
}

// contentPatch converts a full frame into merge-patch form.
//
// LiveProgress is carried through even when false, and that is the point: under
// RFC 7396 an omitted field keeps its stored value, so a completion frame that
// simply left it out would inherit live_progress:true from the last running
// tick and iOS would keep animating a bar that has stopped moving.
//
// StartDate and EndDate are deliberately not cleared alongside it. They are nil
// on a terminal frame, so the stored anchor survives - which is harmless once
// live_progress is false, since nothing reads the dates then, and it saves
// sending two null fields on every closing push.
func contentPatch(c pushward.Content) *pushward.ContentPatch {
	return &pushward.ContentPatch{
		Template:     pushward.StringPtr(c.Template),
		Progress:     pushward.Float64Ptr(c.Progress),
		State:        pushward.StringPtr(c.State),
		Icon:         pushward.StringPtr(c.Icon),
		Subtitle:     pushward.StringPtr(c.Subtitle),
		AccentColor:  pushward.StringPtr(c.AccentColor),
		LiveProgress: c.LiveProgress,
		StartDate:    c.StartDate,
		EndDate:      c.EndDate,
		Lines:        c.Lines,
	}
}

// scheduleEnd runs the two-phase close: a final ONGOING frame so the outcome
// shows on the Dynamic Island, then ENDED once it has been seen.
func (p *Poller) scheduleEnd(ctx context.Context, t *tracked, content pushward.Content) {
	p.mu.Lock()
	if t.ending {
		p.mu.Unlock()
		return
	}
	t.ending = true
	// Marked done here rather than in phase 2: the operation stays in the query
	// window throughout both phases, and every tick in between would otherwise
	// see an untracked terminal row and open the activity again.
	p.done[t.opID] = true
	tg := &syncx.TimerGroup{}
	t.endTimers = tg
	slug := t.slug
	opID := t.opID
	p.mu.Unlock()

	// Detached from the poll context so shutdown does not cancel a delivery
	// already under way; the timer group is what bounds this instead.
	detached := context.WithoutCancel(ctx)

	tg.Reset(p.cfg.PushWard.EndDelay, func() {
		ctx1, cancel1 := context.WithTimeout(detached, endPushTimeout)
		defer cancel1()
		req := pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}
		if err := p.pw.UpdateActivity(ctx1, slug, req); err != nil {
			slog.Error("failed to update activity (end phase 1)", "slug", slug, "error", err)
		}

		tg.Reset(p.cfg.PushWard.EndDisplayTime, func() {
			ctx2, cancel2 := context.WithTimeout(detached, endPushTimeout)
			defer cancel2()
			endReq := pushward.UpdateRequest{State: pushward.StateEnded, Content: content}
			if err := p.pw.UpdateActivity(ctx2, slug, endReq); err != nil {
				slog.Error("failed to end activity (end phase 2)", "slug", slug, "error", err)
			} else {
				slog.Info("ended activity", "slug", slug, "op", opID)
			}
			p.mu.Lock()
			delete(p.tracked, opID)
			p.mu.Unlock()
		})
	})
}

// slugFor identifies the activity.
//
// The hash input is deliberately not the relay provider's ("<plan><repo>", no
// separator): both can run against the same PushWard account, and a shared slug
// would have the two overwriting each other's frames. Kind is in the input too,
// so a prune that starts while a backup is still finishing gets its own
// activity rather than replacing it.
func slugFor(op *backrest.Operation) string {
	return text.SlugHash("backrest", op.PlanID+"/"+op.RepoID+"/"+string(op.Kind()), 4)
}

// sample folds a new byte count into the transfer-rate average.
func (t *tracked) sample(bytesDone int64, now time.Time) {
	if !t.lastSampleAt.IsZero() {
		dt := now.Sub(t.lastSampleAt).Seconds()
		// A counter that did not advance is a real observation - the backup has
		// stalled - and folding in its zero is what makes the ETA grow. Only
		// impossible samples are dropped.
		if dt > 0 && bytesDone >= t.lastBytes {
			inst := float64(bytesDone-t.lastBytes) / dt
			if t.speed == 0 {
				t.speed = inst
			} else {
				t.speed = speedAlpha*inst + (1-speedAlpha)*t.speed
			}
		}
	}
	t.lastBytes = bytesDone
	t.lastSampleAt = now
}

// anchor returns the unix second the bar should animate toward, and whether it
// moved enough to be worth re-sending.
//
// Re-sending an anchor restarts the client-side animation, so an estimate that
// wobbles by a second either way must never reach the payload.
func (t *tracked) anchor(op *backrest.Operation, now time.Time) (int64, bool) {
	// speed is only ever assigned from a second-or-later sample, so a positive
	// rate already implies the two observations an estimate needs.
	if t.speed <= 0 {
		return 0, false
	}
	remaining, ok := op.RemainingBytes()
	if !ok || remaining <= 0 {
		return 0, false
	}

	eta := time.Duration(float64(remaining) / t.speed * float64(time.Second))
	if eta < minLiveWindow || eta > maxLiveWindow {
		return 0, false
	}
	end := now.Add(eta).Unix()

	if t.liveEnd == 0 {
		return end, true
	}
	current := t.liveEnd - now.Unix()
	if current <= 0 {
		// The previous anchor ran out while the backup is still going, so the
		// bar is sitting full and lying about it. Re-anchor regardless of drift.
		return end, true
	}
	drift := math.Abs(eta.Seconds() - float64(current))
	threshold := math.Max(reanchorFloor.Seconds(), float64(current)*reanchorFrac)
	if drift <= threshold {
		return t.liveEnd, false
	}
	return end, true
}

// shouldPush decides whether this tick has anything new to say. Without it a
// multi-hour backup would spend thousands of pushes redrawing one frame.
func (t *tracked) shouldPush(c pushward.Content, phase string, now time.Time, heartbeat time.Duration) bool {
	if phase != t.lastPhase {
		return true
	}
	if c.Template != t.lastTemplate {
		return true
	}
	if math.Abs(c.Progress-t.lastProgress) >= progressChangeFrac {
		return true
	}
	if c.EndDate != nil && *c.EndDate != t.liveEnd {
		return true
	}
	// The heartbeat is what stops the server ending the activity as stale
	// during a long, quiet stretch.
	return now.Sub(t.lastPushAt) >= heartbeat
}

func (t *tracked) markPushed(c pushward.Content, phase string, now time.Time) {
	t.lastPhase = phase
	t.lastTemplate = c.Template
	t.lastProgress = c.Progress
	t.lastPushAt = now
	if c.EndDate != nil {
		t.liveEnd = *c.EndDate
		if c.StartDate != nil {
			t.liveStart = *c.StartDate
		}
	} else {
		t.liveStart, t.liveEnd = 0, 0
	}
}
