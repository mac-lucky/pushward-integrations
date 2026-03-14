package starr

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

func (h *Handler) handleSonarrWebhook(w http.ResponseWriter, r *http.Request) {
	raw, ok := decodePayload(w, r)
	if !ok {
		return
	}

	var envelope struct {
		EventType string `json:"eventType"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	userKey := auth.KeyFromContext(ctx)

	switch envelope.EventType {
	case "Grab":
		var p SonarrGrabPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Error("failed to decode Grab payload", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		h.handleSonarrGrab(ctx, userKey, &p)
	case "Download":
		var p SonarrDownloadPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Error("failed to decode Download payload", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		h.handleSonarrDownload(ctx, userKey, &p)
	default:
		slog.Debug("ignored event", "eventType", envelope.EventType)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleSonarrGrab(ctx context.Context, userKey string, p *SonarrGrabPayload) {
	if p.DownloadID == "" {
		slog.Warn("grab event missing downloadId")
		return
	}

	slug := slugForDownload("sonarr-", p.DownloadID)
	subtitle := FormatSubtitle(p.Series, p.Episodes, p.Release.Quality)
	mapKey := "sonarr:" + p.DownloadID

	// Cancel any existing end timer
	h.stopTimer(userKey, mapKey)

	// Track in state store
	if err := h.setTrackedSlug(ctx, userKey, mapKey, slug); err != nil {
		slog.Error("failed to track download", "slug", slug, "error", err)
		return
	}

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())
	if err := cl.CreateActivity(ctx, slug, truncate(subtitle, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "slug", slug, "error", err)
		h.deleteTrackedSlug(ctx, userKey, mapKey)
		return
	}

	req := pushward.UpdateRequest{
		State: "ONGOING",
		Content: pushward.Content{
			Template:    "generic",
			Progress:    0,
			State:       "Grabbed",
			Icon:        "arrow.down.circle",
			Subtitle:    subtitle,
			AccentColor: "#007AFF",
		},
	}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("grab received", "slug", slug, "series", p.Series.Title, "downloadId", p.DownloadID)
}

func (h *Handler) handleSonarrDownload(ctx context.Context, userKey string, p *SonarrDownloadPayload) {
	if p.DownloadID == "" {
		slog.Warn("download event missing downloadId")
		return
	}

	slug := slugForDownload("sonarr-", p.DownloadID)
	mapKey := "sonarr:" + p.DownloadID

	// Cancel any existing end timer
	h.stopTimer(userKey, mapKey)

	_, tracked := h.getTrackedSlug(ctx, userKey, mapKey)

	quality := p.EpisodeFile.Quality
	if !tracked {
		// Download without a prior Grab — create activity now
		subtitle := FormatSubtitle(p.Series, p.Episodes, quality)

		if err := h.setTrackedSlug(ctx, userKey, mapKey, slug); err != nil {
			slog.Error("failed to track download", "slug", slug, "error", err)
			return
		}

		cl := h.clients.Get(userKey)
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := cl.CreateActivity(ctx, slug, truncate(subtitle, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.deleteTrackedSlug(ctx, userKey, mapKey)
			return
		}
		slog.Info("created activity (download without grab)", "slug", slug)
	}

	state := "Downloaded"
	if p.IsUpgrade {
		state = "Upgraded"
	}

	subtitle := FormatSubtitle(p.Series, p.Episodes, quality)
	content := pushward.Content{
		Template:    "generic",
		Progress:    1.0,
		State:       state,
		Icon:        "checkmark.circle.fill",
		Subtitle:    subtitle,
		AccentColor: "#34C759",
	}

	h.scheduleEnd(userKey, mapKey, slug, content)
	slog.Info("download complete", "slug", slug, "state", state, "series", p.Series.Title)
}
