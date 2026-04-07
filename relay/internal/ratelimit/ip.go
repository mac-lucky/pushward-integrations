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

// clientIP extracts the client IP address from the request.
// Forwarding headers (CF-Connecting-IP, X-Real-IP, X-Forwarded-For) are only
// trusted when the direct RemoteAddr falls within a configured trusted proxy CIDR.
// Falls back to RemoteAddr otherwise.
func clientIP(r *http.Request) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}

	if isTrustedProxy(remoteHost) {
		if ip := r.Header.Get("CF-Connecting-IP"); ip != "" && net.ParseIP(ip) != nil {
			return ip
		}
		if ip := r.Header.Get("X-Real-IP"); ip != "" && net.ParseIP(ip) != nil {
			return ip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
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

// IPMiddleware applies per-IP rate limiting. Should run before auth middleware.
func IPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)

		if !ipLimiters.get(ip).Allow() {
			slog.Warn("ip rate limit exceeded", "ip", ip) // #nosec G706 -- ip is validated via net.ParseIP in clientIP()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}
