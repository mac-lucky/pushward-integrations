package starr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// upstreamStatus maps a pushward API error to the HTTP status the webhook
// caller should see. Auth/authz failures surface unchanged so the caller
// (Sonarr/Radarr/etc.) reports "auth failed" instead of a generic 502.
func upstreamStatus(err error) int {
	var he *pushward.HTTPError
	if errors.As(err, &he) {
		switch he.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
			return he.StatusCode
		}
	}
	return http.StatusBadGateway
}

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

// RegisterRoutes registers the Radarr, Sonarr, and Prowlarr webhook endpoints
// and returns the Handler so the caller can collect the Ender for graceful shutdown.
func RegisterRoutes(api huma.API, store state.Store, clients *client.Pool, cfg *config.StarrConfig) *Handler {
	h := &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, store, "starr", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
	}

	humautil.RegisterWebhook(api, "/radarr", "post-radarr-webhook",
		"Receive Radarr webhook",
		"Processes Radarr movie download and library events.",
		[]string{"Starr", "Radarr"}, h.handleRadarrHuma)

	humautil.RegisterWebhook(api, "/sonarr", "post-sonarr-webhook",
		"Receive Sonarr webhook",
		"Processes Sonarr TV show download and library events.",
		[]string{"Starr", "Sonarr"}, h.handleSonarrHuma)

	humautil.RegisterWebhook(api, "/prowlarr", "post-prowlarr-webhook",
		"Receive Prowlarr webhook",
		"Processes Prowlarr indexer health and application update events.",
		[]string{"Starr", "Prowlarr"}, h.handleProwlarrHuma)

	return h
}

func (h *Handler) Ender() *lifecycle.Ender {
	return h.ender
}

// handleRadarrHuma is the Huma handler for Radarr webhooks. Uses RawBody
// because starr does two-phase JSON parsing (envelope → type-specific struct).
func (h *Handler) handleRadarrHuma(ctx context.Context, input *struct {
	RawBody []byte
},
) (*humautil.WebhookResponse, error) {
	return h.dispatchRaw(ctx, input.RawBody, h.handleRadarrWebhook)
}

func (h *Handler) handleSonarrHuma(ctx context.Context, input *struct {
	RawBody []byte
},
) (*humautil.WebhookResponse, error) {
	return h.dispatchRaw(ctx, input.RawBody, h.handleSonarrWebhook)
}

func (h *Handler) handleProwlarrHuma(ctx context.Context, input *struct {
	RawBody []byte
},
) (*humautil.WebhookResponse, error) {
	return h.dispatchRaw(ctx, input.RawBody, h.handleProwlarrWebhook)
}

// dispatchRaw creates an httptest request/response pair and delegates to the
// existing http.HandlerFunc. This avoids rewriting the complex two-phase parsing
// logic in each starr sub-handler during the initial migration.
// TODO: refactor sub-handlers to accept (ctx, raw []byte) directly.
func (h *Handler) dispatchRaw(ctx context.Context, raw []byte, handler func(http.ResponseWriter, *http.Request)) (*humautil.WebhookResponse, error) {
	// Import is at file level; these are only used in this bridge.
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	handler(w, r)
	switch w.Code {
	case http.StatusBadGateway:
		return nil, huma.Error502BadGateway("upstream API error")
	case http.StatusUnauthorized:
		return nil, huma.Error401Unauthorized("upstream rejected integration key")
	case http.StatusForbidden:
		return nil, huma.Error403Forbidden("upstream denied integration key")
	case http.StatusTooManyRequests:
		return nil, huma.Error429TooManyRequests("upstream rate limited")
	case http.StatusBadRequest:
		return nil, huma.Error400BadRequest("invalid payload")
	default:
		if w.Code >= 400 {
			return nil, huma.Error502BadGateway("upstream API error")
		}
	}
	return humautil.NewOK(), nil
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
	level := "Warning"
	if p.Level == "error" {
		level = "Critical"
	}
	body := level + " · " + text.Truncate(p.Message, 100)

	req := pushward.SendNotificationRequest{
		Title:      titleCase(provider) + " Health",
		Subtitle:   text.Truncate(p.Message, 80),
		Body:       body,
		ThreadID:   provider + "-health",
		CollapseID: provider + "-health",
		Level:      pushward.LevelActive,
		Category:   "health",
		Source:     provider,
		Push:       true,
		URL:        p.WikiURL,
	}
	meta := map[string]string{"level": p.Level}
	if p.Type != "" {
		meta["type"] = p.Type
	}
	req.Metadata = meta
	return h.sendNotification(ctx, userKey, log, req)
}

// handleHealthRestored sends a push notification when a health issue resolves.
func (h *Handler) handleHealthRestored(ctx context.Context, userKey string, log *slog.Logger, provider string, p *HealthRestoredPayload) error {
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title:      titleCase(provider) + " Health",
		Subtitle:   text.Truncate(p.Message, 80),
		Body:       "Resolved · " + text.Truncate(p.Message, 100),
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
	return h.clients.SendNotification(ctx, userKey, log, req)
}

func (h *Handler) handleApplicationUpdate(ctx context.Context, userKey string, log *slog.Logger, provider string, p *ApplicationUpdatePayload) error {
	meta := map[string]string{
		"previous_version": p.PreviousVersion,
		"new_version":      p.NewVersion,
	}
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title:      titleCase(provider),
		Subtitle:   p.PreviousVersion + " → " + p.NewVersion,
		Body:       "Updated · " + p.PreviousVersion + " → " + p.NewVersion,
		ThreadID:   provider,
		CollapseID: provider + "-update",
		Level:      pushward.LevelPassive,
		Category:   "update",
		Source:     provider,
		Push:       true,
		Metadata:   meta,
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
