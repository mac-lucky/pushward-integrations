package overseerr

type overseerrPayload struct {
	NotificationType string      `json:"notification_type"`
	Event            string      `json:"event"`
	Subject          string      `json:"subject"`
	Message          string      `json:"message"`
	Image            string      `json:"image"`
	Media            mediaInfo   `json:"media"`
	Request          requestInfo `json:"request"`
}

type mediaInfo struct {
	MediaType string `json:"media_type"`
	TmdbID    string `json:"tmdbId"`
	TvdbID    string `json:"tvdbId"`
	Status    string `json:"status"`
	Status4K  string `json:"status4k"`
}

type requestInfo struct {
	RequestID   string `json:"request_id"`
	RequestedBy string `json:"requestedBy_username"`
}
