package overseerr

import (
	"net/http/httptest"
	"testing"

	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// A self-test that never reached the upstream must not answer 200. The Test
// button in the provider's own UI exists to tell the user whether their key
// works, so a green checkmark on a refused key answers the one question being
// asked with the wrong answer. This is the path issue #8 was first reported on.
func TestSelfTestRefusalSurfacesUnchanged(t *testing.T) {
	testutil.AssertUpstreamRefusalSurfaces(t, func(t *testing.T, status int) *httptest.ResponseRecorder {
		h, _, _ := newHandlerRejecting(t, testConfig(), status)
		return send(t, h, `{
			"notification_type": "TEST_NOTIFICATION",
			"subject": "Test Notification"
		}`)
	})
}
