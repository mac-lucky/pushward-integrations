package starr

// WebhookPayload is the minimal envelope to determine event type.
type WebhookPayload struct {
	EventType string `json:"eventType"`
}

// RadarrGrabPayload is sent when Radarr grabs a release.
type RadarrGrabPayload struct {
	EventType      string       `json:"eventType"`
	Movie          RadarrMovie  `json:"movie"`
	Release        RadarrRelease `json:"release"`
	DownloadClient string       `json:"downloadClient"`
	DownloadID     string       `json:"downloadId"`
}

// RadarrDownloadPayload is sent when a download is imported.
type RadarrDownloadPayload struct {
	EventType      string         `json:"eventType"`
	Movie          RadarrMovie    `json:"movie"`
	MovieFile      RadarrMovieFile `json:"movieFile"`
	IsUpgrade      bool           `json:"isUpgrade"`
	DownloadClient string         `json:"downloadClient"`
	DownloadID     string         `json:"downloadId"`
}

// RadarrMovie represents a movie in Radarr webhook payloads.
type RadarrMovie struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Year  int    `json:"year"`
}

// RadarrRelease represents a grabbed release.
type RadarrRelease struct {
	Quality      string `json:"quality"`
	Size         int64  `json:"size"`
	Indexer      string `json:"indexer"`
	ReleaseTitle string `json:"releaseTitle"`
}

// RadarrMovieFile represents an imported movie file.
type RadarrMovieFile struct {
	RelativePath string `json:"relativePath"`
	Quality      string `json:"quality"`
	Size         int64  `json:"size"`
}
