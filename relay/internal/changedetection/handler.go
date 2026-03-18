package changedetection

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
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
	h := sha256.Sum256([]byte(url))
	return fmt.Sprintf("cd-%x", h[:4])
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

	ctx := r.Context()
	userKey := auth.KeyFromContext(ctx)
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
		slog.Error("failed to create activity", "slug", slug, "error", err)
		http.Error(w, "failed to create activity", http.StatusInternalServerError)
		return
	}

	content := pushward.Content{
		Template:     "alert",
		Progress:     1.0,
		State:        stateText,
		Icon:         "eye.fill",
		Subtitle:     subtitle,
		AccentColor:  "#FF9500",
		Severity:     "info",
		FiredAt:      firedAtPtr,
		URL:          payload.DiffURL,
		SecondaryURL: payload.PreviewURL,
	}

	ongoingReq := pushward.UpdateRequest{
		State:   pushward.StateOngoing,
		Content: content,
	}
	if err := pwClient.UpdateActivity(ctx, slug, ongoingReq); err != nil {
		slog.Error("failed to update activity to ONGOING", "slug", slug, "error", err)
		http.Error(w, "failed to update activity", http.StatusInternalServerError)
		return
	}

	endedReq := pushward.UpdateRequest{
		State:   pushward.StateEnded,
		Content: content,
	}
	if err := pwClient.UpdateActivity(ctx, slug, endedReq); err != nil {
		slog.Error("failed to update activity to ENDED", "slug", slug, "error", err)
		http.Error(w, "failed to end activity", http.StatusInternalServerError)
		return
	}

	slog.Info("processed changedetection webhook", "slug", slug, "url", payload.URL)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
