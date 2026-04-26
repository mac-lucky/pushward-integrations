package text

import (
	"net"
	"net/url"
	"strings"
)

// SanitizeURL returns rawURL unchanged if it is a valid http or https URL,
// or an empty string otherwise.
func SanitizeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return rawURL
}

// SanitizeHTTPSURL returns a canonicalized https URL or an empty string.
// Use this for notification request URL, ImageURL, and IconURL fields, which
// the PushWard server rejects unless they (a) start with the lowercase scheme
// "https://" and (b) point to a public host. Mirrors the server's two-stage
// validation in pushward-server/internal/api/notification_handler.go so the
// relay drops URLs that would otherwise produce a 400 round-trip per alert.
func SanitizeHTTPSURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	if isPrivateHost(u) {
		return ""
	}
	// Canonicalize: url.Parse lowercases u.Scheme, so re-emitting via
	// u.String() ensures the scheme prefix matches the server's case-sensitive
	// strings.HasPrefix(req.URL, "https://") check even when callers pass
	// "HTTPS://".
	return u.String()
}

// isPrivateHost mirrors pushward-server's isPrivateHost so the relay can
// pre-empt server validation. Returns true for localhost, *.internal, and any
// loopback/private/link-local IP.
func isPrivateHost(u *url.URL) bool {
	host := u.Hostname()
	ip := net.ParseIP(host)
	if ip == nil {
		if strings.EqualFold(host, "localhost") ||
			strings.HasSuffix(strings.ToLower(host), ".internal") {
			return true
		}
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}
