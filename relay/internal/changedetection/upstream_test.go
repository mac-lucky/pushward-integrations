package changedetection

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// newHandlerRejecting builds a handler whose upstream refuses every call with
// status, standing in for a caller whose hlk_ key the server no longer accepts.
func newHandlerRejecting(t *testing.T, status int) http.Handler {
	t.Helper()
	srv, _, _ := testutil.MockPushWardServerRejecting(t, status, status)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, client.NewPool(srv.URL, nil), testConfig())
	return mux
}

func TestUpstreamRefusalSurfacesUnchanged(t *testing.T) {
	testutil.AssertUpstreamRefusalSurfaces(t, func(t *testing.T, status int) *httptest.ResponseRecorder {
		return send(t, newHandlerRejecting(t, status), `{
			"url": "https://example.com/product/widget",
			"title": "Widget Product Page",
			"tag": "prices",
			"diff_url": "https://cd.example.com/diff/550e8400",
			"preview_url": "https://cd.example.com/preview/550e8400",
			"triggered_text": "Price: $29.99",
			"timestamp": "2024-01-15T10:30:00Z"
		}`)
	})
}
