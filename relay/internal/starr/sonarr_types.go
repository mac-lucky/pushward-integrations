package starr

// SonarrSeries represents a series in Sonarr webhook payloads.
type SonarrSeries struct {
	ID        int          `json:"id"`
	Title     string       `json:"title"`
	Year      int          `json:"year"`
	TitleSlug string       `json:"titleSlug"`
	TvdbID    int          `json:"tvdbId"`
	Images    []StarrImage `json:"images"`
}

// SonarrEpisode represents an episode in Sonarr webhook payloads.
type SonarrEpisode struct {
	ID            int    `json:"id"`
	EpisodeNumber int    `json:"episodeNumber"`
	SeasonNumber  int    `json:"seasonNumber"`
	Title         string `json:"title"`
}

// SonarrRelease represents a grabbed release.
type SonarrRelease struct {
	Quality      string `json:"quality"`
	Size         int64  `json:"size"`
	Indexer      string `json:"indexer"`
	ReleaseGroup string `json:"releaseGroup"`
}

// SonarrEpisodeFile represents an imported episode file.
type SonarrEpisodeFile struct {
	RelativePath string `json:"relativePath"`
	Quality      string `json:"quality"`
	Size         int64  `json:"size"`
}

// SonarrGrabPayload is sent when Sonarr grabs a release.
type SonarrGrabPayload struct {
	EventType      string          `json:"eventType"`
	Series         SonarrSeries    `json:"series"`
	Episodes       []SonarrEpisode `json:"episodes"`
	Release        SonarrRelease   `json:"release"`
	DownloadClient string          `json:"downloadClient"`
	DownloadID     string          `json:"downloadId"`
	ApplicationURL string          `json:"applicationUrl"`
}

// SonarrDownloadPayload is sent when a download is imported.
type SonarrDownloadPayload struct {
	EventType      string            `json:"eventType"`
	Series         SonarrSeries      `json:"series"`
	Episodes       []SonarrEpisode   `json:"episodes"`
	EpisodeFile    SonarrEpisodeFile `json:"episodeFile"`
	IsUpgrade      bool              `json:"isUpgrade"`
	DownloadClient string            `json:"downloadClient"`
	DownloadID     string            `json:"downloadId"`
	ApplicationURL string            `json:"applicationUrl"`
}

// SonarrSeriesEventPayload is used for events that carry only the series
// (Rename, SeriesAdd).
type SonarrSeriesEventPayload struct {
	EventType      string       `json:"eventType"`
	Series         SonarrSeries `json:"series"`
	ApplicationURL string       `json:"applicationUrl"`
}

// SonarrSeriesDeletePayload is sent when a series is deleted from Sonarr.
type SonarrSeriesDeletePayload struct {
	EventType      string       `json:"eventType"`
	Series         SonarrSeries `json:"series"`
	DeletedFiles   bool         `json:"deletedFiles"`
	ApplicationURL string       `json:"applicationUrl"`
}

// SonarrEpisodeFileDeletePayload is sent when an episode file is deleted.
type SonarrEpisodeFileDeletePayload struct {
	EventType      string          `json:"eventType"`
	Series         SonarrSeries    `json:"series"`
	Episodes       []SonarrEpisode `json:"episodes"`
	DeleteReason   string          `json:"deleteReason"`
	ApplicationURL string          `json:"applicationUrl"`
}

// SonarrImportCompletePayload is sent when a download import is fully complete (Sonarr v4+).
type SonarrImportCompletePayload struct {
	EventType      string          `json:"eventType"`
	Series         SonarrSeries    `json:"series"`
	Episodes       []SonarrEpisode `json:"episodes"`
	IsUpgrade      bool            `json:"isUpgrade"`
	ApplicationURL string          `json:"applicationUrl"`
}
