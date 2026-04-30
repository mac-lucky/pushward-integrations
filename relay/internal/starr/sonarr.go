package starr

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/mediathread"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

func (h *Handler) handleSonarrWebhook(ctx context.Context, raw []byte) error {
	envelope, err := decodeEnvelope(raw)
	if err != nil {
		return err
	}

	ctx = metrics.WithProvider(ctx, "starr")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))

	var apiErr error
	switch envelope.EventType {
	case "Grab":
		p, err := unmarshalPayload[SonarrGrabPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleSonarrGrab(ctx, userKey, log, p)
	case "Download":
		p, err := unmarshalPayload[SonarrDownloadPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleSonarrDownload(ctx, userKey, log, p)
	case "Test":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "sonarr"); err != nil {
			log.Error("test notification failed", "provider", "sonarr", "error", err)
		}
	case "Health":
		p, err := unmarshalPayload[HealthPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleHealth(ctx, userKey, log, "sonarr", p)
	case "HealthRestored":
		p, err := unmarshalPayload[HealthRestoredPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleHealthRestored(ctx, userKey, log, "sonarr", p)
	case "ManualInteractionRequired":
		p, err := unmarshalPayload[ManualInteractionPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleSonarrManualInteraction(ctx, userKey, log, p)
	case "Rename":
		p, err := unmarshalPayload[SonarrSeriesEventPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleSonarrRename(ctx, userKey, log, p)
	case "SeriesAdd":
		p, err := unmarshalPayload[SonarrSeriesEventPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleSonarrSeriesAdd(ctx, userKey, log, p)
	case "SeriesDelete":
		p, err := unmarshalPayload[SonarrSeriesDeletePayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleSonarrSeriesDelete(ctx, userKey, log, p)
	case "EpisodeFileDelete":
		p, err := unmarshalPayload[SonarrEpisodeFileDeletePayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleSonarrEpisodeFileDelete(ctx, userKey, log, p)
	case "ApplicationUpdate":
		p, err := unmarshalPayload[ApplicationUpdatePayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleApplicationUpdate(ctx, userKey, log, "sonarr", p)
	case "ImportComplete":
		p, err := unmarshalPayload[SonarrImportCompletePayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleSonarrImportComplete(ctx, userKey, log, p)
	default:
		slog.Debug("ignored event", "event_type", envelope.EventType)
	}

	if apiErr != nil {
		return upstreamHumaError(apiErr)
	}
	return nil
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

	slug, mapKey := sonarrContentKey(p.Series, p.Episodes, p.DownloadID)
	subtitle := FormatSubtitle(p.Series, p.Episodes, p.Release.Quality)

	// Cancel any existing end timer
	h.ender.StopTimer(userKey, mapKey)

	cl := h.clients.Get(userKey)

	// Always send notification record
	sgReq := pushward.SendNotificationRequest{
		Title:      "Sonarr",
		Subtitle:   text.Truncate(subtitle, 100),
		Body:       "Grabbed",
		ThreadID:   sonarrMediaThreadID(p.Series),
		CollapseID: "sonarr-grab" + episodeCollapseSuffix(p.Episodes),
		Level:      pushward.LevelActive,
		Source:     "sonarr",
		Push:       h.shouldNotify("Grab"),
		URL:        sonarrSeriesURL(p.ApplicationURL, p.Series.TitleSlug),
		Media:      pushward.MediaImage(posterURL(p.Series.Images)),
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
	if p.Series.Title != "" {
		sgMeta["series_title"] = p.Series.Title
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

	// Activity mode. If the same content key was already tracked (retry of a
	// failed release), skip CreateActivity — the update below refreshes the
	// existing activity back to "Grabbed".
	_, alreadyTracked := h.getTrackedSlug(ctx, userKey, mapKey)
	if err := h.setTrackedSlug(ctx, userKey, mapKey, slug); err != nil {
		log.Error("failed to track download", "slug", slug, "error", err)
		return nil
	}

	if !alreadyTracked {
		endedTTL := int(h.config.CleanupDelay.Seconds())
		staleTTL := int(h.config.StaleTimeout.Seconds())
		if err := cl.CreateActivity(ctx, slug, text.Truncate(subtitle, 100), h.config.Priority, endedTTL, staleTTL); err != nil {
			log.Error("failed to create activity", "slug", slug, "error", err)
			h.deleteTrackedSlug(ctx, userKey, mapKey)
			return err
		}
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

	slug, mapKey := sonarrContentKey(p.Series, p.Episodes, p.DownloadID)

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
		Body:       state,
		ThreadID:   sonarrMediaThreadID(p.Series),
		CollapseID: "sonarr-download" + episodeCollapseSuffix(p.Episodes),
		Level:      pushward.LevelActive,
		Source:     "sonarr",
		Push:       h.shouldNotify("Download"),
		URL:        sonarrSeriesURL(p.ApplicationURL, p.Series.TitleSlug),
		Media:      pushward.MediaImage(posterURL(p.Series.Images)),
	}
	sdMeta := map[string]string{"quality": quality}
	if p.EpisodeFile.Size > 0 {
		sdMeta["size"] = text.FormatBytes(p.EpisodeFile.Size)
	}
	if len(p.Episodes) > 0 && p.Episodes[0].Title != "" {
		sdMeta["episode_title"] = p.Episodes[0].Title
	}
	if p.Series.Title != "" {
		sdMeta["series_title"] = p.Series.Title
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

	// Activity mode. Prefer the slug stored at Grab time to preserve
	// continuity across any payload drift between events.
	stored, tracked := h.getTrackedSlug(ctx, userKey, mapKey)
	if tracked {
		slug = stored
	} else {
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

// handleSonarrManualInteraction sends the manual-interaction push notification
// and, when there is a tracked Live Activity for the series+episode set, flips
// it into a "Needs attention" state. The activity stays ONGOING so a later
// Grab (user picks another release) can update it back to "Grabbed".
func (h *Handler) handleSonarrManualInteraction(ctx context.Context, userKey string, log *slog.Logger, p *ManualInteractionPayload) error {
	if err := h.handleManualInteractionNotify(ctx, userKey, log, "sonarr", p); err != nil {
		log.Error("manual interaction notify failed", "error", err)
	}

	if p.Series == nil || h.shouldNotify("ManualInteractionRequired") {
		return nil
	}

	_, mapKey := sonarrContentKey(*p.Series, p.Episodes, p.DownloadID)
	slug, tracked := h.getTrackedSlug(ctx, userKey, mapKey)
	if !tracked {
		return nil
	}
	h.ender.StopTimer(userKey, mapKey)

	subtitle := FormatSubtitle(*p.Series, p.Episodes, p.DownloadInfo.Quality)
	if reason := manualInteractionReason(p); reason != "" {
		subtitle = text.Truncate(reason, 100)
	}

	cl := h.clients.Get(userKey)
	if err := cl.UpdateActivity(ctx, slug, manualInteractionUpdate(subtitle)); err != nil {
		log.Error("failed to update activity (manual interaction)", "slug", slug, "error", err)
		return err
	}
	log.Info("activity marked needs attention", "slug", slug)
	return nil
}

func (h *Handler) handleSonarrRename(ctx context.Context, userKey string, log *slog.Logger, p *SonarrSeriesEventPayload) error {
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Sonarr", Subtitle: p.Series.Title, Body: "Renamed",
		ThreadID: sonarrMediaThreadID(p.Series), CollapseID: "sonarr-rename",
		Level: pushward.LevelPassive, Source: "sonarr", Push: true,
		URL: sonarrSeriesURL(p.ApplicationURL, p.Series.TitleSlug), Media: pushward.MediaImage(posterURL(p.Series.Images)),
		Metadata: sonarrSeriesMeta(p.Series),
	})
}

func (h *Handler) handleSonarrSeriesAdd(ctx context.Context, userKey string, log *slog.Logger, p *SonarrSeriesEventPayload) error {
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Sonarr", Subtitle: p.Series.Title, Body: "Added",
		ThreadID: sonarrMediaThreadID(p.Series), CollapseID: "sonarr-series-add",
		Level: pushward.LevelActive, Source: "sonarr", Push: true,
		URL: sonarrSeriesURL(p.ApplicationURL, p.Series.TitleSlug), Media: pushward.MediaImage(posterURL(p.Series.Images)),
		Metadata: sonarrSeriesMeta(p.Series),
	})
}

func (h *Handler) handleSonarrSeriesDelete(ctx context.Context, userKey string, log *slog.Logger, p *SonarrSeriesDeletePayload) error {
	body := "Removed"
	if p.DeletedFiles {
		body = "Removed (files deleted)"
	}
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Sonarr", Subtitle: p.Series.Title, Body: body,
		ThreadID: sonarrMediaThreadID(p.Series), CollapseID: "sonarr-series-delete",
		Level: pushward.LevelActive, Source: "sonarr", Push: true,
		URL: sonarrSeriesURL(p.ApplicationURL, p.Series.TitleSlug), Media: pushward.MediaImage(posterURL(p.Series.Images)),
		Metadata: sonarrSeriesMeta(p.Series),
	})
}

func (h *Handler) handleSonarrEpisodeFileDelete(ctx context.Context, userKey string, log *slog.Logger, p *SonarrEpisodeFileDeletePayload) error {
	subtitle := FormatSubtitle(p.Series, p.Episodes, "")
	body := "File deleted"
	if p.DeleteReason != "" {
		body = "File deleted · " + deleteReasonText(p.DeleteReason)
	}
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Sonarr", Subtitle: text.Truncate(subtitle, 100), Body: body,
		ThreadID: sonarrMediaThreadID(p.Series), CollapseID: "sonarr-file-delete" + episodeCollapseSuffix(p.Episodes),
		Level: pushward.LevelPassive, Source: "sonarr", Push: true,
		URL: sonarrSeriesURL(p.ApplicationURL, p.Series.TitleSlug), Media: pushward.MediaImage(posterURL(p.Series.Images)),
		Metadata: sonarrSeriesMeta(p.Series),
	})
}

// sonarrSeriesMeta returns metadata with series_title and tvdb_id for a series.
func sonarrSeriesMeta(s SonarrSeries) map[string]string {
	meta := map[string]string{}
	if s.Title != "" {
		meta["series_title"] = s.Title
	}
	if s.TvdbID > 0 {
		meta["tvdb_id"] = strconv.Itoa(s.TvdbID)
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

// sonarrMediaThreadID returns a cross-provider thread ID for a TV series, falling back to "sonarr".
func sonarrMediaThreadID(s SonarrSeries) string {
	if s.TvdbID > 0 {
		return mediathread.ThreadID("tv", "", strconv.Itoa(s.TvdbID))
	}
	return "sonarr"
}

// episodeCollapseSuffix returns a deterministic "-id1-id2-..." suffix from a set
// of episode IDs, used to differentiate APNs collapse IDs per episode so
// concurrent pushes for different episodes of the same series don't replace
// each other on the Lock Screen. Returns empty string if no episode IDs.
//
// APNs caps apns-collapse-id at 64 bytes total; the suffix is budgeted to
// stay well below that when combined with prefixes like "sonarr-import-complete"
// (22 bytes). Season-pack grabs with many episodes are truncated — the ones
// that fit still uniquely identify the set for realistic concurrent-push cases.
func episodeCollapseSuffix(episodes []SonarrEpisode) string {
	if len(episodes) == 0 {
		return ""
	}
	ids := make([]int, 0, len(episodes))
	for _, ep := range episodes {
		if ep.ID > 0 {
			ids = append(ids, ep.ID)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Ints(ids)
	const maxSuffixLen = 40
	var sb strings.Builder
	for _, id := range ids {
		idStr := strconv.Itoa(id)
		if sb.Len()+1+len(idStr) > maxSuffixLen {
			break
		}
		sb.WriteByte('-')
		sb.WriteString(idStr)
	}
	return sb.String()
}

func (h *Handler) handleSonarrImportComplete(ctx context.Context, userKey string, log *slog.Logger, p *SonarrImportCompletePayload) error {
	subtitle := FormatSubtitle(p.Series, p.Episodes, "")
	body := "Imported"
	if p.IsUpgrade {
		body = "Upgraded"
	}
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Sonarr", Subtitle: text.Truncate(subtitle, 100), Body: body,
		ThreadID: sonarrMediaThreadID(p.Series), CollapseID: "sonarr-import-complete" + episodeCollapseSuffix(p.Episodes),
		Level: pushward.LevelActive, Source: "sonarr", Push: true,
		URL: sonarrSeriesURL(p.ApplicationURL, p.Series.TitleSlug), Media: pushward.MediaImage(posterURL(p.Series.Images)),
		Metadata: sonarrSeriesMeta(p.Series),
	})
}
