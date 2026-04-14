package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mac-lucky/pushward-integrations/sabnzbd/internal/config"
	"github.com/mac-lucky/pushward-integrations/sabnzbd/internal/sabnzbd"
	sharedauth "github.com/mac-lucky/pushward-integrations/shared/auth"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const (
	slug         = "sabnzbd"
	seriesKey    = "Speed" // timeline values/units/history map key
	avgSeriesKey = "Avg"   // timeline key used in completion summary
)

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
	cfg          *config.Config
	sab          *sabnzbd.Client
	pw           *pushward.Client
	mu           sync.Mutex
	active       bool
	wg           sync.WaitGroup
	ctx          context.Context
	historySent  bool
	shuttingDown atomic.Bool
}

func New(ctx context.Context, cfg *config.Config, sab *sabnzbd.Client, pw *pushward.Client) *Tracker {
	return &Tracker{ctx: ctx, cfg: cfg, sab: sab, pw: pw}
}

// Cleanup ends any stale activity left over from a previous run (e.g. crash).
func (t *Tracker) Cleanup(ctx context.Context) {
	req := pushward.UpdateRequest{
		State:   pushward.StateEnded,
		Content: pushward.Content{Template: t.cfg.SABnzbd.Template, Progress: 0, State: "Dismissed"},
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

// Wait marks the tracker as shutting down and blocks until all active
// tracking goroutines finish. After Wait is called, launchTracker is a no-op.
func (t *Tracker) Wait() {
	t.shuttingDown.Store(true)
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
		if !sharedauth.CheckHeader(r, "X-Webhook-Secret", t.cfg.SABnzbd.WebhookSecret) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	t.mu.Lock()
	if t.active {
		t.mu.Unlock()
		slog.Info("tracking already active, skipping webhook")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, `{"status":"already_tracking"}`)
		return
	}
	t.active = true
	t.mu.Unlock()

	slog.Info("webhook received, starting tracker")
	t.launchTracker(false)

	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, `{"status":"tracking_started"}`)
}

func (t *Tracker) launchTracker(resumed bool) {
	if t.shuttingDown.Load() {
		t.mu.Lock()
		t.active = false
		t.mu.Unlock()
		slog.Info("tracker shutting down, skipping launch")
		return
	}
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

func (t *Tracker) send(ctx context.Context, progress float64, state, icon, accentColor string, remainingSeconds *int, subtitle string, activityState string, value *float64) {
	template := t.cfg.SABnzbd.Template
	content := pushward.Content{
		Template:    template,
		Progress:    progress,
		State:       state,
		AccentColor: accentColor,
	}
	if template == "timeline" && value != nil {
		content.Value = map[string]float64{seriesKey: *value}
		content.Units = map[string]string{seriesKey: "MB/s"}
		t.cfg.SABnzbd.Timeline.Apply(&content)

		// Seed sparkline history on first download update
		if !t.historySent && *value > 0 {
			now := time.Now().Unix()
			content.History = map[string][]pushward.HistoryPoint{
				seriesKey: {
					{T: now - 10, V: *value},
					{T: now - 5, V: *value},
				},
			}
			t.historySent = true
		}
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
	t.historySent = false

	// Ensure activity exists (no ended_ttl so the slug persists)
	staleTTL := int(t.cfg.PushWard.StaleTimeout.Seconds())
	if err := t.pw.CreateActivity(ctx, slug, "SABnzbd", t.cfg.PushWard.Priority, 0, staleTTL); err != nil {
		slog.Error("failed to create activity", "error", err)
		return
	}

	// Phase 1: Wait for SABnzbd to start downloading
	slog.Info("waiting for download to start")
	t.send(ctx, 0.0, "Starting...", "arrow.down.circle", "blue", nil, "", pushward.StateOngoing, nil)

	if !t.waitForQueueActive(ctx, 60) {
		slog.Warn("SABnzbd never started downloading, giving up")
		t.send(ctx, 0.0, "No downloads", "checkmark.circle.fill", "green", nil, "", pushward.StateEnded, nil)
		return
	}

	var totalPPElapsed time.Duration

	// Main loop: download → post-processing → check for more
	for {
		// Download phase
		t.trackDownloads(ctx)

		// Post-processing phase
		ppDuration := t.trackPostProcessing(ctx)
		totalPPElapsed += ppDuration

		// Check if queue has more downloads
		if !t.waitForQueueActive(ctx, 10) {
			break
		}
		slog.Info("more downloads in queue, continuing")
	}

	ppSecs := int(totalPPElapsed.Seconds())

	// Read stats from SABnzbd history API instead of calculating locally
	totalBytes, totalDownloadTime := t.getCompletedStats(ctx)
	totalMB := float64(totalBytes) / (1024 * 1024)

	avgSpeed := float64(0)
	if totalDownloadTime > 0 {
		avgSpeed = totalMB / float64(totalDownloadTime)
	}

	// Build state line: "1.2 GB · 45 MB/s avg · unpack 1m 30s"
	// (icon already conveys "done", no need for a "Done" prefix)
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
	stateStr := strings.Join(stateParts, " · ")

	subtitle := text.Truncate(t.getCompletedName(ctx), 30)

	slog.Info("complete", "total_mb", totalMB, "pp_secs", ppSecs, "avg_speed_mb", avgSpeed, "state", stateStr, "subtitle", subtitle)

	// Build final content with "Avg" series key showing average speed.
	finalContent := pushward.Content{
		Template:    t.cfg.SABnzbd.Template,
		Progress:    1.0,
		State:       stateStr,
		Icon:        "checkmark.circle.fill",
		AccentColor: "green",
		Subtitle:    subtitle,
	}
	if t.cfg.SABnzbd.Template == "timeline" && avgSpeed > 0 {
		finalContent.Value = map[string]float64{avgSeriesKey: avgSpeed}
		finalContent.Units = map[string]string{avgSeriesKey: "MB/s"}
		t.cfg.SABnzbd.Timeline.Apply(&finalContent)
	}

	// Two-phase end: ONGOING with final content → short display → ENDED
	if resumed {
		req := pushward.UpdateRequest{State: pushward.StateEnded, Content: finalContent}
		if err := t.pw.UpdateActivity(ctx, slug, req); err != nil {
			slog.Error("failed to send final update", "error", err)
		}
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
		req := pushward.UpdateRequest{State: pushward.StateOngoing, Content: finalContent}
		if err := t.pw.UpdateActivity(ctx, slug, req); err != nil {
			slog.Error("failed to send final update", "error", err)
		}
		slog.Info("two-phase end: sent ONGOING with final content", "display_time", displayTime)

		// Phase 2: ENDED (dismisses Live Activity)
		select {
		case <-ctx.Done():
			return
		case <-time.After(displayTime):
		}
		req = pushward.UpdateRequest{State: pushward.StateEnded, Content: finalContent}
		if err := t.pw.UpdateActivity(ctx, slug, req); err != nil {
			slog.Error("failed to send final update", "error", err)
		}
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

// trackDownloads polls the queue until it goes idle.
func (t *Tracker) trackDownloads(ctx context.Context) {
	slog.Info("tracking downloads")

	for {
		queue, err := t.sab.GetQueue(ctx)
		if err != nil {
			slog.Warn("failed to fetch queue", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(t.cfg.Polling.Interval):
			}
			continue
		}

		if !t.sendDownloadProgress(ctx, queue) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(t.cfg.Polling.Interval):
		}
	}

	slog.Info("downloads finished")
}

// trackPostProcessing polls history for active PP statuses and sends updates.
// Returns total post-processing duration.
func (t *Tracker) trackPostProcessing(ctx context.Context) time.Duration {
	slog.Info("tracking post-processing")
	t.send(ctx, 1.0, "Unpacking...", "archivebox", "orange", nil, "", pushward.StateOngoing, nil)
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
		subtitle := text.Truncate(ppName, 30)
		stateStr := ppStatus + "..."
		t.send(ctx, 1.0, stateStr, icon, "orange", nil, subtitle, pushward.StateOngoing, nil)
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
			subtitle = fmt.Sprintf("%s +%d more", text.Truncate(name, 18), len(queue.Slots)-1)
		} else {
			subtitle = text.Truncate(name, 30)
		}
	}

	if status == "Paused" {
		t.send(ctx, progress, "Paused", "pause.circle.fill", "blue", nil, subtitle, pushward.StateOngoing, pushward.Float64Ptr(0))
		return true
	}

	remainingSeconds := parseTimeLeft(queue.TimeLeft)
	stateStr := fmt.Sprintf("%.1f MB/s", speedMB)

	t.send(ctx, progress, stateStr, "arrow.down.circle.fill", "blue", remainingSeconds, subtitle, pushward.StateOngoing, pushward.Float64Ptr(speedMB))
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

// getCompletedStats reads the SABnzbd history API and returns aggregate bytes
// and download time (seconds) for recently completed slots.
func (t *Tracker) getCompletedStats(ctx context.Context) (totalBytes int64, totalDownloadTime int) {
	history, err := t.sab.GetHistory(ctx, 10)
	if err != nil {
		slog.Warn("failed to fetch history for stats", "error", err)
		return 0, 0
	}
	for _, slot := range history.Slots {
		if slot.Status == "Completed" {
			totalBytes += slot.Bytes
			totalDownloadTime += slot.DownloadTime
		}
	}
	return
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
