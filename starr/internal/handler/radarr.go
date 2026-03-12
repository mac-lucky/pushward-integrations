package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/starr/internal/radarr"
)

func (h *Handler) HandleRadarrWebhook(w http.ResponseWriter, r *http.Request) {
	if h.config.Radarr.Username != "" {
		if !checkBasicAuth(r, h.config.Radarr.Username, h.config.Radarr.Password) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	raw, ok := decodePayload(w, r)
	if !ok {
		return
	}

	var envelope radarr.WebhookPayload
	if err := json.Unmarshal(raw, &envelope); err != nil {
		slog.Error("failed to decode event type", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	switch envelope.EventType {
	case "Grab":
		var p radarr.GrabPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Error("failed to decode grab payload", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		h.handleRadarrGrab(ctx, &p)
	case "Download":
		var p radarr.DownloadPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Error("failed to decode download payload", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		h.handleRadarrDownload(ctx, &p)
	default:
		slog.Debug("ignored event", "event_type", envelope.EventType)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func movieTitle(m radarr.Movie) string {
	if m.Year > 0 {
		return fmt.Sprintf("%s (%d)", m.Title, m.Year)
	}
	return m.Title
}

func radarrSubtitle(title, quality string) string {
	subtitle := "Radarr · " + truncate(title, 40)
	if quality != "" {
		subtitle += " · " + quality
	}
	return subtitle
}

func (h *Handler) handleRadarrGrab(ctx context.Context, p *radarr.GrabPayload) {
	if p.DownloadID == "" {
		slog.Warn("grab event missing downloadId")
		return
	}

	slug := slugForDownload("radarr-", p.DownloadID)
	title := movieTitle(p.Movie)
	mapKey := "radarr:" + p.DownloadID

	h.mu.Lock()
	if dl, exists := h.downloads[mapKey]; exists {
		if dl.endTimer != nil {
			dl.endTimer.Stop()
			dl.endTimer = nil
		}
	}
	h.downloads[mapKey] = &trackedDownload{slug: slug}
	h.mu.Unlock()

	endedTTL := int(h.config.PushWard.CleanupDelay.Seconds())
	staleTTL := int(h.config.PushWard.StaleTimeout.Seconds())

	if err := h.client.CreateActivity(ctx, slug, title, h.config.PushWard.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "slug", slug, "error", err)
		h.mu.Lock()
		delete(h.downloads, mapKey)
		h.mu.Unlock()
		return
	}
	slog.Info("created activity", "slug", slug, "title", title)

	req := pushward.UpdateRequest{
		State: "ONGOING",
		Content: pushward.Content{
			Template:    "generic",
			Progress:    0,
			State:       "Grabbed",
			Icon:        "arrow.down.circle",
			Subtitle:    radarrSubtitle(title, p.Release.Quality),
			AccentColor: "#007AFF",
		},
	}

	if err := h.client.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "state", "Grabbed")
}

func (h *Handler) handleRadarrDownload(ctx context.Context, p *radarr.DownloadPayload) {
	title := movieTitle(p.Movie)
	mapKey := "radarr:" + p.DownloadID

	h.mu.Lock()
	dl, tracked := h.downloads[mapKey]
	if tracked {
		if dl.endTimer != nil {
			dl.endTimer.Stop()
			dl.endTimer = nil
		}
	}
	h.mu.Unlock()

	slug := ""
	if tracked {
		slug = dl.slug
	} else {
		// Untracked download (e.g. bridge restart) — create activity
		if p.DownloadID != "" {
			slug = slugForDownload("radarr-", p.DownloadID)
		} else {
			slug = slugForDownload("radarr-", fmt.Sprintf("movie-%d", p.Movie.ID))
		}

		h.mu.Lock()
		h.downloads[mapKey] = &trackedDownload{slug: slug}
		h.mu.Unlock()

		endedTTL := int(h.config.PushWard.CleanupDelay.Seconds())
		staleTTL := int(h.config.PushWard.StaleTimeout.Seconds())
		if err := h.client.CreateActivity(ctx, slug, title, h.config.PushWard.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.mu.Lock()
			delete(h.downloads, mapKey)
			h.mu.Unlock()
			return
		}
		slog.Info("created activity (untracked download)", "slug", slug, "title", title)
	}

	state := "Imported"
	if p.IsUpgrade {
		state = "Upgraded"
	}

	content := pushward.Content{
		Template:    "generic",
		Progress:    1.0,
		State:       state,
		Icon:        "checkmark.circle.fill",
		Subtitle:    radarrSubtitle(title, p.MovieFile.Quality),
		AccentColor: "#34C759",
	}

	h.scheduleEnd(mapKey, slug, content)
	slog.Info("scheduled end", "slug", slug, "state", state)
}
