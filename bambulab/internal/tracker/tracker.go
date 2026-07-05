package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mac-lucky/pushward-integrations/bambulab/internal/bambulab"
	"github.com/mac-lucky/pushward-integrations/bambulab/internal/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/syncx"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const slugPrefix = "bambu"

// detachedEndTimeout bounds each detached, time-limited end send — the two
// two-phase-end phases and the shutdown-path final ENDED send — so a stuck
// PushWard call cannot block endTimers.Wait at shutdown indefinitely.
const detachedEndTimeout = 30 * time.Second

// frame is the comparable projection of a derived Live-Activity frame, used to
// suppress re-pushing byte-identical content (e.g. the constant "Preparing..."
// frame during a multi-minute heat-up). remaining is the dereferenced pointer
// (0 when nil) so the struct stays comparable with ==.
type frame struct {
	progress  float64
	stateText string
	icon      string
	accent    string
	remaining int
	subtitle  string
}

// Printer abstracts the printer state source for testability.
type Printer interface {
	State() bambulab.MergedState
	UpdateCh() <-chan struct{}
}

type Tracker struct {
	cfg   *config.Config
	bambu Printer
	pw    *pushward.Client
	slug  string

	// tracking flips true only after the seed PATCH succeeds, so a failed seed
	// can be retried on the next tick and merge-patch ticks never run without a
	// server-side Content baseline.
	tracking   bool
	lastState  string    // last gcode_state we acted on
	lastTickAt time.Time // last progress PATCH; debounces same-state ticks driven by MQTT updates
	lastFrame  frame     // last frame sent; suppresses re-pushing identical content

	endTimers syncx.TimerGroup // two-phase end scheduling

	// gen identifies the current print session. startTracking/endActivity bump
	// it so an in-flight two-phase-end callback (which can re-arm its own phase 2
	// after a Stop) bails out instead of ending a newly started print's activity.
	// Read from AfterFunc goroutines, written from the main loop — hence atomic.
	gen atomic.Uint64
}

func New(cfg *config.Config, bambu Printer, pw *pushward.Client) *Tracker {
	serial := strings.ToLower(cfg.BambuLab.Serial)
	return &Tracker{
		cfg:   cfg,
		bambu: bambu,
		pw:    pw,
		slug:  fmt.Sprintf("%s-%s", slugPrefix, serial),
	}
}

// Run is the main loop. It listens for MQTT updates and manages the activity lifecycle.
func (t *Tracker) Run(ctx context.Context) error {
	updateInterval := t.cfg.Polling.UpdateInterval
	ticker := time.NewTicker(updateInterval)
	defer ticker.Stop()

	// Check if printer is already printing on startup
	state := t.bambu.State()
	if state.GcodeState == bambulab.StateRunning || state.GcodeState == bambulab.StatePrepare || state.GcodeState == bambulab.StatePause {
		slog.Info("printer already active on startup, resuming tracking", "state", state.GcodeState)
		t.startTracking(ctx, &state)
	}

	for {
		select {
		case <-ctx.Done():
			// Close (not Stop) so a two-phase callback that re-arms its own
			// phase 2 cannot keep endTimers.Wait blocked past shutdown.
			t.endTimers.Close()
			if t.tracking {
				// ctx is already cancelled here, so the final ENDED frame must
				// run on a fresh detached, time-bounded context — otherwise the
				// "Interrupted" send aborts immediately and the Live Activity is
				// left stuck until the server-side stale timeout.
				endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedEndTimeout)
				t.endActivity(endCtx, "Interrupted", "xmark.circle.fill", pushward.ColorOrange)
				cancel()
			}
			t.endTimers.Wait()
			return nil

		case <-t.bambu.UpdateCh():
			// MQTT pushed new state — process immediately so transitions
			// (finish/fail/cancel) are reported in real time. Same-state
			// progress ticks are debounced in process() to the poll interval.
			t.process(ctx)

		case <-ticker.C:
			t.process(ctx)
		}
	}
}

func (t *Tracker) process(ctx context.Context) {
	state := t.bambu.State()

	if !t.tracking {
		// Not tracking — watch for print start. StatePause is included so a
		// bridge that (re)starts while a print is paused begins tracking once
		// the first MQTT push_status arrives (State() is the zero value until
		// then, so the synchronous startup check in Run cannot catch it).
		switch state.GcodeState {
		case bambulab.StatePrepare, bambulab.StateRunning, bambulab.StatePause:
			slog.Info("print started", "state", state.GcodeState, "file", state.SubtaskName)
			t.startTracking(ctx, &state)
		}
		return
	}

	// Currently tracking — handle state transitions
	switch state.GcodeState {
	case bambulab.StateFinish:
		if t.lastState != bambulab.StateFinish {
			slog.Info("print finished", "file", state.SubtaskName)
			t.finishActivity(ctx, &state)
		}

	case bambulab.StateFailed:
		if t.lastState != bambulab.StateFailed {
			slog.Info("print failed", "file", state.SubtaskName, "error", state.PrintError)
			t.failActivity(ctx, &state)
		}

	case bambulab.StateIdle:
		// Printer went idle without FINISH/FAILED (e.g. cancelled)
		if t.lastState != bambulab.StateIdle {
			slog.Info("print cancelled/stopped", "file", state.SubtaskName)
			t.endActivity(ctx, "Cancelled", "xmark.circle.fill", pushward.ColorOrange)
		}

	case bambulab.StateRunning, bambulab.StatePause, bambulab.StatePrepare:
		f := deriveFrame(&state)
		// Always emit on a genuine state transition. Otherwise emit only when the
		// derived content actually changed AND the poll interval has elapsed: this
		// throttles changing RUNNING frames (layer/percent/temp) to the poll
		// cadence while entirely suppressing the constant PREPARE frame
		// (deriveFrame ignores progress for PREPARE) instead of re-pushing
		// byte-identical content every interval.
		if state.GcodeState != t.lastState || (f != t.lastFrame && time.Since(t.lastTickAt) >= t.cfg.Polling.UpdateInterval) {
			t.lastTickAt = time.Now()
			t.lastFrame = f
			t.send(ctx, f, pushward.StateOngoing)
		}
	}

	t.lastState = state.GcodeState
}

func (t *Tracker) startTracking(ctx context.Context, state *bambulab.MergedState) {
	t.endTimers.Stop()
	// Invalidate any in-flight two-phase-end callback from the previous
	// session so it cannot end this new print's activity (the slug is constant
	// per printer).
	t.gen.Add(1)

	endedTTL := int(t.cfg.PushWard.CleanupDelay.Seconds())
	staleTTL := int(t.cfg.PushWard.StaleTimeout.Seconds())

	name := "BambuLab Print"
	if state.SubtaskName != "" {
		name = text.Truncate(state.SubtaskName, 40)
	}

	if err := t.pw.CreateActivity(ctx, t.slug, name, t.cfg.PushWard.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "error", err)
		return
	}

	f := deriveFrame(state)
	if err := t.sendSeed(ctx, f, pushward.StateOngoing); err != nil {
		slog.Error("failed to seed activity", "error", err)
		return
	}

	t.tracking = true
	t.lastState = state.GcodeState
	// Seed the debounce baseline so the next same-state tick is deferred one poll
	// interval past the seed and identical content isn't immediately re-pushed.
	t.lastTickAt = time.Now()
	t.lastFrame = f
}

func deriveFrame(state *bambulab.MergedState) frame {
	var f frame
	switch state.GcodeState {
	case bambulab.StatePrepare:
		f.stateText = "Preparing..."
		f.icon = "arrow.triangle.2.circlepath"
		f.accent = pushward.ColorBlue
		if state.SubtaskName != "" {
			f.subtitle = text.Truncate(state.SubtaskName, 30)
		}
	case bambulab.StatePause:
		f.progress = float64(state.Percent) / 100.0
		f.stateText = "Paused"
		f.icon = "pause.circle.fill"
		f.accent = pushward.ColorOrange
		f.subtitle = buildSubtitle(state)
	default: // StateRunning and anything else treated as in-progress
		f.progress = float64(state.Percent) / 100.0
		if rem := state.RemainingTime * 60; rem > 0 {
			f.remaining = rem
		}
		f.subtitle = buildSubtitle(state)
		f.stateText = fmt.Sprintf("Layer %d/%d", state.LayerNum, state.TotalLayerNum)
		if state.TotalLayerNum == 0 {
			f.stateText = fmt.Sprintf("%d%%", state.Percent)
		}
		f.icon = "printer.fill"
		f.accent = pushward.ColorBlue
	}
	return f
}

func (t *Tracker) finishActivity(ctx context.Context, state *bambulab.MergedState) {
	subtitle := ""
	if state.SubtaskName != "" {
		subtitle = text.Truncate(state.SubtaskName, 30)
	}
	t.scheduleTwoPhaseEnd(ctx, 1.0, "Complete", "checkmark.circle.fill", pushward.ColorGreen, subtitle)
}

func (t *Tracker) failActivity(ctx context.Context, state *bambulab.MergedState) {
	progress := float64(state.Percent) / 100.0
	subtitle := ""
	if state.SubtaskName != "" {
		subtitle = text.Truncate(state.SubtaskName, 30)
	}
	t.scheduleTwoPhaseEnd(ctx, progress, "Failed", "xmark.circle.fill", pushward.ColorRed, subtitle)
}

// scheduleTwoPhaseEnd resets tracking and arms the two-phase end: after EndDelay
// it sends an ONGOING frame carrying the terminal content (so completion shows on
// the Dynamic Island), then after EndDisplayTime it sends the ENDED frame. Both
// phases bail if the session generation changed (a new print started), and run
// on a detached, time-bounded context so they survive ctx cancellation without
// blocking shutdown. finish vs fail differ only in the content args.
func (t *Tracker) scheduleTwoPhaseEnd(ctx context.Context, progress float64, stateText, icon, accent, subtitle string) {
	endDelay := t.cfg.PushWard.EndDelay
	displayTime := t.cfg.PushWard.EndDisplayTime

	// Reset tracking immediately to unblock the MQTT event loop. lastState is
	// not reset here: process() overwrites it with the terminal state on the
	// same tick, and startTracking re-seeds it for the next session.
	t.tracking = false

	parent := context.WithoutCancel(ctx)
	g := t.gen.Load()
	t.endTimers.Reset(endDelay, func() {
		if t.gen.Load() != g {
			return // a new session started; don't touch its activity
		}
		// Phase 1: ONGOING with terminal content.
		ctx1, cancel1 := context.WithTimeout(parent, detachedEndTimeout)
		defer cancel1()
		t.send(ctx1, frame{progress: progress, stateText: stateText, icon: icon, accent: accent, subtitle: subtitle}, pushward.StateOngoing)
		slog.Info("two-phase end: sent ONGOING with terminal content", "state", stateText, "display_time", displayTime)

		// Phase 2: ENDED.
		t.endTimers.Reset(displayTime, func() {
			if t.gen.Load() != g {
				return
			}
			ctx2, cancel2 := context.WithTimeout(parent, detachedEndTimeout)
			defer cancel2()
			t.send(ctx2, frame{progress: progress, stateText: stateText, icon: icon, accent: accent, subtitle: subtitle}, pushward.StateEnded)
			slog.Info("two-phase end complete", "state", stateText)
		})
	})
}

func (t *Tracker) endActivity(ctx context.Context, stateText, icon, color string) {
	// Bump the session generation so any pending two-phase-end callback from a
	// prior finish/fail does not later re-end the activity.
	t.gen.Add(1)
	t.send(ctx, frame{stateText: stateText, icon: icon, accent: color}, pushward.StateEnded)
	t.tracking = false
}

// sendSeed sends a full-Content PATCH that establishes the session baseline
// (template/icon/accent). Returns the error so startTracking can gate tracking
// on seed success.
func (t *Tracker) sendSeed(ctx context.Context, f frame, activityState string) error {
	content := pushward.Content{
		Template:    pushward.TemplateGeneric,
		Progress:    f.progress,
		State:       f.stateText,
		Icon:        f.icon,
		AccentColor: f.accent,
	}
	if f.remaining > 0 {
		rem := f.remaining
		content.RemainingTime = &rem
		// Opt the generic bar into client-side interpolation: iOS animates it
		// toward end_date and shows a counting-down ETA between the (minute-
		// granularity) pushes.
		content.LiveProgress = pushward.BoolPtr(true)
		content.EndDate = pushward.Int64Ptr(time.Now().Unix() + int64(f.remaining))
	} else {
		// PREPARE/PAUSE/terminal frames carry no ETA, so stop interpolation: a
		// paused print must not keep counting down toward a stale end_date.
		content.LiveProgress = pushward.BoolPtr(false)
	}
	if f.subtitle != "" {
		content.Subtitle = f.subtitle
	}
	return t.pw.UpdateActivity(ctx, t.slug, pushward.UpdateRequest{State: activityState, Content: content})
}

// send emits a merge-patch tick. Must be called after a successful sendSeed in
// the same session — the server-side Content baseline is what merge-patch
// preserves.
func (t *Tracker) send(ctx context.Context, f frame, activityState string) {
	contentPatch := &pushward.ContentPatch{
		Progress:    pushward.Float64Ptr(f.progress),
		State:       pushward.StringPtr(f.stateText),
		Icon:        pushward.StringPtr(f.icon),
		AccentColor: pushward.StringPtr(f.accent),
	}
	if f.remaining > 0 {
		rem := f.remaining
		contentPatch.RemainingTime = &rem
		contentPatch.LiveProgress = pushward.BoolPtr(true)
		contentPatch.EndDate = pushward.Int64Ptr(time.Now().Unix() + int64(f.remaining))
	} else {
		contentPatch.LiveProgress = pushward.BoolPtr(false)
	}
	if f.subtitle != "" {
		contentPatch.Subtitle = pushward.StringPtr(f.subtitle)
	}
	if err := t.pw.PatchActivity(ctx, t.slug, pushward.PatchRequest{
		State:   activityState,
		Content: contentPatch,
	}); err != nil {
		slog.Error("failed to send update", "error", err)
	}
}

func buildSubtitle(state *bambulab.MergedState) string {
	var parts []string

	if state.SubtaskName != "" {
		parts = append(parts, text.Truncate(state.SubtaskName, 20))
	}

	if state.NozzleTemper > 0 {
		parts = append(parts, fmt.Sprintf("%.0f/%.0f\u00b0C", state.NozzleTemper, state.NozzleTarget))
	}

	return strings.Join(parts, " · ")
}
