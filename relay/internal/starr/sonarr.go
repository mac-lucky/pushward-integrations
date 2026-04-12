package starr

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/mediathread"
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

	var envelope starrPayload
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
	_, _ = w.Write([]byte("ok"))
}

// sonarrSeriesURL constructs a deep link to a series in the Sonarr UI.
func sonarrSeriesURL(appURL, titleSlug string) string {
	if appURL == "" || titleSlug == "" {
		return ""
	}
	return strings.TrimRight(appURL, "/") + "/series/" + titleSlug
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
	sgReq := pushward.SendNotificationRequest{
		Title:      "Sonarr",
		Subtitle:   text.Truncate(subtitle, 100),
		Body:       "Grabbed · " + text.Truncate(subtitle, 80),
		ThreadID:   sonarrMediaThreadID(p.Series),
		CollapseID: "sonarr-grab",
		Level:      pushward.LevelActive,
		Category:   "grab",
		Source:     "sonarr",
		Push:       h.shouldNotify("Grab"),
		URL:        sonarrSeriesURL(p.ApplicationURL, p.Series.TitleSlug),
		ImageURL:   posterURL(p.Series.Images),
	}
	sgMeta := map[string]string{"quality": p.Release.Quality}
	if p.Release.Indexer != "" {
		sgMeta["indexer"] = p.Release.Indexer
	}
	if p.Release.ReleaseGroup != "" {
		sgMeta["release_group"] = p.Release.ReleaseGroup
	}
	if p.Release.Size > 0 {
		sgMeta["size"] = text.FormatBytes(p.Release.Size)
	}
	if len(p.Episodes) > 0 && p.Episodes[0].Title != "" {
		sgMeta["episode_title"] = p.Episodes[0].Title
	}
	if p.Series.TvdbID > 0 {
		sgMeta["tvdb_id"] = strconv.Itoa(p.Series.TvdbID)
	}
	sgReq.Metadata = sgMeta
	if err := cl.SendNotification(ctx, sgReq); err != nil {
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
	sdReq := pushward.SendNotificationRequest{
		Title:      "Sonarr",
		Subtitle:   text.Truncate(subtitle, 100),
		Body:       state + " · " + text.Truncate(subtitle, 80),
		ThreadID:   sonarrMediaThreadID(p.Series),
		CollapseID: "sonarr-download",
		Level:      pushward.LevelActive,
		Category:   "download",
		Source:     "sonarr",
		Push:       h.shouldNotify("Download"),
		URL:        sonarrSeriesURL(p.ApplicationURL, p.Series.TitleSlug),
		ImageURL:   posterURL(p.Series.Images),
	}
	sdMeta := map[string]string{"quality": quality}
	if p.EpisodeFile.Size > 0 {
		sdMeta["size"] = text.FormatBytes(p.EpisodeFile.Size)
	}
	if len(p.Episodes) > 0 && p.Episodes[0].Title != "" {
		sdMeta["episode_title"] = p.Episodes[0].Title
	}
	if p.Series.TvdbID > 0 {
		sdMeta["tvdb_id"] = strconv.Itoa(p.Series.TvdbID)
	}
	sdReq.Metadata = sdMeta
	if err := cl.SendNotification(ctx, sdReq); err != nil {
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
		Title: "Sonarr", Subtitle: p.Series.Title, Body: "Renamed · " + p.Series.Title,
		ThreadID: sonarrMediaThreadID(p.Series), CollapseID: "sonarr-rename",
		Level: pushward.LevelPassive, Category: "rename", Source: "sonarr", Push: true,
		URL: sonarrSeriesURL(p.ApplicationURL, p.Series.TitleSlug), ImageURL: posterURL(p.Series.Images),
		Metadata: sonarrSeriesMeta(p.Series),
	})
}

func (h *Handler) handleSonarrSeriesAdd(ctx context.Context, userKey string, log *slog.Logger, p *SonarrSeriesEventPayload) error {
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Sonarr", Subtitle: p.Series.Title, Body: "Added · " + p.Series.Title,
		ThreadID: sonarrMediaThreadID(p.Series), CollapseID: "sonarr-series-add",
		Level: pushward.LevelActive, Category: "series-add", Source: "sonarr", Push: true,
		URL: sonarrSeriesURL(p.ApplicationURL, p.Series.TitleSlug), ImageURL: posterURL(p.Series.Images),
		Metadata: sonarrSeriesMeta(p.Series),
	})
}

func (h *Handler) handleSonarrSeriesDelete(ctx context.Context, userKey string, log *slog.Logger, p *SonarrSeriesDeletePayload) error {
	body := "Removed · " + p.Series.Title
	if p.DeletedFiles {
		body = "Removed (files deleted) · " + p.Series.Title
	}
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Sonarr", Subtitle: p.Series.Title, Body: body,
		ThreadID: sonarrMediaThreadID(p.Series), CollapseID: "sonarr-series-delete",
		Level: pushward.LevelActive, Category: "series-delete", Source: "sonarr", Push: true,
		URL: sonarrSeriesURL(p.ApplicationURL, p.Series.TitleSlug), ImageURL: posterURL(p.Series.Images),
		Metadata: sonarrSeriesMeta(p.Series),
	})
}

func (h *Handler) handleSonarrEpisodeFileDelete(ctx context.Context, userKey string, log *slog.Logger, p *SonarrEpisodeFileDeletePayload) error {
	subtitle := FormatSubtitle(p.Series, p.Episodes, "")
	body := "File deleted · " + text.Truncate(subtitle, 60)
	if p.DeleteReason != "" {
		body = "File deleted · " + deleteReasonText(p.DeleteReason) + " · " + text.Truncate(subtitle, 50)
	}
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Sonarr", Subtitle: text.Truncate(subtitle, 100), Body: body,
		ThreadID: sonarrMediaThreadID(p.Series), CollapseID: "sonarr-file-delete",
		Level: pushward.LevelPassive, Category: "file-delete", Source: "sonarr", Push: true,
		URL: sonarrSeriesURL(p.ApplicationURL, p.Series.TitleSlug), ImageURL: posterURL(p.Series.Images),
		Metadata: sonarrSeriesMeta(p.Series),
	})
}

// sonarrSeriesMeta returns metadata with tvdb_id for a series.
func sonarrSeriesMeta(s SonarrSeries) map[string]string {
	if s.TvdbID > 0 {
		return map[string]string{"tvdb_id": strconv.Itoa(s.TvdbID)}
	}
	return nil
}

// sonarrMediaThreadID returns a cross-provider thread ID for a TV series, falling back to "sonarr".
func sonarrMediaThreadID(s SonarrSeries) string {
	if s.TvdbID > 0 {
		return mediathread.ThreadID("tv", "", strconv.Itoa(s.TvdbID))
	}
	return "sonarr"
}

func (h *Handler) handleSonarrImportComplete(ctx context.Context, userKey string, log *slog.Logger, p *SonarrImportCompletePayload) error {
	subtitle := FormatSubtitle(p.Series, p.Episodes, "")
	body := "Imported · " + text.Truncate(subtitle, 80)
	if p.IsUpgrade {
		body = "Upgraded · " + text.Truncate(subtitle, 80)
	}
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Sonarr", Subtitle: text.Truncate(subtitle, 100), Body: body,
		ThreadID: sonarrMediaThreadID(p.Series), CollapseID: "sonarr-import-complete",
		Level: pushward.LevelActive, Category: "import-complete", Source: "sonarr", Push: true,
		URL: sonarrSeriesURL(p.ApplicationURL, p.Series.TitleSlug), ImageURL: posterURL(p.Series.Images),
		Metadata: sonarrSeriesMeta(p.Series),
	})
}
