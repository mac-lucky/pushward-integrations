package overseerr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/mediathread"
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

// RegisterRoutes registers the Overseerr webhook endpoint and returns the Handler.
func RegisterRoutes(api huma.API, store state.Store, clients *client.Pool, cfg *config.OverseerrConfig) *Handler {
	h := &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "overseerr", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
	}
	humautil.RegisterWebhook(api, "/overseerr", "post-overseerr-webhook",
		"Receive Overseerr media request webhook",
		"Processes Overseerr media request lifecycle events.",
		[]string{"Overseerr"}, h.handleWebhook)
	return h
}

func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

func (h *Handler) handleWebhook(ctx context.Context, input *struct {
	Body overseerrPayload
},
) (*humautil.WebhookResponse, error) {
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	ctx = metrics.WithProvider(ctx, "overseerr")
	payload := &input.Body

	var apiErr error
	switch payload.NotificationType {
	case "MEDIA_PENDING":
		apiErr = h.handleEvent(ctx, userKey, log, payload, 1, "Requested", "hourglass", pushward.ColorOrange, false)
	case "MEDIA_APPROVED", "MEDIA_AUTO_APPROVED":
		apiErr = h.handleEvent(ctx, userKey, log, payload, 2, "Approved", "checkmark.circle", pushward.ColorBlue, false)
	case "MEDIA_AVAILABLE":
		apiErr = h.handleEvent(ctx, userKey, log, payload, 4, "Available", "checkmark.circle.fill", pushward.ColorGreen, true)
	case "MEDIA_DECLINED":
		apiErr = h.handleEvent(ctx, userKey, log, payload, 0, "Declined", "xmark.circle.fill", pushward.ColorRed, true)
	case "MEDIA_FAILED":
		apiErr = h.handleEvent(ctx, userKey, log, payload, 0, "Failed", "xmark.circle.fill", pushward.ColorRed, true)
	case "TEST_NOTIFICATION":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "overseerr"); err != nil {
			log.Error("test notification failed", "provider", "overseerr", "error", err)
		}
	default:
		slog.Debug("unknown overseerr notification type", "type", payload.NotificationType)
	}

	if apiErr != nil {
		return nil, huma.Error502BadGateway("upstream API error")
	}
	return humautil.NewOK(), nil
}

func (h *Handler) handleEvent(ctx context.Context, userKey string, log *slog.Logger, p *overseerrPayload, step int, stateText, icon, accentColor string, terminal bool) error {
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

	// Send notification (always, independent of Live Activity)
	notifReq := pushward.SendNotificationRequest{
		Title:    "Overseerr",
		Subtitle: text.TruncateHard(p.Subject, 100),
		Body:     stateText + " · " + text.TruncateHard(p.Subject, 80),
		ThreadID: mediathread.ThreadID(p.Media.MediaType, p.Media.TmdbID, p.Media.TvdbID),
		Level:    pushward.LevelActive,
		Category: "media-" + strings.ToLower(stateText),
		Source:   "overseerr",
		ImageURL: p.Image,
		Push:     true,
	}
	meta := map[string]string{"media_type": p.Media.MediaType, "tmdb_id": p.Media.TmdbID}
	if p.Request.RequestedBy != "" {
		meta["requested_by"] = p.Request.RequestedBy
	}
	notifReq.Metadata = meta
	if err := cl.SendNotification(ctx, notifReq); err != nil {
		log.Error("failed to send notification", "slug", slug, "error", err)
	}

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
