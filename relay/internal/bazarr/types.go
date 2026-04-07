package bazarr

// subtitleEvent holds the parsed fields from a Bazarr notification message.
type subtitleEvent struct {
	media    string // "Breaking Bad (2008) - S05E14 - Ozymandias"
	language string // "English", "English HI", "French forced"
	action   string // "downloaded", "upgraded", "manually downloaded"
	provider string // "opensubtitles"
	score    string // "96.0"
}
