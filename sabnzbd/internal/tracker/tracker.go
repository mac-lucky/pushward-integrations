package tracker

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mac-lucky/pushward-docker/sabnzbd/internal/config"
	"github.com/mac-lucky/pushward-docker/sabnzbd/internal/sabnzbd"
	"github.com/mac-lucky/pushward-docker/shared/pushward"
)

const slug = "sabnzbd"

var ppStatuses = map[string]bool{
	"Queued":     true,
	"QuickCheck": true,
	"Verifying":  true,
	"Repairing":  true,
	"Fetching":   true,
	"Extracting": true,
	"Moving":     true,
	"Running":    true,
}

var ppIcons = map[string]string{
	"Verifying":  "checkmark.shield",
	"Repairing":  "wrench.and.screwdriver",
	"Extracting": "archivebox",
	"Moving":     "folder",
}

type Tracker struct {
	cfg    *config.Config
	sab    *sabnzbd.Client
	pw     *pushward.Client
	mu     sync.Mutex
	active bool
	wg     sync.WaitGroup
	ctx    context.Context
}

func New(ctx context.Context, cfg *config.Config, sab *sabnzbd.Client, pw *pushward.Client) *Tracker {
	return &Tracker{ctx: ctx, cfg: cfg, sab: sab, pw: pw}
}

// Cleanup ends any stale activity left over from a previous run (e.g. crash).
func (t *Tracker) Cleanup(ctx context.Context) {
	req := pushward.UpdateRequest{
		State:   "ENDED",
		Content: pushward.Content{Template: "generic", Progress: 0, State: "Dismissed"},
	}
	if err := t.pw.UpdateActivity(ctx, slug, req); err != nil {
		slog.Info("no stale activity to clean up")
		return
	}
	slog.Info("cleaned up stale activity from previous run")
}

// ResumeIfActive checks SABnzbd for in-progress downloads or post-processing
// and starts tracking if found. Returns true if tracking was resumed.
func (t *Tracker) ResumeIfActive() bool {
	queue, err := t.sab.GetQueue(t.ctx)
	if err != nil {
		slog.Warn("failed to check SABnzbd queue on startup", "error", err)
		return false
	}

	mb, _ := strconv.ParseFloat(queue.MB, 64)
	if queue.Status != "Idle" && mb > 0 {
		slog.Info("active download found on startup, resuming tracking", "status", queue.Status, "total_mb", mb)
		t.mu.Lock()
		t.active = true
		t.mu.Unlock()
		t.launchTracker(true)
		return true
	}

	ppStatus, _ := t.getPPStatus(t.ctx)
	if ppStatus != "" {
		slog.Info("active post-processing found on startup, resuming tracking", "status", ppStatus)
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
	if t.cfg.SABnzbd.WebhookSecret != "" {
		got := r.Header.Get("X-Webhook-Secret")
		if subtle.ConstantTimeCompare([]byte(got), []byte(t.cfg.SABnzbd.WebhookSecret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	t.mu.Lock()
	if t.active {
		t.mu.Unlock()
		slog.Info("tracking already active, skipping webhook")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"already_tracking"}`)
		return
	}
	t.active = true
	t.mu.Unlock()

	slog.Info("webhook received, starting tracker")
	t.launchTracker(false)

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, `{"status":"tracking_started"}`)
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

func (t *Tracker) track(ctx context.Context, resumed bool) {
	// Ensure activity exists
	endedTTL := int(t.cfg.PushWard.CleanupDelay.Seconds())
	staleTTL := int(t.cfg.PushWard.StaleTimeout.Seconds())
	if err := t.pw.CreateActivity(ctx, slug, "SABnzbd", t.cfg.PushWard.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "error", err)
		return
	}

	// Phase 1: Wait for SABnzbd to start downloading
	slog.Info("waiting for download to start")
	t.send(ctx, 0.0, "Starting...", "arrow.down.circle", "blue", nil, "", "ONGOING")

	if !t.waitForQueueActive(ctx, 60) {
		slog.Warn("SABnzbd never started downloading, giving up")
		t.send(ctx, 0.0, "No downloads", "checkmark.circle.fill", "green", nil, "", "ENDED")
		return
	}

	startTime := time.Now()
	var totalMB float64
	var totalPPElapsed time.Duration

	// Main loop: download → post-processing → check for more
	for {
		// Download phase
		roundMB := t.trackDownloads(ctx)
		totalMB += roundMB

		// Post-processing phase
		ppDuration := t.trackPostProcessing(ctx)
		totalPPElapsed += ppDuration

		// Check if queue has more downloads
		if !t.waitForQueueActive(ctx, 10) {
			break
		}
		slog.Info("more downloads in queue, continuing")
	}

	totalElapsed := int(time.Since(startTime).Seconds())
	downloadElapsed := totalElapsed - int(totalPPElapsed.Seconds())
	ppSecs := int(totalPPElapsed.Seconds())

	// Summary
	avgSpeed := float64(0)
	if downloadElapsed > 0 {
		avgSpeed = totalMB / float64(downloadElapsed)
	}

	// Build state line: "Complete · 1.2 GB · 45 MB/s avg · unpack 1m 30s"
	stateStr := "Done"
	var stateParts []string
	if totalMB > 0 {
		stateParts = append(stateParts, formatSize(totalMB))
	}
	if avgSpeed > 0 {
		stateParts = append(stateParts, fmt.Sprintf("%.0f MB/s avg", avgSpeed))
	}
	if ppSecs > 0 {
		stateParts = append(stateParts, fmt.Sprintf("unpack %s", formatDuration(ppSecs)))
	}
	if len(stateParts) > 0 {
		stateStr += " · " + strings.Join(stateParts, " · ")
	}

	subtitle := truncate(t.getCompletedName(ctx), 30)

	slog.Info("complete", "total_mb", totalMB, "elapsed", totalElapsed, "pp_secs", ppSecs, "avg_speed_mb", avgSpeed, "state", stateStr, "subtitle", subtitle)

	// Two-phase end: ONGOING with final content → short display → ENDED
	if resumed {
		t.send(ctx, 1.0, stateStr, "checkmark.circle.fill", "green", nil, subtitle, "ENDED")
		slog.Info("tracking complete (resumed, skipping two-phase end)")
	} else {
		endDelay := t.cfg.PushWard.EndDelay
		displayTime := t.cfg.PushWard.EndDisplayTime

		// Phase 1: ONGOING with final content (push-update token delivers it)
		select {
		case <-ctx.Done():
			return
		case <-time.After(endDelay):
		}
		t.send(ctx, 1.0, stateStr, "checkmark.circle.fill", "green", nil, subtitle, "ONGOING")
		slog.Info("two-phase end: sent ONGOING with final content", "display_time", displayTime)

		// Phase 2: ENDED (dismisses Live Activity)
		select {
		case <-ctx.Done():
			return
		case <-time.After(displayTime):
		}
		t.send(ctx, 1.0, stateStr, "checkmark.circle.fill", "green", nil, subtitle, "ENDED")
		slog.Info("tracking complete")
	}
}

// waitForQueueActive polls the queue for up to maxPolls iterations waiting for
// an active download. Returns true if the queue became active.
func (t *Tracker) waitForQueueActive(ctx context.Context, maxPolls int) bool {
	for i := 0; i < maxPolls; i++ {
		queue, err := t.sab.GetQueue(ctx)
		if err != nil {
			slog.Warn("failed to fetch queue", "error", err)
			select {
			case <-ctx.Done():
				return false
			case <-time.After(t.cfg.Polling.Interval):
			}
			continue
		}
		mb, _ := strconv.ParseFloat(queue.MB, 64)
		if queue.Status != "Idle" && mb > 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(t.cfg.Polling.Interval):
		}
	}
	return false
}

// trackDownloads polls the queue until it goes idle. Returns total MB seen.
func (t *Tracker) trackDownloads(ctx context.Context) float64 {
	var totalMB float64
	startTime := time.Now()
	slog.Info("tracking downloads")

	for {
		queue, err := t.sab.GetQueue(ctx)
		if err != nil {
			slog.Warn("failed to fetch queue", "error", err)
			select {
			case <-ctx.Done():
				return totalMB
			case <-time.After(t.cfg.Polling.Interval):
			}
			continue
		}

		queueMB, _ := strconv.ParseFloat(queue.MB, 64)
		if queueMB > totalMB {
			totalMB = queueMB
		}

		if !t.sendDownloadProgress(ctx, queue, startTime) {
			break
		}
		select {
		case <-ctx.Done():
			return totalMB
		case <-time.After(t.cfg.Polling.Interval):
		}
	}

	slog.Info("downloads finished", "total_mb", totalMB)
	return totalMB
}

// trackPostProcessing polls history for active PP statuses and sends updates.
// Returns total post-processing duration.
func (t *Tracker) trackPostProcessing(ctx context.Context) time.Duration {
	slog.Info("tracking post-processing")
	t.send(ctx, 1.0, "Unpacking...", "archivebox", "orange", nil, "", "ONGOING")
	ppStart := time.Now()

	for {
		ppStatus, ppName := t.getPPStatus(ctx)
		if ppStatus == "" {
			break
		}
		icon := ppIcons[ppStatus]
		if icon == "" {
			icon = "archivebox"
		}
		subtitle := truncate(ppName, 30)
		t.send(ctx, 1.0, ppStatus+"...", icon, "orange", nil, subtitle, "ONGOING")
		select {
		case <-ctx.Done():
			return time.Since(ppStart)
		case <-time.After(t.cfg.Polling.Interval):
		}
	}

	elapsed := time.Since(ppStart)
	slog.Info("post-processing finished", "elapsed", elapsed)
	return elapsed
}

func (t *Tracker) sendDownloadProgress(ctx context.Context, queue *sabnzbd.Queue, startTime time.Time) bool {
	status := queue.Status
	mbLeft, _ := strconv.ParseFloat(queue.MBLeft, 64)
	mbTotal, _ := strconv.ParseFloat(queue.MB, 64)
	speedKB, _ := strconv.ParseFloat(queue.KBPerSec, 64)
	speedMB := speedKB / 1024

	if status == "Idle" || (mbLeft <= 0 && mbTotal <= 0) {
		return false
	}

	progress := float64(0)
	if mbTotal > 0 {
		progress = (mbTotal - mbLeft) / mbTotal
	}

	// Build subtitle from current slot filename
	subtitle := formatSize(mbLeft)
	if len(queue.Slots) > 0 {
		name := queue.Slots[0].Filename
		if len(queue.Slots) > 1 {
			subtitle = fmt.Sprintf("%s +%d more", truncate(name, 18), len(queue.Slots)-1)
		} else {
			subtitle = truncate(name, 30)
		}
	}

	if status == "Paused" {
		t.send(ctx, progress, "Paused", "pause.circle.fill", "blue", nil, subtitle, "ONGOING")
		return true
	}

	remainingSeconds := parseTimeLeft(queue.TimeLeft)

	stateStr := fmt.Sprintf("%.1f MB/s", speedMB)
	elapsed := time.Since(startTime).Seconds()
	downloaded := mbTotal - mbLeft
	if elapsed > 2 && downloaded > 0 {
		avgMB := downloaded / elapsed
		stateStr += fmt.Sprintf(" · avg %.0f", avgMB)
	}

	t.send(ctx, progress, stateStr, "arrow.down.circle.fill", "blue", remainingSeconds, subtitle, "ONGOING")
	return true
}

// getPPStatus checks history for active post-processing jobs.
// Returns the status and name, or empty strings if none found.
func (t *Tracker) getPPStatus(ctx context.Context) (string, string) {
	history, err := t.sab.GetHistory(ctx, 5)
	if err != nil {
		slog.Warn("failed to fetch history", "error", err)
		return "", ""
	}
	for _, slot := range history.Slots {
		if ppStatuses[slot.Status] {
			return slot.Status, slot.Name
		}
	}
	return "", ""
}

// getCompletedName returns the name of the most recently completed history item.
func (t *Tracker) getCompletedName(ctx context.Context) string {
	history, err := t.sab.GetHistory(ctx, 5)
	if err != nil {
		return ""
	}
	for _, slot := range history.Slots {
		if slot.Status == "Completed" {
			return slot.Name
		}
	}
	return ""
}

func parseTimeLeft(timeleft string) *int {
	parts := strings.Split(timeleft, ":")
	if len(parts) != 3 {
		return nil
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	s, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil
	}
	total := h*3600 + m*60 + s
	return &total
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

func formatDuration(seconds int) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%dm %ds", minutes, seconds%60)
	}
	hours := minutes / 60
	return fmt.Sprintf("%dh %dm", hours, minutes%60)
}

func formatSize(mb float64) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", mb/1024)
	}
	return fmt.Sprintf("%.0f MB", mb)
}
