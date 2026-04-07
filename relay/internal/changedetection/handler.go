package changedetection

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

type Handler struct {
	clients *client.Pool
	config  *config.ChangedetectionConfig
}

func NewHandler(clients *client.Pool, cfg *config.ChangedetectionConfig) *Handler {
	return &Handler{
		clients: clients,
		config:  cfg,
	}
}

// slugForURL derives a stable, URL-safe activity slug from a watched URL.
func slugForURL(url string) string {
	return text.SlugHash("cd", url, 4)
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
	ctx = metrics.WithProvider(ctx, "changedetection")

	if err := h.handleChange(ctx, &payload); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) handleChange(ctx context.Context, payload *webhookPayload) error {
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	pwClient := h.clients.Get(userKey)

	slug := slugForURL(payload.URL)

	name := payload.Title
	if name == "" {
		name = payload.URL
	}

	stateText := payload.TriggeredText
	if stateText == "" {
		stateText = "Page changed"
	}

	subtitle := "Changedetection"
	if payload.Tag != "" {
		subtitle = "Changedetection \u00b7 " + payload.Tag
	}

	var firedAtPtr *int64
	if t, err := time.Parse(time.RFC3339, payload.Timestamp); err == nil {
		firedAtPtr = pushward.Int64Ptr(t.Unix())
	}

	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())
	if err := pwClient.CreateActivity(ctx, slug, name, h.config.Priority, endedTTL, staleTTL); err != nil {
		log.Error("failed to create activity", "slug", slug, "error", err)
		return err
	}

	content := pushward.Content{
		Template:     "alert",
		Progress:     1.0,
		State:        stateText,
		Icon:         "eye.fill",
		Subtitle:     subtitle,
		AccentColor:  pushward.ColorOrange,
		Severity:     "info",
		FiredAt:      firedAtPtr,
		URL:          text.SanitizeURL(payload.DiffURL),
		SecondaryURL: text.SanitizeURL(payload.PreviewURL),
	}

	ongoingReq := pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	}
	if err := pwClient.UpdateActivity(ctx, slug, ongoingReq); err != nil {
		log.Error("failed to update activity to ONGOING", "slug", slug, "error", err)
		return err
	}

	endedReq := pushward.UpdateRequest{
		State:   pushward.StateEnded,
		Content: content,
	}
	if err := pwClient.UpdateActivity(ctx, slug, endedReq); err != nil {
		log.Error("failed to update activity to ENDED", "slug", slug, "error", err)
		return err
	}

	log.Info("processed changedetection webhook", "slug", slug, "url", payload.URL)
	return nil
}
