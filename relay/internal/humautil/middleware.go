package humautil

import (
	"log/slog"
	"net/http"

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
			huma.WriteErr(api, ctx, http.StatusUnauthorized, "missing or invalid integration key")
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
			huma.WriteErr(api, ctx, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next(ctx)
	}
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
			huma.WriteErr(api, ctx, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next(ctx)
	}
}
