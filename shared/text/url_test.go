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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeURL(tt.in); got != tt.want {
				t.Errorf("SanitizeURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeHTTPSURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"https public", "https://example.com/path", "https://example.com/path"},
		{"http rejected", "http://example.com/path", ""},
		{"empty", "", ""},
		{"missing host", "https://", ""},
		{"ftp scheme", "ftp://example.com", ""},
		{"javascript scheme", "javascript:alert(1)", ""},
		{"relative path", "/path/only", ""},
		{"malformed", "://broken", ""},
		// Server uses case-sensitive HasPrefix("https://"); we canonicalize so
		// uppercase callers don't silently fail server-side.
		{"uppercase scheme canonicalized", "HTTPS://example.com/path", "https://example.com/path"},
		// Private/internal hosts mirror server's isPrivateHost rejection.
		{"localhost rejected", "https://localhost/x", ""},
		{"localhost with port rejected", "https://localhost:8443/x", ""},
		{"dot-internal rejected", "https://grafana.internal/d/abc", ""},
		{"rfc1918 192.168 rejected", "https://192.168.1.10/x", ""},
		{"rfc1918 10.x rejected", "https://10.0.0.5/x", ""},
		{"rfc1918 172.16 rejected", "https://172.16.0.1/x", ""},
		{"loopback ip rejected", "https://127.0.0.1/x", ""},
		{"link-local rejected", "https://169.254.169.254/x", ""},
		{"public ip kept", "https://8.8.8.8/path", "https://8.8.8.8/path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeHTTPSURL(tt.in); got != tt.want {
				t.Errorf("SanitizeHTTPSURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
