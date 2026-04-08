package mediathread

import "testing"

func TestThreadID(t *testing.T) {
	tests := []struct {
		name      string
		mediaType string
		tmdbID    string
		tvdbID    string
		want      string
	}{
		{"movie with tmdb", "movie", "27205", "", "media-movie-27205"},
		{"movie no id", "movie", "", "", ""},
		{"tv with tvdb", "tv", "", "12345", "media-tv-12345"},
		{"tv with both prefers tvdb", "tv", "99999", "12345", "media-tv-12345"},
		{"tv with tmdb fallback", "tv", "99999", "", "media-tv-tmdb-99999"},
		{"tv no id", "tv", "", "", ""},
		{"series type", "series", "", "67890", "media-tv-67890"},
		{"episode type", "episode", "", "67890", "media-tv-67890"},
		{"unknown type", "music", "111", "222", ""},
		{"empty type", "", "111", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ThreadID(tt.mediaType, tt.tmdbID, tt.tvdbID)
			if got != tt.want {
				t.Errorf("ThreadID(%q, %q, %q) = %q, want %q", tt.mediaType, tt.tmdbID, tt.tvdbID, got, tt.want)
			}
		})
	}
}
