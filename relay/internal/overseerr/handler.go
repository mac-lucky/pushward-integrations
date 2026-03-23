package overseerr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// Handler processes Overseerr/Jellyseerr webhooks for multiple tenants.
type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.OverseerrConfig
	ender   *lifecycle.Ender
}

// NewHandler creates a new Overseerr/Jellyseerr webhook handler.
func NewHandler(store state.Store, clients *client.Pool, cfg *config.OverseerrConfig) *Handler {
	return &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "overseerr", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
	}
}

// ServeHTTP handles incoming Overseerr/Jellyseerr webhook requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var payload webhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		slog.Error("failed to decode webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	userKey := auth.KeyFromContext(r.Context())
	ctx := r.Context()

	switch payload.NotificationType {
	case "MEDIA_PENDING":
		h.handleEvent(ctx, userKey, &payload, 1, "Requested", "hourglass", "#FF9500", false)
	case "MEDIA_APPROVED", "MEDIA_AUTO_APPROVED":
		h.handleEvent(ctx, userKey, &payload, 2, "Approved", "checkmark.circle", "#007AFF", false)
	case "MEDIA_AVAILABLE":
		h.handleEvent(ctx, userKey, &payload, 4, "Available", "checkmark.circle.fill", "#34C759", true)
	case "MEDIA_DECLINED":
		h.handleEvent(ctx, userKey, &payload, 0, "Declined", "xmark.circle.fill", "#FF3B30", true)
	case "MEDIA_FAILED":
		h.handleEvent(ctx, userKey, &payload, 0, "Failed", "xmark.circle.fill", "#FF3B30", true)
	case "TEST_NOTIFICATION":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "overseerr"); err != nil {
			slog.Error("test notification failed", "provider", "overseerr", "error", err)
		}
	default:
		slog.Debug("unknown overseerr notification type", "type", payload.NotificationType)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// handleEvent creates or updates an activity based on the notification type.
func (h *Handler) handleEvent(ctx context.Context, userKey string, p *webhookPayload, step int, stateText, icon, accentColor string, terminal bool) {
	// Validate media type against allowlist
	switch p.Media.MediaType {
	case "movie", "tv":
	default:
		slog.Warn("overseerr: invalid media_type", "media_type", p.Media.MediaType)
		return
	}
	// Validate TmdbID is numeric and non-empty
	if p.Media.TmdbID == "" || !isNumeric(p.Media.TmdbID) {
		slog.Warn("overseerr: invalid tmdbId", "tmdbId", p.Media.TmdbID)
		return
	}

	slug := fmt.Sprintf("overseerr-%s-%s", p.Media.MediaType, p.Media.TmdbID)
	mapKey := fmt.Sprintf("overseerr:%s:%s", p.Media.MediaType, p.Media.TmdbID)
	subtitle := "Overseerr · " + text.TruncateHard(p.Subject, 50)

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	name := text.TruncateHard(p.Subject, 100)
	if name == "" {
		name = "Media Request"
	}

	if err := cl.CreateActivity(ctx, slug, name, h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create overseerr activity", "slug", slug, "error", err)
		return
	}

	total := 4
	content := pushward.Content{
		Template:    "pipeline",
		State:       text.TruncateHard(stateText, 100),
		Icon:        icon,
		Subtitle:    subtitle,
		AccentColor: accentColor,
	}

	if step > 0 {
		content.Progress = float64(step) / float64(total)
		content.CurrentStep = &step
		content.TotalSteps = &total
	}

	req := pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update overseerr activity", "slug", slug, "error", err)
		return
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	_ = h.store.Set(ctx, "overseerr", userKey, mapKey, "", data, h.config.StaleTimeout)

	if terminal {
		h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	}

	slog.Info("overseerr event", "slug", slug, "type", stateText)
}

func isNumeric(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}
