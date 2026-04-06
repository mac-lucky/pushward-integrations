package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireHeader(t *testing.T) {
	tests := []struct {
		name       string
		headerVal  string
		expected   string
		wantStatus int
	}{
		{"valid", "Bearer secret", "Bearer secret", 200},
		{"wrong value", "Bearer wrong", "Bearer secret", 401},
		{"empty header", "", "Bearer secret", 401},
		{"partial match", "Bearer secre", "Bearer secret", 401},
		{"extra chars", "Bearer secretx", "Bearer secret", 401},
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := RequireHeader("Authorization", tt.expected)(inner)
			req := httptest.NewRequest("POST", "/", nil)
			if tt.headerVal != "" {
				req.Header.Set("Authorization", tt.headerVal)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tt.wantStatus {
				t.Errorf("got %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}

func TestCheckHeader(t *testing.T) {
	tests := []struct {
		name      string
		headerVal string
		expected  string
		want      bool
	}{
		{"match", "my-secret", "my-secret", true},
		{"mismatch", "wrong", "my-secret", false},
		{"empty", "", "my-secret", false},
		{"partial", "my-secre", "my-secret", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if tt.headerVal != "" {
				req.Header.Set("X-Webhook-Secret", tt.headerVal)
			}
			got := CheckHeader(req, "X-Webhook-Secret", tt.expected)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
