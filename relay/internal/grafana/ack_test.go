package grafana

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

const (
	firingDiskFull = `{"alerts": [{
		"status": "firing",
		"labels": {"alertname": "DiskFull", "instance": "nas-01"},
		"annotations": {"summary": "Disk 93% full"},
		"fingerprint": "f1"
	}]}`
	resolvedDiskFull = `{"alerts": [{
		"status": "resolved",
		"labels": {"alertname": "DiskFull", "instance": "nas-01"},
		"annotations": {"summary": "Disk 93% full"},
		"fingerprint": "f1"
	}]}`
)

func handlerAt(t *testing.T, url string) http.Handler {
	t.Helper()
	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, state.NewMemoryStore(), client.NewPool(url, nil), testConfig())
	return mux
}

func postWith(t *testing.T, h http.Handler, query, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, acktest.Target("/grafana", query), strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAck(t *testing.T) {
	acktest.Run(t, acktest.Provider{
		Key:     testKey,
		Tag:     "relay." + text.SlugHash("grafana", "DiskFull", 6),
		Handler: handlerAt,
		Fire: func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
			return postWith(t, h, query, firingDiskFull)
		},
		Resolve: func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
			return postWith(t, h, query, resolvedDiskFull)
		},
	})
}

// A refusal of the caller's own key is not a refusal of the acknowledge: it
// surfaces unchanged, and the alert is not sent a second time.
func TestAckUpstreamRefusalSurfacesUnchanged(t *testing.T) {
	testutil.AssertUpstreamRefusalSurfaces(t, func(t *testing.T, status int) *httptest.ResponseRecorder {
		srv, calls, mu := testutil.MockPushWardServerRejecting(t, status, status)
		w := postWith(t, handlerAt(t, srv.URL), "ack=1", firingDiskFull)
		if n := len(acktest.Notifications(t, testutil.GetCalls(calls, mu))); n != 1 {
			t.Errorf("%d notification sends, want 1", n)
		}
		return w
	})
}

func diskFullGroup(alerts ...string) string {
	return `{"alerts": [` + strings.Join(alerts, ",") + `]}`
}

func diskFullAlert(status, fingerprint string) string {
	return `{"status": "` + status + `", "labels": {"alertname": "DiskFull", "instance": "` + fingerprint +
		`"}, "annotations": {"summary": "Disk 93% full"}, "fingerprint": "` + fingerprint + `"}`
}

// A group that changes while it still fires repeats only for an alert that
// was not firing before. One of its alerts resolving must neither stop the
// repeats of the rest nor start them again for alerts someone already
// acknowledged.
func TestAckGroupStartsRepeatsOnlyForNewlyFiring(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	h := handlerAt(t, srv.URL)
	tag := "relay." + text.SlugHash("grafana", "DiskFull", 6)

	steps := []struct {
		name    string
		payload string
		acked   bool
	}{
		{"a and b fire", diskFullGroup(diskFullAlert("firing", "a"), diskFullAlert("firing", "b")), true},
		{"b resolves", diskFullGroup(diskFullAlert("firing", "a"), diskFullAlert("resolved", "b")), false},
		{"c joins", diskFullGroup(diskFullAlert("firing", "a"), diskFullAlert("resolved", "b"), diskFullAlert("firing", "c")), true},
		{"b fires again", diskFullGroup(diskFullAlert("firing", "a"), diskFullAlert("firing", "b"), diskFullAlert("firing", "c")), true},
		{"c resolves", diskFullGroup(diskFullAlert("firing", "a"), diskFullAlert("firing", "b"), diskFullAlert("resolved", "c")), false},
	}
	for i, step := range steps {
		if w := postWith(t, h, "ack=1", step.payload); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", step.name, w.Code, w.Body.String())
		}
		recorded := testutil.GetCalls(calls, mu)
		ns := acktest.Notifications(t, recorded)
		if len(ns) != i+1 {
			t.Fatalf("%s: %d notifications, want %d", step.name, len(ns), i+1)
		}
		n := ns[i]
		if n.CollapseID != text.SlugHash("grafana", "DiskFull", 6) {
			t.Errorf("%s: collapse id %q", step.name, n.CollapseID)
		}
		if step.acked {
			acktest.AssertAcked(t, n, tag, pushward.NotificationAcknowledge{RepeatSeconds: 300, ExpireSeconds: 3600})
		} else {
			acktest.AssertPlain(t, n)
		}
		if c := testutil.CallsTo(recorded, http.MethodPost, "/notifications/receipts/cancel"); len(c) != 0 {
			t.Fatalf("%s: %d receipt cancels while the group still fires", step.name, len(c))
		}
	}
}

// Without the stored state there is no telling what fired before, so the
// group counts as new: a lost state costs a repeat, never an alert.
func TestAckGroupWithoutStateIsNew(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	payload := diskFullGroup(diskFullAlert("firing", "a"), diskFullAlert("resolved", "b"))
	if w := postWith(t, handlerAt(t, srv.URL), "ack=1", payload); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	ns := acktest.Notifications(t, testutil.GetCalls(calls, mu))
	if len(ns) != 1 {
		t.Fatalf("%d notifications, want 1", len(ns))
	}
	acktest.AssertAcked(t, ns[0], "relay."+text.SlugHash("grafana", "DiskFull", 6),
		pushward.NotificationAcknowledge{RepeatSeconds: 300, ExpireSeconds: 3600})
}
