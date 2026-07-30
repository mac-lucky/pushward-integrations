package uptimekuma

import (
	"net/http/httptest"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func TestUpstreamRefusalSurfacesUnchanged(t *testing.T) {
	testutil.AssertUpstreamRefusalSurfaces(t, func(t *testing.T, status int) *httptest.ResponseRecorder {
		srv, _, _ := testutil.MockPushWardServerRejecting(t, status, status)
		h := newHandlerWithStore(t, testConfig(), state.NewMemoryStore(), srv.URL)
		return send(t, h, downBody)
	})
}

// A self-test that never reached the upstream must not answer 200. The Test
// button in the provider's own UI exists to tell the user whether their key
// works, so a green checkmark on a refused key answers the one question being
// asked with the wrong answer.
func TestSelfTestRefusalSurfacesUnchanged(t *testing.T) {
	testutil.AssertUpstreamRefusalSurfaces(t, func(t *testing.T, status int) *httptest.ResponseRecorder {
		srv, _, _ := testutil.MockPushWardServerRejecting(t, status, status)
		h := newHandlerWithStore(t, testConfig(), state.NewMemoryStore(), srv.URL)
		// Heartbeat status 3 (MAINTENANCE) is Uptime Kuma's test event.
		return send(t, h, `{
			"monitor": {"id": 1, "name": "My Website", "url": "https://example.com", "type": "http"},
			"heartbeat": {"status": 3, "time": "2024-01-15T10:30:00.000Z", "msg": "Testing", "important": true},
			"msg": "Testing"
		}`)
	})
}
