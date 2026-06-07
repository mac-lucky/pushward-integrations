package ratelimit

import (
	"net"
	"strings"
)

var ipLimiters = newLimiterMap(5, 20, 5_000)

// trustedProxies holds parsed CIDRs of trusted reverse proxies.
var trustedProxies []*net.IPNet

// SetTrustedProxyCIDRs parses and stores trusted proxy CIDRs.
// Only requests from these CIDRs will have forwarding headers read.
func SetTrustedProxyCIDRs(cidrs []string) error {
	var nets []*net.IPNet
	for _, cidr := range cidrs {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			return err
		}
		nets = append(nets, ipnet)
	}
	trustedProxies = nets
	return nil
}

// isTrustedProxy checks whether the given IP falls within any trusted CIDR.
func isTrustedProxy(ipStr string) bool {
	if len(trustedProxies) == 0 {
		return false
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, cidr := range trustedProxies {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP extracts the client IP address from the given remote address and
// header getter. Forwarding headers (CF-Connecting-IP, X-Real-IP, X-Forwarded-For)
// are only trusted when the direct remote address falls within a configured trusted
// proxy CIDR. Falls back to remote address otherwise.
//
// This function is used by the Huma IP rate-limit middleware in humautil.
func ClientIP(remoteAddr string, getHeader func(string) string) string {
	remoteHost, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		remoteHost = remoteAddr
	}

	if isTrustedProxy(remoteHost) {
		if ip := getHeader("CF-Connecting-IP"); ip != "" && net.ParseIP(ip) != nil {
			return ip
		}
		if ip := getHeader("X-Real-IP"); ip != "" && net.ParseIP(ip) != nil {
			return ip
		}
		if xff := getHeader("X-Forwarded-For"); xff != "" {
			// Proxies APPEND to X-Forwarded-For, so the leftmost entry is
			// client-supplied and spoofable. Walk right-to-left and return the
			// first address that is not itself a trusted proxy (the closest
			// untrusted client). If all hops are trusted, fall through to the
			// direct peer.
			parts := strings.Split(xff, ",")
			for i := len(parts) - 1; i >= 0; i-- {
				candidate := strings.TrimSpace(parts[i])
				if net.ParseIP(candidate) == nil {
					continue
				}
				if !isTrustedProxy(candidate) {
					return candidate
				}
			}
		}
	}

	return remoteHost
}

// AllowIP checks whether a request from the given IP is allowed under IP rate
// limiting. Returns true if allowed.
func AllowIP(ip string) bool {
	return ipLimiters.get(ip).Allow()
}
