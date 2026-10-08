package truenas

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/ack/acktest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

var ackTag = "relay." + text.SlugHash("truenas", alias, 6)

func handlerAt(t *testing.T, url string) http.Handler {
	t.Helper()
	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, state.NewMemoryStore(), client.NewPool(url, nil), testConfig())
	return mux
}

func call(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "GenieKey hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// The handlers honor ?ack=1 like any other route's. TrueNAS itself cannot
// send it; TestAckRoutes covers the paths it uses instead.
func TestAck(t *testing.T) {
	acktest.Run(t, acktest.Provider{
		Key:     "hlk_test",
		Tag:     ackTag,
		Handler: handlerAt,
		Fire: func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
			return call(t, h, http.MethodPost, acktest.Target("/truenas/v2/alerts", query), createBody)
		},
		Resolve: func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
			q := "identifierType=alias"
			if query != "" {
				q += "&" + query
			}
			return call(t, h, http.MethodDelete, acktest.Target("/truenas/v2/alerts/"+alias, q), "")
		},
	})
}

// TrueNAS appends /v2/alerts to its API URL, so it opts in by path:
// https://relay.pushward.app/truenas/ack.
func TestAckRoutes(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	h := handlerAt(t, srv.URL)

	if w := call(t, h, http.MethodPost, "/truenas/ack/v2/alerts", createBody); w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, h, http.MethodDelete, "/truenas/ack/v2/alerts/"+alias+"?identifierType=alias", ""); w.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	recorded := testutil.GetCalls(calls, mu)
	ns := acktest.Notifications(t, recorded)
	if len(ns) != 2 {
		t.Fatalf("%d notifications, want the alert and its resolve", len(ns))
	}
	acktest.AssertAcked(t, ns[0], ackTag, pushward.NotificationAcknowledge{RepeatSeconds: 300, ExpireSeconds: 3600})
	acktest.AssertPlain(t, ns[1])
	acktest.AssertCanceledBeforeResolved(t, recorded, ackTag)
}

// The other overrides still reach the handlers on the ack routes.
func TestAckRoutesKeepOtherOverrides(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	h := handlerAt(t, srv.URL)
	if w := call(t, h, http.MethodPost, "/truenas/ack/v2/alerts?channels=notification&level=critical", createBody); w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	recorded := testutil.GetCalls(calls, mu)
	ns := acktest.Notifications(t, recorded)
	if len(recorded) != 1 || len(ns) != 1 {
		t.Fatalf("calls = %+v, want only the notification", recorded)
	}
	if ns[0].Level != pushward.LevelCritical || ns[0].Acknowledge == nil {
		t.Errorf("level %q acknowledge %+v, want critical and acknowledged", ns[0].Level, ns[0].Acknowledge)
	}
}
