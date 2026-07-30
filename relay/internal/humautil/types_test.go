package humautil

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

func TestUpstreamError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"unauthorized", &pushward.HTTPError{StatusCode: http.StatusUnauthorized}, http.StatusUnauthorized},
		{"forbidden", &pushward.HTTPError{StatusCode: http.StatusForbidden}, http.StatusForbidden},
		{"rate limited", &pushward.HTTPError{StatusCode: http.StatusTooManyRequests}, http.StatusTooManyRequests},
		{"other status", &pushward.HTTPError{StatusCode: http.StatusInternalServerError}, http.StatusBadGateway},
		{"wrapped", fmt.Errorf("send: %w", &pushward.HTTPError{StatusCode: http.StatusUnauthorized}), http.StatusUnauthorized},
		{"untyped", errors.New("dial tcp: connection refused"), http.StatusBadGateway},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var se huma.StatusError
			if !errors.As(UpstreamError(tt.err), &se) {
				t.Fatalf("UpstreamError(%v) is not a huma.StatusError", tt.err)
			}
			if se.GetStatus() != tt.want {
				t.Errorf("status = %d, want %d", se.GetStatus(), tt.want)
			}
		})
	}
}

func TestNewIgnoredCarriesTheReason(t *testing.T) {
	r := NewIgnored(StatusIgnoredActivity, "media.tmdbId is empty")
	if r.Body.Status != StatusIgnoredActivity {
		t.Errorf("status = %q, want %q", r.Body.Status, StatusIgnoredActivity)
	}
	if r.Body.Detail != "media.tmdbId is empty" {
		t.Errorf("detail = %q", r.Body.Detail)
	}

	// NewOK stays detail-free so the field is omitted for every other provider.
	if ok := NewOK(); ok.Body.Status != "ok" || ok.Body.Detail != "" {
		t.Errorf("NewOK() = %+v", ok.Body)
	}
}
