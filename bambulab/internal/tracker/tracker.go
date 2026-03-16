package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mac-lucky/pushward-integrations/bambulab/internal/bambulab"
	"github.com/mac-lucky/pushward-integrations/bambulab/internal/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
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

	tracking  bool
	lastState string      // last gcode_state we acted on
	endTimer  *time.Timer // pending two-phase end
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
			if t.endTimer != nil {
				t.endTimer.Stop()
			}
			if t.tracking {
				t.endActivity(ctx, "Interrupted", "xmark.circle.fill", "orange")
			}
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
			t.endActivity(ctx, "Cancelled", "xmark.circle.fill", "orange")
		}

	case bambulab.StateRunning:
		t.sendProgress(ctx, &state)

	case bambulab.StatePause:
		t.sendPaused(ctx, &state)

	case bambulab.StatePrepare:
		t.sendPreparing(ctx, &state)
	}

	t.lastState = state.GcodeState
}

func (t *Tracker) startTracking(ctx context.Context, state *bambulab.MergedState) {
	if t.endTimer != nil {
		t.endTimer.Stop()
		t.endTimer = nil
	}
	t.tracking = true
	t.lastState = state.GcodeState

	endedTTL := int(t.cfg.PushWard.CleanupDelay.Seconds())
	staleTTL := int(t.cfg.PushWard.StaleTimeout.Seconds())

	name := "BambuLab Print"
	if state.SubtaskName != "" {
		name = truncate(state.SubtaskName, 40)
	}

	if err := t.pw.CreateActivity(ctx, t.slug, name, t.cfg.PushWard.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "error", err)
		return
	}

	if state.GcodeState == bambulab.StatePrepare {
		t.sendPreparing(ctx, state)
	} else {
		t.sendProgress(ctx, state)
	}
}

func (t *Tracker) sendProgress(ctx context.Context, state *bambulab.MergedState) {
	progress := float64(state.Percent) / 100.0
	remaining := state.RemainingTime * 60 // minutes → seconds

	subtitle := buildSubtitle(state)
	stateText := fmt.Sprintf("Layer %d/%d", state.LayerNum, state.TotalLayerNum)
	if state.TotalLayerNum == 0 {
		stateText = fmt.Sprintf("%d%%", state.Percent)
	}

	t.send(ctx, progress, stateText, "printer.fill", "blue", &remaining, subtitle, pushward.StateOngoing)
}

func (t *Tracker) sendPaused(ctx context.Context, state *bambulab.MergedState) {
	progress := float64(state.Percent) / 100.0
	subtitle := buildSubtitle(state)
	t.send(ctx, progress, "Paused", "pause.circle.fill", "orange", nil, subtitle, pushward.StateOngoing)
}

func (t *Tracker) sendPreparing(ctx context.Context, state *bambulab.MergedState) {
	subtitle := ""
	if state.SubtaskName != "" {
		subtitle = truncate(state.SubtaskName, 30)
	}
	t.send(ctx, 0.0, "Preparing...", "arrow.triangle.2.circlepath", "blue", nil, subtitle, pushward.StateOngoing)
}

func (t *Tracker) finishActivity(_ context.Context, state *bambulab.MergedState) {
	subtitle := ""
	if state.SubtaskName != "" {
		subtitle = truncate(state.SubtaskName, 30)
	}

	endDelay := t.cfg.PushWard.EndDelay
	displayTime := t.cfg.PushWard.EndDisplayTime

	// Reset tracking immediately to unblock the MQTT event loop
	t.tracking = false
	t.lastState = ""

	t.endTimer = time.AfterFunc(endDelay, func() {
		// Phase 1: ONGOING with final content
		ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
		t.send(ctx1, 1.0, "Complete", "checkmark.circle.fill", "green", nil, subtitle, pushward.StateOngoing)
		cancel1()
		slog.Info("two-phase end: sent ONGOING with final content", "display_time", displayTime)

		// Phase 2: ENDED
		time.AfterFunc(displayTime, func() {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel2()
			t.send(ctx2, 1.0, "Complete", "checkmark.circle.fill", "green", nil, subtitle, pushward.StateEnded)
			slog.Info("print tracking complete")
		})
	})
}

func (t *Tracker) failActivity(_ context.Context, state *bambulab.MergedState) {
	progress := float64(state.Percent) / 100.0
	subtitle := ""
	if state.SubtaskName != "" {
		subtitle = truncate(state.SubtaskName, 30)
	}

	endDelay := t.cfg.PushWard.EndDelay
	displayTime := t.cfg.PushWard.EndDisplayTime

	// Reset tracking immediately to unblock the MQTT event loop
	t.tracking = false
	t.lastState = ""

	t.endTimer = time.AfterFunc(endDelay, func() {
		// Phase 1: ONGOING with failure content
		ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
		t.send(ctx1, progress, "Failed", "xmark.circle.fill", "red", nil, subtitle, pushward.StateOngoing)
		cancel1()
		slog.Info("two-phase end: sent ONGOING with failure content", "display_time", displayTime)

		// Phase 2: ENDED
		time.AfterFunc(displayTime, func() {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel2()
			t.send(ctx2, progress, "Failed", "xmark.circle.fill", "red", nil, subtitle, pushward.StateEnded)
			slog.Info("print failure tracking complete")
		})
	})
}

func (t *Tracker) endActivity(ctx context.Context, stateText, icon, color string) {
	t.send(ctx, 0.0, stateText, icon, color, nil, "", pushward.StateEnded)
	t.tracking = false
	t.lastState = ""
}

func (t *Tracker) send(ctx context.Context, progress float64, stateText, icon, accentColor string, remainingSeconds *int, subtitle string, activityState string) {
	content := pushward.Content{
		Template:    "generic",
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

	req := pushward.UpdateRequest{
		State:   activityState,
		Content: content,
	}
	if err := t.pw.UpdateActivity(ctx, t.slug, req); err != nil {
		slog.Error("failed to send update", "error", err)
	}
}

func buildSubtitle(state *bambulab.MergedState) string {
	var parts []string

	if state.SubtaskName != "" {
		parts = append(parts, truncate(state.SubtaskName, 20))
	}

	if state.NozzleTemper > 0 {
		parts = append(parts, fmt.Sprintf("%.0f/%.0f\u00b0C", state.NozzleTemper, state.NozzleTarget))
	}

	return strings.Join(parts, " · ")
}

func truncate(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return string([]rune(s)[:maxLen])
	}
	return string([]rune(s)[:maxLen-3]) + "..."
}
