package overseerr

type webhookPayload struct {
	NotificationType string      `json:"notification_type"` // "MEDIA_PENDING", "MEDIA_APPROVED", etc.
	Event            string      `json:"event"`
	Subject          string      `json:"subject"`
	Message          string      `json:"message"`
	Image            string      `json:"image"`
	Media            mediaInfo   `json:"media"`
	Request          requestInfo `json:"request"`
}

type mediaInfo struct {
	MediaType string `json:"media_type"` // "movie" or "tv"
	TmdbID    string `json:"tmdbId"`
	TvdbID    string `json:"tvdbId"`
	Status    string `json:"status"`   // "UNKNOWN","PENDING","PROCESSING","PARTIALLY_AVAILABLE","AVAILABLE"
	Status4K  string `json:"status4k"`
}

type requestInfo struct {
	RequestID   string `json:"request_id"`
	RequestedBy string `json:"requestedBy_username"`
}
