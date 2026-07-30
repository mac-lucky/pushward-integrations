package argocd

import (
	"net/http/httptest"
	"testing"

	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func TestUpstreamRefusalSurfacesUnchanged(t *testing.T) {
	testutil.AssertUpstreamRefusalSurfaces(t, func(t *testing.T, status int) *httptest.ResponseRecorder {
		srv, _, _ := testutil.MockPushWardServerRejecting(t, status, status)
		mux, _ := setupHandler(t, testConfig(), srv.URL)
		return sendWebhook(t, mux, `{
			"app": "pushward-server",
			"project": "default",
			"event": "sync-running",
			"sync_status": "OutOfSync",
			"health_status": "Healthy",
			"operation_phase": "Running",
			"revision": "abc123",
			"message": "synchronization started"
		}`)
	})
}
