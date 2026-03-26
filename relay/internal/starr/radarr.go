package starr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

func (h *Handler) handleRadarrWebhook(w http.ResponseWriter, r *http.Request) {
	raw, ok := decodePayload(w, r)
	if !ok {
		return
	}

	var envelope webhookPayload
	if err := json.Unmarshal(raw, &envelope); err != nil {
		slog.Error("failed to decode event type", "error", err)
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	userKey := auth.KeyFromContext(ctx)

	switch envelope.EventType {
	case "Grab":
		var p RadarrGrabPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Error("failed to decode grab payload", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		h.handleRadarrGrab(ctx, userKey, &p)
	case "Download":
		var p RadarrDownloadPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Error("failed to decode download payload", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		h.handleRadarrDownload(ctx, userKey, &p)
	case "Test":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "radarr"); err != nil {
			slog.Error("test notification failed", "provider", "radarr", "error", err)
		}
	case "Health":
		var p HealthPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Error("failed to decode health payload", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		h.handleHealth(ctx, userKey, "radarr", &p)
	case "HealthRestored":
		var p HealthRestoredPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Error("failed to decode health restored payload", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		h.handleHealthRestored(ctx, userKey, "radarr", &p)
	case "ManualInteractionRequired":
		var p ManualInteractionPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			slog.Error("failed to decode manual interaction payload", "error", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		h.handleManualInteraction(ctx, userKey, "radarr", &p)
	default:
		slog.Debug("ignored event", "event_type", envelope.EventType)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func movieTitle(m RadarrMovie) string {
	if m.Year > 0 {
		return fmt.Sprintf("%s (%d)", m.Title, m.Year)
	}
	return m.Title
}

func radarrSubtitle(title, quality string) string {
	subtitle := "Radarr · " + text.Truncate(title, 40)
	if quality != "" {
		subtitle += " · " + quality
	}
	return subtitle
}

func (h *Handler) handleRadarrGrab(ctx context.Context, userKey string, p *RadarrGrabPayload) {
	if p.DownloadID == "" {
		slog.Warn("grab event missing downloadId")
		return
	}

	slug := slugForDownload("radarr-", p.DownloadID)
	title := movieTitle(p.Movie)
	mapKey := "radarr:" + p.DownloadID

	// Cancel any existing end timer for this download
	h.ender.StopTimer(userKey, mapKey)

	// Track in state store
	if err := h.setTrackedSlug(ctx, userKey, mapKey, slug); err != nil {
		slog.Error("failed to track download", "slug", slug, "error", err)
		return
	}

	cl := h.clients.Get(userKey)
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())

	if err := cl.CreateActivity(ctx, slug, title, h.config.Priority, endedTTL, staleTTL); err != nil {
		slog.Error("failed to create activity", "slug", slug, "error", err)
		h.deleteTrackedSlug(ctx, userKey, mapKey)
		return
	}
	slog.Info("created activity", "slug", slug, "title", title)

	step := 1
	total := 2
	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:    "steps",
			Progress:    float64(step) / float64(total),
			State:       "Grabbed",
			Icon:        "arrow.down.circle",
			Subtitle:    radarrSubtitle(title, p.Release.Quality),
			AccentColor: "#007AFF",
			CurrentStep: &step,
			TotalSteps:  &total,
		},
	}

	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		slog.Error("failed to update activity", "slug", slug, "error", err)
		return
	}
	slog.Info("updated activity", "slug", slug, "state", "Grabbed")
}

func (h *Handler) handleRadarrDownload(ctx context.Context, userKey string, p *RadarrDownloadPayload) {
	if p.DownloadID == "" {
		slog.Warn("download event missing downloadId")
		return
	}

	title := movieTitle(p.Movie)
	mapKey := "radarr:" + p.DownloadID

	// Cancel any existing end timer
	h.ender.StopTimer(userKey, mapKey)

	slug, tracked := h.getTrackedSlug(ctx, userKey, mapKey)

	if !tracked {
		// Untracked download (e.g. bridge restart) — create activity
		slug = slugForDownload("radarr-", p.DownloadID)

		if err := h.setTrackedSlug(ctx, userKey, mapKey, slug); err != nil {
			slog.Error("failed to track download", "slug", slug, "error", err)
			return
		}

		cl := h.clients.Get(userKey)
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := cl.CreateActivity(ctx, slug, title, h.config.Priority, endedTTL, staleTTL); err != nil {
			slog.Error("failed to create activity", "slug", slug, "error", err)
			h.deleteTrackedSlug(ctx, userKey, mapKey)
			return
		}
		slog.Info("created activity (untracked download)", "slug", slug, "title", title)
	}

	state := "Imported"
	if p.IsUpgrade {
		state = "Upgraded"
	}

	step := 2
	total := 2
	content := pushward.Content{
		Template:    "steps",
		Progress:    1.0,
		State:       state,
		Icon:        "checkmark.circle.fill",
		Subtitle:    radarrSubtitle(title, p.MovieFile.Quality),
		AccentColor: "#34C759",
		CurrentStep: &step,
		TotalSteps:  &total,
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	slog.Info("scheduled end", "slug", slug, "state", state)
}
