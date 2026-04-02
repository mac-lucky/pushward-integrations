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
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.OverseerrConfig
	ender   *lifecycle.Ender
}

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

func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var payload webhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		slog.Error("failed to decode webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	userKey := auth.KeyFromContext(r.Context())
	log := slog.With("tenant", auth.KeyHash(userKey))
	ctx := r.Context()
	ctx = metrics.WithProvider(ctx, "overseerr")

	var apiErr error
	switch payload.NotificationType {
	case "MEDIA_PENDING":
		apiErr = h.handleEvent(ctx, userKey, log, &payload, 1, "Requested", "hourglass", pushward.ColorOrange, false)
	case "MEDIA_APPROVED", "MEDIA_AUTO_APPROVED":
		apiErr = h.handleEvent(ctx, userKey, log, &payload, 2, "Approved", "checkmark.circle", pushward.ColorBlue, false)
	case "MEDIA_AVAILABLE":
		apiErr = h.handleEvent(ctx, userKey, log, &payload, 4, "Available", "checkmark.circle.fill", pushward.ColorGreen, true)
	case "MEDIA_DECLINED":
		apiErr = h.handleEvent(ctx, userKey, log, &payload, 0, "Declined", "xmark.circle.fill", pushward.ColorRed, true)
	case "MEDIA_FAILED":
		apiErr = h.handleEvent(ctx, userKey, log, &payload, 0, "Failed", "xmark.circle.fill", pushward.ColorRed, true)
	case "TEST_NOTIFICATION":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "overseerr"); err != nil {
			log.Error("test notification failed", "provider", "overseerr", "error", err)
		}
	default:
		slog.Debug("unknown overseerr notification type", "type", payload.NotificationType)
	}

	if apiErr != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleEvent(ctx context.Context, userKey string, log *slog.Logger, p *webhookPayload, step int, stateText, icon, accentColor string, terminal bool) error {
	// Validate media type against allowlist
	switch p.Media.MediaType {
	case "movie", "tv":
	default:
		log.Warn("overseerr: invalid media_type", "media_type", p.Media.MediaType)
		return nil
	}
	// Validate TmdbID is numeric and non-empty
	if p.Media.TmdbID == "" || !isNumeric(p.Media.TmdbID) {
		log.Warn("overseerr: invalid tmdbId", "tmdbId", p.Media.TmdbID)
		return nil
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
		log.Error("failed to create overseerr activity", "slug", slug, "error", err)
		return err
	}

	total := 4
	content := pushward.Content{
		Template:    "steps",
		State:       text.TruncateHard(stateText, 100),
		Icon:        icon,
		Subtitle:    subtitle,
		AccentColor: accentColor,
		CurrentStep: &step,
		TotalSteps:  &total,
	}

	if step > 0 {
		content.Progress = float64(step) / float64(total)
	}

	req := pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update overseerr activity", "slug", slug, "error", err)
		return err
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "overseerr", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		log.Warn("state store write failed", "error", err, "provider", "overseerr", "slug", slug)
	}

	if terminal {
		h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	}

	log.Info("overseerr event", "slug", slug, "type", stateText)
	return nil
}

func isNumeric(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}
