package gatus

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

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
	config  *config.GatusConfig
	ender   *lifecycle.Ender
}

func NewHandler(store state.Store, clients *client.Pool, cfg *config.GatusConfig) *Handler {
	return &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "gatus", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
	}
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
	pwClient := h.clients.Get(userKey)

	switch payload.Status {
	case "TRIGGERED":
		h.handleTriggered(ctx, userKey, pwClient, &payload)
	case "RESOLVED":
		h.handleResolved(ctx, userKey, pwClient, &payload)
	default:
		slog.Warn("unknown gatus status", "status", payload.Status, "endpoint", payload.EndpointName)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) slugAndKey(p *webhookPayload) (string, string) {
	identifier := p.EndpointName
	if p.EndpointGroup != "" {
		identifier = p.EndpointGroup + "/" + p.EndpointName
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(identifier)))[:12]
	slug := "gatus-" + hash
	mapKey := "gatus:" + hash
	return slug, mapKey
}

func (h *Handler) handleTriggered(ctx context.Context, userKey string, pwClient *pushward.Client, p *webhookPayload) {
	slug, mapKey := h.slugAndKey(p)

	// Cancel any pending end timer from a previous RESOLVED event
	h.ender.StopTimer(userKey, mapKey)

	existing, err := h.store.Get(ctx, "gatus", userKey, mapKey, "")
	if err != nil {
		slog.Error("failed to check state", "endpoint", p.EndpointName, "error", err)
		return
	}

	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	if err := h.store.Set(ctx, "gatus", userKey, mapKey, "", data, h.config.StaleTimeout); err != nil {
		slog.Error("failed to store state", "endpoint", p.EndpointName, "error", err)
		return
	}

	if existing == nil {
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := pwClient.CreateActivity(ctx, slug, text.TruncateHard(p.EndpointName, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			_ = h.store.Delete(ctx, "gatus", userKey, mapKey, "")
			return
		}
		slog.Info("created activity", "slug", slug, "endpoint", p.EndpointName)
	}

	stateText := text.TruncateHard(p.ResultErrors, 100)
	if stateText == "" {
		stateText = text.TruncateHard(p.Description, 100)
	}
	if stateText == "" {
		stateText = "Health Check Failed"
	}

	firedAt := pushward.Int64Ptr(time.Now().Unix())

	subtitle := "Gatus \u00b7 " + text.TruncateHard(p.EndpointName, 50)
	if p.EndpointGroup != "" {
		subtitle = "Gatus \u00b7 " + text.TruncateHard(p.EndpointGroup+"/"+p.EndpointName, 50)
	}

	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       stateText,
			Icon:        "exclamationmark.triangle.fill",
			Subtitle:    subtitle,
			AccentColor: "#FF3B30",
			Severity:    "error",
			FiredAt:     firedAt,
			URL:         text.SanitizeURL(p.EndpointURL),
		},
	}
	if err := pwClient.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "state", pushward.StateOngoing, "severity", "error")
}

func (h *Handler) handleResolved(ctx context.Context, userKey string, pwClient *pushward.Client, p *webhookPayload) {
	slug, mapKey := h.slugAndKey(p)

	existing, err := h.store.Get(ctx, "gatus", userKey, mapKey, "")
	if err != nil {
		slog.Error("failed to check state", "endpoint", p.EndpointName, "error", err)
		return
	}
	if existing == nil {
		return // No prior TRIGGERED — skip routine RESOLVED
	}
	subtitle := "Gatus \u00b7 " + text.TruncateHard(p.EndpointName, 50)
	if p.EndpointGroup != "" {
		subtitle = "Gatus \u00b7 " + text.TruncateHard(p.EndpointGroup+"/"+p.EndpointName, 50)
	}

	content := pushward.Content{
		Template:    "alert",
		Progress:    1.0,
		State:       "Resolved",
		Icon:        "checkmark.circle.fill",
		Subtitle:    subtitle,
		AccentColor: "#34C759",
		Severity:    "info",
		URL:         text.SanitizeURL(p.EndpointURL),
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)

	slog.Info("scheduled end for activity", "slug", slug, "endpoint", p.EndpointName)
}

