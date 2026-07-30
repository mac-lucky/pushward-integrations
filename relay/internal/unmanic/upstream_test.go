package unmanic

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// newHandlerRejecting builds a handler whose upstream refuses every call with
// status, standing in for a caller whose hlk_ key the server no longer accepts.
func newHandlerRejecting(t *testing.T, status int) http.Handler {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, _, _ := testutil.MockPushWardServerRejecting(t, status, status)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, client.NewPool(srv.URL, nil), testConfig())
	return mux
}

func TestUpstreamRefusalSurfacesUnchanged(t *testing.T) {
	testutil.AssertUpstreamRefusalSurfaces(t, func(t *testing.T, status int) *httptest.ResponseRecorder {
		return send(t, newHandlerRejecting(t, status), `{
			"version": "1.0",
			"title": "Unmanic - Task Completed",
			"message": "Successfully processed: /library/movies/Inception.mkv",
			"type": "success"
		}`)
	})
}

// A self-test that never reached the upstream must not answer 200. The Test
// button in the provider's own UI exists to tell the user whether their key
// works, so a green checkmark on a refused key answers the one question being
// asked with the wrong answer.
func TestSelfTestRefusalSurfacesUnchanged(t *testing.T) {
	testutil.AssertUpstreamRefusalSurfaces(t, func(t *testing.T, status int) *httptest.ResponseRecorder {
		return send(t, newHandlerRejecting(t, status), `{
			"version": "1.0",
			"title": "Unmanic - Test",
			"message": "This is a test notification",
			"type": "info"
		}`)
	})
}
