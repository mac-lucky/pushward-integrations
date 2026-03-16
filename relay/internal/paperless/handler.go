package paperless

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.PaperlessConfig
	mu      sync.Mutex
	timers  map[string]*time.Timer // "userKey:mapKey" -> end timer
}

func NewHandler(store state.Store, clients *client.Pool, cfg *config.PaperlessConfig) *Handler {
	return &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		timers:  make(map[string]*time.Timer),
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

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

	h.scheduleEnd(userKey, mapKey, slug, content)

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
	h.scheduleEnd(userKey, mapKey, slug, content)

	slog.Info("paperless consumption started", "slug", slug, "filename", p.Filename)
}

// scheduleEnd schedules a two-phase end for an activity:
//   - Phase 1 (after EndDelay): ONGOING update with final content
//   - Phase 2 (EndDisplayTime later): ENDED with same content
func (h *Handler) scheduleEnd(userKey, mapKey, slug string, content pushward.Content) {
	timerKey := userKey + ":" + mapKey
	cl := h.clients.Get(userKey)
	endDelay := h.config.EndDelay
	displayTime := h.config.EndDisplayTime

	h.mu.Lock()
	if existing, ok := h.timers[timerKey]; ok {
		existing.Stop()
	}
	h.timers[timerKey] = time.AfterFunc(endDelay, func() {
		// Phase 1: ONGOING with final content
		ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
		ongoingReq := pushward.UpdateRequest{
			State:   pushward.StateOngoing,
			Content: content,
		}
		if err := cl.UpdateActivity(ctx1, slug, ongoingReq); err != nil {
			slog.Error("failed to update activity (end phase 1)", "slug", slug, "error", err)
		} else {
			slog.Info("updated activity", "slug", slug, "state", content.State)
		}
		cancel1()

		time.Sleep(displayTime)

		// Phase 2: ENDED with same content
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel2()
		endedReq := pushward.UpdateRequest{
			State:   pushward.StateEnded,
			Content: content,
		}
		if err := cl.UpdateActivity(ctx2, slug, endedReq); err != nil {
			slog.Error("failed to end activity (end phase 2)", "slug", slug, "error", err)
		} else {
			slog.Info("ended activity", "slug", slug, "state", content.State)
		}

		// Clean up state store and timer
		delCtx, delCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer delCancel()
		_ = h.store.Delete(delCtx, "paperless", userKey, mapKey, "")

		h.mu.Lock()
		delete(h.timers, timerKey)
		h.mu.Unlock()
	})
	h.mu.Unlock()
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
