package jellyfin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

type Handler struct {
	store        state.Store
	clients      *client.Pool
	config       *config.JellyfinConfig
	ender        *lifecycle.Ender
	mu           sync.Mutex
	pauseTimers  map[string]*time.Timer // debounceKey → pause auto-end timer
	lastUpdate   map[string]time.Time   // "userKey:slug" → last progress update time
	lastPaused   map[string]bool        // "userKey:slug" → last IsPaused state
	lastProgress map[string]float64     // "userKey:slug" → last progress value
}

// RegisterRoutes registers the Jellyfin webhook endpoint and returns the Handler.
func RegisterRoutes(api huma.API, store state.Store, clients *client.Pool, cfg *config.JellyfinConfig) *Handler {
	h := &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "jellyfin", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
		pauseTimers:  make(map[string]*time.Timer),
		lastUpdate:   make(map[string]time.Time),
		lastPaused:   make(map[string]bool),
		lastProgress: make(map[string]float64),
	}
	humautil.RegisterWebhook(api, "/jellyfin", "post-jellyfin-webhook",
		"Receive Jellyfin webhook",
		"Processes Jellyfin playback and library events.",
		[]string{"Jellyfin"}, h.handleWebhook)
	return h
}

func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

// StartCleanup starts a background goroutine that periodically removes stale
// entries from debounce maps (lastUpdate, lastPaused, lastProgress, pauseTimers).
func (h *Handler) StartCleanup(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(h.config.StaleTimeout)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.mu.Lock()
				now := time.Now()
				for key, last := range h.lastUpdate {
					if now.Sub(last) > h.config.StaleTimeout {
						delete(h.lastUpdate, key)
						delete(h.lastPaused, key)
						delete(h.lastProgress, key)
						if t, ok := h.pauseTimers[key]; ok {
							t.Stop()
							delete(h.pauseTimers, key)
						}
					}
				}
				h.mu.Unlock()
			}
		}
	}()
}

func playbackSlug(itemID, userName string) string {
	return text.SlugHash("jellyfin", itemID+userName, 5)
}

func mediaName(p *jellyfinPayload) string {
	if p.SeriesName != "" {
		return p.SeriesName
	}
	return p.Name
}

func playbackSubtitle(p *jellyfinPayload) string {
	if p.SeriesName != "" {
		return fmt.Sprintf("Jellyfin \u00b7 S%02dE%02d \u00b7 %s", p.SeasonNumber, p.EpisodeNumber, p.Name)
	}
	if p.ProductionYear > 0 {
		return fmt.Sprintf("Jellyfin \u00b7 %d \u00b7 %s", p.ProductionYear, p.UserName)
	}
	return fmt.Sprintf("Jellyfin \u00b7 %s", p.UserName)
}

func playbackProgress(p *jellyfinPayload) float64 {
	if p.RunTimeTicks <= 0 {
		return 0
	}
	return float64(p.PlaybackPositionTicks) / float64(p.RunTimeTicks)
}

func remainingSeconds(p *jellyfinPayload) int {
	if p.RunTimeTicks <= 0 {
		return 0
	}
	return max(int((p.RunTimeTicks-p.PlaybackPositionTicks)/10_000_000), 0)
}

func (h *Handler) handleWebhook(ctx context.Context, input *struct {
	Body jellyfinPayload
}) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, "jellyfin")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	payload := &input.Body

	var apiErr error
	switch payload.NotificationType {
	case "PlaybackStart":
		apiErr = h.handlePlaybackStart(ctx, userKey, log, payload)
	case "PlaybackProgress":
		apiErr = h.handlePlaybackProgress(ctx, userKey, log, payload)
	case "PlaybackStop":
		h.handlePlaybackStop(ctx, userKey, log, payload)
	case "ItemAdded":
		apiErr = h.handleItemAdded(ctx, userKey, log, payload)
	case "ScheduledTaskStarted":
		apiErr = h.handleTaskStarted(ctx, userKey, log, payload)
	case "ScheduledTaskCompleted":
		apiErr = h.handleTaskCompleted(ctx, userKey, log, payload)
	case "AuthenticationFailure":
		apiErr = h.handleAuthFailure(ctx, userKey, log, payload)
	case "GenericUpdateNotification":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "jellyfin"); err != nil {
			log.Error("test notification failed", "provider", "jellyfin", "error", err)
		}
	default:
		log.Warn("unknown notification type", "type", payload.NotificationType)
	}

	if apiErr != nil {
		return nil, huma.Error502BadGateway("upstream API error")
	}
	return humautil.NewOK(), nil
}

func (h *Handler) handlePlaybackStart(ctx context.Context, userKey string, log *slog.Logger, p *jellyfinPayload) error {
	slug := playbackSlug(p.ItemID, p.UserName)

	// Skip paused starts �� Jellyfin fires PlaybackStart with IsPaused=true
	// for stale sessions, causing false-positive activities. Record debounce
	// state so a real resume (IsPaused=false) triggers late-join creation.
	if p.IsPaused {
		debounceKey := userKey + ":" + slug
		h.mu.Lock()
		h.lastPaused[debounceKey] = true
		h.lastProgress[debounceKey] = playbackProgress(p)
		h.lastUpdate[debounceKey] = time.Now()
		h.mu.Unlock()
		log.Info("skipped paused playback start", "slug", slug)
		return nil
	}

	mapKey := "playback:" + p.ItemID + ":" + p.UserName

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	name := mediaName(p)
	if err := cl.CreateActivity(ctx, slug, name, h.config.Priority, endedTTL, staleTTL); err != nil {
		log.Error("failed to create activity", "slug", slug, "error", err)
		return err
	}
	log.Info("created activity", "slug", slug, "name", name)

	// Store in state store
	data, _ := json.Marshal(map[string]string{"slug": slug})
	if err := h.store.Set(ctx, "jellyfin", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		log.Warn("state store write failed", "error", err, "provider", "jellyfin", "slug", slug)
	}

	remaining := remainingSeconds(p)
	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:      "generic",
			Progress:      playbackProgress(p),
			State:         "Playing on " + p.DeviceName + " by " + p.UserName,
			Icon:          "play.circle.fill",
			Subtitle:      playbackSubtitle(p),
			AccentColor:   pushward.ColorBlue,
			RemainingTime: &remaining,
		},
	}

	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update activity", "slug", slug, "error", err)
		return err
	}

	// Record last update time and paused state for debounce
	h.mu.Lock()
	debounceKey := userKey + ":" + slug
	h.lastUpdate[debounceKey] = time.Now()
	h.lastPaused[debounceKey] = false
	h.lastProgress[debounceKey] = playbackProgress(p)
	h.mu.Unlock()

	log.Info("updated activity", "slug", slug, "state", "Playing on "+p.DeviceName+" by "+p.UserName)
	return nil
}

func (h *Handler) handlePlaybackProgress(ctx context.Context, userKey string, log *slog.Logger, p *jellyfinPayload) error {
	slug := playbackSlug(p.ItemID, p.UserName)
	mapKey := "playback:" + p.ItemID + ":" + p.UserName

	// Debounce check — bypass on state change, suppress while paused
	debounceKey := userKey + ":" + slug
	h.mu.Lock()
	last, hasLast := h.lastUpdate[debounceKey]
	prevPaused, hasPrev := h.lastPaused[debounceKey]
	stateChanged := hasPrev && prevPaused != p.IsPaused

	// Handle pause→play: cancel pause timer
	if stateChanged && !p.IsPaused {
		if t, ok := h.pauseTimers[debounceKey]; ok {
			t.Stop()
			delete(h.pauseTimers, debounceKey)
		}
	}

	// Suppress all updates while still paused; reset timer on seek
	if hasPrev && prevPaused && p.IsPaused {
		progress := playbackProgress(p)
		if h.config.PauseTimeout > 0 {
			if prev, ok := h.lastProgress[debounceKey]; ok && progress != prev {
				h.lastProgress[debounceKey] = progress
				if t, ok2 := h.pauseTimers[debounceKey]; ok2 {
					t.Stop()
				}
				deviceName := p.DeviceName
				userName := p.UserName
				subtitle := playbackSubtitle(p)
				h.pauseTimers[debounceKey] = time.AfterFunc(h.config.PauseTimeout, func() {
					h.endPaused(userKey, mapKey, slug, deviceName, userName, subtitle, progress, debounceKey)
				})
			}
		}
		h.mu.Unlock()
		return nil
	}

	if hasLast && !stateChanged && time.Since(last) < h.config.ProgressDebounce {
		h.mu.Unlock()
		return nil
	}

	h.lastUpdate[debounceKey] = time.Now()
	h.lastPaused[debounceKey] = p.IsPaused
	progress := playbackProgress(p)
	h.lastProgress[debounceKey] = progress

	// Start pause timer on play→pause or initial pause
	if p.IsPaused && h.config.PauseTimeout > 0 && (stateChanged || !hasPrev) {
		if t, ok := h.pauseTimers[debounceKey]; ok {
			t.Stop()
		}
		deviceName := p.DeviceName
		userName := p.UserName
		subtitle := playbackSubtitle(p)
		h.pauseTimers[debounceKey] = time.AfterFunc(h.config.PauseTimeout, func() {
			h.endPaused(userKey, mapKey, slug, deviceName, userName, subtitle, progress, debounceKey)
		})
	}
	h.mu.Unlock()

	cl := h.clients.Get(userKey)

	// Create activity if it doesn't exist (e.g. PlaybackStart was missed)
	if exists, _ := h.store.Exists(ctx, "jellyfin", userKey, mapKey, ""); !exists {
		// Don't create activity for paused playback — wait for a real play event.
		if p.IsPaused {
			h.mu.Lock()
			if t, ok := h.pauseTimers[debounceKey]; ok {
				t.Stop()
				delete(h.pauseTimers, debounceKey)
			}
			h.mu.Unlock()
			return nil
		}
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		name := mediaName(p)
		if err := cl.CreateActivity(ctx, slug, name, h.config.Priority, endedTTL, staleTTL); err != nil {
			log.Error("failed to create activity", "slug", slug, "error", err)
			return err
		}
		log.Info("created activity (late join)", "slug", slug, "name", name)
		data, _ := json.Marshal(map[string]string{"slug": slug})
		if err := h.store.Set(ctx, "jellyfin", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
			log.Warn("state store write failed", "error", err, "provider", "jellyfin", "slug", slug)
		}
	}

	stateText := "Playing on " + p.DeviceName + " by " + p.UserName
	icon := "play.circle.fill"
	if p.IsPaused {
		stateText = "Paused on " + p.DeviceName + " by " + p.UserName
		icon = "pause.circle.fill"
	}

	remaining := remainingSeconds(p)
	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:      "generic",
			Progress:      playbackProgress(p),
			State:         stateText,
			Icon:          icon,
			Subtitle:      playbackSubtitle(p),
			AccentColor:   pushward.ColorBlue,
			RemainingTime: &remaining,
		},
	}

	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update activity", "slug", slug, "error", err)
		return err
	}
	log.Info("updated activity (progress)", "slug", slug, "paused", p.IsPaused)
	return nil
}

func (h *Handler) handlePlaybackStop(ctx context.Context, userKey string, log *slog.Logger, p *jellyfinPayload) {
	slug := playbackSlug(p.ItemID, p.UserName)
	mapKey := "playback:" + p.ItemID + ":" + p.UserName

	// Cancel pause timer if running
	debounceKey := userKey + ":" + slug
	h.mu.Lock()
	if t, ok := h.pauseTimers[debounceKey]; ok {
		t.Stop()
		delete(h.pauseTimers, debounceKey)
	}
	h.mu.Unlock()

	progress := playbackProgress(p)
	if progress <= 0 {
		// PlaybackStop may lack position ticks; fall back to last known progress.
		h.mu.Lock()
		if prev, ok := h.lastProgress[debounceKey]; ok {
			progress = prev
		}
		h.mu.Unlock()
	}

	content := pushward.Content{
		Template:    "generic",
		Progress:    progress,
		State:       "Watched on " + p.DeviceName + " by " + p.UserName,
		Icon:        "checkmark.circle.fill",
		Subtitle:    playbackSubtitle(p),
		AccentColor: pushward.ColorGreen,
	}

	h.scheduleEnd(userKey, mapKey, slug, content)
	log.Info("scheduled end", "slug", slug, "state", "Watched on "+p.DeviceName+" by "+p.UserName)
}

func (h *Handler) handleItemAdded(ctx context.Context, userKey string, log *slog.Logger, p *jellyfinPayload) error {
	subtitle := "Jellyfin"
	if p.ProductionYear > 0 {
		subtitle = fmt.Sprintf("Jellyfin \u00b7 %d", p.ProductionYear)
	}

	var meta map[string]string
	if p.ProviderTmdb != "" || p.ProviderTvdb != "" {
		meta = make(map[string]string)
		if p.ProviderTmdb != "" {
			meta["tmdb_id"] = p.ProviderTmdb
		}
		if p.ProviderTvdb != "" {
			meta["tvdb_id"] = p.ProviderTvdb
		}
	}

	return h.clients.SendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title:      mediaName(p),
		Subtitle:   subtitle,
		Body:       "Added to library",
		ThreadID:   "jellyfin",
		CollapseID: "jellyfin-item-" + p.ItemID,
		Level:      pushward.LevelPassive,
		Category:   "item-added",
		Source:     "jellyfin",
		Push:       true,
		Metadata:   meta,
	})
}

func (h *Handler) handleTaskStarted(ctx context.Context, userKey string, log *slog.Logger, p *jellyfinPayload) error {
	return h.clients.SendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title:      p.TaskName,
		Subtitle:   "Jellyfin",
		Body:       "Started",
		ThreadID:   "jellyfin-tasks",
		CollapseID: "jellyfin-task-" + p.TaskName,
		Level:      pushward.LevelPassive,
		Category:   "task-started",
		Source:     "jellyfin",
		Push:       true,
	})
}

func (h *Handler) handleTaskCompleted(ctx context.Context, userKey string, log *slog.Logger, p *jellyfinPayload) error {
	body := "Complete"
	level := pushward.LevelPassive
	if p.TaskResult != "Completed" {
		body = "Failed"
		level = pushward.LevelActive
	}

	return h.clients.SendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title:      p.TaskName,
		Subtitle:   "Jellyfin",
		Body:       body,
		ThreadID:   "jellyfin-tasks",
		CollapseID: "jellyfin-task-" + p.TaskName,
		Level:      level,
		Category:   "task-completed",
		Source:     "jellyfin",
		Push:       true,
	})
}

func (h *Handler) handleAuthFailure(ctx context.Context, userKey string, log *slog.Logger, p *jellyfinPayload) error {
	return h.clients.SendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title:      "Auth Failure",
		Subtitle:   "Jellyfin",
		Body:       "Failed login: " + text.TruncateHard(p.UserName, 40) + " from " + text.TruncateHard(p.RemoteEndPoint, 40),
		ThreadID:   "jellyfin-security",
		CollapseID: "jellyfin-auth",
		Level:      pushward.LevelActive,
		Category:   "auth-failure",
		Source:     "jellyfin",
		Push:       true,
		Metadata:   map[string]string{"user": p.UserName, "remote": p.RemoteEndPoint},
	})
}

// scheduleEnd schedules a two-phase end for an activity via lifecycle.Ender,
// with an onComplete callback that cleans up debounce state.
func (h *Handler) scheduleEnd(userKey, mapKey, slug string, content pushward.Content) {
	debounceKey := userKey + ":" + slug
	h.ender.ScheduleEnd(userKey, mapKey, slug, content, func() {
		// Clean up debounce entries for playback slugs
		h.mu.Lock()
		delete(h.lastUpdate, debounceKey)
		delete(h.lastPaused, debounceKey)
		delete(h.lastProgress, debounceKey)
		if pt, ok := h.pauseTimers[debounceKey]; ok {
			pt.Stop()
			delete(h.pauseTimers, debounceKey)
		}
		h.mu.Unlock()
	})
}

// endPaused is called when the pause timer fires — auto-ends the activity
// because it has been paused with no progress change.
func (h *Handler) endPaused(userKey, mapKey, slug, deviceName, userName, subtitle string, progress float64, debounceKey string) {
	h.mu.Lock()
	delete(h.pauseTimers, debounceKey)
	h.mu.Unlock()

	content := pushward.Content{
		Template:    "generic",
		Progress:    progress,
		State:       "Paused on " + deviceName + " by " + userName,
		Icon:        "pause.circle.fill",
		Subtitle:    subtitle,
		AccentColor: pushward.ColorBlue,
	}

	h.scheduleEnd(userKey, mapKey, slug, content)
	slog.Info("auto-ending paused activity", "slug", slug)
}
