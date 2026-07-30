package komodo

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

func TestUpstreamRefusalSurfacesUnchanged(t *testing.T) {
	testutil.AssertUpstreamRefusalSurfaces(t, func(t *testing.T, status int) *httptest.ResponseRecorder {
		// Not sendTo: that helper fatals on any non-200, which is the response
		// this test exists to assert.
		req := httptest.NewRequest(http.MethodPost, "/komodo", strings.NewReader(bodyUnreachable))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer hlk_test")
		w := httptest.NewRecorder()
		newHandlerRejecting(t, status).ServeHTTP(w, req)
		return w
	})
}
