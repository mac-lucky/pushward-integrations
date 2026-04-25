package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/mac-lucky/pushward-integrations/sabnzbd/internal/config"
	"github.com/mac-lucky/pushward-integrations/sabnzbd/internal/sabnzbd"
	sharedauth "github.com/mac-lucky/pushward-integrations/shared/auth"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const (
	slug      = "sabnzbd"
	seriesKey = "Speed" // timeline values/units/history map key

	// Mode keys for the change-detection guard. These are the semantic dedup
	// keys — different from the human-readable state string passed to send(),
	// which for downloads is "%.1f MB/s" and would defeat speed bucketing.
	modeDownloading = "downloading"
	modePaused      = "paused"

	// Change-detection thresholds for the polling loops. Send when any
	// threshold is crossed, otherwise skip until heartbeatInterval elapses so
	// the server's stale_ttl doesn't auto-end the activity.
	heartbeatInterval  = 30 * time.Second
	progressChangeFrac = 0.02
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
	cfg    *config.Config
	sab    *sabnzbd.Client
	pw     *pushward.Client
	mu     sync.Mutex // guards active
	active bool
	wg     sync.WaitGroup
	// historySent, maxSlots and the last* fields are owned by the tracker
	// goroutine; do not access from other goroutines.
	historySent  bool
	maxSlots     int
	lastProgress float64
	lastSpeedMB  float64
	lastMode     string
	lastSubtitle string
	lastSendTime time.Time
	shuttingDown atomic.Bool
}

func New(cfg *config.Config, sab *sabnzbd.Client, pw *pushward.Client) *Tracker {
	return &Tracker{cfg: cfg, sab: sab, pw: pw}
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
func (t *Tracker) ResumeIfActive(ctx context.Context) bool {
	queue, err := t.sab.GetQueue(ctx)
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
		t.launchTracker(ctx, true)
		return true
	}

	ppStatus, _ := t.getPPStatus(ctx)
	if ppStatus != "" {
		slog.Info("active post-processing found on startup, resuming tracking", "status", ppStatus)
		t.mu.Lock()
		t.active = true
		t.mu.Unlock()
		t.launchTracker(ctx, true)
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

// WebhookHandler returns the HTTP handler for POST /webhook. The returned
// handler launches tracking goroutines on the provided lifecycle context — not
// the request context, which is cancelled when the HTTP response completes.
func (t *Tracker) WebhookHandler(lifecycleCtx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		t.launchTracker(lifecycleCtx, false)

		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, `{"status":"tracking_started"}`)
	}
}

func (t *Tracker) launchTracker(ctx context.Context, resumed bool) {
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
		t.track(ctx, resumed)
	}()
}

func (t *Tracker) applyTimelineSpeed(content *pushward.Content, sample float64) {
	content.Value = map[string]float64{seriesKey: sample}
	content.Units = map[string]string{seriesKey: "MB/s"}
	t.cfg.SABnzbd.Timeline.Apply(content)
}

// timelineSample returns the sample value to emit for timeline ticks. Non-
// download callers pass value=nil; the server requires a labeled value map
// on every timeline update, so we substitute 0.
func timelineSample(value *float64) float64 {
	if value != nil {
		return *value
	}
	return 0
}

// seedHistory returns a bootstrap history for the first non-zero sample, or
// nil when history has already been seeded or sample is 0. Flips t.historySent
// on first emission.
func (t *Tracker) seedHistory(sample float64) map[string][]pushward.HistoryPoint {
	if t.historySent || sample <= 0 {
		return nil
	}
	t.historySent = true
	now := time.Now().Unix()
	return map[string][]pushward.HistoryPoint{
		seriesKey: {
			{T: now - 10, V: sample},
			{T: now - 5, V: sample},
		},
	}
}

// positiveRemaining passes a pointer through only when it points to a positive
// value, so zero/negative remaining times don't land in the payload.
func positiveRemaining(p *int) *int {
	if p == nil || *p <= 0 {
		return nil
	}
	return p
}

func (t *Tracker) sendSeed(ctx context.Context, progress float64, state, icon, accentColor string, remainingSeconds *int, subtitle string, activityState string, value *float64) error {
	template := t.cfg.SABnzbd.Template
	content := pushward.Content{
		Template:      template,
		Progress:      progress,
		State:         state,
		Icon:          icon,
		Subtitle:      subtitle,
		AccentColor:   accentColor,
		RemainingTime: positiveRemaining(remainingSeconds),
	}
	if template == pushward.TemplateTimeline {
		sample := timelineSample(value)
		t.applyTimelineSpeed(&content, sample)
		content.History = t.seedHistory(sample)
	}

	return t.pw.UpdateActivity(ctx, slug, pushward.UpdateRequest{
		State:   activityState,
		Content: content,
	})
}

func (t *Tracker) send(ctx context.Context, progress float64, state, icon, accentColor string, remainingSeconds *int, subtitle string, activityState string, value *float64) {
	template := t.cfg.SABnzbd.Template
	contentPatch := &pushward.ContentPatch{
		Progress:      pushward.Float64Ptr(progress),
		State:         pushward.StringPtr(state),
		AccentColor:   pushward.StringPtr(accentColor),
		RemainingTime: positiveRemaining(remainingSeconds),
	}
	if icon != "" {
		contentPatch.Icon = pushward.StringPtr(icon)
	}
	if subtitle != "" {
		contentPatch.Subtitle = pushward.StringPtr(subtitle)
	}
	if template == pushward.TemplateTimeline {
		sample := timelineSample(value)
		contentPatch.Value = map[string]float64{seriesKey: sample}
		contentPatch.History = t.seedHistory(sample)
	}

	if err := t.pw.PatchActivity(ctx, slug, pushward.PatchRequest{
		State:   activityState,
		Content: contentPatch,
	}); err != nil {
		slog.Error("failed to send update", "error", err)
	}
}

func (t *Tracker) track(ctx context.Context, resumed bool) {
	t.historySent = false
	t.maxSlots = 0
	t.lastProgress = 0
	t.lastSpeedMB = 0
	t.lastMode = ""
	t.lastSubtitle = ""
	t.lastSendTime = time.Time{}

	sessionStart := time.Now()

	// Ensure activity exists (no ended_ttl so the slug persists)
	staleTTL := int(t.cfg.PushWard.StaleTimeout.Seconds())
	if err := t.pw.CreateActivity(ctx, slug, "SABnzbd", t.cfg.PushWard.Priority, 0, staleTTL); err != nil {
		slog.Error("failed to create activity", "error", err)
		return
	}

	// Phase 1: Wait for SABnzbd to start downloading. The "Starting..." frame
	// is the session seed — subsequent ticks merge-patch against it.
	slog.Info("waiting for download to start")
	if err := t.sendSeed(ctx, 0.0, "Starting...", "arrow.down.circle", pushward.ColorBlue, nil, "", pushward.StateOngoing, nil); err != nil {
		slog.Error("failed to seed activity", "error", err)
		return
	}

	if !t.waitForQueueActive(ctx, 12) {
		slog.Warn("SABnzbd never started downloading, giving up")
		t.send(ctx, 0.0, "No downloads", "checkmark.circle.fill", pushward.ColorGreen, nil, "", pushward.StateEnded, nil)
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

	totalBytes, totalDownloadTime, latestName := t.getCompletedSummary(ctx, sessionStart)
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

	subtitle := text.Truncate(latestName, 30)

	slog.Info("complete", "total_mb", totalMB, "pp_secs", ppSecs, "avg_speed_mb", avgSpeed, "state", stateStr, "subtitle", subtitle)

	// Keep the "Speed" series so the server retains the accumulated download
	// history. Switching series keys here would cause AccumulateHistory to
	// prune the prior series and leave the chart with only the final points.
	// Download is done, so the final sample is 0 — chart tapers to zero.
	finalContent := pushward.Content{
		Template:    t.cfg.SABnzbd.Template,
		Progress:    1.0,
		State:       stateStr,
		Icon:        "checkmark.circle.fill",
		AccentColor: pushward.ColorGreen,
		Subtitle:    subtitle,
	}
	if t.cfg.SABnzbd.Template == pushward.TemplateTimeline {
		t.applyTimelineSpeed(&finalContent, 0)
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
	interval := t.cfg.Polling.Interval
	maxBackoff := 5 * interval
	consecutiveErrs := 0

	timer := time.NewTimer(0) // fire immediately on first iteration
	defer timer.Stop()

	for i := 0; i < maxPolls; i++ {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}

		queue, err := t.sab.GetQueue(ctx)
		if err != nil {
			consecutiveErrs++
			backoff := backoffDuration(consecutiveErrs, interval, maxBackoff)
			slog.Warn("failed to fetch queue", "error", err, "backoff", backoff)
			timer.Reset(backoff)
			continue
		}
		consecutiveErrs = 0

		mb, _ := strconv.ParseFloat(queue.MB, 64)
		if queue.Status != "Idle" && mb > 0 {
			return true
		}
		timer.Reset(interval)
	}
	return false
}

// trackDownloads polls the queue until it goes idle.
func (t *Tracker) trackDownloads(ctx context.Context) {
	slog.Info("tracking downloads")

	interval := t.cfg.Polling.Interval
	maxBackoff := 5 * interval
	consecutiveErrs := 0

	timer := time.NewTimer(0) // fire immediately on first iteration
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		queue, err := t.sab.GetQueue(ctx)
		if err != nil {
			consecutiveErrs++
			backoff := backoffDuration(consecutiveErrs, interval, maxBackoff)
			slog.Warn("failed to fetch queue", "error", err, "backoff", backoff)
			timer.Reset(backoff)
			continue
		}
		consecutiveErrs = 0

		if !t.sendDownloadProgress(ctx, queue) {
			break
		}
		timer.Reset(interval)
	}

	slog.Info("downloads finished")
}

// trackPostProcessing polls history for active PP statuses and sends updates.
// Returns total post-processing duration.
func (t *Tracker) trackPostProcessing(ctx context.Context) time.Duration {
	slog.Info("tracking post-processing")
	t.send(ctx, 1.0, "Unpacking...", "archivebox", pushward.ColorOrange, nil, "", pushward.StateOngoing, nil)
	ppStart := time.Now()

	timer := time.NewTimer(t.cfg.Polling.Interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return time.Since(ppStart)
		case <-timer.C:
		}

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
		if t.shouldSend(1.0, 0, ppStatus, subtitle) {
			t.send(ctx, 1.0, stateStr, icon, pushward.ColorOrange, nil, subtitle, pushward.StateOngoing, nil)
			t.recordSent(1.0, 0, ppStatus, subtitle)
		}
		timer.Reset(t.cfg.Polling.Interval)
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

	// Build subtitle from current slot filename. When multiple downloads are
	// queued, show "X/Y · filename" where X is the current job's position
	// (computed from the max slot count observed across polls, since SABnzbd's
	// queue is FIFO and slot[0] is the one being downloaded).
	subtitle := formatSize(mbLeft)
	if len(queue.Slots) > 0 {
		if len(queue.Slots) > t.maxSlots {
			t.maxSlots = len(queue.Slots)
		}
		name := queue.Slots[0].Filename
		if t.maxSlots > 1 {
			current := t.maxSlots - len(queue.Slots) + 1
			prefix := fmt.Sprintf("%d/%d · ", current, t.maxSlots)
			subtitle = prefix + text.Truncate(name, 30-utf8.RuneCountInString(prefix))
		} else {
			subtitle = text.Truncate(name, 30)
		}
	}

	if status == "Paused" {
		if !t.shouldSend(progress, 0, modePaused, subtitle) {
			return true
		}
		t.send(ctx, progress, "Paused", "pause.circle.fill", pushward.ColorBlue, nil, subtitle, pushward.StateOngoing, pushward.Float64Ptr(0))
		t.recordSent(progress, 0, modePaused, subtitle)
		return true
	}

	remainingSeconds := parseTimeLeft(queue.TimeLeft)
	stateStr := fmt.Sprintf("%.1f MB/s", speedMB)

	// Skip redundant polls on a steady download. Heartbeat every heartbeatInterval
	// so the server's stale_ttl doesn't auto-end the activity, and so the capped
	// MaxHistoryStorage covers a longer wall-clock window.
	if !t.shouldSend(progress, speedMB, modeDownloading, subtitle) {
		return true
	}

	t.send(ctx, progress, stateStr, "arrow.down.circle.fill", pushward.ColorBlue, remainingSeconds, subtitle, pushward.StateOngoing, pushward.Float64Ptr(speedMB))
	t.recordSent(progress, speedMB, modeDownloading, subtitle)
	return true
}

func (t *Tracker) shouldSend(progress, speedMB float64, mode, subtitle string) bool {
	if t.lastSendTime.IsZero() {
		return true
	}
	if mode != t.lastMode {
		return true
	}
	if subtitle != t.lastSubtitle {
		return true
	}
	if math.Round(speedMB) != math.Round(t.lastSpeedMB) {
		return true
	}
	if math.Abs(progress-t.lastProgress) >= progressChangeFrac {
		return true
	}
	return time.Since(t.lastSendTime) >= heartbeatInterval
}

func (t *Tracker) recordSent(progress, speedMB float64, mode, subtitle string) {
	t.lastProgress = progress
	t.lastSpeedMB = speedMB
	t.lastMode = mode
	t.lastSubtitle = subtitle
	t.lastSendTime = time.Now()
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

// getCompletedSummary reads the SABnzbd history API once and returns aggregate
// bytes, download time (seconds), and the most recently completed item's name
// for slots completed in the current session.
func (t *Tracker) getCompletedSummary(ctx context.Context, sessionStart time.Time) (totalBytes int64, totalDownloadTime int, latestName string) {
	history, err := t.sab.GetHistory(ctx, 10)
	if err != nil {
		slog.Warn("failed to fetch history for stats", "error", err)
		return 0, 0, ""
	}
	cutoff := sessionStart.Unix()
	for _, slot := range history.Slots {
		if slot.Status != "Completed" || slot.Completed < cutoff {
			continue
		}
		totalBytes += slot.Bytes
		totalDownloadTime += slot.DownloadTime
		if latestName == "" {
			latestName = slot.Name
		}
	}
	return
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

// backoffDuration returns an exponentially increasing duration with ±10% jitter,
// capped at maxBackoff. n is the number of consecutive errors (1-indexed).
func backoffDuration(n int, base, maxBackoff time.Duration) time.Duration {
	shift := n - 1
	if shift > 10 {
		shift = 10
	}
	d := base * (1 << shift)
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	jitter := time.Duration(rand.Float64()*0.2*float64(d)) - d/10 // #nosec G404 -- backoff jitter doesn't need cryptographic randomness
	d += jitter
	if d < base {
		d = base
	}
	return d
}
