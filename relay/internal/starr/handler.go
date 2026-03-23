package starr

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
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

var nonAlphanumeric = regexp.MustCompile(`[^a-z0-9]+`)

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

// RadarrHandler returns an http.Handler for Radarr webhooks.
func (h *Handler) RadarrHandler() http.Handler {
	return http.HandlerFunc(h.handleRadarrWebhook)
}

// SonarrHandler returns an http.Handler for Sonarr webhooks.
func (h *Handler) SonarrHandler() http.Handler {
	return http.HandlerFunc(h.handleSonarrWebhook)
}

func slugForDownload(prefix, downloadID string) string {
	s := strings.ToLower(downloadID)
	s = nonAlphanumeric.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return prefix + s
}

func decodePayload(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, false
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		slog.Error("failed to decode webhook payload", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return nil, false
	}
	return raw, true
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
	_ = h.store.Delete(ctx, "starr", userKey, mapKey, "")
}

// titleCase capitalises the first rune of a string.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return strings.ToUpper(string(r)) + s[size:]
}

// healthSlug derives a stable slug from the provider and health check type.
func healthSlug(provider, checkType string) string {
	h := sha256.Sum256([]byte(checkType))
	return fmt.Sprintf("%s-health-%x", provider, h[:4])
}

// handleHealth creates an alert-style activity for a health issue.
func (h *Handler) handleHealth(ctx context.Context, userKey, provider string, p *HealthPayload) {
	slug := healthSlug(provider, p.Type)
	mapKey := provider + ":health:" + p.Type

	// Cancel any pending HealthRestored end timer for this check type
	h.ender.StopTimer(userKey, mapKey)

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	name := titleCase(provider) + " Health"
	if err := cl.CreateActivity(ctx, slug, name, h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create health activity", "slug", slug, "error", err)
		return
	}

	icon := "exclamationmark.triangle.fill"
	accent := "#FF9500"
	severity := "warning"
	if p.Level == "error" {
		icon = "exclamationmark.octagon.fill"
		accent = "#FF3B30"
		severity = "error"
	}

	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:    "alert",
			Progress:    1.0,
			State:       text.Truncate(p.Message, 60),
			Icon:        icon,
			Subtitle:    titleCase(provider),
			AccentColor: accent,
			Severity:    severity,
			URL:         p.WikiURL,
		},
	}

	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update health activity", "slug", slug, "error", err)
		return
	}

	// Track in state store for HealthRestored to find
	if err := h.setTrackedSlug(ctx, userKey, mapKey, slug); err != nil {
		slog.Error("failed to track health issue", "slug", slug, "error", err)
	}

	slog.Info("health issue", "slug", slug, "provider", provider, "type", p.Type, "level", p.Level)
}

// handleHealthRestored ends the health activity with a resolved state.
func (h *Handler) handleHealthRestored(ctx context.Context, userKey, provider string, p *HealthRestoredPayload) {
	mapKey := provider + ":health:" + p.Type
	slug, tracked := h.getTrackedSlug(ctx, userKey, mapKey)
	if !tracked {
		slug = healthSlug(provider, p.Type)
	}

	content := pushward.Content{
		Template:    "alert",
		Progress:    1.0,
		State:       text.Truncate(p.Message, 60),
		Icon:        "checkmark.circle.fill",
		Subtitle:    titleCase(provider),
		AccentColor: "#34C759",
		Severity:    "info",
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	slog.Info("health restored", "slug", slug, "provider", provider, "type", p.Type)
}

// handleManualInteraction sends an ONGOING warning update on an existing tracked download.
func (h *Handler) handleManualInteraction(ctx context.Context, userKey, provider string, p *ManualInteractionPayload) {
	if p.DownloadID == "" {
		slog.Warn("manual interaction missing downloadId", "provider", provider)
		return
	}

	mapKey := provider + ":" + p.DownloadID
	slug, tracked := h.getTrackedSlug(ctx, userKey, mapKey)
	if !tracked {
		slog.Debug("manual interaction for untracked download", "provider", provider, "downloadId", p.DownloadID)
		return
	}

	reason := "Import requires manual interaction"
	if len(p.DownloadInfo.StatusMessages) > 0 && len(p.DownloadInfo.StatusMessages[0].Messages) > 0 {
		reason = p.DownloadInfo.StatusMessages[0].Messages[0]
	}

	subtitle := titleCase(provider) + " \u00b7 " + text.Truncate(reason, 50)

	step := 1
	total := 2
	cl := h.clients.Get(userKey)
	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:    "pipeline",
			Progress:    float64(step) / float64(total),
			State:       "Import Failed",
			Icon:        "exclamationmark.triangle.fill",
			Subtitle:    subtitle,
			AccentColor: "#FF9500",
			CurrentStep: &step,
			TotalSteps:  &total,
		},
	}

	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity for manual interaction", "slug", slug, "error", err)
		return
	}
	slog.Info("manual interaction required", "slug", slug, "provider", provider, "downloadId", p.DownloadID)
}
