package jellyfin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// Handler processes Jellyfin webhooks for multiple tenants.
type Handler struct {
	store      state.Store
	clients    *client.Pool
	config     *config.JellyfinConfig
	mu         sync.Mutex
	timers     map[string]*time.Timer // "userKey:mapKey" → end timer
	lastUpdate map[string]time.Time   // "userKey:slug" → last progress update time
}

// NewHandler creates a new Jellyfin webhook handler.
func NewHandler(store state.Store, clients *client.Pool, cfg *config.JellyfinConfig) *Handler {
	return &Handler{
		store:      store,
		clients:    clients,
		config:     cfg,
		timers:     make(map[string]*time.Timer),
		lastUpdate: make(map[string]time.Time),
	}
}

func playbackSlug(itemID, userName string) string {
	h := sha256.Sum256([]byte(itemID + userName))
	return fmt.Sprintf("jellyfin-%x", h[:5])
}

func itemSlug(itemID string) string {
	h := sha256.Sum256([]byte(itemID))
	return fmt.Sprintf("jellyfin-item-%x", h[:4])
}

func taskSlug(taskName string) string {
	h := sha256.Sum256([]byte(taskName))
	return fmt.Sprintf("jellyfin-task-%x", h[:4])
}

func authSlug(userName, remoteEndPoint string) string {
	h := sha256.Sum256([]byte(userName + remoteEndPoint))
	return fmt.Sprintf("jellyfin-auth-%x", h[:4])
}

func mediaName(p *webhookPayload) string {
	if p.SeriesName != "" {
		return p.SeriesName
	}
	return p.Name
}

func playbackSubtitle(p *webhookPayload) string {
	if p.SeriesName != "" {
		return fmt.Sprintf("Jellyfin \u00b7 S%02dE%02d \u00b7 %s", p.SeasonNumber, p.EpisodeNumber, p.Name)
	}
	if p.ProductionYear > 0 {
		return fmt.Sprintf("Jellyfin \u00b7 %d \u00b7 %s", p.ProductionYear, p.UserName)
	}
	return fmt.Sprintf("Jellyfin \u00b7 %s", p.UserName)
}

func playbackProgress(p *webhookPayload) float64 {
	if p.RunTimeTicks <= 0 {
		return 0
	}
	return float64(p.PlaybackPositionTicks) / float64(p.RunTimeTicks)
}

func remainingSeconds(p *webhookPayload) int {
	if p.RunTimeTicks <= 0 {
		return 0
	}
	return int((p.RunTimeTicks - p.PlaybackPositionTicks) / 10_000_000)
}

// ServeHTTP handles incoming Jellyfin webhook requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload webhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		slog.Error("failed to decode webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	userKey := auth.KeyFromContext(ctx)

	switch payload.NotificationType {
	case "PlaybackStart":
		h.handlePlaybackStart(ctx, userKey, &payload)
	case "PlaybackProgress":
		h.handlePlaybackProgress(ctx, userKey, &payload)
	case "PlaybackStop":
		h.handlePlaybackStop(ctx, userKey, &payload)
	case "ItemAdded":
		h.handleItemAdded(ctx, userKey, &payload)
	case "ScheduledTaskStarted":
		h.handleTaskStarted(ctx, userKey, &payload)
	case "ScheduledTaskCompleted":
		h.handleTaskCompleted(ctx, userKey, &payload)
	case "AuthenticationFailure":
		h.handleAuthFailure(ctx, userKey, &payload)
	case "GenericUpdateNotification":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "jellyfin"); err != nil {
			slog.Error("test notification failed", "provider", "jellyfin", "error", err)
		}
	default:
		slog.Warn("unknown notification type", "type", payload.NotificationType)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handlePlaybackStart(ctx context.Context, userKey string, p *webhookPayload) {
	slug := playbackSlug(p.ItemID, p.UserName)
	mapKey := "playback:" + p.ItemID + ":" + p.UserName

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	name := mediaName(p)
	if err := cl.CreateActivity(ctx, slug, name, h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "slug", slug, "error", err)
		return
	}
	slog.Info("created activity", "slug", slug, "name", name)

	// Store in state store
	data, _ := json.Marshal(map[string]string{"slug": slug})
	_ = h.store.Set(ctx, "jellyfin", userKey, mapKey, "", data, h.config.StaleTimeout)

	remaining := remainingSeconds(p)
	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:      "generic",
			Progress:      playbackProgress(p),
			State:         "Playing on " + p.DeviceName,
			Icon:          "play.circle.fill",
			Subtitle:      playbackSubtitle(p),
			AccentColor:   "#007AFF",
			RemainingTime: &remaining,
		},
	}

	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}

	// Record last update time for debounce
	h.mu.Lock()
	h.lastUpdate[userKey+":"+slug] = time.Now()
	h.mu.Unlock()

	slog.Info("updated activity", "slug", slug, "state", "Playing on "+p.DeviceName)
}

func (h *Handler) handlePlaybackProgress(ctx context.Context, userKey string, p *webhookPayload) {
	slug := playbackSlug(p.ItemID, p.UserName)

	// Debounce check
	debounceKey := userKey + ":" + slug
	h.mu.Lock()
	last, ok := h.lastUpdate[debounceKey]
	if ok && time.Since(last) < h.config.ProgressDebounce {
		h.mu.Unlock()
		return
	}
	h.lastUpdate[debounceKey] = time.Now()
	h.mu.Unlock()

	cl := h.clients.Get(userKey)
	remaining := remainingSeconds(p)
	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:      "generic",
			Progress:      playbackProgress(p),
			State:         "Playing on " + p.DeviceName,
			Icon:          "play.circle.fill",
			Subtitle:      playbackSubtitle(p),
			AccentColor:   "#007AFF",
			RemainingTime: &remaining,
		},
	}

	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity (progress)", "slug", slug)
}

func (h *Handler) handlePlaybackStop(ctx context.Context, userKey string, p *webhookPayload) {
	slug := playbackSlug(p.ItemID, p.UserName)
	mapKey := "playback:" + p.ItemID + ":" + p.UserName

	content := pushward.Content{
		Template:    "generic",
		Progress:    1.0,
		State:       "Watched on " + p.DeviceName,
		Icon:        "checkmark.circle.fill",
		Subtitle:    playbackSubtitle(p),
		AccentColor: "#34C759",
	}

	h.scheduleEnd(userKey, mapKey, slug, content)
	slog.Info("scheduled end", "slug", slug, "state", "Watched on "+p.DeviceName)
}

func (h *Handler) handleItemAdded(ctx context.Context, userKey string, p *webhookPayload) {
	slug := itemSlug(p.ItemID)
	mapKey := "item:" + p.ItemID

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	name := mediaName(p)
	if err := cl.CreateActivity(ctx, slug, name, h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "slug", slug, "error", err)
		return
	}
	slog.Info("created activity", "slug", slug, "name", name)

	subtitle := "Jellyfin"
	if p.ProductionYear > 0 {
		subtitle = fmt.Sprintf("Jellyfin \u00b7 %d", p.ProductionYear)
	}

	step := 1
	total := 1
	content := pushward.Content{
		Template:    "pipeline",
		Progress:    1.0,
		State:       "Added to library",
		Icon:        "plus.circle.fill",
		Subtitle:    subtitle,
		AccentColor: "#34C759",
		CurrentStep: &step,
		TotalSteps:  &total,
		StepLabels:  []string{"Added"},
	}

	// Send ONGOING update first
	req := pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}

	// Immediate two-phase end
	h.scheduleEnd(userKey, mapKey, slug, content)
	slog.Info("scheduled end", "slug", slug, "state", "Added to library")
}

func (h *Handler) handleTaskStarted(ctx context.Context, userKey string, p *webhookPayload) {
	slug := taskSlug(p.TaskName)
	mapKey := "task:" + p.TaskName

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, p.TaskName, h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "slug", slug, "error", err)
		return
	}
	slog.Info("created activity", "slug", slug, "name", p.TaskName)

	// Store in state store
	data, _ := json.Marshal(map[string]string{"slug": slug})
	_ = h.store.Set(ctx, "jellyfin", userKey, mapKey, "", data, h.config.StaleTimeout)

	step := 1
	total := 2
	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:    "pipeline",
			Progress:    0,
			State:       "Running...",
			Icon:        "arrow.triangle.2.circlepath",
			Subtitle:    "Jellyfin \u00b7 " + p.TaskName,
			AccentColor: "#007AFF",
			CurrentStep: &step,
			TotalSteps:  &total,
			StepLabels:  []string{"Running", "Done"},
		},
	}

	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "step", "1/2", "state", "Running...")
}

func (h *Handler) handleTaskCompleted(ctx context.Context, userKey string, p *webhookPayload) {
	slug := taskSlug(p.TaskName)
	mapKey := "task:" + p.TaskName

	stateText := "Complete"
	icon := "checkmark.circle.fill"
	accent := "#34C759"
	if p.TaskResult != "Completed" {
		stateText = "Failed"
		icon = "xmark.circle.fill"
		accent = "#FF3B30"
	}

	step := 2
	total := 2
	content := pushward.Content{
		Template:    "pipeline",
		Progress:    1.0,
		State:       stateText,
		Icon:        icon,
		Subtitle:    "Jellyfin \u00b7 " + p.TaskName,
		AccentColor: accent,
		CurrentStep: &step,
		TotalSteps:  &total,
		StepLabels:  []string{"Running", "Done"},
	}

	h.scheduleEnd(userKey, mapKey, slug, content)
	slog.Info("scheduled end", "slug", slug, "step", "2/2", "state", stateText)
}

func (h *Handler) handleAuthFailure(ctx context.Context, userKey string, p *webhookPayload) {
	slug := authSlug(p.UserName, p.RemoteEndPoint)

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, "Auth Failure", h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "slug", slug, "error", err)
		return
	}
	slog.Info("created activity", "slug", slug)

	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       "Failed login: " + p.UserName + " from " + p.RemoteEndPoint,
			Icon:        "lock.shield.fill",
			Subtitle:    "Jellyfin",
			AccentColor: "#FF3B30",
			Severity:    "warning",
		},
	}

	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("auth failure", "slug", slug, "user", p.UserName, "remote", p.RemoteEndPoint)
}

// scheduleEnd schedules a two-phase end for an activity:
//   - Phase 1 (after EndDelay): ONGOING update with final content
//   - Phase 2 (EndDisplayTime later): ENDED with same content
func (h *Handler) scheduleEnd(userKey, mapKey, slug string, content pushward.Content) {
	timerKey := userKey + ":" + mapKey
	cl := h.clients.Get(userKey)
	endDelay := h.config.EndDelay
	displayTime := h.config.EndDisplayTime

	h.mu.Lock()
	if existing, ok := h.timers[timerKey]; ok {
		existing.Stop()
	}
	h.timers[timerKey] = time.AfterFunc(endDelay, func() {
		// Phase 1: ONGOING with final content
		ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
		ongoingReq := pushward.UpdateRequest{
			State:   pushward.StateOngoing,
			Content: content,
		}
		if err := cl.UpdateActivity(ctx1, slug, ongoingReq); err != nil {
			slog.Error("failed to update activity (end phase 1)", "slug", slug, "error", err)
		} else {
			slog.Info("updated activity", "slug", slug, "state", content.State)
		}
		cancel1()

		time.Sleep(displayTime)

		// Phase 2: ENDED with same content
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel2()
		endedReq := pushward.UpdateRequest{
			State:   pushward.StateEnded,
			Content: content,
		}
		if err := cl.UpdateActivity(ctx2, slug, endedReq); err != nil {
			slog.Error("failed to end activity (end phase 2)", "slug", slug, "error", err)
		} else {
			slog.Info("ended activity", "slug", slug, "state", content.State)
		}

		// Clean up state store and timer
		delCtx, delCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer delCancel()
		_ = h.store.Delete(delCtx, "jellyfin", userKey, mapKey, "")

		h.mu.Lock()
		delete(h.timers, timerKey)
		// Clean up debounce entry for playback slugs
		delete(h.lastUpdate, userKey+":"+slug)
		h.mu.Unlock()
	})
	h.mu.Unlock()
}
