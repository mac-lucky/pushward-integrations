package starr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// trackedSlug is stored in the state store as JSON.
type trackedSlug struct {
	Slug string `json:"slug"`
}

type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.StarrConfig
	ender   *lifecycle.Ender
}

func NewHandler(store state.Store, clients *client.Pool, cfg *config.StarrConfig) *Handler {
	return &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "starr", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
	}
}

func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

// RadarrHandler returns an http.Handler for Radarr webhooks.
func (h *Handler) RadarrHandler() http.Handler {
	return http.HandlerFunc(h.handleRadarrWebhook)
}

// SonarrHandler returns an http.Handler for Sonarr webhooks.
func (h *Handler) SonarrHandler() http.Handler {
	return http.HandlerFunc(h.handleSonarrWebhook)
}

// ProwlarrHandler returns an http.Handler for Prowlarr webhooks.
func (h *Handler) ProwlarrHandler() http.Handler {
	return http.HandlerFunc(h.handleProwlarrWebhook)
}

// shouldNotify returns true if the given event type should send a push notification
// instead of (or in addition to) creating a Live Activity.
func (h *Handler) shouldNotify(eventType string) bool {
	switch h.config.Mode {
	case "", config.ModeActivity:
		return false
	case config.ModeNotify:
		return true
	default: // smart
		return eventType == "Grab" || eventType == "Download"
	}
}

func slugForDownload(prefix, downloadID string) string {
	return text.Slug(prefix, downloadID)
}

func decodePayload(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		slog.Error("failed to decode webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return nil, false
	}
	return raw, true
}

func unmarshalPayload[T any](raw json.RawMessage, w http.ResponseWriter) (*T, bool) {
	var p T
	if err := json.Unmarshal(raw, &p); err != nil {
		slog.Error("failed to decode payload", "type", fmt.Sprintf("%T", p), "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return nil, false
	}
	return &p, true
}

// getTrackedSlug retrieves a tracked download slug from the state store.
func (h *Handler) getTrackedSlug(ctx context.Context, userKey, mapKey string) (string, bool) {
	raw, err := h.store.Get(ctx, "starr", userKey, mapKey, "")
	if err != nil || raw == nil {
		return "", false
	}
	var ts trackedSlug
	if err := json.Unmarshal(raw, &ts); err != nil {
		return "", false
	}
	return ts.Slug, true
}

// setTrackedSlug stores a tracked download slug in the state store.
func (h *Handler) setTrackedSlug(ctx context.Context, userKey, mapKey, slug string) error {
	data, err := json.Marshal(trackedSlug{Slug: slug})
	if err != nil {
		return err
	}
	return h.store.Set(ctx, "starr", userKey, mapKey, "", data, 60*time.Minute)
}

// deleteTrackedSlug removes a tracked download from the state store.
func (h *Handler) deleteTrackedSlug(ctx context.Context, userKey, mapKey string) {
	if err := h.store.Delete(ctx, "starr", userKey, mapKey, ""); err != nil {
		slog.Warn("state store delete failed", "error", err, "provider", "starr")
	}
}

// titleCase capitalises the first rune of a string.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return strings.ToUpper(string(r)) + s[size:]
}

// handleHealth sends a push notification for a health issue.
func (h *Handler) handleHealth(ctx context.Context, userKey string, log *slog.Logger, provider string, p *HealthPayload) error {
	body := "Warning"
	if p.Level == "error" {
		body = "Critical"
	}

	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title:      titleCase(provider) + " Health",
		Subtitle:   text.Truncate(p.Message, 80),
		Body:       body,
		ThreadID:   provider + "-health",
		CollapseID: provider + "-health",
		Level:      pushward.LevelActive,
		Category:   "health",
		Source:     provider,
		Push:       true,
	})
}

// handleHealthRestored sends a push notification when a health issue resolves.
func (h *Handler) handleHealthRestored(ctx context.Context, userKey string, log *slog.Logger, provider string, p *HealthRestoredPayload) error {
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title:      titleCase(provider) + " Health",
		Subtitle:   text.Truncate(p.Message, 80),
		Body:       "Resolved",
		ThreadID:   provider + "-health",
		CollapseID: provider + "-health-restored",
		Level:      pushward.LevelPassive,
		Category:   "health-restored",
		Source:     provider,
		Push:       true,
	})
}

// handleManualInteraction sends a push notification when a download needs manual import.
func (h *Handler) handleManualInteraction(ctx context.Context, userKey string, log *slog.Logger, provider string, p *ManualInteractionPayload) error {
	reason := "Import requires manual interaction"
	if len(p.DownloadInfo.StatusMessages) > 0 && len(p.DownloadInfo.StatusMessages[0].Messages) > 0 {
		reason = p.DownloadInfo.StatusMessages[0].Messages[0]
	}

	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title:      titleCase(provider),
		Subtitle:   text.Truncate(p.DownloadInfo.Title, 80),
		Body:       reason,
		ThreadID:   provider,
		CollapseID: provider + "-manual-interaction",
		Level:      pushward.LevelActive,
		Category:   "manual-interaction",
		Source:     provider,
		Push:       true,
	})
}

func (h *Handler) sendNotification(ctx context.Context, userKey string, log *slog.Logger, req pushward.SendNotificationRequest) error {
	cl := h.clients.Get(userKey)
	if err := cl.SendNotification(ctx, req); err != nil {
		log.Error("failed to send notification", "category", req.Category, "error", err)
		return err
	}
	log.Info("notification sent", "category", req.Category, "subtitle", req.Subtitle)
	return nil
}

func (h *Handler) handleApplicationUpdate(ctx context.Context, userKey string, log *slog.Logger, provider string, p *ApplicationUpdatePayload) error {
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title:      titleCase(provider),
		Subtitle:   p.PreviousVersion + " → " + p.NewVersion,
		Body:       "Updated",
		ThreadID:   provider,
		CollapseID: provider + "-update",
		Level:      pushward.LevelPassive,
		Category:   "update",
		Source:     provider,
		Push:       true,
	})
}

// deleteReasonText converts a Radarr/Sonarr delete reason to human-readable text.
func deleteReasonText(reason string) string {
	switch reason {
	case "upgrade":
		return "Upgrade"
	case "manual":
		return "Manual"
	case "missingFromDisk":
		return "Missing from disk"
	case "noLinkedEpisodes":
		return "No linked episodes"
	default:
		return titleCase(reason)
	}
}
