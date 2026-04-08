package starr

// starrPayload is the minimal envelope to determine event type.
type starrPayload struct {
	EventType string `json:"eventType"`
}

// StarrImage represents an image from a Starr application (poster, banner, etc.).
type StarrImage struct {
	CoverType string `json:"coverType"`
	RemoteURL string `json:"remoteUrl"`
}

// RadarrGrabPayload is sent when Radarr grabs a release.
type RadarrGrabPayload struct {
	EventType      string        `json:"eventType"`
	Movie          RadarrMovie   `json:"movie"`
	Release        RadarrRelease `json:"release"`
	DownloadClient string        `json:"downloadClient"`
	DownloadID     string        `json:"downloadId"`
	ApplicationURL string        `json:"applicationUrl"`
}

// RadarrDownloadPayload is sent when a download is imported.
type RadarrDownloadPayload struct {
	EventType      string          `json:"eventType"`
	Movie          RadarrMovie     `json:"movie"`
	MovieFile      RadarrMovieFile `json:"movieFile"`
	IsUpgrade      bool            `json:"isUpgrade"`
	DownloadClient string          `json:"downloadClient"`
	DownloadID     string          `json:"downloadId"`
	ApplicationURL string          `json:"applicationUrl"`
}

// RadarrMovie represents a movie in Radarr webhook payloads.
type RadarrMovie struct {
	ID     int          `json:"id"`
	Title  string       `json:"title"`
	Year   int          `json:"year"`
	TmdbID int          `json:"tmdbId"`
	Images []StarrImage `json:"images"`
}

// RadarrRelease represents a grabbed release.
type RadarrRelease struct {
	Quality      string `json:"quality"`
	Size         int64  `json:"size"`
	Indexer      string `json:"indexer"`
	ReleaseTitle string `json:"releaseTitle"`
	ReleaseGroup string `json:"releaseGroup"`
}

// RadarrMovieFile represents an imported movie file.
type RadarrMovieFile struct {
	RelativePath string `json:"relativePath"`
	Quality      string `json:"quality"`
	Size         int64  `json:"size"`
}

// RadarrMovieEventPayload is used for events that carry only the movie
// (Rename, MovieAdded).
type RadarrMovieEventPayload struct {
	EventType      string      `json:"eventType"`
	Movie          RadarrMovie `json:"movie"`
	ApplicationURL string      `json:"applicationUrl"`
}

// RadarrMovieDeletePayload is sent when a movie is deleted from Radarr.
type RadarrMovieDeletePayload struct {
	EventType      string      `json:"eventType"`
	Movie          RadarrMovie `json:"movie"`
	DeletedFiles   bool        `json:"deletedFiles"`
	ApplicationURL string      `json:"applicationUrl"`
}

// RadarrMovieFileDeletePayload is sent when a movie file is deleted.
type RadarrMovieFileDeletePayload struct {
	EventType      string          `json:"eventType"`
	Movie          RadarrMovie     `json:"movie"`
	MovieFile      RadarrMovieFile `json:"movieFile"`
	DeleteReason   string          `json:"deleteReason"`
	ApplicationURL string          `json:"applicationUrl"`
}
