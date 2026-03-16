package tracker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mac-lucky/pushward-integrations/octoprint/internal/api"
	"github.com/mac-lucky/pushward-integrations/octoprint/internal/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

const slug = "octoprint"

// webhookPayload is the payload sent by OctoPrint webhook plugins.
type webhookPayload struct {
	Topic string `json:"topic"` // e.g. "PrintStarted", "PrintDone", "PrintFailed", "PrintCancelled", "PrintPaused", "PrintResumed"
	State string `json:"state"` // alternative: "started", "done", "failed", "cancelled", "paused", "resumed"
}

// OctoPrintAPI abstracts the OctoPrint API for testability.
type OctoPrintAPI interface {
	GetJob(ctx context.Context) (*api.JobResponse, error)
	GetPrinter(ctx context.Context) (*api.PrinterResponse, error)
}

type Tracker struct {
	cfg        *config.Config
	octo       OctoPrintAPI
	pw         *pushward.Client
	mu         sync.Mutex
	active     bool
	cancelling bool // set by PrintCancelled webhook to distinguish from success
	wg         sync.WaitGroup
	ctx        context.Context
}

func New(ctx context.Context, cfg *config.Config, octo OctoPrintAPI, pw *pushward.Client) *Tracker {
	return &Tracker{ctx: ctx, cfg: cfg, octo: octo, pw: pw}
}

// Cleanup ends any stale activity left over from a previous run.
func (t *Tracker) Cleanup(ctx context.Context) {
	req := pushward.UpdateRequest{
		State:   pushward.StateEnded,
		Content: pushward.Content{Template: "generic", Progress: 0, State: "Dismissed"},
	}
	if err := t.pw.UpdateActivity(ctx, slug, req); err != nil {
		slog.Info("no stale activity to clean up")
		return
	}
	slog.Info("cleaned up stale activity from previous run")
}

// ResumeIfActive checks OctoPrint for an in-progress print and starts tracking if found.
func (t *Tracker) ResumeIfActive() bool {
	job, err := t.octo.GetJob(t.ctx)
	if err != nil {
		slog.Warn("failed to check OctoPrint job on startup", "error", err)
		return false
	}

	if isPrinting(job.State) {
		slog.Info("active print found on startup, resuming tracking", "state", job.State, "file", job.Job.File.Name)
		t.mu.Lock()
		t.active = true
		t.mu.Unlock()
		t.launchTracker(true)
		return true
	}

	return false
}

// Wait blocks until all active tracking goroutines finish.
func (t *Tracker) Wait() {
	t.wg.Wait()
}

// HandleWebhook is the HTTP handler for POST /webhook.
func (t *Tracker) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	// Webhook secret validation
	if t.cfg.OctoPrint.WebhookSecret != "" {
		got := r.Header.Get("X-Webhook-Secret")
		if subtle.ConstantTimeCompare([]byte(got), []byte(t.cfg.OctoPrint.WebhookSecret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var payload webhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		slog.Error("failed to decode webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	event := normalizeEvent(payload.Topic, payload.State)
	slog.Info("webhook received", "event", event, "topic", payload.Topic, "state", payload.State)

	switch event {
	case "started":
		t.mu.Lock()
		if t.active {
			t.mu.Unlock()
			slog.Info("tracking already active, skipping webhook")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, `{"status":"already_tracking"}`)
			return
		}
		t.active = true
		t.cancelling = false
		t.mu.Unlock()
		t.launchTracker(false)
	case "cancelled":
		// Set cancelling flag so polling loop can distinguish from success
		t.mu.Lock()
		t.cancelling = true
		t.mu.Unlock()
	case "done", "failed":
		// Polling loop will detect state change; no action needed here.
	case "paused", "resumed":
		// Polling loop handles these states; no action needed.
	default:
		slog.Debug("ignoring unknown webhook event", "event", event)
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, `{"status":"ok"}`)
}

func (t *Tracker) launchTracker(resumed bool) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer func() {
			t.mu.Lock()
			t.active = false
			t.mu.Unlock()
		}()
		t.track(t.ctx, resumed)
	}()
}

func (t *Tracker) track(ctx context.Context, resumed bool) {
	staleTTL := int(t.cfg.PushWard.StaleTimeout.Seconds())
	endedTTL := int(t.cfg.PushWard.CleanupDelay.Seconds())

	if err := t.pw.CreateActivity(ctx, slug, "OctoPrint", t.cfg.PushWard.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "error", err)
		return
	}

	t.send(ctx, 0.0, "Starting...", "arrow.triangle.2.circlepath", "#007AFF", nil, "", pushward.StateOngoing)

	ticker := time.NewTicker(t.cfg.Polling.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		job, err := t.octo.GetJob(ctx)
		if err != nil {
			slog.Warn("failed to fetch job status", "error", err)
			continue
		}

		filename := truncate(job.Job.File.Name, 30)

		switch {
		case job.State == "Printing" || job.State == "Finishing":
			progress := float64(0)
			if job.Progress.Completion != nil {
				progress = *job.Progress.Completion / 100.0
			}

			stateText := fmt.Sprintf("%.0f%%", progress*100)

			// Build subtitle with filename and nozzle temp
			subtitle := buildSubtitle(ctx, t.octo, filename)

			t.send(ctx, progress, stateText, "printer.fill", "#007AFF", job.Progress.PrintTimeLeft, subtitle, pushward.StateOngoing)

		case job.State == "Pausing" || job.State == "Paused":
			progress := float64(0)
			if job.Progress.Completion != nil {
				progress = *job.Progress.Completion / 100.0
			}
			subtitle := buildSubtitle(ctx, t.octo, filename)
			t.send(ctx, progress, "Paused", "pause.circle.fill", "#FF9500", nil, subtitle, pushward.StateOngoing)

		case job.State == "Cancelling":
			progress := float64(0)
			if job.Progress.Completion != nil {
				progress = *job.Progress.Completion / 100.0
			}
			t.send(ctx, progress, "Cancelling...", "xmark.circle", "#FF9500", nil, filename, pushward.StateOngoing)

		case job.State == "Operational":
			t.mu.Lock()
			wasCancelled := t.cancelling
			t.cancelling = false
			t.mu.Unlock()

			if wasCancelled {
				slog.Info("print cancelled", "file", job.Job.File.Name)
				progress := float64(0)
				if job.Progress.Completion != nil {
					progress = *job.Progress.Completion / 100.0
				}
				t.twoPhaseEnd(ctx, resumed, progress, "Cancelled", "xmark.circle.fill", "#FF9500", filename)
			} else {
				slog.Info("print finished", "file", job.Job.File.Name)
				t.twoPhaseEnd(ctx, resumed, 1.0, "Complete", "checkmark.circle.fill", "#34C759", filename)
			}
			return

		case job.State == "Error":
			slog.Info("print error", "file", job.Job.File.Name)
			progress := float64(0)
			if job.Progress.Completion != nil {
				progress = *job.Progress.Completion / 100.0
			}
			t.twoPhaseEnd(ctx, resumed, progress, "Failed", "xmark.circle.fill", "#FF3B30", filename)
			return

		case job.State == "Offline":
			slog.Warn("printer went offline")
			t.send(ctx, 0.0, "Offline", "wifi.slash", "#FF3B30", nil, "", pushward.StateEnded)
			return
		}
	}
}

func (t *Tracker) twoPhaseEnd(ctx context.Context, resumed bool, progress float64, stateText, icon, color, subtitle string) {
	if resumed {
		t.send(ctx, progress, stateText, icon, color, nil, subtitle, pushward.StateEnded)
		slog.Info("tracking complete (resumed, skipping two-phase end)")
		return
	}

	endDelay := t.cfg.PushWard.EndDelay
	displayTime := t.cfg.PushWard.EndDisplayTime

	// Phase 1: ONGOING with final content
	select {
	case <-ctx.Done():
		return
	case <-time.After(endDelay):
	}
	t.send(ctx, progress, stateText, icon, color, nil, subtitle, pushward.StateOngoing)
	slog.Info("two-phase end: sent ONGOING with final content", "display_time", displayTime)

	// Phase 2: ENDED
	select {
	case <-ctx.Done():
		return
	case <-time.After(displayTime):
	}
	t.send(ctx, progress, stateText, icon, color, nil, subtitle, pushward.StateEnded)
	slog.Info("tracking complete")
}

func (t *Tracker) send(ctx context.Context, progress float64, state, icon, accentColor string, remainingSeconds *int, subtitle string, activityState string) {
	content := pushward.Content{
		Template:    "generic",
		Progress:    progress,
		State:       state,
		AccentColor: accentColor,
	}
	if icon != "" {
		content.Icon = icon
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
	if err := t.pw.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to send update", "error", err)
	}
}

// normalizeEvent maps OctoPrint webhook topic/state to a canonical event name.
func normalizeEvent(topic, state string) string {
	// Try topic first (OctoPrint-Webhook plugin format)
	switch topic {
	case "PrintStarted":
		return "started"
	case "PrintDone":
		return "done"
	case "PrintFailed":
		return "failed"
	case "PrintCancelled", "PrintCanceling":
		return "cancelled"
	case "PrintPaused":
		return "paused"
	case "PrintResumed":
		return "resumed"
	}

	// Fallback to state field (alternative webhook formats)
	return strings.ToLower(state)
}

// isPrinting returns true if the OctoPrint state indicates an active print.
func isPrinting(state string) bool {
	switch state {
	case "Printing", "Pausing", "Paused", "Finishing", "Cancelling":
		return true
	}
	return false
}

func buildSubtitle(ctx context.Context, octo OctoPrintAPI, filename string) string {
	printer, err := octo.GetPrinter(ctx)
	if err != nil || printer.Temperature.Tool0 == nil {
		return filename
	}

	temp := printer.Temperature.Tool0
	if temp.Actual > 0 {
		return fmt.Sprintf("%s · %.0f/%.0f°C", filename, temp.Actual, temp.Target)
	}
	return filename
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
