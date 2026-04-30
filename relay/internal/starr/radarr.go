package starr

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/mediathread"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/selftest"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

func (h *Handler) handleRadarrWebhook(ctx context.Context, raw []byte) error {
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
		p, err := unmarshalPayload[RadarrGrabPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleRadarrGrab(ctx, userKey, log, p)
	case "Download":
		p, err := unmarshalPayload[RadarrDownloadPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleRadarrDownload(ctx, userKey, log, p)
	case "Test":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "radarr"); err != nil {
			log.Error("test notification failed", "provider", "radarr", "error", err)
		}
	case "Health":
		p, err := unmarshalPayload[HealthPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleHealth(ctx, userKey, log, "radarr", p)
	case "HealthRestored":
		p, err := unmarshalPayload[HealthRestoredPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleHealthRestored(ctx, userKey, log, "radarr", p)
	case "ManualInteractionRequired":
		p, err := unmarshalPayload[ManualInteractionPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleRadarrManualInteraction(ctx, userKey, log, p)
	case "Rename":
		p, err := unmarshalPayload[RadarrMovieEventPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleRadarrRename(ctx, userKey, log, p)
	case "MovieAdded":
		p, err := unmarshalPayload[RadarrMovieEventPayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleRadarrMovieAdded(ctx, userKey, log, p)
	case "MovieDelete":
		p, err := unmarshalPayload[RadarrMovieDeletePayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleRadarrMovieDelete(ctx, userKey, log, p)
	case "MovieFileDelete":
		p, err := unmarshalPayload[RadarrMovieFileDeletePayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleRadarrMovieFileDelete(ctx, userKey, log, p)
	case "ApplicationUpdate":
		p, err := unmarshalPayload[ApplicationUpdatePayload](raw)
		if err != nil {
			return err
		}
		apiErr = h.handleApplicationUpdate(ctx, userKey, log, "radarr", p)
	default:
		slog.Debug("ignored event", "event_type", envelope.EventType)
	}

	if apiErr != nil {
		return upstreamHumaError(apiErr)
	}
	return nil
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

// posterURL returns the remote URL of the first poster image, or empty string.
func posterURL(images []StarrImage) string {
	for _, img := range images {
		if strings.EqualFold(img.CoverType, "poster") && img.RemoteURL != "" {
			return img.RemoteURL
		}
	}
	return ""
}

// radarrMovieURL constructs a deep link to a movie in the Radarr UI.
func radarrMovieURL(appURL string, tmdbID int) string {
	if appURL == "" || tmdbID == 0 {
		return ""
	}
	return strings.TrimRight(appURL, "/") + "/movie/" + strconv.Itoa(tmdbID)
}

func (h *Handler) handleRadarrGrab(ctx context.Context, userKey string, log *slog.Logger, p *RadarrGrabPayload) error {
	if p.DownloadID == "" {
		log.Warn("grab event missing downloadId")
		return nil
	}

	slug, mapKey := radarrContentKey(p.Movie, p.DownloadID)
	title := movieTitle(p.Movie)

	// Cancel any existing end timer for this download
	h.ender.StopTimer(userKey, mapKey)

	cl := h.clients.Get(userKey)

	body := "Grabbed"
	if p.Release.Quality != "" {
		body = "Grabbed · " + p.Release.Quality
	}

	// Always send notification record
	grabReq := pushward.SendNotificationRequest{
		Title:      "Radarr",
		Subtitle:   title,
		Body:       body,
		ThreadID:   radarrMediaThreadID(p.Movie),
		CollapseID: "radarr-grab",
		Level:      pushward.LevelActive,
		Source:     "radarr",
		Push:       h.shouldNotify("Grab"),
		URL:        radarrMovieURL(p.ApplicationURL, p.Movie.TmdbID),
		Media:      pushward.MediaImage(posterURL(p.Movie.Images)),
	}
	meta := map[string]string{"quality": p.Release.Quality}
	if p.Release.Indexer != "" {
		meta["indexer"] = p.Release.Indexer
	}
	if p.Release.ReleaseGroup != "" {
		meta["release_group"] = p.Release.ReleaseGroup
	}
	if p.Release.Size > 0 {
		meta["size"] = text.FormatBytes(p.Release.Size)
	}
	if p.Movie.Title != "" {
		meta["media_title"] = movieTitle(p.Movie)
	}
	if p.Movie.TmdbID > 0 {
		meta["tmdb_id"] = strconv.Itoa(p.Movie.TmdbID)
	}
	grabReq.Metadata = meta
	if err := cl.SendNotification(ctx, grabReq); err != nil {
		log.Error("failed to send notification", "slug", slug, "error", err)
		// Non-fatal: continue to activity creation if applicable
	}

	// In notify/smart mode for Grab, skip Live Activity
	if h.shouldNotify("Grab") {
		log.Info("grab notification sent", "slug", slug, "title", title, "mode", h.config.Mode)
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
		if err := cl.CreateActivity(ctx, slug, title, h.config.Priority, endedTTL, staleTTL); err != nil {
			log.Error("failed to create activity", "slug", slug, "error", err)
			h.deleteTrackedSlug(ctx, userKey, mapKey)
			return err
		}
		log.Info("created activity", "slug", slug, "title", title)
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
			Subtitle:    radarrSubtitle(title, p.Release.Quality),
			AccentColor: pushward.ColorBlue,
			CurrentStep: &step,
			TotalSteps:  &total,
		},
	}

	if err := cl.UpdateActivity(ctx, slug, req); err != nil {
		log.Error("failed to update activity", "slug", slug, "error", err)
		return err
	}
	log.Info("updated activity", "slug", slug, "state", "Grabbed")
	return nil
}

func (h *Handler) handleRadarrDownload(ctx context.Context, userKey string, log *slog.Logger, p *RadarrDownloadPayload) error {
	if p.DownloadID == "" {
		log.Warn("download event missing downloadId")
		return nil
	}

	title := movieTitle(p.Movie)
	slug, mapKey := radarrContentKey(p.Movie, p.DownloadID)

	// Cancel any existing end timer
	h.ender.StopTimer(userKey, mapKey)

	cl := h.clients.Get(userKey)

	state := "Imported"
	if p.IsUpgrade {
		state = "Upgraded"
	}

	// Always send notification record
	dlReq := pushward.SendNotificationRequest{
		Title:      "Radarr",
		Subtitle:   title,
		Body:       state,
		ThreadID:   radarrMediaThreadID(p.Movie),
		CollapseID: "radarr-download",
		Level:      pushward.LevelActive,
		Source:     "radarr",
		Push:       h.shouldNotify("Download"),
		URL:        radarrMovieURL(p.ApplicationURL, p.Movie.TmdbID),
		Media:      pushward.MediaImage(posterURL(p.Movie.Images)),
	}
	dlMeta := map[string]string{"quality": p.MovieFile.Quality}
	if p.MovieFile.Size > 0 {
		dlMeta["size"] = text.FormatBytes(p.MovieFile.Size)
	}
	if p.Movie.Title != "" {
		dlMeta["media_title"] = title
	}
	if p.Movie.TmdbID > 0 {
		dlMeta["tmdb_id"] = strconv.Itoa(p.Movie.TmdbID)
	}
	dlReq.Metadata = dlMeta
	if err := cl.SendNotification(ctx, dlReq); err != nil {
		log.Error("failed to send notification", "error", err)
	}

	// In notify/smart mode for Download, skip Live Activity
	if h.shouldNotify("Download") {
		log.Info("download notification sent", "slug", slug, "title", title, "mode", h.config.Mode)
		return nil
	}

	// Prefer the slug stored at Grab time to preserve continuity across any
	// payload drift between events.
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
		if err := cl.CreateActivity(ctx, slug, title, h.config.Priority, endedTTL, staleTTL); err != nil {
			log.Error("failed to create activity", "slug", slug, "error", err)
			h.deleteTrackedSlug(ctx, userKey, mapKey)
			return err
		}
		log.Info("created activity (untracked download)", "slug", slug, "title", title)
	}

	step := 2
	total := 2
	content := pushward.Content{
		Template:    "steps",
		Progress:    1.0,
		State:       state,
		Icon:        "checkmark.circle.fill",
		Subtitle:    radarrSubtitle(title, p.MovieFile.Quality),
		AccentColor: pushward.ColorGreen,
		CurrentStep: &step,
		TotalSteps:  &total,
	}

	h.ender.ScheduleEnd(userKey, mapKey, slug, content)
	log.Info("scheduled end", "slug", slug, "state", state)
	return nil
}

// handleRadarrManualInteraction sends the manual-interaction push notification
// and, when there is a tracked Live Activity for the movie, flips it into a
// "Needs attention" state so the user sees the failure on lock screen. The
// activity is intentionally left ONGOING — a subsequent Grab for a new release
// (different downloadId, same tmdbId) will update the same slug back to
// "Grabbed", and a successful Download will end it normally.
func (h *Handler) handleRadarrManualInteraction(ctx context.Context, userKey string, log *slog.Logger, p *ManualInteractionPayload) error {
	if err := h.handleManualInteractionNotify(ctx, userKey, log, "radarr", p); err != nil {
		log.Error("manual interaction notify failed", "error", err)
	}

	if p.Movie == nil || h.shouldNotify("ManualInteractionRequired") {
		return nil
	}

	_, mapKey := radarrContentKey(*p.Movie, p.DownloadID)
	slug, tracked := h.getTrackedSlug(ctx, userKey, mapKey)
	if !tracked {
		return nil
	}
	h.ender.StopTimer(userKey, mapKey)

	subtitle := radarrSubtitle(movieTitle(*p.Movie), p.DownloadInfo.Quality)
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

func (h *Handler) handleRadarrRename(ctx context.Context, userKey string, log *slog.Logger, p *RadarrMovieEventPayload) error {
	title := movieTitle(p.Movie)
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Radarr", Subtitle: title, Body: "Renamed",
		ThreadID: radarrMediaThreadID(p.Movie), CollapseID: "radarr-rename",
		Level: pushward.LevelPassive, Source: "radarr", Push: true,
		URL: radarrMovieURL(p.ApplicationURL, p.Movie.TmdbID), Media: pushward.MediaImage(posterURL(p.Movie.Images)),
		Metadata: radarrMovieMeta(p.Movie),
	})
}

func (h *Handler) handleRadarrMovieAdded(ctx context.Context, userKey string, log *slog.Logger, p *RadarrMovieEventPayload) error {
	title := movieTitle(p.Movie)
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Radarr", Subtitle: title, Body: "Added",
		ThreadID: radarrMediaThreadID(p.Movie), CollapseID: "radarr-movie-added",
		Level: pushward.LevelActive, Source: "radarr", Push: true,
		URL: radarrMovieURL(p.ApplicationURL, p.Movie.TmdbID), Media: pushward.MediaImage(posterURL(p.Movie.Images)),
		Metadata: radarrMovieMeta(p.Movie),
	})
}

func (h *Handler) handleRadarrMovieDelete(ctx context.Context, userKey string, log *slog.Logger, p *RadarrMovieDeletePayload) error {
	title := movieTitle(p.Movie)
	body := "Removed"
	if p.DeletedFiles {
		body = "Removed (files deleted)"
	}
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Radarr", Subtitle: title, Body: body,
		ThreadID: radarrMediaThreadID(p.Movie), CollapseID: "radarr-movie-delete",
		Level: pushward.LevelActive, Source: "radarr", Push: true,
		URL: radarrMovieURL(p.ApplicationURL, p.Movie.TmdbID), Media: pushward.MediaImage(posterURL(p.Movie.Images)),
		Metadata: radarrMovieMeta(p.Movie),
	})
}

func (h *Handler) handleRadarrMovieFileDelete(ctx context.Context, userKey string, log *slog.Logger, p *RadarrMovieFileDeletePayload) error {
	title := movieTitle(p.Movie)
	body := "File deleted"
	if p.DeleteReason != "" {
		body = "File deleted · " + deleteReasonText(p.DeleteReason)
	}
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Radarr", Subtitle: title, Body: body,
		ThreadID: radarrMediaThreadID(p.Movie), CollapseID: "radarr-file-delete",
		Level: pushward.LevelPassive, Source: "radarr", Push: true,
		URL: radarrMovieURL(p.ApplicationURL, p.Movie.TmdbID), Media: pushward.MediaImage(posterURL(p.Movie.Images)),
		Metadata: radarrMovieMeta(p.Movie),
	})
}

// radarrMovieMeta returns metadata with media_title and tmdb_id for a movie.
func radarrMovieMeta(m RadarrMovie) map[string]string {
	meta := map[string]string{}
	if m.Title != "" {
		meta["media_title"] = movieTitle(m)
	}
	if m.TmdbID > 0 {
		meta["tmdb_id"] = strconv.Itoa(m.TmdbID)
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

// radarrMediaThreadID returns a cross-provider thread ID for a movie, falling back to "radarr".
func radarrMediaThreadID(m RadarrMovie) string {
	if m.TmdbID > 0 {
		return mediathread.ThreadID("movie", strconv.Itoa(m.TmdbID), "")
	}
	return "radarr"
}
