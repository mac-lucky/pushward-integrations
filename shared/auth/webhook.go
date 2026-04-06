package auth

import (
	"crypto/subtle"
	"net/http"
)

// RequireHeader returns middleware that rejects requests where the given
// header does not match the expected value (constant-time comparison).
func RequireHeader(header, expected string) func(http.Handler) http.Handler {
	expectedBytes := []byte(expected)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := []byte(r.Header.Get(header))
			if subtle.ConstantTimeCompare(got, expectedBytes) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CheckHeader reports whether the given header matches the expected value
// using constant-time comparison. Useful for inline auth checks inside
// handler methods that cannot easily be wrapped with middleware.
func CheckHeader(r *http.Request, header, expected string) bool {
	got := []byte(r.Header.Get(header))
	return subtle.ConstantTimeCompare(got, []byte(expected)) == 1
}
