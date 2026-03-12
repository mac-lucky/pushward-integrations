package starr

import "testing"

func TestFormatSubtitle_SingleEpisode(t *testing.T) {
	series := SonarrSeries{Title: "Breaking Bad"}
	episodes := []SonarrEpisode{{SeasonNumber: 2, EpisodeNumber: 5}}
	got := FormatSubtitle(series, episodes, "1080p")
	want := "Breaking Bad - S02E05 · 1080p"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatSubtitle_MultipleEpisodes(t *testing.T) {
	series := SonarrSeries{Title: "Breaking Bad"}

	t.Run("consecutive", func(t *testing.T) {
		episodes := []SonarrEpisode{
			{SeasonNumber: 2, EpisodeNumber: 5},
			{SeasonNumber: 2, EpisodeNumber: 6},
			{SeasonNumber: 2, EpisodeNumber: 7},
		}
		got := FormatSubtitle(series, episodes, "1080p")
		want := "Breaking Bad - S02E05-E07 · 1080p"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("non-consecutive", func(t *testing.T) {
		episodes := []SonarrEpisode{
			{SeasonNumber: 2, EpisodeNumber: 5},
			{SeasonNumber: 2, EpisodeNumber: 7},
		}
		got := FormatSubtitle(series, episodes, "1080p")
		want := "Breaking Bad - S02E05, S02E07 · 1080p"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("unsorted input", func(t *testing.T) {
		episodes := []SonarrEpisode{
			{SeasonNumber: 2, EpisodeNumber: 7},
			{SeasonNumber: 2, EpisodeNumber: 5},
			{SeasonNumber: 2, EpisodeNumber: 6},
		}
		got := FormatSubtitle(series, episodes, "720p")
		want := "Breaking Bad - S02E05-E07 · 720p"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestFormatSubtitle_FullSeason(t *testing.T) {
	series := SonarrSeries{Title: "Breaking Bad"}
	episodes := make([]SonarrEpisode, 13)
	for i := range episodes {
		episodes[i] = SonarrEpisode{SeasonNumber: 2, EpisodeNumber: i + 1}
	}
	got := FormatSubtitle(series, episodes, "1080p")
	want := "Breaking Bad - Season 2 (13 episodes) · 1080p"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
