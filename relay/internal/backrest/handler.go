package backrest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.BackrestConfig
	ender   *lifecycle.Ender
}

func NewHandler(store state.Store, clients *client.Pool, cfg *config.BackrestConfig) *Handler {
	return &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "backrest", lifecycle.EndConfig{
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
	tenant := auth.KeyHash(userKey)
	ctx := r.Context()

	switch payload.Event {
	case "CONDITION_SNAPSHOT_START":
		h.handleStart(ctx, userKey, tenant, &payload, "Backing up...")
	case "CONDITION_SNAPSHOT_SUCCESS":
		stateText := "Complete · " + formatBytes(payload.DataAdded)
		h.handleEnd(ctx, userKey, tenant, &payload, stateText, pushward.ColorGreen, "checkmark.circle.fill")
	case "CONDITION_SNAPSHOT_WARNING":
		h.handleEnd(ctx, userKey, tenant, &payload, "Complete (warnings)", pushward.ColorOrange, "exclamationmark.triangle.fill")
	case "CONDITION_SNAPSHOT_ERROR":
		stateText := "Failed"
		if payload.Error != "" {
			msg := text.TruncateHard(payload.Error, 50)
			stateText = "Failed: " + msg
		}
		h.handleEnd(ctx, userKey, tenant, &payload, stateText, pushward.ColorRed, "xmark.circle.fill")
	case "CONDITION_PRUNE_START":
		h.handleStart(ctx, userKey, tenant, &payload, "Pruning...")
	case "CONDITION_PRUNE_SUCCESS":
		h.handleEnd(ctx, userKey, tenant, &payload, "Pruned", pushward.ColorGreen, "checkmark.circle.fill")
	case "CONDITION_PRUNE_ERROR":
		h.handleEnd(ctx, userKey, tenant, &payload, "Prune Failed", pushward.ColorRed, "xmark.circle.fill")
	case "CONDITION_CHECK_START":
		h.handleStart(ctx, userKey, tenant, &payload, "Checking...")
	case "CONDITION_CHECK_SUCCESS":
		h.handleEnd(ctx, userKey, tenant, &payload, "Check Passed", pushward.ColorGreen, "checkmark.circle.fill")
	case "CONDITION_CHECK_ERROR":
		h.handleEnd(ctx, userKey, tenant, &payload, "Check Failed", pushward.ColorRed, "xmark.circle.fill")
	default:
		slog.Debug("unknown backrest event", "event", payload.Event)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) slugAndKey(p *webhookPayload) (string, string) {
	slug := text.SlugHash("backrest", p.Plan+p.Repo, 4)
	mapKey := fmt.Sprintf("backrest:%s:%s", p.Plan, p.Repo)
	return slug, mapKey
}

func (h *Handler) handleStart(ctx context.Context, userKey, tenant string, p *webhookPayload, stateText string) {
	slug, mapKey := h.slugAndKey(p)

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	name := text.TruncateHard(p.Plan, 100)
	if name == "" {
		name = "Backup"
	}

	if err := cl.CreateActivity(ctx, slug, name, h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create backrest activity", "slug", slug, "error", err, "tenant", tenant)
		return
	}

	subtitle := "Backrest"
	if p.Plan != "" {
		subtitle += " · " + text.TruncateHard(p.Plan, 50)
	}
	if p.Repo != "" {
		subtitle += " · " + text.TruncateHard(p.Repo, 50)
	}

	content := pushward.Content{
		Template:    "generic",
		Progress:    0,
		State:       stateText,
		Icon:        "arrow.triangle.2.circlepath",
		Subtitle:    subtitle,
		AccentColor: pushward.ColorBlue,
	}

	req := pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update backrest activity", "slug", slug, "error", err, "tenant", tenant)
		return
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "backrest", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		slog.Warn("state store write failed", "error", err, "provider", "backrest", "slug", slug, "tenant", tenant)
	}

	slog.Info("backrest started", "slug", slug, "event", p.Event, "state", stateText, "tenant", tenant)
}

func (h *Handler) handleEnd(ctx context.Context, userKey, tenant string, p *webhookPayload, stateText, color, icon string) {
	slug, mapKey := h.slugAndKey(p)

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	name := text.TruncateHard(p.Plan, 100)
	if name == "" {
		name = "Backup"
	}

	// Create activity in case we missed the start event
	if err := cl.CreateActivity(ctx, slug, name, h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create backrest activity", "slug", slug, "error", err, "tenant", tenant)
		return
	}

	subtitle := "Backrest"
	if p.Plan != "" {
		subtitle += " · " + text.TruncateHard(p.Plan, 50)
	}
	if p.Repo != "" {
		subtitle += " · " + text.TruncateHard(p.Repo, 50)
	}

	content := pushward.Content{
		Template:    "generic",
		Progress:    1.0,
		State:       stateText,
		Icon:        icon,
		Subtitle:    subtitle,
		AccentColor: color,
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "backrest", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		slog.Warn("state store write failed", "error", err, "provider", "backrest", "slug", slug, "tenant", tenant)
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)

	slog.Info("backrest end scheduled", "slug", slug, "event", p.Event, "state", stateText, "tenant", tenant)
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
