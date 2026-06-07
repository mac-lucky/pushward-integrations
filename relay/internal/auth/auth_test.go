package auth

import (
	"encoding/base64"
	"testing"
)

// basicHeader builds a "Basic <base64(user:pass)>" Authorization header value.
func basicHeader(scheme, user, pass string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	return scheme + " " + enc
}

func TestExtractKey(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		// Bearer scheme is case-insensitive (RFC 7235).
		{"bearer canonical", "Bearer hlk_x", "hlk_x"},
		{"bearer lowercase", "bearer hlk_x", "hlk_x"},
		{"bearer uppercase", "BEARER hlk_x", "hlk_x"},
		{"bearer mixed case", "BeArEr hlk_x", "hlk_x"},
		{"bearer non-hlk token", "Bearer abc123", ""},
		{"bearer empty token", "Bearer ", ""},

		// Basic scheme: hlk_ taken from the password field, scheme case-insensitive.
		{"basic canonical password", basicHeader("Basic", "user", "hlk_x"), "hlk_x"},
		{"basic lowercase scheme", basicHeader("basic", "user", "hlk_x"), "hlk_x"},
		{"basic empty username", basicHeader("Basic", "", "hlk_x"), "hlk_x"},
		{"basic non-hlk password", basicHeader("Basic", "user", "secret"), ""},
		{"basic no colon", "Basic " + base64.StdEncoding.EncodeToString([]byte("nopassword")), ""},
		{"basic invalid base64", "Basic not_base64!!!", ""},

		// Missing / malformed headers.
		{"empty header", "", ""},
		{"no space", "Bearer", ""},
		{"unknown scheme", "Token hlk_x", ""},
		{"garbage", "garbage", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractKey(tt.header); got != tt.want {
				t.Errorf("ExtractKey(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}
