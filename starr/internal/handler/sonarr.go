package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/starr/internal/sonarr"
)

func (h *Handler) HandleSonarrWebhook(w http.ResponseWriter, r *http.Request) {
	if h.config.Sonarr.Username != "" {
		if !checkBasicAuth(r, h.config.Sonarr.Username, h.config.Sonarr.Password) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

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

	switch envelope.EventType {
	case "Grab":
		var p sonarr.GrabPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Error("failed to decode Grab payload", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		h.handleSonarrGrab(ctx, &p)
	case "Download":
		var p sonarr.DownloadPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Error("failed to decode Download payload", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		h.handleSonarrDownload(ctx, &p)
	default:
		slog.Debug("ignored event", "eventType", envelope.EventType)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleSonarrGrab(ctx context.Context, p *sonarr.GrabPayload) {
	if p.DownloadID == "" {
		slog.Warn("grab event missing downloadId")
		return
	}

	slug := slugForDownload("sonarr-", p.DownloadID)
	subtitle := sonarr.FormatSubtitle(p.Series, p.Episodes, p.Release.Quality)
	mapKey := "sonarr:" + p.DownloadID

	h.mu.Lock()
	if existing, ok := h.downloads[mapKey]; ok {
		if existing.endTimer != nil {
			existing.endTimer.Stop()
		}
	}
	h.downloads[mapKey] = &trackedDownload{slug: slug}
	h.mu.Unlock()

	endedTTL := int(h.config.PushWard.CleanupDelay.Seconds())
	staleTTL := int(h.config.PushWard.StaleTimeout.Seconds())
	if err := h.client.CreateActivity(ctx, slug, truncate(subtitle, 100), h.config.PushWard.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "slug", slug, "error", err)
		h.mu.Lock()
		delete(h.downloads, mapKey)
		h.mu.Unlock()
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
	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("grab received", "slug", slug, "series", p.Series.Title, "downloadId", p.DownloadID)
}

func (h *Handler) handleSonarrDownload(ctx context.Context, p *sonarr.DownloadPayload) {
	if p.DownloadID == "" {
		slog.Warn("download event missing downloadId")
		return
	}

	slug := slugForDownload("sonarr-", p.DownloadID)
	mapKey := "sonarr:" + p.DownloadID

	h.mu.Lock()
	dl, exists := h.downloads[mapKey]
	if exists {
		if dl.endTimer != nil {
			dl.endTimer.Stop()
			dl.endTimer = nil
		}
	}
	h.mu.Unlock()

	quality := p.EpisodeFile.Quality
	if !exists {
		// Download without a prior Grab -- create activity now
		subtitle := sonarr.FormatSubtitle(p.Series, p.Episodes, quality)

		h.mu.Lock()
		h.downloads[mapKey] = &trackedDownload{slug: slug}
		h.mu.Unlock()

		endedTTL := int(h.config.PushWard.CleanupDelay.Seconds())
		staleTTL := int(h.config.PushWard.StaleTimeout.Seconds())
		if err := h.client.CreateActivity(ctx, slug, truncate(subtitle, 100), h.config.PushWard.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			delete(h.downloads, mapKey)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity (download without grab)", "slug", slug)
	}

	state := "Downloaded"
	if p.IsUpgrade {
		state = "Upgraded"
	}

	subtitle := sonarr.FormatSubtitle(p.Series, p.Episodes, quality)
	content := pushward.Content{
		Template:    "generic",
		Progress:    1.0,
		State:       state,
		Icon:        "checkmark.circle.fill",
		Subtitle:    subtitle,
		AccentColor: "#34C759",
	}

	h.scheduleEnd(mapKey, slug, content)
	slog.Info("download complete", "slug", slug, "state", state, "series", p.Series.Title)
}
