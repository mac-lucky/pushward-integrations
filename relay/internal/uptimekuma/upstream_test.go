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
