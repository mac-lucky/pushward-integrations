package text

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

// NonAlphanumeric matches one or more characters that are not lowercase
// letters or digits. Useful for sanitising strings into URL-safe slugs.
var NonAlphanumeric = regexp.MustCompile(`[^a-z0-9]+`)

// HashHex returns the first n bytes of the SHA-256 digest of input, encoded as
// lowercase hexadecimal with no separator. Callers that already supply their own
// join character use this directly; SlugHash wraps it with a "-" separator.
func HashHex(input string, n int) string {
	h := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%x", h[:n])
}

// SlugHash returns a slug of the form "<prefix>-<hex>" where <hex> is the
// first hashBytes bytes of the SHA-256 digest of input, encoded as lowercase
// hexadecimal.
func SlugHash(prefix, input string, hashBytes int) string {
	return prefix + "-" + HashHex(input, hashBytes)
}

// Slug returns a URL-safe slug by lower-casing input, replacing
// non-alphanumeric runs with hyphens, trimming leading/trailing hyphens,
// and prepending prefix.
//
// When input is non-empty but has no ASCII-alphanumeric characters (e.g. a
// CJK-only or emoji-only name, or a pure-symbol string) the normalized body
// collapses to empty, which would make every such input map to the same slug —
// a real hazard because callers use these as Live-Activity identifiers and
// dedup/state keys. In that case it falls back to SlugHash so distinct inputs
// stay distinct. Genuinely-empty input has nothing to disambiguate and still
// returns just the prefix.
func Slug(prefix, input string) string {
	s := strings.ToLower(input)
	s = NonAlphanumeric.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		if input == "" {
			return prefix
		}
		// Distinct non-alphanumeric inputs must stay distinct (used as state
		// keys); Slug's prefix already carries its own separator, so append the
		// bare hash — no double hyphen for "argocd-" prefixes, no leading hyphen
		// for an empty prefix, both of which the slug pattern can reject.
		return prefix + HashHex(input, 8)
	}
	return prefix + s
}
