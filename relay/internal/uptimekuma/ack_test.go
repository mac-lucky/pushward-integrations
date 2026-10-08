package uptimekuma

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/ack/acktest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const upBody = `{
	"monitor": {"id": 1, "name": "My Website", "url": "https://example.com", "type": "http"},
	"heartbeat": {"status": 1, "time": "2024-01-15T10:35:00.000Z", "msg": "", "ping": 42, "duration": 300, "important": true},
	"msg": "My Website is UP"
}`

func postWith(t *testing.T, h http.Handler, query, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, acktest.Target("/uptimekuma", query), strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAck(t *testing.T) {
	acktest.Run(t, acktest.Provider{
		Key: "hlk_test",
		Tag: "relay." + text.SlugHash("uptimekuma", "1", 6),
		Handler: func(t *testing.T, url string) http.Handler {
			return newHandlerWithStore(t, testConfig(), state.NewMemoryStore(), url)
		},
		Fire: func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
			return postWith(t, h, query, downBody)
		},
		Resolve: func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
			return postWith(t, h, query, upBody)
		},
	})
}

// level=passive would make the alert one that cannot repeat.
func TestAckWithPassiveLevelRejected(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())
	if w := postWith(t, h, "ack=1&level=passive", downBody); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (%s)", w.Code, w.Body.String())
	}
	if n := len(testutil.GetCalls(calls, mu)); n != 0 {
		t.Errorf("%d upstream calls for a rejected request", n)
	}
}
