package overseerr

type overseerrPayload struct {
	NotificationType string      `json:"notification_type"`
	Event            string      `json:"event"`
	Subject          string      `json:"subject"`
	Message          string      `json:"message"`
	Image            string      `json:"image"`
	Media            mediaInfo   `json:"media"`
	Request          requestInfo `json:"request"`
	Issue            issueInfo   `json:"issue"`
	Comment          commentInfo `json:"comment"`
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

// issueInfo and commentInfo back the ISSUE_* notification types. Overseerr and
// Seerr send `"issue": null` (not an absent key) on every non-issue event; huma
// skips a null on a property that is not required, which every property here is
// under humautil.NewAPI's FieldsOptionalByDefault, so it decodes to the zero
// value instead of failing validation.
type issueInfo struct {
	IssueID     string `json:"issue_id"`
	IssueType   string `json:"issue_type"`
	IssueStatus string `json:"issue_status"`
	ReportedBy  string `json:"reportedBy_username"`
}

type commentInfo struct {
	Message     string `json:"comment_message"`
	CommentedBy string `json:"commentedBy_username"`
}

// threadType returns the media type to group notifications by, independent of
// whether the payload also carries an id the Live Activity slug can use. A `tv`
// payload listing only tvdbId still threads with Sonarr's and Jellyfin's pushes
// about the same show, so gating this on the TMDB id would drop a grouping that
// was fully derivable.
func (p *overseerrPayload) threadType() string {
	switch p.Media.MediaType {
	case "movie", "tv":
		return p.Media.MediaType
	}
	return ""
}

// tvdbID returns the TheTVDB id when it is usable as a thread key. An unrendered
// template variable would otherwise become part of the thread id verbatim.
func (p *overseerrPayload) tvdbID() string {
	if isNumeric(p.Media.TvdbID) {
		return p.Media.TvdbID
	}
	return ""
}

// isNumeric reports whether s is a non-empty run of ASCII digits.
//
// Deliberately stricter than strconv.Atoi, which accepts a leading sign: "+5"
// would pass the gate and then build the slug "overseerr-movie-+5", which
// pushward-server rejects against ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$ - turning a
// payload defect into an opaque upstream error. "-5" is worse, because it
// succeeds and yields "overseerr-movie--5". Digits-only also keeps an
// out-of-range id ("9" x 30) reported as such rather than as a syntax error.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
