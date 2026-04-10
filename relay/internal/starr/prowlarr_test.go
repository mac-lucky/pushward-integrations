package starr

import "testing"

func TestReleaseBaseTitle(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  string
	}{
		{"tv episode", "Outlander.S08E06.1080p.WEB.h264-ETHEL", "Outlander"},
		{"tv multi-word", "For.All.Mankind.S05E03.DV.2160p.ATVP.WEB-DL.DDPA5.1.H.265-NTb", "For.All.Mankind"},
		{"tv season pack", "Monarch.Legacy.of.Monsters.S02.1080p.ATVP.WEB-DL-GROUP", "Monarch.Legacy.of.Monsters"},
		{"tv multi-episode", "Show.Name.S01E01E02.720p.BluRay-GROUP", "Show.Name"},
		{"tv lowercase", "the.show.s03e10.hdtv-group", "the.show"},
		{"movie with year", "Dune.Part.Two.2024.2160p.WEB-DL.DDP5.1.Atmos-GROUP", "Dune.Part.Two"},
		{"movie older year", "The.Shawshank.Redemption.1994.REMASTERED.1080p.BluRay-GROUP", "The.Shawshank.Redemption"},
		{"daily show", "The.Daily.Show.2024.01.15.720p.WEB-GROUP", "The.Daily.Show"},
		{"no match", "random-blob-without-pattern", ""},
		{"empty string", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := releaseBaseTitle(tt.title)
			if got != tt.want {
				t.Errorf("releaseBaseTitle(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}
