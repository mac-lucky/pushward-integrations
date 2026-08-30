package text

import (
	"strings"
	"unicode/utf8"
)

// Capitalize upper-cases the first rune of s and leaves the rest alone. It
// decodes that rune rather than slicing s[:1], so a multi-byte leading rune
// survives instead of being cut mid-sequence into U+FFFD - which matters
// because every caller feeds it a field off an inbound webhook.
func Capitalize(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return strings.ToUpper(string(r)) + s[size:]
}
