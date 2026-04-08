// Package mediathread provides cross-provider notification thread IDs for media items.
// Using a shared thread ID format ensures notifications about the same movie/show
// from different providers (Overseerr, Radarr, Sonarr, Jellyfin) group together
// in iOS Notification Center.
package mediathread

// ThreadID returns a cross-provider thread ID for media grouping.
// Movies use TMDB ID (shared by Radarr, Overseerr, Jellyfin).
// TV shows use TVDB ID (shared by Sonarr, Overseerr, Jellyfin).
// Falls back to TMDB for TV if TVDB is unavailable.
// Returns empty string if no usable ID is available.
func ThreadID(mediaType string, tmdbID, tvdbID string) string {
	switch mediaType {
	case "movie":
		if tmdbID != "" {
			return "media-movie-" + tmdbID
		}
	case "tv", "series", "episode":
		if tvdbID != "" {
			return "media-tv-" + tvdbID
		}
		if tmdbID != "" {
			return "media-tv-tmdb-" + tmdbID
		}
	}
	return ""
}
