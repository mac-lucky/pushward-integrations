package gatus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/ack/acktest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const resolvedBody = `{
	"endpoint_name": "My API",
	"endpoint_group": "",
	"endpoint_url": "https://api.example.com/health",
	"alert_description": "Health check failed",
	"status": "RESOLVED"
}`

func postWith(t *testing.T, h http.Handler, query, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, acktest.Target("/gatus", query), strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAck(t *testing.T) {
	acktest.Run(t, acktest.Provider{
		Key: "hlk_test",
		Tag: "relay." + text.SlugHash("gatus", "My API", 6),
		Handler: func(t *testing.T, url string) http.Handler {
			return newHandlerWithStore(t, testConfig(), state.NewMemoryStore(), url)
		},
		Fire: func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
			return postWith(t, h, query, triggeredBody)
		},
		Resolve: func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
			return postWith(t, h, query, resolvedBody)
		},
	})
}
