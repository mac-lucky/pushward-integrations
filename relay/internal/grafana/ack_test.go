package grafana

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
	return handlerWith(t, url, state.NewMemoryStore())
}

func handlerWith(t *testing.T, url string, store state.Store) http.Handler {
	t.Helper()
	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, store, client.NewPool(url, nil), testConfig())
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
	p := acktest.Provider{
		Key:        testKey,
		Tag:        "relay." + text.SlugHash("grafana", "DiskFull", 6),
		CollapseID: text.SlugHash("grafana", "DiskFull", 6),
		Handler:    handlerAt,
		Fire: func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
			return postWith(t, h, query, firingDiskFull)
		},
		Resolve: func(t *testing.T, h http.Handler, query string) *httptest.ResponseRecorder {
			return postWith(t, h, query, resolvedDiskFull)
		},
	}
	acktest.Run(t, p)
	t.Run("retract", func(t *testing.T) { acktest.RunRetract(t, p) })
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

// instanceGroup is the webhook of a policy grouped by instance: one group per
// alert, each with its own groupKey.
func instanceGroup(status, instance string) string {
	return `{"groupKey": "{}:{instance=\"` + instance + `\"}", "alerts": [` + diskFullAlert(status, instance) + `]}`
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

// A policy that groups by instance splits one alert name into groups whose
// pushes share the collapse id. A group resolving while another still fires
// sends its Resolved; the retract waits for the last group, whichever order
// they resolve in.
func TestRetractWaitsForEveryGroupOfTheName(t *testing.T) {
	for _, order := range [][2]string{{"a", "b"}, {"b", "a"}} {
		t.Run(order[0]+" resolves first", func(t *testing.T) {
			srv, calls, mu := testutil.MockPushWardServer(t)
			h := handlerAt(t, srv.URL)
			post := func(status, instance string) {
				t.Helper()
				if w := postWith(t, h, "retract=1", instanceGroup(status, instance)); w.Code != http.StatusOK {
					t.Fatalf("%d %s", w.Code, w.Body.String())
				}
			}
			post("firing", "a")
			post("firing", "b")
			post("resolved", order[0])

			recorded := testutil.GetCalls(calls, mu)
			if rs := testutil.CallsTo(recorded, http.MethodPost, "/notifications/retract"); len(rs) != 0 {
				t.Fatalf("%d retracts while %s still fires", len(rs), order[1])
			}
			ns := acktest.Notifications(t, recorded)
			if len(ns) != 3 || ns[2].Level != pushward.LevelPassive {
				t.Fatalf("notifications = %+v, want the Resolved of %s", ns, order[0])
			}

			post("resolved", order[1])
			recorded = testutil.GetCalls(calls, mu)
			rs := testutil.CallsTo(recorded, http.MethodPost, "/notifications/retract")
			if len(rs) != 1 || !strings.Contains(string(rs[0].Body), text.SlugHash("grafana", "DiskFull", 6)) {
				t.Errorf("%d retracts, want one of the alert name's id once nothing fires", len(rs))
			}
			if n := len(acktest.Notifications(t, recorded)); n != 3 {
				t.Errorf("%d notifications, want no Resolved after the retract", n)
			}
		})
	}
}

// One resolve webhook can clear several alert names. Once a retract does not
// reach, the rest resolve without one, so a rate-limited account does not
// make each of them wait out its own timeout. A 403 stands in for the 429,
// which the client retries until that timeout.
func TestRetractStopsAfterOneMisses(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServerWith(t, testutil.MockOptions{RetractStatus: http.StatusForbidden})
	h := handlerAt(t, srv.URL)
	alert := func(name, status string) string {
		return `{"status": "` + status + `", "labels": {"alertname": "` + name + `"}, "annotations": {"summary": "` + name + `"}, "fingerprint": "` + name + `"}`
	}
	names := []string{"DiskFull", "HighCPU", "HighMem"}
	var firing, resolved []string
	for _, n := range names {
		firing = append(firing, alert(n, "firing"))
		resolved = append(resolved, alert(n, "resolved"))
	}
	for _, payload := range []string{diskFullGroup(firing...), diskFullGroup(resolved...)} {
		if w := postWith(t, h, "retract=1", payload); w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	recorded := testutil.GetCalls(calls, mu)
	if rs := testutil.CallsTo(recorded, http.MethodPost, "/notifications/retract"); len(rs) != 1 {
		t.Errorf("%d retracts, want 1", len(rs))
	}
	if ns := acktest.Notifications(t, recorded); len(ns) != 2*len(names) {
		t.Errorf("%d notifications, want each alert and its resolve", len(ns))
	}
}

// With ack=1 a group resolving cancels the repeats of the alert name, which
// another group's push may own. That group's next push must start them again,
// with retract=1 as without it.
func TestAckRestartsForAGroupStillFiringAfterAnotherResolves(t *testing.T) {
	for _, query := range []string{"ack=1", "ack=1&retract=1"} {
		t.Run(query, func(t *testing.T) {
			srv, calls, mu := testutil.MockPushWardServer(t)
			h := handlerAt(t, srv.URL)
			steps := []struct{ status, instance string }{{"firing", "a"}, {"firing", "b"}, {"resolved", "a"}, {"firing", "b"}}
			for _, s := range steps {
				if w := postWith(t, h, query, instanceGroup(s.status, s.instance)); w.Code != http.StatusOK {
					t.Fatalf("%d %s", w.Code, w.Body.String())
				}
			}
			var acked []bool
			for _, n := range acktest.Notifications(t, testutil.GetCalls(calls, mu)) {
				acked = append(acked, n.Acknowledge != nil)
			}
			if want := []bool{true, true, false, true}; !slices.Equal(acked, want) {
				t.Errorf("acknowledged = %v, want %v", acked, want)
			}
		})
	}
}

// readBarrier holds the store's reads until n of them wait, so the webhooks a
// test sends at once all read their state before any of them writes.
type readBarrier struct {
	state.Store
	mu      sync.Mutex
	n       int
	release chan struct{}
}

func (b *readBarrier) arm(n int) {
	b.mu.Lock()
	b.n, b.release = n, make(chan struct{})
	b.mu.Unlock()
}

func (b *readBarrier) wait() {
	b.mu.Lock()
	if b.n == 0 {
		b.mu.Unlock()
		return
	}
	if b.n--; b.n == 0 {
		close(b.release)
	}
	release := b.release
	b.mu.Unlock()
	select {
	case <-release:
	case <-time.After(5 * time.Second):
	}
}

func (b *readBarrier) Get(ctx context.Context, provider, userKey, key, subKey string) (json.RawMessage, error) {
	b.wait()
	return b.Store.Get(ctx, provider, userKey, key, subKey)
}

func (b *readBarrier) GetGroup(ctx context.Context, provider, userKey, key string) (map[string]json.RawMessage, error) {
	b.wait()
	return b.Store.GetGroup(ctx, provider, userKey, key)
}

// postAtOnce sends one webhook per payload concurrently, every one of them
// reading the store before any writes.
func postAtOnce(t *testing.T, h http.Handler, store *readBarrier, query string, payloads ...string) {
	t.Helper()
	store.arm(len(payloads))
	var wg sync.WaitGroup
	codes := make([]int, len(payloads))
	for i, p := range payloads {
		wg.Go(func() { codes[i] = postWith(t, h, query, p).Code })
	}
	wg.Wait()
	for _, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("status %d", c)
		}
	}
}

// Grafana posts the groups of one rule evaluation at the same moment, to any
// relay pod. Two groups firing at once must both stay recorded, so the first
// to resolve does not take the push back while the other still fires.
func TestRetractWithGroupsFiringAtOnce(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := &readBarrier{Store: state.NewMemoryStore()}
	h := handlerWith(t, srv.URL, store)
	postAtOnce(t, h, store, "retract=1", instanceGroup("firing", "a"), instanceGroup("firing", "b"))

	for _, step := range []struct {
		instance string
		retracts int
	}{{"a", 0}, {"b", 1}} {
		if w := postWith(t, h, "retract=1", instanceGroup("resolved", step.instance)); w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		rs := testutil.CallsTo(testutil.GetCalls(calls, mu), http.MethodPost, "/notifications/retract")
		if len(rs) != step.retracts {
			t.Fatalf("after %s resolved: %d retracts, want %d", step.instance, len(rs), step.retracts)
		}
	}
}

// Two groups resolving at once each still see the other firing, so both send
// their Resolved. Neither may leave the other's alert recorded as firing:
// that would keep the next firing of it from repeating with ack=1, and its
// resolve from retracting.
func TestRetractWithGroupsResolvingAtOnce(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := &readBarrier{Store: state.NewMemoryStore()}
	h := handlerWith(t, srv.URL, store)
	const query = "ack=1&retract=1"
	for _, inst := range []string{"a", "b"} {
		if w := postWith(t, h, query, instanceGroup("firing", inst)); w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	postAtOnce(t, h, store, query, instanceGroup("resolved", "a"), instanceGroup("resolved", "b"))
	recorded := testutil.GetCalls(calls, mu)
	if rs := testutil.CallsTo(recorded, http.MethodPost, "/notifications/retract"); len(rs) != 0 {
		t.Fatalf("%d retracts while each saw the other firing", len(rs))
	}
	if ns := acktest.Notifications(t, recorded); len(ns) != 4 {
		t.Fatalf("%d notifications, want both alerts and both Resolved", len(ns))
	}

	if w := postWith(t, h, query, instanceGroup("firing", "b")); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	ns := acktest.Notifications(t, testutil.GetCalls(calls, mu))
	if len(ns) != 5 || ns[4].Acknowledge == nil {
		t.Errorf("b firing again: %d notifications, last acknowledge %+v, want a repeating push", len(ns), ns[len(ns)-1].Acknowledge)
	}
	if w := postWith(t, h, query, instanceGroup("resolved", "b")); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if rs := testutil.CallsTo(testutil.GetCalls(calls, mu), http.MethodPost, "/notifications/retract"); len(rs) != 1 {
		t.Errorf("%d retracts once b resolved with nothing else firing, want 1", len(rs))
	}
}

// A row written without retract=1, or by a relay before groups had rows of
// their own, shows only the group that posted last. While one is there the
// alert name is resolved with a Resolved push, never a retract.
func TestRetractNotWhileAnOlderRowIsThere(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	old, _ := json.Marshal(alertGroupState{Firing: []string{"a"}})
	stateKey := text.SlugHash("grafana", "DiskFull", 6)
	if err := store.Set(context.Background(), "grafana", testKey, stateKey, "", old, time.Hour); err != nil {
		t.Fatal(err)
	}
	h := handlerWith(t, srv.URL, store)
	for _, status := range []string{"firing", "resolved"} {
		if w := postWith(t, h, "retract=1", instanceGroup(status, "a")); w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	recorded := testutil.GetCalls(calls, mu)
	if rs := testutil.CallsTo(recorded, http.MethodPost, "/notifications/retract"); len(rs) != 0 {
		t.Errorf("%d retracts with an older row of the name still there", len(rs))
	}
	if ns := acktest.Notifications(t, recorded); len(ns) != 2 || ns[1].Level != pushward.LevelPassive {
		t.Errorf("notifications = %+v, want the alert and its Resolved", ns)
	}
}
