package starr

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

func (h *Handler) handleSonarrWebhook(w http.ResponseWriter, r *http.Request) {
	raw, ok := decodePayload(w, r)
	if !ok {
		return
	}

	var envelope webhookPayload
	if err := json.Unmarshal(raw, &envelope); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	ctx = metrics.WithProvider(ctx, "starr")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))

	var apiErr error
	switch envelope.EventType {
	case "Grab":
		p, ok := unmarshalPayload[SonarrGrabPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleSonarrGrab(ctx, userKey, log, p)
	case "Download":
		p, ok := unmarshalPayload[SonarrDownloadPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleSonarrDownload(ctx, userKey, log, p)
	case "Test":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "sonarr"); err != nil {
			log.Error("test notification failed", "provider", "sonarr", "error", err)
		}
	case "Health":
		p, ok := unmarshalPayload[HealthPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleHealth(ctx, userKey, log, "sonarr", p)
	case "HealthRestored":
		p, ok := unmarshalPayload[HealthRestoredPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleHealthRestored(ctx, userKey, log, "sonarr", p)
	case "ManualInteractionRequired":
		p, ok := unmarshalPayload[ManualInteractionPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleManualInteraction(ctx, userKey, log, "sonarr", p)
	case "Rename":
		p, ok := unmarshalPayload[SonarrSeriesEventPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleSonarrRename(ctx, userKey, log, p)
	case "SeriesAdd":
		p, ok := unmarshalPayload[SonarrSeriesEventPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleSonarrSeriesAdd(ctx, userKey, log, p)
	case "SeriesDelete":
		p, ok := unmarshalPayload[SonarrSeriesDeletePayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleSonarrSeriesDelete(ctx, userKey, log, p)
	case "EpisodeFileDelete":
		p, ok := unmarshalPayload[SonarrEpisodeFileDeletePayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleSonarrEpisodeFileDelete(ctx, userKey, log, p)
	case "ApplicationUpdate":
		p, ok := unmarshalPayload[ApplicationUpdatePayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleApplicationUpdate(ctx, userKey, log, "sonarr", p)
	case "ImportComplete":
		p, ok := unmarshalPayload[SonarrImportCompletePayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleSonarrImportComplete(ctx, userKey, log, p)
	default:
		slog.Debug("ignored event", "event_type", envelope.EventType)
	}

	if apiErr != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *Handler) handleSonarrGrab(ctx context.Context, userKey string, log *slog.Logger, p *SonarrGrabPayload) error {
	if p.DownloadID == "" {
		log.Warn("grab event missing downloadId")
		return nil
	}

	slug := slugForDownload("sonarr-", p.DownloadID)
	subtitle := FormatSubtitle(p.Series, p.Episodes, p.Release.Quality)
	mapKey := "sonarr:" + p.DownloadID

	// Cancel any existing end timer
	h.ender.StopTimer(userKey, mapKey)

	cl := h.clients.Get(userKey)

	// Always send notification record
	if err := cl.SendNotification(ctx, pushward.SendNotificationRequest{
		Title:      "Sonarr",
		Subtitle:   text.Truncate(subtitle, 100),
		Body:       "Grabbed · " + p.Release.Quality,
		ThreadID:   "sonarr",
		CollapseID: "sonarr-grab",
		Level:      pushward.LevelActive,
		Category:   "grab",
		Source:     "sonarr",
		Push:       h.shouldNotify("Grab"),
	}); err != nil {
		log.Error("failed to send notification", "slug", slug, "error", err)
	}

	// In notify/smart mode for Grab, skip Live Activity
	if h.shouldNotify("Grab") {
		log.Info("grab notification sent", "slug", slug, "series", p.Series.Title, "mode", h.config.Mode)
		return nil
	}

	// Activity mode: existing behavior
	if err := h.setTrackedSlug(ctx, userKey, mapKey, slug); err != nil {
		log.Error("failed to track download", "slug", slug, "error", err)
		return nil
	}

	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())
	if err := cl.CreateActivity(ctx, slug, text.Truncate(subtitle, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
		log.Error("failed to create activity", "slug", slug, "error", err)
		h.deleteTrackedSlug(ctx, userKey, mapKey)
		return err
	}

	step := 1
	total := 2
	req := pushward.UpdateRequest{
		State: pushward.StateOngoing,
		Content: pushward.Content{
			Template:    "steps",
			Progress:    float64(step) / float64(total),
			State:       "Grabbed",
			Icon:        "arrow.down.circle",
			Subtitle:    subtitle,
			AccentColor: pushward.ColorBlue,
			CurrentStep: &step,
			TotalSteps:  &total,
		},
	}
	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update activity", "slug", slug, "error", err)
		return err
	}
	log.Info("grab received", "slug", slug, "series", p.Series.Title, "downloadId", p.DownloadID)
	return nil
}

func (h *Handler) handleSonarrDownload(ctx context.Context, userKey string, log *slog.Logger, p *SonarrDownloadPayload) error {
	if p.DownloadID == "" {
		log.Warn("download event missing downloadId")
		return nil
	}

	slug := slugForDownload("sonarr-", p.DownloadID)
	mapKey := "sonarr:" + p.DownloadID

	// Cancel any existing end timer
	h.ender.StopTimer(userKey, mapKey)

	cl := h.clients.Get(userKey)
	quality := p.EpisodeFile.Quality

	state := "Downloaded"
	if p.IsUpgrade {
		state = "Upgraded"
	}

	subtitle := FormatSubtitle(p.Series, p.Episodes, quality)

	// Always send notification record
	if err := cl.SendNotification(ctx, pushward.SendNotificationRequest{
		Title:      "Sonarr",
		Subtitle:   text.Truncate(subtitle, 100),
		Body:       state,
		ThreadID:   "sonarr",
		CollapseID: "sonarr-download",
		Level:      pushward.LevelActive,
		Category:   "download",
		Source:     "sonarr",
		Push:       h.shouldNotify("Download"),
	}); err != nil {
		log.Error("failed to send notification", "error", err)
	}

	// In notify/smart mode for Download, skip Live Activity
	if h.shouldNotify("Download") {
		log.Info("download notification sent", "slug", slug, "series", p.Series.Title, "mode", h.config.Mode)
		return nil
	}

	// Activity mode: existing behavior
	_, tracked := h.getTrackedSlug(ctx, userKey, mapKey)

	if !tracked {
		if err := h.setTrackedSlug(ctx, userKey, mapKey, slug); err != nil {
			log.Error("failed to track download", "slug", slug, "error", err)
			return nil
		}

		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := cl.CreateActivity(ctx, slug, text.Truncate(subtitle, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
			log.Error("failed to create activity", "slug", slug, "error", err)
			h.deleteTrackedSlug(ctx, userKey, mapKey)
			return err
		}
		log.Info("created activity (download without grab)", "slug", slug)
	}

	step := 2
	total := 2
	content := pushward.Content{
		Template:    "steps",
		Progress:    1.0,
		State:       state,
		Icon:        "checkmark.circle.fill",
		Subtitle:    subtitle,
		AccentColor: pushward.ColorGreen,
		CurrentStep: &step,
		TotalSteps:  &total,
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	log.Info("download complete", "slug", slug, "state", state, "series", p.Series.Title)
	return nil
}

func (h *Handler) handleSonarrRename(ctx context.Context, userKey string, log *slog.Logger, p *SonarrSeriesEventPayload) error {
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Sonarr", Subtitle: p.Series.Title, Body: "Files renamed",
		ThreadID: "sonarr", CollapseID: "sonarr-rename",
		Level: pushward.LevelPassive, Category: "rename", Source: "sonarr", Push: true,
	})
}

func (h *Handler) handleSonarrSeriesAdd(ctx context.Context, userKey string, log *slog.Logger, p *SonarrSeriesEventPayload) error {
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Sonarr", Subtitle: p.Series.Title, Body: "Added to library",
		ThreadID: "sonarr", CollapseID: "sonarr-series-add",
		Level: pushward.LevelActive, Category: "series-add", Source: "sonarr", Push: true,
	})
}

func (h *Handler) handleSonarrSeriesDelete(ctx context.Context, userKey string, log *slog.Logger, p *SonarrSeriesDeletePayload) error {
	body := "Removed"
	if p.DeletedFiles {
		body = "Removed (files deleted)"
	}
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Sonarr", Subtitle: p.Series.Title, Body: body,
		ThreadID: "sonarr", CollapseID: "sonarr-series-delete",
		Level: pushward.LevelActive, Category: "series-delete", Source: "sonarr", Push: true,
	})
}

func (h *Handler) handleSonarrEpisodeFileDelete(ctx context.Context, userKey string, log *slog.Logger, p *SonarrEpisodeFileDeletePayload) error {
	body := "File deleted"
	if p.DeleteReason != "" {
		body += " · " + deleteReasonText(p.DeleteReason)
	}
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Sonarr", Subtitle: text.Truncate(FormatSubtitle(p.Series, p.Episodes, ""), 100), Body: body,
		ThreadID: "sonarr", CollapseID: "sonarr-file-delete",
		Level: pushward.LevelPassive, Category: "file-delete", Source: "sonarr", Push: true,
	})
}

func (h *Handler) handleSonarrImportComplete(ctx context.Context, userKey string, log *slog.Logger, p *SonarrImportCompletePayload) error {
	body := "Import complete"
	if p.IsUpgrade {
		body = "Upgrade complete"
	}
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Sonarr", Subtitle: text.Truncate(FormatSubtitle(p.Series, p.Episodes, ""), 100), Body: body,
		ThreadID: "sonarr", CollapseID: "sonarr-import-complete",
		Level: pushward.LevelActive, Category: "import-complete", Source: "sonarr", Push: true,
	})
}
