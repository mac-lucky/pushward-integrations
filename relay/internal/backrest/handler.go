package backrest

import (
	"context"
	"crypto/sha256"
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

// Handler processes Backrest webhooks for multiple tenants.
type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.BackrestConfig
	ender   *lifecycle.Ender
}

// NewHandler creates a new Backrest webhook handler.
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

// ServeHTTP handles incoming Backrest webhook requests.
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

	switch payload.Event {
	case "CONDITION_SNAPSHOT_START":
		h.handleStart(ctx, userKey, &payload, "Backing up...")
	case "CONDITION_SNAPSHOT_SUCCESS":
		stateText := "Complete · " + formatBytes(payload.DataAdded)
		h.handleEnd(ctx, userKey, &payload, stateText, "#34C759", "checkmark.circle.fill")
	case "CONDITION_SNAPSHOT_WARNING":
		h.handleEnd(ctx, userKey, &payload, "Complete (warnings)", "#FF9500", "exclamationmark.triangle.fill")
	case "CONDITION_SNAPSHOT_ERROR":
		stateText := "Failed"
		if payload.Error != "" {
			msg := text.TruncateHard(payload.Error, 50)
			stateText = "Failed: " + msg
		}
		h.handleEnd(ctx, userKey, &payload, stateText, "#FF3B30", "xmark.circle.fill")
	case "CONDITION_PRUNE_START":
		h.handleStart(ctx, userKey, &payload, "Pruning...")
	case "CONDITION_PRUNE_SUCCESS":
		h.handleEnd(ctx, userKey, &payload, "Pruned", "#34C759", "checkmark.circle.fill")
	case "CONDITION_PRUNE_ERROR":
		h.handleEnd(ctx, userKey, &payload, "Prune Failed", "#FF3B30", "xmark.circle.fill")
	case "CONDITION_CHECK_START":
		h.handleStart(ctx, userKey, &payload, "Checking...")
	case "CONDITION_CHECK_SUCCESS":
		h.handleEnd(ctx, userKey, &payload, "Check Passed", "#34C759", "checkmark.circle.fill")
	case "CONDITION_CHECK_ERROR":
		h.handleEnd(ctx, userKey, &payload, "Check Failed", "#FF3B30", "xmark.circle.fill")
	default:
		slog.Debug("unknown backrest event", "event", payload.Event)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) slugAndKey(p *webhookPayload) (string, string) {
	hash := sha256.Sum256([]byte(p.Plan + p.Repo))
	slug := fmt.Sprintf("backrest-%x", hash[:4])
	mapKey := fmt.Sprintf("backrest:%s:%s", p.Plan, p.Repo)
	return slug, mapKey
}

func (h *Handler) handleStart(ctx context.Context, userKey string, p *webhookPayload, stateText string) {
	slug, mapKey := h.slugAndKey(p)

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	name := text.TruncateHard(p.Plan, 100)
	if name == "" {
		name = "Backup"
	}

	if err := cl.CreateActivity(ctx, slug, name, h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create backrest activity", "slug", slug, "error", err)
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
		AccentColor: "#007AFF",
	}

	req := pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update backrest activity", "slug", slug, "error", err)
		return
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	_ = h.store.Set(ctx, "backrest", userKey, mapKey, "", data, h.config.StaleTimeout)

	slog.Info("backrest started", "slug", slug, "event", p.Event, "state", stateText)
}

func (h *Handler) handleEnd(ctx context.Context, userKey string, p *webhookPayload, stateText, color, icon string) {
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
		slog.Error("failed to create backrest activity", "slug", slug, "error", err)
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
	_ = h.store.Set(ctx, "backrest", userKey, mapKey, "", data, h.config.StaleTimeout)

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)

	slog.Info("backrest end scheduled", "slug", slug, "event", p.Event, "state", stateText)
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
