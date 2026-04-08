package ratelimit

import (
	"log/slog"
	"net"
	"net/http"
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
// This function is used by both the stdlib IPMiddleware and Huma middleware.
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
			if first, _, _ := strings.Cut(xff, ","); first != "" {
				first = strings.TrimSpace(first)
				if net.ParseIP(first) != nil {
					return first
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

// IPMiddleware applies per-IP rate limiting. Should run before auth middleware.
func IPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r.RemoteAddr, r.Header.Get)

		if !AllowIP(ip) {
			slog.Warn("ip rate limit exceeded", "ip", ip) // #nosec G706 -- ip is validated via net.ParseIP in ClientIP()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}
