package humautil

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeJSONContentType(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		headers map[string]string
		want    string
	}{
		{"post missing", http.MethodPost, nil, "application/json"},
		{"post text/plain", http.MethodPost, map[string]string{"Content-Type": "text/plain; charset=utf-8"}, "application/json"},
		{"post Text/Plain case-insensitive", http.MethodPost, map[string]string{"Content-Type": "Text/Plain"}, "application/json"},
		{"post text/plain trailing whitespace", http.MethodPost, map[string]string{"Content-Type": " text/plain ; charset=utf-8"}, "application/json"},
		{"post text/plaintext not a match", http.MethodPost, map[string]string{"Content-Type": "text/plaintext"}, "text/plaintext"},
		{"post application/json unchanged", http.MethodPost, map[string]string{"Content-Type": "application/json"}, "application/json"},
		{"post form unchanged", http.MethodPost, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, "application/x-www-form-urlencoded"},
		{"get text/plain unchanged", http.MethodGet, map[string]string{"Content-Type": "text/plain"}, "text/plain"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			h := NormalizeJSONContentType(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Content-Type")
			}))

			req := httptest.NewRequest(tt.method, "/jellyfin", strings.NewReader("{}"))
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			if got != tt.want {
				t.Errorf("Content-Type = %q, want %q", got, tt.want)
			}
		})
	}
}
