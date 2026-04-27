package text

import "net/url"

// SanitizeURL returns rawURL unchanged if it is a valid http or https URL,
// or an empty string otherwise.
func SanitizeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return rawURL
}
