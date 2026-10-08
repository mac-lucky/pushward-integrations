package universalhook

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/ack/acktest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overrides"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// The Alertmanager preset keys its card on groupKey, the same in the firing
// and the resolved fixture, and the tag is that card's slug. It is pinned: a
// relay that changed it would no longer cancel what the one before it sent.
func TestAck(t *testing.T) {
	acktest.Run(t, acktest.Provider{
		Key: testKey,
		Tag: "relay.u-6cf632a91dc7",
		Handler: func(t *testing.T, url string) http.Handler {
			hs := newHarnessAt(t, url, nil, nil, nil)
			hs.h.config.Presets = true
			return hs.mux
		},
		Fire: func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
			return postTo(t, h, acktest.Target("/universal", query), fixture(t, "alertmanager_firing.json"))
		},
		Resolve: func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
			return postTo(t, h, acktest.Target("/universal", query), fixture(t, "alertmanager_resolved.json"))
		},
	})
}

func postTo(t *testing.T, h http.Handler, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// Only an alert repeats. A plain notification and a progress run, which
// nothing acknowledges, go out as they always did.
func TestAckOnlyForAlerts(t *testing.T) {
	hs := newHarness(t)
	if w := hs.post(t, "/universal?ack=1", fixture(t, "plain_notify.json")); w.Code != http.StatusOK {
		t.Fatalf("plain: %d %s", w.Code, w.Body.String())
	}
	acktest.AssertPlain(t, only(t, hs))

	// channels=notification makes a progress start notify.
	hs = newHarness(t)
	for _, name := range []string{"ci_progress_running.json", "ci_progress_done.json"} {
		if err := hs.deliverWith(t, "ci", "channels=notification&ack=1", fixture(t, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	calls := hs.snapshot()
	ns := acktest.Notifications(t, calls)
	if len(ns) != 2 {
		t.Fatalf("%d notifications, want 2", len(ns))
	}
	for _, n := range ns {
		acktest.AssertPlain(t, n)
	}
	if c := testutil.CallsTo(calls, http.MethodPost, "/notifications/receipts/cancel"); len(c) != 0 {
		t.Errorf("%d receipt cancels for a progress run", len(c))
	}
}

// With channels=notification the alert has no card, and still repeats until
// its resolve.
func TestAckWithoutACard(t *testing.T) {
	hs := newHarness(t)
	for _, name := range []string{"alertmanager_firing.json", "alertmanager_resolved.json"} {
		if err := hs.deliverWith(t, "", "channels=notification&ack=1", fixture(t, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	calls := hs.snapshot()
	ns := acktest.Notifications(t, calls)
	if len(ns) != 2 {
		t.Fatalf("%d notifications, want 2", len(ns))
	}
	tag := "relay." + ns[0].CollapseID
	if ns[0].CollapseID == "" || len(ns[0].Tags) != 1 || ns[0].Tags[0] != tag {
		t.Fatalf("alert collapse id %q tags %q, want tagged by its card slug", ns[0].CollapseID, ns[0].Tags)
	}
	acktest.AssertCanceledBeforeResolved(t, calls, tag)
}

// deliverWith is deliverAs with any query's overrides.
func (hs *harness) deliverWith(t *testing.T, source, query string, body []byte) error {
	t.Helper()
	fields, truncated, err := universal.Flatten(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	shapes := universal.ShapesOf(fields)
	m := universal.NewMapping(universal.ProposeShapes(shapes), shapes)
	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	ov, err := overrides.Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), overrides.ContextKey(), ov)
	r := &request{key: testKey, source: source, fields: fields, shapes: shapes, truncated: truncated, log: slog.Default(), sendLog: slog.Default()}
	_, err = hs.h.deliver(ctx, r, m, viaPreset)
	return err
}
