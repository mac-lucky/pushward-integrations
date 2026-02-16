package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-docker/sabnzbd/internal/config"
	"github.com/mac-lucky/pushward-docker/sabnzbd/internal/pushward"
	"github.com/mac-lucky/pushward-docker/sabnzbd/internal/sabnzbd"
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
}

func New(cfg *config.Config, sab *sabnzbd.Client, pw *pushward.Client) *Tracker {
	return &Tracker{cfg: cfg, sab: sab, pw: pw}
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
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer func() {
			t.mu.Lock()
			t.active = false
			t.mu.Unlock()
		}()
		t.track()
	}()

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, `{"status":"tracking_started"}`)
}

func (t *Tracker) send(ctx context.Context, progress float64, state, icon string, remainingSeconds *int, subtitle string, activityState string) {
	content := pushward.Content{
		Template:    "generic",
		Progress:    progress,
		State:       state,
		AccentColor: "green",
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

func (t *Tracker) track() {
	ctx := context.Background()

	// Ensure activity exists
	if err := t.pw.CreateActivity(ctx, slug, "SABnzbd", t.cfg.PushWard.Priority); err != nil {
		slog.Error("failed to create activity", "error", err)
		return
	}

	// Phase 1: Wait for SABnzbd to start downloading
	slog.Info("waiting for download to start")
	t.send(ctx, 0.0, "Starting...", "arrow.down.circle", nil, "", "ONGOING")

	if !t.waitForQueueActive(ctx, 60) {
		slog.Warn("SABnzbd never started downloading, giving up")
		t.send(ctx, 0.0, "No downloads", "checkmark.circle.fill", nil, "", "ENDED")
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

	subtitle := fmt.Sprintf("%s in %s", formatSize(totalMB), formatDuration(totalElapsed))
	if ppSecs > 1 {
		subtitle += fmt.Sprintf(" · PP: %s", formatDuration(ppSecs))
	} else {
		subtitle += fmt.Sprintf(" · %.0f MB/s", avgSpeed)
	}
	slog.Info("complete", "total_mb", totalMB, "elapsed", totalElapsed, "pp_secs", ppSecs, "avg_speed_mb", avgSpeed)

	if len(subtitle) > 30 {
		subtitle = subtitle[:30]
	}
	t.send(ctx, 1.0, "Complete", "checkmark.circle.fill", nil, subtitle, "ONGOING")

	slog.Info("waiting before ending activity", "cleanup_delay", t.cfg.PushWard.CleanupDelay)
	time.Sleep(t.cfg.PushWard.CleanupDelay)
	t.send(ctx, 1.0, "Complete", "checkmark.circle.fill", nil, subtitle, "ENDED")
	slog.Info("tracking complete")
}

// waitForQueueActive polls the queue for up to maxPolls iterations waiting for
// an active download. Returns true if the queue became active.
func (t *Tracker) waitForQueueActive(ctx context.Context, maxPolls int) bool {
	for i := 0; i < maxPolls; i++ {
		queue, err := t.sab.GetQueue(ctx)
		if err != nil {
			slog.Warn("failed to fetch queue", "error", err)
			time.Sleep(t.cfg.Polling.Interval)
			continue
		}
		mb, _ := strconv.ParseFloat(queue.MB, 64)
		if queue.Status != "Idle" && mb > 0 {
			return true
		}
		time.Sleep(t.cfg.Polling.Interval)
	}
	return false
}

// trackDownloads polls the queue until it goes idle. Returns total MB seen.
func (t *Tracker) trackDownloads(ctx context.Context) float64 {
	var totalMB float64
	slog.Info("tracking downloads")

	for {
		queue, err := t.sab.GetQueue(ctx)
		if err != nil {
			slog.Warn("failed to fetch queue", "error", err)
			time.Sleep(t.cfg.Polling.Interval)
			continue
		}

		queueMB, _ := strconv.ParseFloat(queue.MB, 64)
		if queueMB > totalMB {
			totalMB = queueMB
		}

		if !t.sendDownloadProgress(ctx, queue) {
			break
		}
		time.Sleep(t.cfg.Polling.Interval)
	}

	slog.Info("downloads finished", "total_mb", totalMB)
	return totalMB
}

// trackPostProcessing polls history for active PP statuses and sends updates.
// Returns total post-processing duration.
func (t *Tracker) trackPostProcessing(ctx context.Context) time.Duration {
	slog.Info("tracking post-processing")
	t.send(ctx, 1.0, "Unpacking...", "archivebox", nil, "", "ONGOING")
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
		t.send(ctx, 1.0, ppStatus+"...", icon, nil, subtitle, "ONGOING")
		time.Sleep(t.cfg.Polling.Interval)
	}

	elapsed := time.Since(ppStart)
	slog.Info("post-processing finished", "elapsed", elapsed)
	return elapsed
}

func (t *Tracker) sendDownloadProgress(ctx context.Context, queue *sabnzbd.Queue) bool {
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
		t.send(ctx, progress, "Paused", "pause.circle.fill", nil, subtitle, "ONGOING")
		return true
	}

	remainingSeconds := parseTimeLeft(queue.TimeLeft)

	stateStr := fmt.Sprintf("%.1f MB/s", speedMB)
	t.send(ctx, progress, stateStr, "arrow.down.circle.fill", remainingSeconds, subtitle, "ONGOING")
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
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
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
