package sonarr

import (
	"fmt"
	"sort"
	"strings"
)

// FormatSubtitle produces the display subtitle for a Sonarr download.
//
// Examples:
//
//	1 episode:             "Breaking Bad - S02E05 · 1080p"
//	2-3 consecutive:       "Breaking Bad - S02E05-E07 · 1080p"
//	2-3 non-consecutive:   "Breaking Bad - S02E05, S02E07 · 1080p"
//	Full season (>3, same): "Breaking Bad - Season 2 (13 episodes) · 1080p"
func FormatSubtitle(series Series, episodes []Episode, quality string) string {
	if len(episodes) == 0 {
		if quality != "" {
			return series.Title + " · " + quality
		}
		return series.Title
	}

	// Sort episodes for consistent display
	sorted := make([]Episode, len(episodes))
	copy(sorted, episodes)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].SeasonNumber != sorted[j].SeasonNumber {
			return sorted[i].SeasonNumber < sorted[j].SeasonNumber
		}
		return sorted[i].EpisodeNumber < sorted[j].EpisodeNumber
	})

	var epPart string
	switch {
	case len(sorted) == 1:
		ep := sorted[0]
		epPart = fmt.Sprintf("S%02dE%02d", ep.SeasonNumber, ep.EpisodeNumber)
	case len(sorted) > 3 && allSameSeason(sorted):
		epPart = fmt.Sprintf("Season %d (%d episodes)", sorted[0].SeasonNumber, len(sorted))
	case allSameSeason(sorted) && isConsecutive(sorted):
		first := sorted[0]
		last := sorted[len(sorted)-1]
		epPart = fmt.Sprintf("S%02dE%02d-E%02d", first.SeasonNumber, first.EpisodeNumber, last.EpisodeNumber)
	default:
		parts := make([]string, len(sorted))
		for i, ep := range sorted {
			parts[i] = fmt.Sprintf("S%02dE%02d", ep.SeasonNumber, ep.EpisodeNumber)
		}
		epPart = strings.Join(parts, ", ")
	}

	subtitle := series.Title + " - " + epPart
	if quality != "" {
		subtitle += " · " + quality
	}
	return subtitle
}

func allSameSeason(episodes []Episode) bool {
	if len(episodes) <= 1 {
		return true
	}
	season := episodes[0].SeasonNumber
	for _, ep := range episodes[1:] {
		if ep.SeasonNumber != season {
			return false
		}
	}
	return true
}

// isConsecutive assumes episodes are already sorted by season and episode number.
func isConsecutive(episodes []Episode) bool {
	for i := 1; i < len(episodes); i++ {
		if episodes[i].EpisodeNumber != episodes[i-1].EpisodeNumber+1 {
			return false
		}
	}
	return true
}
