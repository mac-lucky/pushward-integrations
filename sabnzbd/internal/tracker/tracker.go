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
		t.track(r.Context())
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

func (t *Tracker) track(webhookCtx context.Context) {
	// Use a background context so tracking continues after the HTTP request completes.
	ctx := context.Background()

	// Ensure activity exists
	if err := t.pw.CreateActivity(ctx, slug, "SABnzbd", t.cfg.PushWard.Priority); err != nil {
		slog.Error("failed to create activity", "error", err)
		return
	}

	// Phase 1: Wait for SABnzbd to start downloading
	slog.Info("phase 1: waiting for download to start")
	t.send(ctx, 0.0, "Starting...", "arrow.down.circle", nil, "", "ONGOING")

	var totalMB float64
	started := false
	for i := 0; i < 60; i++ {
		queue, err := t.sab.GetQueue(ctx)
		if err != nil {
			slog.Warn("failed to fetch queue", "error", err)
			time.Sleep(t.cfg.Polling.Interval)
			continue
		}
		mb, _ := strconv.ParseFloat(queue.MB, 64)
		if queue.Status != "Idle" && mb > 0 {
			totalMB = mb
			started = true
			break
		}
		time.Sleep(t.cfg.Polling.Interval)
	}

	if !started {
		slog.Warn("SABnzbd never started downloading, giving up")
		t.send(ctx, 0.0, "No downloads", "checkmark.circle.fill", nil, "", "ENDED")
		return
	}

	startTime := time.Now()

	// Phase 2: Track downloading
	slog.Info("phase 2: tracking download", "total_mb", totalMB)
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

	downloadElapsed := int(time.Since(startTime).Seconds())

	// Phase 3: Track post-processing
	slog.Info("phase 3: tracking post-processing")
	t.send(ctx, 1.0, "Unpacking...", "archivebox", nil, "", "ONGOING")
	ppStart := time.Now()

	for {
		ppStatus := t.getPPStatus(ctx)
		if ppStatus == "" {
			break
		}
		icon := ppIcons[ppStatus]
		if icon == "" {
			icon = "archivebox"
		}
		t.send(ctx, 1.0, ppStatus+"...", icon, nil, "", "ONGOING")
		time.Sleep(t.cfg.Polling.Interval)
	}

	ppElapsed := int(time.Since(ppStart).Seconds())
	totalElapsed := int(time.Since(startTime).Seconds())

	// Phase 4: Summary
	avgSpeed := float64(0)
	if downloadElapsed > 0 {
		avgSpeed = totalMB / float64(downloadElapsed)
	}
	summary := fmt.Sprintf("%s in %s", formatSize(totalMB), formatDuration(totalElapsed))
	subtitle := summary
	if ppElapsed > 1 {
		subtitle += fmt.Sprintf(" · Unpack: %s", formatDuration(ppElapsed))
	} else {
		subtitle += fmt.Sprintf(" · Avg: %.0f MB/s", avgSpeed)
	}

	slog.Info("phase 4: complete", "summary", summary)
	if len(subtitle) > 30 {
		subtitle = subtitle[:30]
	}
	t.send(ctx, 1.0, "Complete", "checkmark.circle.fill", nil, subtitle, "ONGOING")

	slog.Info("waiting before ending activity", "cleanup_delay", t.cfg.PushWard.CleanupDelay)
	time.Sleep(t.cfg.PushWard.CleanupDelay)
	t.send(ctx, 1.0, "Complete", "checkmark.circle.fill", nil, "", "ENDED")
	slog.Info("tracking complete")
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

	if status == "Paused" {
		t.send(ctx, progress, "Paused", "pause.circle.fill", nil, formatSize(mbLeft), "ONGOING")
		return true
	}

	var remainingSeconds *int
	parts := strings.Split(queue.TimeLeft, ":")
	if len(parts) == 3 {
		h, err1 := strconv.Atoi(parts[0])
		m, err2 := strconv.Atoi(parts[1])
		s, err3 := strconv.Atoi(parts[2])
		if err1 == nil && err2 == nil && err3 == nil {
			total := h*3600 + m*60 + s
			remainingSeconds = &total
		}
	}

	stateStr := fmt.Sprintf("%.1f MB/s", speedMB)
	t.send(ctx, progress, stateStr, "arrow.down.circle.fill", remainingSeconds, formatSize(mbLeft), "ONGOING")
	return true
}

func (t *Tracker) getPPStatus(ctx context.Context) string {
	history, err := t.sab.GetHistory(ctx, 5)
	if err != nil {
		slog.Warn("failed to fetch history", "error", err)
		return ""
	}
	for _, slot := range history.Slots {
		if ppStatuses[slot.Status] {
			return slot.Status
		}
	}
	return ""
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
