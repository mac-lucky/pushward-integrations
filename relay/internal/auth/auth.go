package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

type contextKey struct{}

// ContextKey returns the context key used for the integration key.
func ContextKey() any { return contextKey{} }

// KeyHash returns a short hex hash of an API key for log correlation.
func KeyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h[:4])
}

// KeyFromContext retrieves the hlk_ integration key from the request context.
func KeyFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(contextKey{}).(string); ok {
		return v
	}
	return ""
}

// ExtractKey extracts the hlk_ integration key from an Authorization header value.
//
// Supported patterns:
//  1. Bearer hlk_... → use as integration key
//  2. Basic Auth → extract hlk_ from password field
func ExtractKey(authHeader string) string {
	if authHeader == "" {
		return ""
	}

	// Pattern 1: Bearer hlk_...
	if after, ok := strings.CutPrefix(authHeader, "Bearer "); ok {
		if strings.HasPrefix(after, "hlk_") {
			return after
		}
	}

	// Pattern 2: Basic Auth — hlk_ in password field
	if after, ok := strings.CutPrefix(authHeader, "Basic "); ok {
		decoded, err := base64.StdEncoding.DecodeString(after)
		if err == nil {
			if _, password, ok := strings.Cut(string(decoded), ":"); ok {
				if strings.HasPrefix(password, "hlk_") {
					return password
				}
			}
		}
	}

	return ""
}

// Middleware extracts the hlk_ integration key from the request.
// Returns 401 if no valid key is found.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := ExtractKey(r.Header.Get("Authorization"))
		if key == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"missing or invalid integration key"}`))
			return
		}

		ctx := context.WithValue(r.Context(), contextKey{}, key)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
