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

// SlugHash returns a slug of the form "<prefix>-<hex>" where <hex> is the
// first hashBytes bytes of the SHA-256 digest of input, encoded as lowercase
// hexadecimal.
func SlugHash(prefix, input string, hashBytes int) string {
	h := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%s-%x", prefix, h[:hashBytes])
}

// Slug returns a URL-safe slug by lower-casing input, replacing
// non-alphanumeric runs with hyphens, trimming leading/trailing hyphens,
// and prepending prefix.
func Slug(prefix, input string) string {
	s := strings.ToLower(input)
	s = NonAlphanumeric.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return prefix + s
}
