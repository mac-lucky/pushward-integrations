package radarr

// WebhookPayload is the minimal envelope to determine event type.
type WebhookPayload struct {
	EventType string `json:"eventType"`
}

// GrabPayload is sent when Radarr grabs a release.
type GrabPayload struct {
	EventType      string  `json:"eventType"`
	Movie          Movie   `json:"movie"`
	Release        Release `json:"release"`
	DownloadClient string  `json:"downloadClient"`
	DownloadID     string  `json:"downloadId"`
}

// DownloadPayload is sent when a download is imported.
type DownloadPayload struct {
	EventType      string    `json:"eventType"`
	Movie          Movie     `json:"movie"`
	MovieFile      MovieFile `json:"movieFile"`
	IsUpgrade      bool      `json:"isUpgrade"`
	DownloadClient string    `json:"downloadClient"`
	DownloadID     string    `json:"downloadId"`
}

// Movie represents a movie in Radarr webhook payloads.
type Movie struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Year  int    `json:"year"`
}

// Release represents a grabbed release.
type Release struct {
	Quality      string `json:"quality"`
	Size         int64  `json:"size"`
	Indexer      string `json:"indexer"`
	ReleaseTitle string `json:"releaseTitle"`
}

// MovieFile represents an imported movie file.
type MovieFile struct {
	RelativePath string `json:"relativePath"`
	Quality      string `json:"quality"`
	Size         int64  `json:"size"`
}
