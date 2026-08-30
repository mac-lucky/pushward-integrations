package text

import "testing"

// The multi-byte cases use escapes rather than literal characters so this file
// stays ASCII: they are test input, not the product typography the repo
// deliberately exempts.
func TestCapitalize(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"error", "Error"},
		{"Error", "Error"},
		{"eRROR", "ERROR"}, // only the first rune is touched
		// Slicing s[:1] would cut these mid-sequence and emit U+FFFD. Every
		// caller feeds this a field off an inbound webhook, so it has to hold.
		{"\u00e9chec", "\u00c9chec"},
		{"\u00fcbung", "\u00dcbung"},
		{"\U0001f680 launch", "\U0001f680 launch"},
	} {
		if got := Capitalize(tc.in); got != tc.want {
			t.Errorf("Capitalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
