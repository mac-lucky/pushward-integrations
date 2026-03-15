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
type ManualInteractionPayload struct {
	EventType    string       `json:"eventType"`
	DownloadID   string       `json:"downloadId"`
	DownloadInfo ManualDLInfo `json:"downloadInfo"`
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
