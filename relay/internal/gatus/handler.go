package gatus

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.GatusConfig
	ender   *lifecycle.Ender
}

// RegisterRoutes registers the Gatus webhook endpoint and returns the Handler.
func RegisterRoutes(api huma.API, store state.Store, clients *client.Pool, cfg *config.GatusConfig) *Handler {
	h := &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "gatus", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
	}
	humautil.RegisterWebhook(api, "/gatus", "post-gatus-webhook",
		"Receive Gatus health check webhook",
		"Processes Gatus endpoint health check alerts (TRIGGERED/RESOLVED).",
		[]string{"Gatus"}, h.handleWebhook)
	return h
}

func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

func (h *Handler) handleWebhook(ctx context.Context, input *struct {
	Body gatusPayload
},
) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, "gatus")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	pwClient := h.clients.Get(userKey)
	payload := &input.Body

	var err error
	switch payload.Status {
	case "TRIGGERED":
		err = h.handleTriggered(ctx, userKey, log, pwClient, payload)
	case "RESOLVED":
		err = h.handleResolved(ctx, userKey, log, pwClient, payload)
	default:
		log.Warn("unknown gatus status", "status", payload.Status, "endpoint", payload.EndpointName)
	}

	if err != nil {
		return nil, huma.Error502BadGateway("upstream API error")
	}
	return humautil.NewOK(), nil
}

func (h *Handler) slugAndKey(p *gatusPayload) (string, string) {
	identifier := p.EndpointName
	if p.EndpointGroup != "" {
		identifier = p.EndpointGroup + "/" + p.EndpointName
	}
	slug := text.SlugHash("gatus", identifier, 6)
	mapKey := "gatus:" + slug[len("gatus-"):]
	return slug, mapKey
}

func (h *Handler) subtitle(p *gatusPayload) string {
	name := p.EndpointName
	if p.EndpointGroup != "" {
		name = p.EndpointGroup + "/" + p.EndpointName
	}
	return "Gatus \u00b7 " + text.TruncateHard(name, 50)
}

func (h *Handler) buildMetadata(p *gatusPayload, slug string) map[string]string {
	meta := map[string]string{
		"alert_name":    p.EndpointName,
		"endpoint_name": p.EndpointName,
		"fingerprint":   slug,
	}
	if p.EndpointGroup != "" {
		meta["endpoint_group"] = p.EndpointGroup
	}
	return meta
}

func (h *Handler) buildNotification(p *gatusPayload, slug, subtitle string) pushward.SendNotificationRequest {
	return pushward.SendNotificationRequest{
		Title:      text.TruncateHard(p.EndpointName, 100),
		Subtitle:   subtitle,
		ThreadID:   "gatus",
		CollapseID: slug,
		Source:     "gatus",
		Push:       true,
		Metadata:   h.buildMetadata(p, slug),
	}
}

func (h *Handler) handleTriggered(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, p *gatusPayload) error {
	slug, mapKey := h.slugAndKey(p)

	// Cancel any pending end timer from a previous RESOLVED event
	h.ender.StopTimer(userKey, mapKey)

	existing, err := h.store.Get(ctx, "gatus", userKey, mapKey, "")
	if err != nil {
		log.Error("failed to check state", "endpoint", p.EndpointName, "error", err)
		return nil
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "gatus", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		log.Error("failed to store state", "endpoint", p.EndpointName, "error", err)
		return nil
	}

	isNew := existing == nil
	if isNew {
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pwClient.CreateActivity(ctx, slug, text.TruncateHard(p.EndpointName, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
			log.Error("failed to create activity", "slug", slug, "error", err)
			if err := h.store.Delete(ctx, "gatus", userKey, mapKey, ""); err != nil {
				log.Warn("state store delete failed", "error", err, "provider", "gatus", "slug", slug)
			}
			return err
		}
		log.Info("created activity", "slug", slug, "endpoint", p.EndpointName)
	}

	stateText := text.TruncateHard(p.ResultErrors, 100)
	if stateText == "" {
		stateText = text.TruncateHard(p.Description, 100)
	}
	if stateText == "" {
		stateText = "Health Check Failed"
	}

	firedAt := pushward.Int64Ptr(time.Now().Unix())
	subtitle := h.subtitle(p)

	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       stateText,
			Icon:        "exclamationmark.triangle.fill",
			Subtitle:    subtitle,
			AccentColor: pushward.ColorRed,
			Severity:    "critical",
			FiredAt:     firedAt,
			URL:         text.SanitizeURL(p.EndpointURL),
		},
	}
	if err := pwClient.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update activity", "slug", slug, "error", err)
		return err
	}
	log.Info("updated activity", "slug", slug, "state", pushward.StateOngoing, "severity", "error")

	if isNew {
		notifReq := h.buildNotification(p, slug, subtitle)
		notifReq.Body = p.EndpointName + " · " + stateText
		notifReq.Level = pushward.LevelActive
		notifReq.Category = "critical"
		if err := pwClient.SendNotification(ctx, notifReq); err != nil {
			log.Error("failed to send notification", "slug", slug, "error", err)
		}
	}
	return nil
}

func (h *Handler) handleResolved(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, p *gatusPayload) error {
	slug, mapKey := h.slugAndKey(p)

	existing, err := h.store.Get(ctx, "gatus", userKey, mapKey, "")
	if err != nil {
		log.Error("failed to check state", "endpoint", p.EndpointName, "error", err)
		return nil
	}
	if existing == nil {
		return nil // No prior TRIGGERED — skip routine RESOLVED
	}

	subtitle := h.subtitle(p)

	content := pushward.Content{
		Template:    "alert",
		Progress:    1.0,
		State:       "Resolved",
		Icon:        "checkmark.circle.fill",
		Subtitle:    subtitle,
		AccentColor: pushward.ColorGreen,
		Severity:    "info",
		URL:         text.SanitizeURL(p.EndpointURL),
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)

	notifReq := h.buildNotification(p, slug, subtitle)
	notifReq.Body = "Resolved · " + p.EndpointName
	notifReq.Level = pushward.LevelPassive
	notifReq.Category = "resolved"
	if err := pwClient.SendNotification(ctx, notifReq); err != nil {
		log.Error("failed to send notification", "slug", slug, "error", err)
	}

	log.Info("scheduled end for activity", "slug", slug, "endpoint", p.EndpointName)
	return nil
}
