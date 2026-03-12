package sonarr

// Webhook payload types

type Series struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Year  int    `json:"year"`
}

type Episode struct {
	EpisodeNumber int    `json:"episodeNumber"`
	SeasonNumber  int    `json:"seasonNumber"`
	Title         string `json:"title"`
}

type Release struct {
	Quality string `json:"quality"`
	Size    int64  `json:"size"`
}

type EpisodeFile struct {
	RelativePath string `json:"relativePath"`
	Quality      string `json:"quality"`
	Size         int64  `json:"size"`
}

type GrabPayload struct {
	EventType      string    `json:"eventType"`
	Series         Series    `json:"series"`
	Episodes       []Episode `json:"episodes"`
	Release        Release   `json:"release"`
	DownloadClient string    `json:"downloadClient"`
	DownloadID     string    `json:"downloadId"`
}

type DownloadPayload struct {
	EventType      string      `json:"eventType"`
	Series         Series      `json:"series"`
	Episodes       []Episode   `json:"episodes"`
	EpisodeFile    EpisodeFile `json:"episodeFile"`
	IsUpgrade      bool        `json:"isUpgrade"`
	DownloadClient string      `json:"downloadClient"`
	DownloadID     string      `json:"downloadId"`
}
