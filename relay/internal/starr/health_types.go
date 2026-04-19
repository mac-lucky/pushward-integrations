package starr

// HealthPayload is sent by Radarr/Sonarr on health issues.
type HealthPayload struct {
	EventType string `json:"eventType"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	Type      string `json:"type"`
	WikiURL   string `json:"wikiUrl"`
}

// HealthRestoredPayload is sent when a health issue resolves.
type HealthRestoredPayload struct {
	EventType     string `json:"eventType"`
	Level         string `json:"level"`
	Message       string `json:"message"`
	Type          string `json:"type"`
	PreviousLevel string `json:"previousLevel"`
}

// ManualInteractionPayload is sent when a download needs manual import.
// Movie/Series/Episodes are optional and provider-specific: Radarr populates
// Movie; Sonarr populates Series and Episodes. These are used to derive a
// content-based activity key so the failed-download Live Activity can be
// updated rather than orphaned.
type ManualInteractionPayload struct {
	EventType    string          `json:"eventType"`
	DownloadID   string          `json:"downloadId"`
	DownloadInfo ManualDLInfo    `json:"downloadInfo"`
	Movie        *RadarrMovie    `json:"movie,omitempty"`
	Series       *SonarrSeries   `json:"series,omitempty"`
	Episodes     []SonarrEpisode `json:"episodes,omitempty"`
}

// ManualDLInfo contains download details for manual interaction events.
type ManualDLInfo struct {
	Quality        string              `json:"quality"`
	Title          string              `json:"title"`
	Status         string              `json:"status"`
	StatusMessages []ManualDLStatusMsg `json:"statusMessages"`
}

// ManualDLStatusMsg contains status message details.
type ManualDLStatusMsg struct {
	Title    string   `json:"title"`
	Messages []string `json:"messages"`
}
