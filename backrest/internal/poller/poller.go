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

	// heartbeatInterval bounds how long the bridge can stay silent. The server
	// ends an activity that goes quiet for stale_timeout, so a backup sitting
	// on one unchanging frame still has to say it is alive.
	heartbeatInterval = 30 * time.Second

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
)

// tracked is the poller's memory of one operation it has an open activity for.
// Its fields belong to the poll goroutine; the mutex guards the maps, which the
// end timers also touch.
type tracked struct {
	opID   int64
	slug   string
	kind   backrest.Kind
	seeded bool

	// Push throttle.
	lastProgress float64
	lastState    string
	lastPushAt   time.Time

	// Transfer-rate estimate.
	lastBytes    int64
	lastSampleAt time.Time
	speed        float64
	samples      int

	// Live-progress anchor: the unix second the bar is animating toward, and
	// whether the client was told to animate at all.
	liveEnd  int64
	liveSent bool

	// Cached command-output tail for a running prune or check.
	logLines    []pushward.LogLine
	lastLogPoll time.Time

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
		content := pushward.Content{
			Template:     pushward.TemplateGeneric,
			Progress:     t.lastProgress,
			State:        stateFailed,
			Icon:         iconWarn,
			AccentColor:  pushward.ColorOrange,
			LiveProgress: pushward.BoolPtr(false),
		}
		p.scheduleEnd(ctx, t, content)
	}
}

// handleRunning creates the activity on first sight and pushes an update when
// the tick has something new to say.
func (p *Poller) handleRunning(ctx context.Context, op *backrest.Operation) {
	id := op.ID.Int64()
	if p.isDone(id) {
		// A late in-progress row for an operation already closed out must not
		// reopen the activity behind its own ENDED frame.
		return
	}

	p.mu.Lock()
	t, ok := p.tracked[id]
	if !ok {
		t = &tracked{opID: id, slug: slugFor(op), kind: op.Kind()}
		p.tracked[id] = t
	}
	p.mu.Unlock()

	if t.ending {
		return
	}

	now := p.now()
	content := p.runningContent(ctx, t, op, now)

	if !t.seeded {
		if err := p.create(ctx, t, op, content); err != nil {
			return
		}
		t.markPushed(content, now)
		return
	}
	if !t.shouldPush(content, now) {
		return
	}
	if err := p.patch(ctx, t, content); err != nil {
		return
	}
	t.markPushed(content, now)
}

// runningContent builds the frame for an operation still in flight.
func (p *Poller) runningContent(ctx context.Context, t *tracked, op *backrest.Operation, now time.Time) pushward.Content {
	if op.Kind() != backrest.KindBackup {
		return repoTaskContent(op, p.logLines(ctx, t, op, now, false))
	}

	if st := op.BackupStatus(); st != nil {
		t.sample(st.BytesDone.Int64(), now)
	}

	liveEnd := int64(0)
	if p.cfg.Render.LiveProgress {
		if end, ok := t.anchor(op, now); ok {
			liveEnd = end
		} else {
			liveEnd = t.liveEnd
		}
	}
	return runningContent(op, t.speed, liveEnd, now)
}

// handleTerminal sends the completion frame and schedules the two-phase end.
func (p *Poller) handleTerminal(ctx context.Context, op *backrest.Operation) {
	id := op.ID.Int64()
	if p.isDone(id) {
		return
	}

	p.mu.Lock()
	t, ok := p.tracked[id]
	if !ok {
		// The operation started and finished inside one poll interval. Its
		// outcome is still the part worth showing, so it gets an activity that
		// opens straight onto the result.
		t = &tracked{opID: id, slug: slugFor(op), kind: op.Kind()}
		p.tracked[id] = t
	}
	p.mu.Unlock()

	if t.ending {
		return
	}

	lines := errorLines(op)
	if len(lines) == 0 {
		lines = p.logLines(ctx, t, op, p.now(), true)
	}
	content := endContent(op, lines)

	if !t.seeded {
		if err := p.create(ctx, t, op, content); err != nil {
			return
		}
	} else if err := p.patch(ctx, t, content); err != nil {
		return
	}
	t.markPushed(content, p.now())

	slog.Info("operation finished", "slug", t.slug, "op", id, "status", op.Status, "state", content.State)
	p.scheduleEnd(ctx, t, content)
}

// logLines renders the command output of a prune or check. Backups are skipped:
// their task log is restic's raw JSON status stream, which is not something to
// put in front of a person.
//
// The tail is cached between refreshes because a running prune is polled every
// few seconds while its log changes far more slowly. force bypasses the cache
// for the final frame, where the last lines are the whole point.
func (p *Poller) logLines(ctx context.Context, t *tracked, op *backrest.Operation, now time.Time, force bool) []pushward.LogLine {
	if !p.cfg.Render.Logs {
		return nil
	}
	ref := op.OutputLogref()
	if ref == "" {
		return nil
	}
	if !force && !t.lastLogPoll.IsZero() && now.Sub(t.lastLogPoll) < logRefreshInterval {
		return t.logLines
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
	slog.Info("tracking operation", "slug", t.slug, "op", t.opID, "kind", t.kind)
	return nil
}

// patch sends a merge-patch tick.
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
		ctx1, cancel1 := context.WithTimeout(detached, 30*time.Second)
		defer cancel1()
		req := pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}
		if err := p.pw.UpdateActivity(ctx1, slug, req); err != nil {
			slog.Error("failed to update activity (end phase 1)", "slug", slug, "error", err)
		}

		tg.Reset(p.cfg.PushWard.EndDisplayTime, func() {
			ctx2, cancel2 := context.WithTimeout(detached, 30*time.Second)
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
	if t.samples > 0 {
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
	t.samples++
}

// anchor returns the unix second the bar should animate toward, and whether it
// moved enough to be worth re-sending.
//
// Re-sending an anchor restarts the client-side animation, so an estimate that
// wobbles by a second either way must never reach the payload.
func (t *tracked) anchor(op *backrest.Operation, now time.Time) (int64, bool) {
	st := op.BackupStatus()
	if st == nil || t.samples < 2 || t.speed <= 0 {
		return 0, false
	}
	remaining := st.TotalBytes.Int64() - st.BytesDone.Int64()
	if remaining <= 0 {
		return 0, false
	}

	eta := time.Duration(float64(remaining) / t.speed * float64(time.Second))
	if eta < minLiveWindow || eta > maxLiveWindow {
		return 0, false
	}
	end := now.Add(eta).Unix()

	if !t.liveSent {
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
func (t *tracked) shouldPush(c pushward.Content, now time.Time) bool {
	if c.State != t.lastState {
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
	return now.Sub(t.lastPushAt) >= heartbeatInterval
}

func (t *tracked) markPushed(c pushward.Content, now time.Time) {
	t.lastState = c.State
	t.lastProgress = c.Progress
	t.lastPushAt = now
	if c.EndDate != nil {
		t.liveEnd = *c.EndDate
		t.liveSent = true
	} else {
		t.liveEnd = 0
		t.liveSent = false
	}
}
