package starr

import (
	"context"
	"encoding/json"
	"fmt"
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

func (h *Handler) handleRadarrWebhook(w http.ResponseWriter, r *http.Request) {
	raw, ok := decodePayload(w, r)
	if !ok {
		return
	}

	var envelope starrPayload
	if err := json.Unmarshal(raw, &envelope); err != nil {
		slog.Error("failed to decode event type", "error", err)
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
		p, ok := unmarshalPayload[RadarrGrabPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleRadarrGrab(ctx, userKey, log, p)
	case "Download":
		p, ok := unmarshalPayload[RadarrDownloadPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleRadarrDownload(ctx, userKey, log, p)
	case "Test":
		cl := h.clients.Get(userKey)
		if err := selftest.SendTest(ctx, cl, "radarr"); err != nil {
			log.Error("test notification failed", "provider", "radarr", "error", err)
		}
	case "Health":
		p, ok := unmarshalPayload[HealthPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleHealth(ctx, userKey, log, "radarr", p)
	case "HealthRestored":
		p, ok := unmarshalPayload[HealthRestoredPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleHealthRestored(ctx, userKey, log, "radarr", p)
	case "ManualInteractionRequired":
		p, ok := unmarshalPayload[ManualInteractionPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleManualInteraction(ctx, userKey, log, "radarr", p)
	case "Rename":
		p, ok := unmarshalPayload[RadarrMovieEventPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleRadarrRename(ctx, userKey, log, p)
	case "MovieAdded":
		p, ok := unmarshalPayload[RadarrMovieEventPayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleRadarrMovieAdded(ctx, userKey, log, p)
	case "MovieDelete":
		p, ok := unmarshalPayload[RadarrMovieDeletePayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleRadarrMovieDelete(ctx, userKey, log, p)
	case "MovieFileDelete":
		p, ok := unmarshalPayload[RadarrMovieFileDeletePayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleRadarrMovieFileDelete(ctx, userKey, log, p)
	case "ApplicationUpdate":
		p, ok := unmarshalPayload[ApplicationUpdatePayload](raw, w)
		if !ok {
			return
		}
		apiErr = h.handleApplicationUpdate(ctx, userKey, log, "radarr", p)
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

	slug := slugForDownload("radarr-", p.DownloadID)
	title := movieTitle(p.Movie)
	mapKey := "radarr:" + p.DownloadID

	// Cancel any existing end timer for this download
	h.ender.StopTimer(userKey, mapKey)

	cl := h.clients.Get(userKey)

	// Always send notification record
	grabReq := pushward.SendNotificationRequest{
		Title:      "Radarr",
		Subtitle:   title,
		Body:       "Grabbed · " + p.Release.Quality,
		ThreadID:   radarrMediaThreadID(p.Movie),
		CollapseID: "radarr-grab",
		Level:      pushward.LevelActive,
		Category:   "grab",
		Source:     "radarr",
		Push:       h.shouldNotify("Grab"),
		URL:        radarrMovieURL(p.ApplicationURL, p.Movie.TmdbID),
		ImageURL:   posterURL(p.Movie.Images),
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

	// Activity mode: create Live Activity (existing behavior)
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
	log.Info("created activity", "slug", slug, "title", title)

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
	mapKey := "radarr:" + p.DownloadID

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
		Category:   "download",
		Source:     "radarr",
		Push:       h.shouldNotify("Download"),
		URL:        radarrMovieURL(p.ApplicationURL, p.Movie.TmdbID),
		ImageURL:   posterURL(p.Movie.Images),
	}
	dlMeta := map[string]string{"quality": p.MovieFile.Quality}
	if p.MovieFile.Size > 0 {
		dlMeta["size"] = text.FormatBytes(p.MovieFile.Size)
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
		log.Info("download notification sent", "slug", slugForDownload("radarr-", p.DownloadID), "title", title, "mode", h.config.Mode)
		return nil
	}

	// Activity mode: existing behavior
	slug, tracked := h.getTrackedSlug(ctx, userKey, mapKey)

	if !tracked {
		slug = slugForDownload("radarr-", p.DownloadID)

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

func (h *Handler) handleRadarrRename(ctx context.Context, userKey string, log *slog.Logger, p *RadarrMovieEventPayload) error {
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Radarr", Subtitle: movieTitle(p.Movie), Body: "Files renamed",
		ThreadID: radarrMediaThreadID(p.Movie), CollapseID: "radarr-rename",
		Level: pushward.LevelPassive, Category: "rename", Source: "radarr", Push: true,
		URL: radarrMovieURL(p.ApplicationURL, p.Movie.TmdbID), ImageURL: posterURL(p.Movie.Images),
		Metadata: radarrMovieMeta(p.Movie),
	})
}

func (h *Handler) handleRadarrMovieAdded(ctx context.Context, userKey string, log *slog.Logger, p *RadarrMovieEventPayload) error {
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Radarr", Subtitle: movieTitle(p.Movie), Body: "Added to library",
		ThreadID: radarrMediaThreadID(p.Movie), CollapseID: "radarr-movie-added",
		Level: pushward.LevelActive, Category: "movie-added", Source: "radarr", Push: true,
		URL: radarrMovieURL(p.ApplicationURL, p.Movie.TmdbID), ImageURL: posterURL(p.Movie.Images),
		Metadata: radarrMovieMeta(p.Movie),
	})
}

func (h *Handler) handleRadarrMovieDelete(ctx context.Context, userKey string, log *slog.Logger, p *RadarrMovieDeletePayload) error {
	body := "Removed"
	if p.DeletedFiles {
		body = "Removed (files deleted)"
	}
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Radarr", Subtitle: movieTitle(p.Movie), Body: body,
		ThreadID: radarrMediaThreadID(p.Movie), CollapseID: "radarr-movie-delete",
		Level: pushward.LevelActive, Category: "movie-delete", Source: "radarr", Push: true,
		URL: radarrMovieURL(p.ApplicationURL, p.Movie.TmdbID), ImageURL: posterURL(p.Movie.Images),
		Metadata: radarrMovieMeta(p.Movie),
	})
}

func (h *Handler) handleRadarrMovieFileDelete(ctx context.Context, userKey string, log *slog.Logger, p *RadarrMovieFileDeletePayload) error {
	body := "File deleted"
	if p.DeleteReason != "" {
		body += " · " + deleteReasonText(p.DeleteReason)
	}
	return h.sendNotification(ctx, userKey, log, pushward.SendNotificationRequest{
		Title: "Radarr", Subtitle: movieTitle(p.Movie), Body: body,
		ThreadID: radarrMediaThreadID(p.Movie), CollapseID: "radarr-file-delete",
		Level: pushward.LevelPassive, Category: "file-delete", Source: "radarr", Push: true,
		URL: radarrMovieURL(p.ApplicationURL, p.Movie.TmdbID), ImageURL: posterURL(p.Movie.Images),
		Metadata: radarrMovieMeta(p.Movie),
	})
}

// radarrMovieMeta returns metadata with tmdb_id for a movie.
func radarrMovieMeta(m RadarrMovie) map[string]string {
	if m.TmdbID > 0 {
		return map[string]string{"tmdb_id": strconv.Itoa(m.TmdbID)}
	}
	return nil
}

// radarrMediaThreadID returns a cross-provider thread ID for a movie, falling back to "radarr".
func radarrMediaThreadID(m RadarrMovie) string {
	if m.TmdbID > 0 {
		return mediathread.ThreadID("movie", strconv.Itoa(m.TmdbID), "")
	}
	return "radarr"
}
