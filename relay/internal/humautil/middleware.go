package humautil

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/ratelimit"
)

// AuthMiddleware returns a Huma middleware that extracts the hlk_ integration
// key from the Authorization header and stores it in context.
// Returns 401 if no valid key is found.
func AuthMiddleware(api huma.API) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		key := auth.ExtractKey(ctx.Header("Authorization"))
		if key == "" {
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "missing or invalid integration key")
			return
		}
		ctx = huma.WithValue(ctx, auth.ContextKey(), key)
		next(ctx)
	}
}

// IPRateLimitMiddleware returns a Huma middleware that applies per-IP rate limiting.
// Should be registered before AuthMiddleware.
func IPRateLimitMiddleware(api huma.API) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		ip := ratelimit.ClientIP(ctx.RemoteAddr(), ctx.Header)
		if !ratelimit.AllowIP(ip) {
			slog.Warn("ip rate limit exceeded", "ip", ip)
			ctx.SetHeader("Retry-After", "1")
			_ = huma.WriteErr(api, ctx, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next(ctx)
	}
}

// NormalizeJSONContentType rewrites Content-Type to application/json on POST
// requests arriving with a missing or text/plain header, so Huma's strict
// content negotiation accepts JSON bodies sent by misconfigured webhook
// senders. A non-JSON body still fails — with a 400 from the decoder
// instead of a 415 from negotiation.
func NormalizeJSONContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			ct := r.Header.Get("Content-Type")
			if ct == "" || isTextPlain(ct) {
				r.Header.Set("Content-Type", "application/json")
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isTextPlain reports whether the Content-Type's media type (ignoring
// parameters and whitespace) is text/plain, case-insensitively per RFC 9110.
func isTextPlain(ct string) bool {
	mediaType, _, _ := strings.Cut(ct, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "text/plain")
}

// KeyRateLimitMiddleware returns a Huma middleware that applies per-key rate limiting.
// Must be registered after AuthMiddleware so the key is available in context.
func KeyRateLimitMiddleware(api huma.API) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		key := auth.KeyFromContext(ctx.Context())
		if key == "" {
			next(ctx)
			return
		}
		if !ratelimit.AllowKey(ctx.Method()+" "+ctx.Operation().Path, key) {
			ctx.SetHeader("Retry-After", "1")
			_ = huma.WriteErr(api, ctx, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next(ctx)
	}
}
