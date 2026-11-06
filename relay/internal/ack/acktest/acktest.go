// Package acktest runs a relay provider's alert through the ?ack=1 and
// ?retract=1 cases against the mock PushWard server, shared across the
// provider handler tests.
package acktest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// Provider is one provider's alert: how to build its handler, how to fire and
// resolve the alert, and the tag its receipt must carry.
type Provider struct {
	// Key is the hlk_ key Fire and Resolve authenticate with.
	Key string
	// Tag is the receipt tag the firing notification must carry.
	Tag string
	// CollapseID is the firing notification's collapse id, the one a
	// ?retract=1 resolve takes back. RunRetract needs it.
	CollapseID string
	// Handler builds the provider's routes, with a fresh state store,
	// against a PushWard server at url.
	Handler func(t *testing.T, url string) http.Handler
	// Fire and Resolve post the alert's firing and resolving webhooks with
	// query appended (see Target). The cancel and the resolved notification
	// are made before Resolve is answered; a two-phase end, which sends no
	// notification, is not waited for.
	Fire, Resolve func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder
}

// Target is path with query appended, when there is one.
func Target(path, query string) string {
	if query == "" {
		return path
	}
	return path + "?" + query
}

// Run checks p against every ?ack=1 case that does not depend on the
// provider: off by default, the tag and durations on the firing alert, a
// cancel that precedes the resolved notification and still works after a
// restart, the fallback to a plain send, a failing cancel, and
// channels=activity.
func Run(t *testing.T, p Provider) {
	t.Helper()

	t.Run("off without the query", func(t *testing.T) {
		srv, calls, mu := testutil.MockPushWardServer(t)
		h := p.Handler(t, srv.URL)
		ok(t, p.Fire(t, h, ""))
		ok(t, p.Resolve(t, h, ""))
		recorded := testutil.GetCalls(calls, mu)
		ns := Notifications(t, recorded)
		if len(ns) == 0 {
			t.Fatal("no notifications")
		}
		for _, n := range ns {
			AssertPlain(t, n)
		}
		if c := testutil.CallsTo(recorded, http.MethodPost, cancelPath); len(c) != 0 {
			t.Errorf("%d receipt cancels without ?ack=1", len(c))
		}
	})

	t.Run("fire then resolve", func(t *testing.T) {
		srv, calls, mu := testutil.MockPushWardServer(t)
		h := p.Handler(t, srv.URL)
		ok(t, p.Fire(t, h, "ack=1"))
		ns := Notifications(t, testutil.GetCalls(calls, mu))
		if len(ns) != 1 {
			t.Fatalf("%d notifications on fire, want 1", len(ns))
		}
		AssertAcked(t, ns[0], p.Tag, pushward.NotificationAcknowledge{RepeatSeconds: 300, ExpireSeconds: 3600})

		ok(t, p.Resolve(t, h, "ack=1"))
		recorded := testutil.GetCalls(calls, mu)
		ns = Notifications(t, recorded)
		if len(ns) != 2 {
			t.Fatalf("%d notifications after resolve, want 2", len(ns))
		}
		AssertPlain(t, ns[1])
		AssertCanceledBeforeResolved(t, recorded, p.Tag)
		assertCanceled(t, srv.URL, p.Key)
	})

	t.Run("repeat and expire pass through", func(t *testing.T) {
		srv, calls, mu := testutil.MockPushWardServer(t)
		ok(t, p.Fire(t, p.Handler(t, srv.URL), "ack=1&ack_repeat=30&ack_expire=600"))
		ns := Notifications(t, testutil.GetCalls(calls, mu))
		if len(ns) != 1 {
			t.Fatalf("%d notifications, want 1", len(ns))
		}
		AssertAcked(t, ns[0], p.Tag, pushward.NotificationAcknowledge{RepeatSeconds: 30, ExpireSeconds: 600})
	})

	t.Run("refused acknowledge falls back", func(t *testing.T) {
		srv, calls, mu := testutil.MockPushWardServerRejectingAck(t, http.StatusConflict, pushward.ErrCodeNotificationReceiptLimit)
		ok(t, p.Fire(t, p.Handler(t, srv.URL), "ack=1"))
		ns := Notifications(t, testutil.GetCalls(calls, mu))
		if len(ns) != 2 {
			t.Fatalf("%d notifications, want the refused one and its plain resend", len(ns))
		}
		AssertAcked(t, ns[0], p.Tag, pushward.NotificationAcknowledge{RepeatSeconds: 300, ExpireSeconds: 3600})
		AssertPlain(t, ns[1])
	})

	// The tag is rebuilt from the resolve webhook alone, so a relay that lost
	// its state (a restart, another replica) still stops the repeats.
	t.Run("resolve after a restart cancels", func(t *testing.T) {
		srv, calls, mu := testutil.MockPushWardServer(t)
		ok(t, p.Fire(t, p.Handler(t, srv.URL), "ack=1"))
		ok(t, p.Resolve(t, p.Handler(t, srv.URL), "ack=1"))
		recorded := testutil.GetCalls(calls, mu)
		if c := cancelsFor(t, recorded, p.Tag); len(c) != 1 {
			t.Fatalf("%d cancels for %s, want 1", len(c), p.Tag)
		}
		assertCanceled(t, srv.URL, p.Key)
	})

	t.Run("failing cancel still resolves", func(t *testing.T) {
		srv, calls, mu := testutil.MockPushWardServerWith(t, testutil.MockOptions{CancelStatus: http.StatusBadRequest})
		h := p.Handler(t, srv.URL)
		ok(t, p.Fire(t, h, "ack=1"))
		ok(t, p.Resolve(t, h, "ack=1"))
		recorded := testutil.GetCalls(calls, mu)
		if ns := Notifications(t, recorded); len(ns) != 2 {
			t.Fatalf("%d notifications, want the alert and its resolve", len(ns))
		}
		AssertCanceledBeforeResolved(t, recorded, p.Tag)
	})

	t.Run("channels=activity sends no acknowledge", func(t *testing.T) {
		srv, calls, mu := testutil.MockPushWardServer(t)
		h := p.Handler(t, srv.URL)
		ok(t, p.Fire(t, h, "channels=activity&ack=1"))
		ok(t, p.Resolve(t, h, "channels=activity&ack=1"))
		recorded := testutil.GetCalls(calls, mu)
		if ns := Notifications(t, recorded); len(ns) != 0 {
			t.Errorf("%d notifications under channels=activity", len(ns))
		}
		if c := testutil.CallsTo(recorded, http.MethodPost, cancelPath); len(c) != 0 {
			t.Errorf("%d receipt cancels under channels=activity", len(c))
		}
	})
}

// RunRetract checks p against the ?retract=1 cases: the resolve takes the
// alert back instead of sending Resolved over it, the Resolved still goes out
// under the alert's collapse id when the retract reaches no device or fails,
// and nothing is retracted without the query or with notifications off.
func RunRetract(t *testing.T, p Provider) {
	t.Helper()
	zero := 0
	for _, tc := range []struct {
		name, query   string
		opts          testutil.MockOptions
		retracts      int
		notifications int
	}{
		{"retracts instead of resolving", "retract=1", testutil.MockOptions{}, 1, 1},
		{"no device reached sends the resolved", "retract=1", testutil.MockOptions{RetractDevices: &zero}, 1, 2},
		// A 4xx: the client retries a 5xx with backoff until the retract
		// times out, which ends the same way, only slower.
		{"failing retract sends the resolved", "retract=1", testutil.MockOptions{RetractStatus: http.StatusForbidden}, 1, 2},
		{"off without the query", "", testutil.MockOptions{}, 0, 2},
		{"channels=activity retracts nothing", "channels=activity&retract=1", testutil.MockOptions{}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls, mu := testutil.MockPushWardServerWith(t, tc.opts)
			h := p.Handler(t, srv.URL)
			ok(t, p.Fire(t, h, tc.query))
			ok(t, p.Resolve(t, h, tc.query))
			recorded := testutil.GetCalls(calls, mu)
			rs := testutil.CallsTo(recorded, http.MethodPost, retractPath)
			if len(rs) != tc.retracts {
				t.Fatalf("%d retracts, want %d", len(rs), tc.retracts)
			}
			for _, c := range rs {
				var body struct {
					CollapseID string `json:"collapse_id"`
				}
				testutil.UnmarshalBody(t, c.Body, &body)
				if body.CollapseID != p.CollapseID {
					t.Errorf("retracted %q, want %q", body.CollapseID, p.CollapseID)
				}
			}
			ns := Notifications(t, recorded)
			if len(ns) != tc.notifications {
				t.Fatalf("%d notifications, want %d", len(ns), tc.notifications)
			}
			for _, n := range ns {
				if n.CollapseID != p.CollapseID {
					t.Errorf("notification %q under %q, want %q", n.Body, n.CollapseID, p.CollapseID)
				}
			}
		})
	}
}

const (
	cancelPath  = "/notifications/receipts/cancel"
	retractPath = "/notifications/retract"
)

// Notifications decodes every POST /notifications in calls, in order.
func Notifications(t *testing.T, calls []testutil.APICall) []pushward.SendNotificationRequest {
	t.Helper()
	var out []pushward.SendNotificationRequest
	for _, c := range testutil.CallsTo(calls, http.MethodPost, "/notifications") {
		var n pushward.SendNotificationRequest
		testutil.UnmarshalBody(t, c.Body, &n)
		out = append(out, n)
	}
	return out
}

// AssertAcked fails unless n repeats as want says, tagged tag and nothing
// else, and keeps a collapse id.
func AssertAcked(t *testing.T, n pushward.SendNotificationRequest, tag string, want pushward.NotificationAcknowledge) {
	t.Helper()
	if n.Acknowledge == nil || *n.Acknowledge != want {
		t.Errorf("acknowledge = %+v, want %+v", n.Acknowledge, want)
	}
	if len(n.Tags) != 1 || n.Tags[0] != tag {
		t.Errorf("tags = %q, want [%s]", n.Tags, tag)
	}
	if n.CollapseID == "" {
		t.Error("an acknowledged alert went out without a collapse id")
	}
}

// AssertPlain fails if n asks to repeat or carries tags.
func AssertPlain(t *testing.T, n pushward.SendNotificationRequest) {
	t.Helper()
	if n.Acknowledge != nil || len(n.Tags) != 0 {
		t.Errorf("notification %q asks for acknowledge %+v with tags %q, want neither", n.Body, n.Acknowledge, n.Tags)
	}
}

// AssertCanceledBeforeResolved fails unless calls hold one cancel by tag,
// made after the first notification (the alert) and before the last (its
// resolve). The order matters: the repeats share the alert's collapse id, so
// one landing after the resolve would replace it.
func AssertCanceledBeforeResolved(t *testing.T, calls []testutil.APICall, tag string) {
	t.Helper()
	cancel, first, last := -1, -1, -1
	for i, c := range calls {
		switch {
		case c.Method == http.MethodPost && c.Path == "/notifications":
			if first < 0 {
				first = i
			}
			last = i
		case c.Method == http.MethodPost && c.Path == cancelPath:
			if cancel >= 0 {
				t.Errorf("a second receipt cancel at call %d", i)
			}
			cancel = i
			if got := cancelTag(t, c); got != tag {
				t.Errorf("cancel tag = %q, want %q", got, tag)
			}
		}
	}
	if cancel < 0 || cancel < first || cancel > last {
		t.Errorf("cancel at call %d, want it between the alert (%d) and the resolved notification (%d)", cancel, first, last)
	}
}

func cancelsFor(t *testing.T, calls []testutil.APICall, tag string) []testutil.APICall {
	t.Helper()
	var out []testutil.APICall
	for _, c := range testutil.CallsTo(calls, http.MethodPost, cancelPath) {
		if cancelTag(t, c) == tag {
			out = append(out, c)
		}
	}
	return out
}

func cancelTag(t *testing.T, c testutil.APICall) string {
	t.Helper()
	var body struct {
		Tag string `json:"tag"`
	}
	testutil.UnmarshalBody(t, c.Body, &body)
	return body.Tag
}

// assertCanceled reads the first notification's receipt from the mock server
// and fails unless a cancel by tag stopped it.
func assertCanceled(t *testing.T, baseURL, key string) {
	t.Helper()
	r := Receipt(t, baseURL, key, 1)
	if r.Status != pushward.ReceiptStatusCanceled || r.CancelReason != pushward.ReceiptCancelTag {
		t.Errorf("receipt %s/%s, want canceled by tag", r.Status, r.CancelReason)
	}
}

// Receipt reads the receipt of notification id from the mock server at
// baseURL, as the relay holding key sees it.
func Receipt(t *testing.T, baseURL, key string, id int64) pushward.NotificationReceipt {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/notifications/receipts/%d", baseURL, id), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var r pushward.NotificationReceipt
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("receipt %d: %d %v", id, resp.StatusCode, err)
	}
	return r
}

func ok(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("webhook answered %d: %s", w.Code, w.Body.String())
	}
}
