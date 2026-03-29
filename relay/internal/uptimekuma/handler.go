package uptimekuma

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
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

func NewHandler(store state.Store, clients *client.Pool, cfg *config.UptimeKumaConfig) *Handler {
	return &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "uptimekuma", lifecycle.EndConfig{
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

	ctx := r.Context()
	userKey := auth.KeyFromContext(ctx)
	tenant := auth.KeyHash(userKey)
	pwClient := h.clients.Get(userKey)

	switch payload.Heartbeat.Status {
	case 0: // DOWN
		h.handleDown(ctx, userKey, tenant, pwClient, &payload)
	case 1: // UP
		h.handleUp(ctx, userKey, tenant, pwClient, &payload)
	case 2: // PENDING
		h.handlePending(ctx, userKey, tenant, pwClient, &payload)
	case 3: // MAINTENANCE — used as test event
		if err := selftest.SendTest(ctx, pwClient, "uptimekuma"); err != nil {
			slog.Error("test notification failed", "provider", "uptimekuma", "error", err, "tenant", tenant)
		}
	default:
		slog.Warn("unknown heartbeat status", "status", payload.Heartbeat.Status, "monitor_id", payload.Monitor.ID, "tenant", tenant)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleDown(ctx context.Context, userKey, tenant string, pwClient *pushward.Client, p *webhookPayload) {
	slug := fmt.Sprintf("uptime-%d", p.Monitor.ID)
	mapKey := fmt.Sprintf("uptimekuma:%d", p.Monitor.ID)

	// Cancel any pending end timer from a previous UP event to prevent
	// a race where ENDED fires after this new ONGOING update.
	h.ender.StopTimer(userKey, mapKey)

	existing, err := h.store.Get(ctx, "uptimekuma", userKey, mapKey, "")
	if err != nil {
		slog.Error("failed to check state", "monitor_id", p.Monitor.ID, "error", err, "tenant", tenant)
		return
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "uptimekuma", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		slog.Error("failed to store state", "monitor_id", p.Monitor.ID, "error", err, "tenant", tenant)
		return
	}

	if existing == nil {
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pwClient.CreateActivity(ctx, slug, text.TruncateHard(p.Monitor.Name, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err, "tenant", tenant)
			if err := h.store.Delete(ctx, "uptimekuma", userKey, mapKey, ""); err != nil {
				slog.Warn("state store delete failed", "error", err, "provider", "uptimekuma", "slug", slug, "tenant", tenant)
			}
			return
		}
		slog.Info("created activity", "slug", slug, "monitor", p.Monitor.Name, "tenant", tenant)
	}

	stateText := text.TruncateHard(p.Heartbeat.Msg, 100)
	if stateText == "" {
		stateText = "Monitor Down"
	}

	var firedAtPtr *int64
	if t, err := time.Parse(time.RFC3339Nano, p.Heartbeat.Time); err == nil {
		firedAtPtr = pushward.Int64Ptr(t.Unix())
	}

	subtitle := "Uptime Kuma \u00b7 " + text.TruncateHard(p.Monitor.Name, 50)

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
		slog.Error("failed to update activity", "slug", slug, "error", err, "tenant", tenant)
		return
	}
	slog.Info("updated activity", "slug", slug, "state", pushward.StateOngoing, "severity", "critical", "tenant", tenant)
}

func (h *Handler) handleUp(ctx context.Context, userKey, tenant string, pwClient *pushward.Client, p *webhookPayload) {
	mapKey := fmt.Sprintf("uptimekuma:%d", p.Monitor.ID)

	existing, err := h.store.Get(ctx, "uptimekuma", userKey, mapKey, "")
	if err != nil {
		slog.Error("failed to check state", "monitor_id", p.Monitor.ID, "error", err, "tenant", tenant)
		return
	}
	if existing == nil {
		return // No prior DOWN — skip routine UP checks
	}

	slug := fmt.Sprintf("uptime-%d", p.Monitor.ID)
	subtitle := "Uptime Kuma \u00b7 " + text.TruncateHard(p.Monitor.Name, 50)

	stateText := "Resolved"
	if p.Heartbeat.Ping != nil {
		stateText = fmt.Sprintf("Resolved \u00b7 %dms", *p.Heartbeat.Ping)
	}

	content := pushward.Content{
		Template:    "alert",
		Progress:    1.0,
		State:       stateText,
		Icon:        "checkmark.circle.fill",
		Subtitle:    subtitle,
		AccentColor: pushward.ColorGreen,
		Severity:    "info",
		URL:         text.SanitizeURL(p.Monitor.URL),
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)

	slog.Info("scheduled end for activity", "slug", slug, "monitor", p.Monitor.Name, "tenant", tenant)
}

func (h *Handler) handlePending(ctx context.Context, userKey, tenant string, pwClient *pushward.Client, p *webhookPayload) {
	slug := fmt.Sprintf("uptime-%d", p.Monitor.ID)
	mapKey := fmt.Sprintf("uptimekuma:%d", p.Monitor.ID)

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "uptimekuma", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		slog.Error("failed to store state", "monitor_id", p.Monitor.ID, "error", err, "tenant", tenant)
		return
	}

	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())
	if err := pwClient.CreateActivity(ctx, slug, text.TruncateHard(p.Monitor.Name, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "slug", slug, "error", err, "tenant", tenant)
		return
	}

	stateText := "Checking..."
	subtitle := "Uptime Kuma \u00b7 " + text.TruncateHard(p.Monitor.Name, 50)

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
		slog.Error("failed to update activity", "slug", slug, "error", err, "tenant", tenant)
		return
	}
	slog.Info("created pending activity", "slug", slug, "monitor", p.Monitor.Name, "tenant", tenant)
}
