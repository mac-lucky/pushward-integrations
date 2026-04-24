package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mac-lucky/pushward-integrations/bambulab/internal/bambulab"
	"github.com/mac-lucky/pushward-integrations/bambulab/internal/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/syncx"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const slugPrefix = "bambu"

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
	tracking  bool
	lastState string // last gcode_state we acted on

	endTimers syncx.TimerGroup // two-phase end scheduling
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
			t.endTimers.Stop()
			if t.tracking {
				t.endActivity(ctx, "Interrupted", "xmark.circle.fill", pushward.ColorOrange)
			}
			t.endTimers.Wait()
			return nil

		case <-t.bambu.UpdateCh():
			// New MQTT data arrived — drain and process on next tick

		case <-ticker.C:
			t.process(ctx)
		}
	}
}

func (t *Tracker) process(ctx context.Context) {
	state := t.bambu.State()

	if !t.tracking {
		// Not tracking — watch for print start
		switch state.GcodeState {
		case bambulab.StatePrepare, bambulab.StateRunning:
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
		t.sendTick(ctx, &state)
	}

	t.lastState = state.GcodeState
}

func (t *Tracker) startTracking(ctx context.Context, state *bambulab.MergedState) {
	t.endTimers.Stop()

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

	progress, stateText, icon, accent, remaining, subtitle := deriveFrame(state)
	if err := t.sendSeed(ctx, progress, stateText, icon, accent, remaining, subtitle, pushward.StateOngoing); err != nil {
		slog.Error("failed to seed activity", "error", err)
		return
	}

	t.tracking = true
	t.lastState = state.GcodeState
}

func deriveFrame(state *bambulab.MergedState) (progress float64, stateText, icon, accent string, remaining *int, subtitle string) {
	switch state.GcodeState {
	case bambulab.StatePrepare:
		stateText = "Preparing..."
		icon = "arrow.triangle.2.circlepath"
		accent = pushward.ColorBlue
		if state.SubtaskName != "" {
			subtitle = text.Truncate(state.SubtaskName, 30)
		}
	case bambulab.StatePause:
		progress = float64(state.Percent) / 100.0
		stateText = "Paused"
		icon = "pause.circle.fill"
		accent = pushward.ColorOrange
		subtitle = buildSubtitle(state)
	default: // StateRunning and anything else treated as in-progress
		progress = float64(state.Percent) / 100.0
		if rem := state.RemainingTime * 60; rem > 0 {
			remaining = &rem
		}
		subtitle = buildSubtitle(state)
		stateText = fmt.Sprintf("Layer %d/%d", state.LayerNum, state.TotalLayerNum)
		if state.TotalLayerNum == 0 {
			stateText = fmt.Sprintf("%d%%", state.Percent)
		}
		icon = "printer.fill"
		accent = pushward.ColorBlue
	}
	return
}

// sendTick emits an in-session frame derived from the current printer state.
func (t *Tracker) sendTick(ctx context.Context, state *bambulab.MergedState) {
	progress, stateText, icon, accent, remaining, subtitle := deriveFrame(state)
	t.send(ctx, progress, stateText, icon, accent, remaining, subtitle, pushward.StateOngoing)
}

func (t *Tracker) finishActivity(ctx context.Context, state *bambulab.MergedState) {
	subtitle := ""
	if state.SubtaskName != "" {
		subtitle = text.Truncate(state.SubtaskName, 30)
	}

	endDelay := t.cfg.PushWard.EndDelay
	displayTime := t.cfg.PushWard.EndDisplayTime

	// Reset tracking immediately to unblock the MQTT event loop
	t.tracking = false
	t.lastState = ""

	parent := context.WithoutCancel(ctx)
	t.endTimers.Reset(endDelay, func() {
		// Phase 1: ONGOING with final content
		ctx1, cancel1 := context.WithTimeout(parent, 30*time.Second)
		t.send(ctx1, 1.0, "Complete", "checkmark.circle.fill", pushward.ColorGreen, nil, subtitle, pushward.StateOngoing)
		cancel1()
		slog.Info("two-phase end: sent ONGOING with final content", "display_time", displayTime)

		// Phase 2: ENDED
		t.endTimers.Reset(displayTime, func() {
			ctx2, cancel2 := context.WithTimeout(parent, 30*time.Second)
			defer cancel2()
			t.send(ctx2, 1.0, "Complete", "checkmark.circle.fill", pushward.ColorGreen, nil, subtitle, pushward.StateEnded)
			slog.Info("print tracking complete")
		})
	})
}

func (t *Tracker) failActivity(ctx context.Context, state *bambulab.MergedState) {
	progress := float64(state.Percent) / 100.0
	subtitle := ""
	if state.SubtaskName != "" {
		subtitle = text.Truncate(state.SubtaskName, 30)
	}

	endDelay := t.cfg.PushWard.EndDelay
	displayTime := t.cfg.PushWard.EndDisplayTime

	// Reset tracking immediately to unblock the MQTT event loop
	t.tracking = false
	t.lastState = ""

	parent := context.WithoutCancel(ctx)
	t.endTimers.Reset(endDelay, func() {
		// Phase 1: ONGOING with failure content
		ctx1, cancel1 := context.WithTimeout(parent, 30*time.Second)
		t.send(ctx1, progress, "Failed", "xmark.circle.fill", pushward.ColorRed, nil, subtitle, pushward.StateOngoing)
		cancel1()
		slog.Info("two-phase end: sent ONGOING with failure content", "display_time", displayTime)

		// Phase 2: ENDED
		t.endTimers.Reset(displayTime, func() {
			ctx2, cancel2 := context.WithTimeout(parent, 30*time.Second)
			defer cancel2()
			t.send(ctx2, progress, "Failed", "xmark.circle.fill", pushward.ColorRed, nil, subtitle, pushward.StateEnded)
			slog.Info("print failure tracking complete")
		})
	})
}

func (t *Tracker) endActivity(ctx context.Context, stateText, icon, color string) {
	t.send(ctx, 0.0, stateText, icon, color, nil, "", pushward.StateEnded)
	t.tracking = false
	t.lastState = ""
}

// sendSeed sends a full-Content PATCH that establishes the session baseline
// (template/icon/accent). Returns the error so startTracking can gate tracking
// on seed success.
func (t *Tracker) sendSeed(ctx context.Context, progress float64, stateText, icon, accentColor string, remainingSeconds *int, subtitle string, activityState string) error {
	content := pushward.Content{
		Template:    pushward.TemplateGeneric,
		Progress:    progress,
		State:       stateText,
		Icon:        icon,
		AccentColor: accentColor,
	}
	if remainingSeconds != nil && *remainingSeconds > 0 {
		content.RemainingTime = remainingSeconds
	}
	if subtitle != "" {
		content.Subtitle = subtitle
	}
	return t.pw.UpdateActivity(ctx, t.slug, pushward.UpdateRequest{State: activityState, Content: content})
}

// send emits a merge-patch tick. Must be called after a successful sendSeed in
// the same session — the server-side Content baseline is what merge-patch
// preserves.
func (t *Tracker) send(ctx context.Context, progress float64, stateText, icon, accentColor string, remainingSeconds *int, subtitle string, activityState string) {
	contentPatch := &pushward.ContentPatch{
		Progress:    pushward.Float64Ptr(progress),
		State:       pushward.StringPtr(stateText),
		Icon:        pushward.StringPtr(icon),
		AccentColor: pushward.StringPtr(accentColor),
	}
	if remainingSeconds != nil && *remainingSeconds > 0 {
		contentPatch.RemainingTime = remainingSeconds
	}
	if subtitle != "" {
		contentPatch.Subtitle = pushward.StringPtr(subtitle)
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
