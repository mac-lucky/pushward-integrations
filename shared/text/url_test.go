package text

import "testing"

func TestSanitizeURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"https", "https://example.com/path", "https://example.com/path"},
		{"http", "http://example.com/path", "http://example.com/path"},
		{"empty", "", ""},
		{"missing host", "https://", ""},
		{"ftp scheme", "ftp://example.com", ""},
		{"javascript scheme", "javascript:alert(1)", ""},
		{"relative path", "/path/only", ""},
		{"malformed", "://broken", ""},
		{"private host kept", "http://grafana.internal/d/abc", "http://grafana.internal/d/abc"},
		{"rfc1918 kept", "https://192.168.1.10/x", "https://192.168.1.10/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeURL(tt.in); got != tt.want {
				t.Errorf("SanitizeURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
