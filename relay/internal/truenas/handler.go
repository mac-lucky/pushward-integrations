package truenas

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
	"github.com/mac-lucky/pushward-integrations/relay/internal/overrides"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.TrueNASConfig
	ender   *lifecycle.Ender
}

// RegisterRoutes registers the TrueNAS OpsGenie-compatible endpoints and returns
// the Handler. TrueNAS's OpsGenie alert service creates an alert with a POST and
// clears it with a DELETE keyed by the alert alias, so the relay emulates that
// pair.
func RegisterRoutes(api huma.API, store state.Store, clients *client.Pool, cfg *config.TrueNASConfig) *Handler {
	h := &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "truenas", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
	}
	humautil.RegisterWebhook(api, "/truenas/v2/alerts", "post-truenas-alert",
		"Receive TrueNAS OpsGenie alert",
		"Creates a Live Activity from a TrueNAS OpsGenie create-alert call.",
		[]string{"TrueNAS"}, h.handleCreate)
	humautil.RegisterDelete(api, "/truenas/v2/alerts/{id}", "delete-truenas-alert",
		"Clear TrueNAS OpsGenie alert",
		"Ends the Live Activity for a TrueNAS alert cleared via the OpsGenie close-alert call.",
		[]string{"TrueNAS"}, h.handleDelete)
	return h
}

func (h *Handler) Ender() *lifecycle.Ender { return h.ender }

func slugFor(alias string) string   { return text.SlugHash("truenas", alias, 6) }
func mapKeyFor(alias string) string { return "truenas:" + alias }

func (h *Handler) handleCreate(ctx context.Context, input *struct {
	Body createAlert
},
) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, "truenas")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	pwClient := h.clients.Get(userKey)
	p := &input.Body

	if p.Alias == "" {
		log.Warn("truenas alert without alias, ignoring")
		return humautil.NewOK(), nil
	}
	if err := h.create(ctx, userKey, log, pwClient, p); err != nil {
		return nil, huma.Error502BadGateway("upstream API error")
	}
	return humautil.NewOK(), nil
}

func (h *Handler) create(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, p *createAlert) error {
	slug := slugFor(p.Alias)
	mapKey := mapKeyFor(p.Alias)
	ov := overrides.FromContext(ctx)

	// Cancel any pending end from a prior clear so ENDED can't land after this
	// new ONGOING update (re-fired alert reusing the alias).
	h.ender.StopTimer(userKey, mapKey)

	// Degrade to best-effort delivery on store errors: dropping a new alert on a
	// transient DB blip is worse than a possible duplicate.
	existing, err := h.store.Get(ctx, "truenas", userKey, mapKey, "")
	if err != nil {
		log.Error("failed to check state, treating alert as new", "alias", p.Alias, "error", err)
		existing = nil
	}

	title := p.Message
	if title == "" {
		title = "TrueNAS alert"
	}
	data, _ := json.Marshal(trackedAlert{Slug: slug, Title: title})
	if err := h.store.Set(ctx, "truenas", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		log.Error("failed to store state, continuing", "alias", p.Alias, "error", err)
	}

	isNew := existing == nil
	// channels=notification suppresses the Live Activity; the isNew notification
	// below still fires so the alert reaches the user as a one-shot.
	if isNew && ov.AllowsActivity() {
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pwClient.CreateActivity(ctx, slug, text.TruncateHard(title, 100), ov.PriorityOr(h.config.Priority), endedTTL, staleTTL); err != nil {
			log.Error("failed to create activity", "slug", slug, "error", err)
			if derr := h.store.Delete(ctx, "truenas", userKey, mapKey, ""); derr != nil {
				log.Warn("state store delete failed", "error", derr, "provider", "truenas", "slug", slug)
			}
			return err
		}
	}

	stateText := text.TruncateHard(p.Message, 100)
	if stateText == "" {
		stateText = "Alert"
	}
	if ov.AllowsActivity() {
		// The payload carries no severity; TrueNAS filters alerts by Level per
		// service, so a fixed warning styling keeps the Live Activity readable.
		req := pushward.UpdateRequest{
			State: pushward.StateOngoing,
			Content: pushward.Content{
				Template:    pushward.TemplateAlert,
				Progress:    1.0,
				State:       stateText,
				Icon:        "exclamationmark.triangle.fill",
				Subtitle:    "TrueNAS",
				AccentColor: pushward.ColorOrange,
				Severity:    "warning",
				FiredAt:     pushward.Int64Ptr(time.Now().Unix()),
			},
		}
		if err := pwClient.UpdateActivity(ctx, slug, req); err != nil {
			log.Error("failed to update activity", "slug", slug, "error", err)
			if isNew {
				if derr := h.store.Delete(ctx, "truenas", userKey, mapKey, ""); derr != nil {
					log.Warn("state store delete failed", "error", derr, "provider", "truenas", "slug", slug)
				}
			}
			return err
		}
	}

	if isNew && ov.AllowsNotification() {
		notif := notification(p.Alias, title, slug)
		notif.Body = stateText
		notif.Level = ov.LevelOr(pushward.LevelActive)
		if err := pwClient.SendNotification(ctx, notif); err != nil {
			log.Error("failed to send notification", "slug", slug, "error", err)
		}
	}
	log.Info("truenas alert", "slug", slug, "alias", p.Alias)
	return nil
}

func (h *Handler) handleDelete(ctx context.Context, input *struct {
	ID             string `path:"id"`
	IdentifierType string `query:"identifierType"`
},
) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, "truenas")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	pwClient := h.clients.Get(userKey)

	if input.ID == "" {
		return humautil.NewOK(), nil
	}
	if err := h.clear(ctx, userKey, log, pwClient, input.ID); err != nil {
		return nil, huma.Error502BadGateway("upstream API error")
	}
	return humautil.NewOK(), nil
}

func (h *Handler) clear(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, alias string) error {
	mapKey := mapKeyFor(alias)
	raw, err := h.store.Get(ctx, "truenas", userKey, mapKey, "")
	if err != nil {
		log.Error("failed to check state", "alias", alias, "error", err)
		return nil
	}
	if raw == nil {
		// Unknown alias: the alert was never tracked or already ended server-side
		// (stale-timeout). A no-op keeps the OpsGenie close call idempotent.
		log.Debug("truenas clear for unknown alias, no-op", "alias", alias)
		return nil
	}

	var rec trackedAlert
	if err := json.Unmarshal(raw, &rec); err != nil {
		log.Error("failed to decode tracked alert", "alias", alias, "error", err)
		return nil
	}

	ov := overrides.FromContext(ctx)
	if ov.AllowsActivity() {
		content := pushward.Content{
			Template:    pushward.TemplateAlert,
			Progress:    1.0,
			State:       "Resolved",
			Icon:        "checkmark.circle.fill",
			Subtitle:    "TrueNAS",
			AccentColor: pushward.ColorGreen,
			Severity:    "info",
		}
		h.ender.ScheduleEnd(userKey, mapKey, rec.Slug, content)
	} else {
		// ScheduleEnd normally clears the dedup row after the two-phase end.
		// With the activity suppressed it never runs, so drop the row here or
		// the next alert within the stale timeout would be deduped into silence.
		if derr := h.store.Delete(ctx, "truenas", userKey, mapKey, ""); derr != nil {
			log.Warn("state store delete failed", "error", derr, "provider", "truenas", "slug", rec.Slug)
		}
	}

	if ov.AllowsNotification() {
		notif := notification(alias, rec.Title, rec.Slug)
		notif.Body = "Resolved \u00b7 " + rec.Title
		notif.Level = ov.LevelOr(pushward.LevelPassive)
		if err := pwClient.SendNotification(ctx, notif); err != nil {
			log.Error("failed to send notification", "slug", rec.Slug, "error", err)
		}
	}
	log.Info("truenas alert cleared", "slug", rec.Slug, "alias", alias)
	return nil
}

func notification(alias, title, slug string) pushward.SendNotificationRequest {
	return pushward.SendNotificationRequest{
		Title:      text.TruncateHard(title, 100),
		Subtitle:   "TrueNAS",
		ThreadID:   "truenas",
		CollapseID: slug,
		Source:     "truenas",
		Push:       true,
		Metadata: map[string]string{
			"alias":       alias,
			"fingerprint": slug,
		},
	}
}
