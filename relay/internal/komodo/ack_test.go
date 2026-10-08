package komodo

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

func handlerAt(t *testing.T, url string) http.Handler {
	t.Helper()
	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, state.NewMemoryStore(), client.NewPool(url, nil), testConfig())
	return mux
}

func postWith(t *testing.T, h http.Handler, query, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, acktest.Target("/komodo", query), strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAck(t *testing.T) {
	acktest.Run(t, acktest.Provider{
		Key:     "hlk_test",
		Tag:     "relay." + text.SlugHash("komodo", "Server/srv-1/ServerUnreachable", 6),
		Handler: handlerAt,
		Fire: func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
			return postWith(t, h, query, bodyUnreachable)
		},
		Resolve: func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
			return postWith(t, h, query, bodyUnreachableResolved)
		},
	})
}

// Nothing resolves a one-shot, so it repeats until acknowledged or expired,
// tagged by its condition. An OK one is passive, does not repeat and keeps
// its per-event collapse id.
func TestAckOneShot(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	h := handlerAt(t, srv.URL)
	for _, body := range []string{bodyBuildFailed, bodyCustom} {
		if w := postWith(t, h, "ack=1&ack_expire=900", body); w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
		}
	}
	ns := acktest.Notifications(t, testutil.GetCalls(calls, mu))
	if len(ns) != 2 {
		t.Fatalf("%d notifications, want 2", len(ns))
	}
	collapse := text.SlugHash("komodo", "Build/bld-1/BuildFailed", 6)
	if ns[0].CollapseID != collapse {
		t.Errorf("collapse id = %q, want %q", ns[0].CollapseID, collapse)
	}
	acktest.AssertAcked(t, ns[0], "relay."+collapse, pushward.NotificationAcknowledge{RepeatSeconds: 300, ExpireSeconds: 900})
	if ns[1].Level != pushward.LevelPassive {
		t.Fatalf("custom OK level = %q, want passive", ns[1].Level)
	}
	acktest.AssertPlain(t, ns[1])
	if want := text.SlugHash("komodo", "System/system/Custom/1730000000000", 6); ns[1].CollapseID != want {
		t.Errorf("passive one-shot collapse id = %q, want the per-event %q", ns[1].CollapseID, want)
	}
}

// A crash loop sends the same condition again and again. Each acknowledged
// event supersedes the one before, so they hold one receipt between them, not
// one each against the account's 25.
func TestAckOneShotSupersedesPerCondition(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	h := handlerAt(t, srv.URL)
	later := strings.Replace(bodyBuildFailed, "1730000000000", "1730000060000", 1)
	for _, body := range []string{bodyBuildFailed, later} {
		if w := postWith(t, h, "ack=1", body); w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
		}
	}
	ns := acktest.Notifications(t, testutil.GetCalls(calls, mu))
	if len(ns) != 2 || ns[0].CollapseID != ns[1].CollapseID {
		t.Fatalf("notifications = %+v, want two with one collapse id", ns)
	}
	if r := acktest.Receipt(t, srv.URL, "hlk_test", 1); r.Status != pushward.ReceiptStatusCanceled || r.CancelReason != pushward.ReceiptCancelSuperseded {
		t.Errorf("first receipt %s/%s, want superseded", r.Status, r.CancelReason)
	}
	if r := acktest.Receipt(t, srv.URL, "hlk_test", 2); r.Status != pushward.ReceiptStatusActive {
		t.Errorf("second receipt %s, want active", r.Status)
	}
}

// Without ?ack=1 every event keeps its own collapse id, as before.
func TestOneShotCollapsePerEventWithoutAck(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	h := handlerAt(t, srv.URL)
	later := strings.Replace(bodyBuildFailed, "1730000000000", "1730000060000", 1)
	for _, body := range []string{bodyBuildFailed, later} {
		if w := postWith(t, h, "", body); w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
		}
	}
	ns := acktest.Notifications(t, testutil.GetCalls(calls, mu))
	if len(ns) != 2 {
		t.Fatalf("%d notifications, want 2", len(ns))
	}
	for i, ts := range []string{"1730000000000", "1730000060000"} {
		if want := text.SlugHash("komodo", "Build/bld-1/BuildFailed/"+ts, 6); ns[i].CollapseID != want {
			t.Errorf("event %d collapse id = %q, want %q", i, ns[i].CollapseID, want)
		}
	}
}

// Every Custom alert targets System/"system", so the source in its message
// keeps two senders' alerts from superseding each other.
func TestAckCustomOneShotKeyedBySource(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	h := handlerAt(t, srv.URL)
	custom := func(msg string) string {
		return `{"ts": 1730000000000, "resolved": false, "level": "WARNING",
			"target": {"type": "System", "id": "system"},
			"data": {"type": "Custom", "data": {"message": "` + msg + `"}}}`
	}
	for _, msg := range []string{"Backup: snapshot failed", "Deploy: rollout failed", "Backup: snapshot failed again"} {
		if w := postWith(t, h, "ack=1", custom(msg)); w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
		}
	}
	ns := acktest.Notifications(t, testutil.GetCalls(calls, mu))
	if len(ns) != 3 {
		t.Fatalf("%d notifications, want 3", len(ns))
	}
	if ns[0].CollapseID == ns[1].CollapseID || ns[0].CollapseID != ns[2].CollapseID {
		t.Errorf("collapse ids %q %q %q, want one per source", ns[0].CollapseID, ns[1].CollapseID, ns[2].CollapseID)
	}
}
