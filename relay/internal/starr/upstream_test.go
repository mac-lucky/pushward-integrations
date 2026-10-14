package starr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/poster"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// newHandlerRejecting builds a handler whose upstream refuses every call with
// status, standing in for a caller whose hlk_ key the server no longer accepts.
func newHandlerRejecting(t *testing.T, status int) http.Handler {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, _, _ := testutil.MockPushWardServerRejecting(t, status, status)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, state.NewMemoryStore(), client.NewPool(srv.URL, nil), testConfig(), poster.Disabled{})
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

// atActivityLimit builds a handler whose upstream takes notifications and
// answers every activity create with the server's 409 activity.limit_exceeded.
func atActivityLimit(t *testing.T) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServerRejecting(t, 0, http.StatusConflict)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, state.NewMemoryStore(), client.NewPool(srv.URL, nil), testConfig(), poster.Disabled{})
	return mux, calls, mu
}

// A Grab or Download past the account's activity limit has already gone out
// as a notification, so it answers 200 ignored_activity. An error status here
// made every file of a bulk import count as an upstream failure.
func TestActivityLimitAnswersIgnoredActivity(t *testing.T) {
	for _, tc := range []struct{ route, fixture string }{
		{"/sonarr", "sonarr/grab.json"},
		{"/sonarr", "sonarr/download.json"},
		{"/radarr", "radarr/grab.json"},
		{"/radarr", "radarr/download.json"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			payload, err := os.ReadFile(filepath.Join("../../testdata", tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			mux, calls, mu := atActivityLimit(t)
			w := sendRaw(t, mux, tc.route, string(payload))
			if w.Code != http.StatusOK {
				t.Fatalf("got %d, want 200 (%s)", w.Code, w.Body.String())
			}
			var body struct{ Status, Detail string }
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Status != string(humautil.StatusIgnoredActivity) || body.Detail != "activity limit reached" {
				t.Errorf("body = %+v, want ignored_activity / activity limit reached", body)
			}

			got := testutil.GetCalls(calls, mu)
			if n := testutil.CountPath(got, "/notifications"); n != 1 {
				t.Errorf("%d notifications, want 1", n)
			}
			if n := testutil.CountPath(got, "/activities"); n != 1 {
				t.Errorf("%d creates, want 1", n)
			}
			for _, c := range got {
				if c.Method == http.MethodPatch {
					t.Errorf("unexpected %s %s after a refused create", c.Method, c.Path)
				}
			}
		})
	}
}

// With notifications turned off for the request the card was the only
// delivery, so the limit is the request's failure: a 409, not a 200 and not
// the 502 it used to be.
func TestActivityLimitWithoutNotificationIsConflict(t *testing.T) {
	payload, err := os.ReadFile("../../testdata/sonarr/grab.json")
	if err != nil {
		t.Fatal(err)
	}
	mux, calls, mu := atActivityLimit(t)
	if w := sendRaw(t, mux, "/sonarr?channels=activity", string(payload)); w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409 (%s)", w.Code, w.Body.String())
	}
	if n := testutil.CountPath(testutil.GetCalls(calls, mu), "/notifications"); n != 0 {
		t.Errorf("%d notifications under channels=activity", n)
	}
}

// The self-test opens only an activity, so at the limit it reached nobody and
// must not answer 200; the 409 passes through rather than becoming a 502.
func TestSelfTestAtActivityLimitIsConflict(t *testing.T) {
	for _, route := range []string{"/radarr", "/sonarr", "/prowlarr"} {
		t.Run(route, func(t *testing.T) {
			mux, _, _ := atActivityLimit(t)
			if w := sendRaw(t, mux, route, `{"eventType": "Test"}`); w.Code != http.StatusConflict {
				t.Fatalf("got %d, want 409 (%s)", w.Code, w.Body.String())
			}
		})
	}
}
