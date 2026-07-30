package starr

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// newHandlerRejecting builds a handler whose upstream refuses every call with
// status, standing in for a caller whose hlk_ key the server no longer accepts.
func newHandlerRejecting(t *testing.T, status int) http.Handler {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, _, _ := testutil.MockPushWardServerRejecting(t, status, status)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, state.NewMemoryStore(), client.NewPool(srv.URL, nil), testConfig())
	return mux
}

func sendRaw(t *testing.T, h http.Handler, path, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// A self-test that never reached the upstream must not answer 200. The Test
// button in the provider's own UI exists to tell the user whether their key
// works, so a green checkmark on a refused key answers the one question being
// asked with the wrong answer. All three Starr routes share the shape, and
// Prowlarr has no path other than Test that opens an activity.
func TestSelfTestRefusalSurfacesUnchanged(t *testing.T) {
	for _, route := range []string{"/radarr", "/sonarr", "/prowlarr"} {
		t.Run(route, func(t *testing.T) {
			testutil.AssertUpstreamRefusalSurfaces(t, func(t *testing.T, status int) *httptest.ResponseRecorder {
				return sendRaw(t, newHandlerRejecting(t, status), route, `{"eventType": "Test"}`)
			})
		})
	}
}
