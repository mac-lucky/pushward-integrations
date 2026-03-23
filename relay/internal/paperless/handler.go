package paperless

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
)

type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.PaperlessConfig
	ender   *lifecycle.Ender
}

func NewHandler(store state.Store, clients *client.Pool, cfg *config.PaperlessConfig) *Handler {
	return &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "paperless", lifecycle.EndConfig{
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

	userKey := auth.KeyFromContext(r.Context())
	ctx := r.Context()

	switch payload.Event {
	case "added":
		h.handleDocument(ctx, userKey, &payload, "Processed")
	case "updated":
		h.handleDocument(ctx, userKey, &payload, "Updated")
	case "consumption_started":
		h.handleConsumptionStarted(ctx, userKey, &payload)
	default:
		slog.Debug("unknown paperless event", "event", payload.Event)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// handleDocument processes "added" and "updated" events.
func (h *Handler) handleDocument(ctx context.Context, userKey string, p *webhookPayload, stateText string) {
	if p.DocID == nil {
		slog.Warn("document event missing doc_id", "event", p.Event)
		return
	}

	slug := fmt.Sprintf("paperless-%d", *p.DocID)
	mapKey := slug

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	name := p.Title
	if name == "" {
		name = "Document"
	}

	if err := cl.CreateActivity(ctx, slug, name, h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create paperless activity", "slug", slug, "error", err)
		return
	}

	subtitle := buildSubtitle(p.DocumentType, p.Correspondent)

	content := pushward.Content{
		Template:    "generic",
		Progress:    1.0,
		State:       stateText,
		Icon:        "doc.text.fill",
		Subtitle:    subtitle,
		AccentColor: "#34C759",
		URL:         p.DocURL,
	}

	req := pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update paperless activity", "slug", slug, "error", err)
		return
	}

	// Store state and schedule two-phase end
	data, _ := json.Marshal(struct{ Slug string }{Slug: slug})
	_ = h.store.Set(ctx, "paperless", userKey, mapKey, "", data, h.config.StaleTimeout)

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)

	slog.Info("paperless document", "slug", slug, "event", p.Event, "state", stateText)
}

// handleConsumptionStarted processes "consumption_started" events.
func (h *Handler) handleConsumptionStarted(ctx context.Context, userKey string, p *webhookPayload) {
	hash := sha256.Sum256([]byte(p.Filename))
	slug := fmt.Sprintf("paperless-%x", hash[:4])
	mapKey := slug

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	name := p.Filename
	if name == "" {
		name = "Document"
	}

	if err := cl.CreateActivity(ctx, slug, name, h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create paperless activity", "slug", slug, "error", err)
		return
	}

	subtitle := buildSubtitle(p.DocumentType, p.Correspondent)

	content := pushward.Content{
		Template:    "generic",
		Progress:    0,
		State:       "Processing...",
		Icon:        "arrow.triangle.2.circlepath",
		Subtitle:    subtitle,
		AccentColor: "#007AFF",
	}

	req := pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update paperless activity", "slug", slug, "error", err)
		return
	}

	// Schedule two-phase end — the activity will be dismissed after EndDelay + EndDisplayTime.
	// If a subsequent "added" event arrives for the same document, it creates a new activity
	// with a doc_id-based slug (different from this filename-based slug).
	h.ender.ScheduleEnd(userKey, mapKey, slug, content)

	slog.Info("paperless consumption started", "slug", slug, "filename", p.Filename)
}

// buildSubtitle constructs "Paperless · DocumentType · Correspondent", omitting empty parts.
func buildSubtitle(docType, correspondent string) string {
	parts := []string{"Paperless"}
	if docType != "" {
		parts = append(parts, docType)
	}
	if correspondent != "" {
		parts = append(parts, correspondent)
	}
	result := parts[0]
	for _, p := range parts[1:] {
		result += " \u00b7 " + p
	}
	return result
}
