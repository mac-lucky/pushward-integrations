package auth

import (
	"context"
	"net/http"
	"strings"
)

type contextKey struct{}

// KeyFromContext retrieves the hlk_ integration key from the request context.
func KeyFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(contextKey{}).(string); ok {
		return v
	}
	return ""
}

// Middleware extracts the hlk_ integration key from the request.
//
// Supported patterns:
//  1. Authorization: Bearer hlk_... → use as integration key
//  2. HTTP Basic Auth → extract hlk_ from username field
//
// Returns 401 if no valid key is found.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := extractKey(r)
		if key == "" {
			http.Error(w, `{"error":"missing or invalid integration key"}`, http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), contextKey{}, key)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func extractKey(r *http.Request) string {
	// Pattern 1: Authorization: Bearer hlk_...
	if auth := r.Header.Get("Authorization"); auth != "" {
		if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
			if strings.HasPrefix(after, "hlk_") {
				return after
			}
		}
	}

	// Pattern 2: Basic Auth — hlk_ in username field
	if username, _, ok := r.BasicAuth(); ok {
		if strings.HasPrefix(username, "hlk_") {
			return username
		}
	}

	return ""
}
