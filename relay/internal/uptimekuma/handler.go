package uptimekuma

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
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
	store   state.Store
	clients *client.Pool
	config  *config.UptimeKumaConfig
	ender   *lifecycle.Ender
}

// RegisterRoutes registers the Uptime Kuma webhook endpoint and returns the Handler.
func RegisterRoutes(api huma.API, store state.Store, clients *client.Pool, cfg *config.UptimeKumaConfig) *Handler {
	h := &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "uptimekuma", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
	}
	humautil.RegisterWebhook(api, "/uptimekuma", "post-uptimekuma-webhook",
		"Receive Uptime Kuma webhook",
		"Processes Uptime Kuma monitor status events (DOWN/UP/PENDING/MAINTENANCE).",
		[]string{"Uptime Kuma"}, h.handleWebhook)
	return h
}

func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

func (h *Handler) handleWebhook(ctx context.Context, input *struct {
	Body uptimekumaPayload
},
) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, "uptimekuma")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	pwClient := h.clients.Get(userKey)
	payload := &input.Body

	var err error
	switch payload.Heartbeat.Status {
	case 0: // DOWN
		err = h.handleDown(ctx, userKey, log, pwClient, payload)
	case 1: // UP
		err = h.handleUp(ctx, userKey, log, pwClient, payload)
	case 2: // PENDING
		err = h.handlePending(ctx, userKey, log, pwClient, payload)
	case 3: // MAINTENANCE — used as test event
		if err := selftest.SendTest(ctx, pwClient, "uptimekuma"); err != nil {
			log.Error("test notification failed", "provider", "uptimekuma", "error", err)
		}
	default:
		log.Warn("unknown heartbeat status", "status", payload.Heartbeat.Status, "monitor_id", payload.Monitor.ID)
	}

	if err != nil {
		return nil, huma.Error502BadGateway("upstream API error")
	}
	return humautil.NewOK(), nil
}

func (h *Handler) slugAndKey(p *uptimekumaPayload) (slug, mapKey, monitorIDStr string) {
	monitorIDStr = strconv.Itoa(p.Monitor.ID)
	slug = "uptime-" + monitorIDStr
	mapKey = "uptimekuma:" + monitorIDStr
	return
}

func (h *Handler) subtitle(p *uptimekumaPayload) string {
	return "Uptime Kuma \u00b7 " + text.TruncateHard(p.Monitor.Name, 50)
}

func (h *Handler) buildNotification(p *uptimekumaPayload, subtitle string, monitorIDStr string) pushward.SendNotificationRequest {
	return pushward.SendNotificationRequest{
		Title:      text.TruncateHard(p.Monitor.Name, 100),
		Subtitle:   subtitle,
		ThreadID:   text.Slug("uptimekuma-", p.Monitor.Name),
		CollapseID: text.SlugHash("uptimekuma", monitorIDStr, 6),
		Source:     "uptimekuma",
		Push:       true,
		Metadata: map[string]string{
			"alert_name":  p.Monitor.Name,
			"monitor_id":  monitorIDStr,
			"fingerprint": monitorIDStr,
		},
	}
}

func (h *Handler) handleDown(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, p *uptimekumaPayload) error {
	slug, mapKey, monitorIDStr := h.slugAndKey(p)

	// Cancel any pending end timer from a previous UP event to prevent
	// a race where ENDED fires after this new ONGOING update.
	h.ender.StopTimer(userKey, mapKey)

	existing, err := h.store.Get(ctx, "uptimekuma", userKey, mapKey, "")
	if err != nil {
		log.Error("failed to check state", "monitor_id", p.Monitor.ID, "error", err)
		return nil
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "uptimekuma", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		log.Error("failed to store state", "monitor_id", p.Monitor.ID, "error", err)
		return nil
	}

	isNew := existing == nil
	if isNew {
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pwClient.CreateActivity(ctx, slug, text.TruncateHard(p.Monitor.Name, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
			log.Error("failed to create activity", "slug", slug, "error", err)
			if err := h.store.Delete(ctx, "uptimekuma", userKey, mapKey, ""); err != nil {
				log.Warn("state store delete failed", "error", err, "provider", "uptimekuma", "slug", slug)
			}
			return err
		}
		log.Info("created activity", "slug", slug, "monitor", p.Monitor.Name)
	}

	stateText := text.TruncateHard(p.Heartbeat.Msg, 100)
	if stateText == "" {
		stateText = "Monitor Down"
	}

	var firedAtPtr *int64
	if t, err := time.Parse(time.RFC3339Nano, p.Heartbeat.Time); err == nil {
		firedAtPtr = pushward.Int64Ptr(t.Unix())
	}

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
			FiredAt:     firedAtPtr,
			URL:         text.SanitizeURL(p.Monitor.URL),
		},
	}
	if err := pwClient.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update activity", "slug", slug, "error", err)
		return err
	}
	log.Info("updated activity", "slug", slug, "state", pushward.StateOngoing, "severity", "critical")

	if isNew {
		notifReq := h.buildNotification(p, subtitle, monitorIDStr)
		notifReq.Body = p.Monitor.Name + " · " + stateText
		notifReq.Level = pushward.LevelActive
		notifReq.Category = "critical"
		if err := pwClient.SendNotification(ctx, notifReq); err != nil {
			log.Error("failed to send notification", "slug", slug, "error", err)
		}
	}
	return nil
}

func (h *Handler) handleUp(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, p *uptimekumaPayload) error {
	_, mapKey, _ := h.slugAndKey(p)

	existing, err := h.store.Get(ctx, "uptimekuma", userKey, mapKey, "")
	if err != nil {
		log.Error("failed to check state", "monitor_id", p.Monitor.ID, "error", err)
		return nil
	}
	if existing == nil {
		return nil // No prior DOWN — skip routine UP checks
	}

	slug, _, monitorIDStr := h.slugAndKey(p)
	subtitle := h.subtitle(p)

	activityState := "Resolved"
	notifBody := "Resolved · " + p.Monitor.Name
	if p.Heartbeat.Ping != nil {
		activityState = fmt.Sprintf("Resolved \u00b7 %dms", *p.Heartbeat.Ping)
		notifBody = fmt.Sprintf("Resolved \u00b7 %s \u00b7 %dms", p.Monitor.Name, *p.Heartbeat.Ping)
	}

	content := pushward.Content{
		Template:    "alert",
		Progress:    1.0,
		State:       activityState,
		Icon:        "checkmark.circle.fill",
		Subtitle:    subtitle,
		AccentColor: pushward.ColorGreen,
		Severity:    "info",
		URL:         text.SanitizeURL(p.Monitor.URL),
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)

	notifReq := h.buildNotification(p, subtitle, monitorIDStr)
	notifReq.Body = notifBody
	notifReq.Level = pushward.LevelPassive
	notifReq.Category = "resolved"
	if err := pwClient.SendNotification(ctx, notifReq); err != nil {
		log.Error("failed to send notification", "slug", slug, "error", err)
	}

	log.Info("scheduled end for activity", "slug", slug, "monitor", p.Monitor.Name)
	return nil
}

func (h *Handler) handlePending(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, p *uptimekumaPayload) error {
	slug, mapKey, _ := h.slugAndKey(p)

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "uptimekuma", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		log.Error("failed to store state", "monitor_id", p.Monitor.ID, "error", err)
		return nil
	}

	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())
	if err := pwClient.CreateActivity(ctx, slug, text.TruncateHard(p.Monitor.Name, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
		log.Error("failed to create activity", "slug", slug, "error", err)
		if err := h.store.Delete(ctx, "uptimekuma", userKey, mapKey, ""); err != nil {
			log.Warn("state store delete failed", "error", err, "provider", "uptimekuma", "slug", slug)
		}
		return err
	}

	stateText := "Checking..."
	subtitle := h.subtitle(p)

	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       stateText,
			Icon:        "hourglass",
			Subtitle:    subtitle,
			AccentColor: pushward.ColorOrange,
			Severity:    "warning",
			URL:         text.SanitizeURL(p.Monitor.URL),
		},
	}
	if err := pwClient.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update activity", "slug", slug, "error", err)
		return err
	}
	log.Info("created pending activity", "slug", slug, "monitor", p.Monitor.Name)
	return nil
}
