package metrics

import (
	"net/http"
	"testing"
)

// TestSanitizeMethod verifies that the metric "method" label is capped to the
// known RFC 9110 method set plus "other", so an attacker-controlled method
// token cannot inflate Prometheus label cardinality (CVE-2022-21698).
func TestSanitizeMethod(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"GET", http.MethodGet, "GET"},
		{"POST", http.MethodPost, "POST"},
		{"PUT", http.MethodPut, "PUT"},
		{"PATCH", http.MethodPatch, "PATCH"},
		{"DELETE", http.MethodDelete, "DELETE"},
		{"HEAD", http.MethodHead, "HEAD"},
		{"OPTIONS", http.MethodOptions, "OPTIONS"},
		{"CONNECT", http.MethodConnect, "CONNECT"},
		{"TRACE", http.MethodTrace, "TRACE"},
		{"attacker token", "ATTACK-1", "other"},
		{"lowercase get", "get", "other"},
		{"empty", "", "other"},
		{"arbitrary word", "FETCH", "other"},
		{"whitespace", " GET", "other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeMethod(tt.in); got != tt.want {
				t.Errorf("sanitizeMethod(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
