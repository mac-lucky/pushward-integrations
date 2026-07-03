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

// MapKeyPrefix returns a short hex hash of an API key for use as an
// in-memory map key prefix. Uses 8 bytes (16 hex chars) of SHA-256
// for collision resistance across many tenants.
func MapKeyPrefix(key string) string {
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h[:8])
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
//  1. Bearer hlk_... -> use as integration key
//  2. Basic Auth -> extract hlk_ from password field
//  3. GenieKey hlk_... -> TrueNAS's OpsGenie alert service sends the API key
//     under the OpsGenie "GenieKey" scheme
func ExtractKey(authHeader string) string {
	// RFC 7235 defines the auth-scheme token as case-insensitive, so match it
	// with EqualFold rather than an exact-case prefix (webhook UIs and HTTP
	// libraries vary in casing).
	scheme, rest, ok := strings.Cut(authHeader, " ")
	if !ok {
		return ""
	}

	switch {
	// Pattern 1: Bearer hlk_...
	case strings.EqualFold(scheme, "Bearer"):
		if strings.HasPrefix(rest, "hlk_") {
			return rest
		}
	// Pattern 2: Basic Auth — hlk_ in password field
	case strings.EqualFold(scheme, "Basic"):
		if decoded, err := base64.StdEncoding.DecodeString(rest); err == nil {
			if _, password, ok := strings.Cut(string(decoded), ":"); ok {
				if strings.HasPrefix(password, "hlk_") {
					return password
				}
			}
		}
	// Pattern 3: GenieKey hlk_... (OpsGenie scheme used by TrueNAS)
	case strings.EqualFold(scheme, "GenieKey"):
		if strings.HasPrefix(rest, "hlk_") {
			return rest
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
