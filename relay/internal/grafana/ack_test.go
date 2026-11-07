package grafana

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	mem := state.NewMemoryStore()
	store := &readBarrier{Store: mem}
	srv, _, _ := testutil.MockPushWardServer(t)
	postAtOnce(t, handlerWith(t, srv.URL, store), store, "retract=1", instanceGroup("firing", "a"), instanceGroup("firing", "b"))

	// Both resolve orders, each from a copy of what the two webhooks left:
	// whichever group a lost write would drop, one order resolves the other
	// group first.
	for _, order := range [][2]string{{"a", "b"}, {"b", "a"}} {
		t.Run(order[0]+" resolves first", func(t *testing.T) {
			twin := state.NewMemoryStore()
			for _, r := range mem.Rows() {
				if err := twin.Set(context.Background(), r.Provider, r.UserKey, r.Key, r.SubKey, r.Value, time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			srv, calls, mu := testutil.MockPushWardServer(t)
			h := handlerWith(t, srv.URL, twin)
			for i, instance := range order {
				if w := postWith(t, h, "retract=1", instanceGroup("resolved", instance)); w.Code != http.StatusOK {
					t.Fatalf("%d %s", w.Code, w.Body.String())
				}
				rs := testutil.CallsTo(testutil.GetCalls(calls, mu), http.MethodPost, "/notifications/retract")
				if len(rs) != i {
					t.Fatalf("after %s resolved: %d retracts, want %d", instance, len(rs), i)
				}
			}
		})
	}
}

// A group's record is written before its push goes out: another group that
// resolves while that push is still on its way must see it firing and send
// its Resolved, or its retract would go out ahead of the push.
func TestRetractSeesAGroupWhosePushIsStillOut(t *testing.T) {
	mock, calls, mu := testutil.MockPushWardServer(t)
	var hold atomic.Bool
	arrived, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/notifications" && hold.CompareAndSwap(true, false) {
			close(arrived)
			<-release
		}
		mock.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	h := handlerAt(t, srv.URL)

	if w := postWith(t, h, "retract=1", instanceGroup("firing", "b")); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	hold.Store(true)
	done := make(chan int)
	go func() { done <- postWith(t, h, "retract=1", instanceGroup("firing", "a")).Code }()
	<-arrived
	if w := postWith(t, h, "retract=1", instanceGroup("resolved", "b")); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	letGo()
	if code := <-done; code != http.StatusOK {
		t.Fatalf("a firing: status %d", code)
	}
	recorded := testutil.GetCalls(calls, mu)
	if rs := testutil.CallsTo(recorded, http.MethodPost, "/notifications/retract"); len(rs) != 0 {
		t.Errorf("%d retracts while a's push was still on its way", len(rs))
	}
	if ns := acktest.Notifications(t, recorded); len(ns) != 3 {
		t.Errorf("%d notifications, want b, its Resolved and a", len(ns))
	}
}

// agingStore stands in for the passing of StaleTimeout: age drops every row
// nothing wrote since the previous age.
type agingStore struct {
	*state.MemoryStore
	mu      sync.Mutex
	written map[string]bool
}

func (s *agingStore) Set(ctx context.Context, provider, userKey, key, subKey string, value json.RawMessage, ttl time.Duration) error {
	s.mu.Lock()
	s.written[key+"/"+subKey] = true
	s.mu.Unlock()
	return s.MemoryStore.Set(ctx, provider, userKey, key, subKey, value, ttl)
}

func (s *agingStore) age(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.Rows() {
		if !s.written[r.Key+"/"+r.SubKey] {
			if err := s.Delete(context.Background(), r.Provider, r.UserKey, r.Key, r.SubKey); err != nil {
				t.Fatal(err)
			}
		}
	}
	clear(s.written)
}

// A group that keeps firing past StaleTimeout stays recorded through its
// repeats, the deduped ones included, so another group's resolve sends its
// Resolved instead of taking the still-firing alert's push back.
func TestRetractKeepsAGroupRecordedThroughItsRepeats(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := &agingStore{MemoryStore: state.NewMemoryStore(), written: map[string]bool{}}
	h := handlerWith(t, srv.URL, store)
	post := func(status, instance string) {
		t.Helper()
		if w := postWith(t, h, "retract=1", instanceGroup(status, instance)); w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}

	post("firing", "a")
	store.age(t)
	post("firing", "a") // Grafana's repeat: deduped, nothing sent
	if ns := acktest.Notifications(t, testutil.GetCalls(calls, mu)); len(ns) != 1 {
		t.Fatalf("%d notifications after the repeat, want it deduped", len(ns))
	}
	store.age(t) // StaleTimeout after the first webhook, not after the repeat
	post("firing", "b")
	post("resolved", "b")
	recorded := testutil.GetCalls(calls, mu)
	if rs := testutil.CallsTo(recorded, http.MethodPost, "/notifications/retract"); len(rs) != 0 {
		t.Errorf("%d retracts while a still fires", len(rs))
	}
	if ns := acktest.Notifications(t, recorded); len(ns) != 3 || ns[2].Level != pushward.LevelPassive {
		t.Errorf("notifications = %+v, want a, b and b's Resolved", ns)
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
// Until the name's first push or retract writes its own last push, that row
// is also the last push: the alert it shows firing is not sent again on its
// next repeat, and with ack=1 its acknowledged repeats do not start over.
func TestRetractNotWhileAnOlderRowIsThere(t *testing.T) {
	for _, query := range []string{"retract=1", "ack=1&retract=1"} {
		t.Run(query, func(t *testing.T) {
			srv, calls, mu := testutil.MockPushWardServer(t)
			store := state.NewMemoryStore()
			old, _ := json.Marshal(alertGroupState{Firing: []string{"a"}})
			stateKey := text.SlugHash("grafana", "DiskFull", 6)
			if err := store.Set(context.Background(), "grafana", testKey, stateKey, "", old, time.Hour); err != nil {
				t.Fatal(err)
			}
			h := handlerWith(t, srv.URL, store)
			for _, status := range []string{"firing", "resolved"} {
				if w := postWith(t, h, query, instanceGroup(status, "a")); w.Code != http.StatusOK {
					t.Fatalf("%d %s", w.Code, w.Body.String())
				}
			}
			recorded := testutil.GetCalls(calls, mu)
			if rs := testutil.CallsTo(recorded, http.MethodPost, "/notifications/retract"); len(rs) != 0 {
				t.Errorf("%d retracts with an older row of the name still there", len(rs))
			}
			if ns := acktest.Notifications(t, recorded); len(ns) != 1 || ns[0].Level != pushward.LevelPassive {
				t.Errorf("notifications = %+v, want only the Resolved", ns)
			}
		})
	}
}

// A resolve is the name's last push too, so the alert firing again goes out,
// with ack=1 repeating, whether or not an older row of the name is still there.
func TestRetractedNameFiresAgain(t *testing.T) {
	stateKey := text.SlugHash("grafana", "DiskFull", 6)
	for _, query := range []string{"retract=1", "ack=1&retract=1"} {
		for _, older := range []bool{false, true} {
			name := query
			if older {
				name += "/older row"
			}
			t.Run(name, func(t *testing.T) {
				srv, calls, mu := testutil.MockPushWardServer(t)
				store := state.NewMemoryStore()
				if older {
					old, _ := json.Marshal(alertGroupState{Firing: []string{"a"}})
					if err := store.Set(context.Background(), "grafana", testKey, stateKey, "", old, time.Hour); err != nil {
						t.Fatal(err)
					}
				}
				h := handlerWith(t, srv.URL, store)
				for _, status := range []string{"firing", "resolved", "firing"} {
					if w := postWith(t, h, query, instanceGroup(status, "a")); w.Code != http.StatusOK {
						t.Fatalf("%d %s", w.Code, w.Body.String())
					}
				}
				ns := acktest.Notifications(t, testutil.GetCalls(calls, mu))
				if len(ns) != 2 || ns[1].Level != pushward.LevelActive {
					t.Fatalf("notifications = %+v, want the alert firing again last", ns)
				}
				if strings.Contains(query, "ack=1") {
					acktest.AssertAcked(t, ns[1], "relay."+stateKey, pushward.NotificationAcknowledge{RepeatSeconds: 300, ExpireSeconds: 3600})
				}
			})
		}
	}
}

// An older row left by group b: group a's Resolved replaces b's alert under
// the shared collapse id, so b's next repeat goes out again.
func TestRetractedNameRepeatsAnotherGroupAfterAResolve(t *testing.T) {
	for _, q := range [][2]string{{"", "retract=1"}, {"ack=1", "ack=1&retract=1"}} {
		t.Run(q[1], func(t *testing.T) {
			srv, calls, mu := testutil.MockPushWardServer(t)
			h := handlerAt(t, srv.URL)
			steps := []struct{ query, status, instance string }{
				{q[0], "firing", "a"}, {q[0], "firing", "b"}, {q[1], "resolved", "a"}, {q[1], "firing", "b"},
			}
			for _, s := range steps {
				if w := postWith(t, h, s.query, instanceGroup(s.status, s.instance)); w.Code != http.StatusOK {
					t.Fatalf("%d %s", w.Code, w.Body.String())
				}
			}
			ns := acktest.Notifications(t, testutil.GetCalls(calls, mu))
			if len(ns) != 4 || ns[2].Level != pushward.LevelPassive || ns[3].Level != pushward.LevelActive {
				t.Fatalf("notifications = %+v, want a's Resolved and then b again", ns)
			}
			if q[0] != "" && ns[3].Acknowledge == nil {
				t.Error("b again: no acknowledge, want its repeats started again")
			}
		})
	}
}

// A resolve webhook that comes again, as Grafana resends one after an error,
// repeats the name's last push and sends nothing: a Resolved after the
// retract would go down with it.
func TestRetractedResolveSentAgainSendsNothing(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	h := handlerAt(t, srv.URL)
	for _, status := range []string{"firing", "resolved", "resolved"} {
		if w := postWith(t, h, "retract=1", instanceGroup(status, "a")); w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	recorded := testutil.GetCalls(calls, mu)
	if rs := testutil.CallsTo(recorded, http.MethodPost, "/notifications/retract"); len(rs) != 1 {
		t.Errorf("%d retracts, want 1", len(rs))
	}
	if ns := acktest.Notifications(t, recorded); len(ns) != 1 {
		t.Errorf("notifications = %+v, want only the alert", ns)
	}
}

// An alert in two groups (two policies routing it to one contact point): once
// one group's resolve took the push back, the other's identical resolve is
// skipped as a repeat and still drops that group's row. Left there, it would
// show the alert firing, and the name's next resolve would send a Resolved.
func TestRetractSkippedResolveDropsItsGroupRow(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	h := handlerAt(t, srv.URL)
	steps := []struct{ group, status, fp string }{
		{"A", "firing", "a"},
		{"B", "firing", "a"},
		{"A", "resolved", "a"},
		{"B", "resolved", "a"},
		{"A", "firing", "c"},
		{"A", "resolved", "c"},
	}
	for _, s := range steps {
		payload := `{"groupKey": "` + s.group + `", "alerts": [` + diskFullAlert(s.status, s.fp) + `]}`
		if w := postWith(t, h, "retract=1", payload); w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	recorded := testutil.GetCalls(calls, mu)
	if rs := testutil.CallsTo(recorded, http.MethodPost, "/notifications/retract"); len(rs) != 2 {
		t.Errorf("%d retracts, want one for a and one for c", len(rs))
	}
	if ns := acktest.Notifications(t, recorded); len(ns) != 2 {
		t.Errorf("notifications = %+v, want a and c firing, no Resolved", ns)
	}
}

// A Resolved sent without retract=1 is the name's last push as well, so the
// alert firing again under retract=1 goes out and is not skipped as a repeat
// of the push before that Resolved.
func TestRetractedNameFiresAgainAfterAResolveWithoutIt(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	h := handlerAt(t, srv.URL)
	steps := []struct{ query, status string }{{"retract=1", "firing"}, {"", "resolved"}, {"retract=1", "firing"}}
	for _, s := range steps {
		if w := postWith(t, h, s.query, instanceGroup(s.status, "a")); w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	ns := acktest.Notifications(t, testutil.GetCalls(calls, mu))
	if len(ns) != 3 || ns[1].Level != pushward.LevelPassive || ns[2].Level != pushward.LevelActive {
		t.Fatalf("notifications = %+v, want the alert, its Resolved and the alert again", ns)
	}
}

// failingPeer is a second handler on store, as another relay pod would be,
// whose every notification send fails. A 4xx, since the client retries a 5xx
// with backoff.
func failingPeer(t *testing.T, store state.Store) http.Handler {
	t.Helper()
	srv, _, _ := testutil.MockPushWardServerWith(t, testutil.MockOptions{NotifyStatus: http.StatusForbidden})
	return handlerWith(t, srv.URL, store)
}

// A send that failed may still have reached the devices (an answer lost on
// the way), so it leaves the name's last push unknown. The resolve after it
// takes the push back instead of being skipped as a repeat of the resolve
// before the failed send.
func TestRetractAfterAFailedSend(t *testing.T) {
	store := state.NewMemoryStore()
	srv, calls, mu := testutil.MockPushWardServer(t)
	h, peer := handlerWith(t, srv.URL, store), failingPeer(t, store)
	steps := []struct {
		h      http.Handler
		status string
	}{{h, "firing"}, {h, "resolved"}, {peer, "firing"}, {h, "resolved"}}
	for _, s := range steps {
		postWith(t, s.h, "retract=1", instanceGroup(s.status, "a"))
	}
	if rs := testutil.CallsTo(testutil.GetCalls(calls, mu), http.MethodPost, "/notifications/retract"); len(rs) != 2 {
		t.Errorf("%d retracts, want the second resolve to take the push back too", len(rs))
	}
}

// Nor does an older "" row stand in for the last push after a failed send:
// Grafana's retry of the alert goes out, though that row shows it firing.
func TestRetractAfterAFailedSendWithAnOlderRow(t *testing.T) {
	store := state.NewMemoryStore()
	old, _ := json.Marshal(alertGroupState{Firing: []string{"a"}})
	if err := store.Set(context.Background(), "grafana", testKey, text.SlugHash("grafana", "DiskFull", 6), "", old, time.Hour); err != nil {
		t.Fatal(err)
	}
	srv, calls, mu := testutil.MockPushWardServer(t)
	h, peer := handlerWith(t, srv.URL, store), failingPeer(t, store)
	steps := []struct {
		h      http.Handler
		status string
	}{{h, "resolved"}, {peer, "firing"}, {h, "firing"}}
	for _, s := range steps {
		postWith(t, s.h, "retract=1", instanceGroup(s.status, "a"))
	}
	ns := acktest.Notifications(t, testutil.GetCalls(calls, mu))
	if len(ns) != 2 || ns[0].Level != pushward.LevelPassive || ns[1].Level != pushward.LevelActive {
		t.Fatalf("notifications = %+v, want the Resolved and then the retried alert", ns)
	}
}

// After a failed send the alert going out again starts its repeats only for
// alerts the push before it did not have firing: one someone acknowledged
// stays acknowledged.
func TestRetractAckAfterAFailedSend(t *testing.T) {
	store := state.NewMemoryStore()
	srv, calls, mu := testutil.MockPushWardServer(t)
	h, peer := handlerWith(t, srv.URL, store), failingPeer(t, store)
	withB := func(status string) string {
		return diskFullGroup(diskFullAlert("firing", "a"), diskFullAlert(status, "b"))
	}
	steps := []struct {
		h       http.Handler
		payload string
	}{{h, withB("firing")}, {peer, withB("resolved")}, {h, withB("resolved")}}
	for _, s := range steps {
		postWith(t, s.h, "ack=1&retract=1", s.payload)
	}
	ns := acktest.Notifications(t, testutil.GetCalls(calls, mu))
	if len(ns) != 2 {
		t.Fatalf("%d notifications, want a and b, then b resolved again", len(ns))
	}
	acktest.AssertPlain(t, ns[1])
}

// A resolve stops the alert's repeats before it sends, so after its send
// failed the alert firing again is a new incident and repeats again.
func TestRetractAckAfterAFailedResolve(t *testing.T) {
	store := state.NewMemoryStore()
	srv, calls, mu := testutil.MockPushWardServer(t)
	zero := 0
	peerSrv, _, _ := testutil.MockPushWardServerWith(t, testutil.MockOptions{NotifyStatus: http.StatusForbidden, RetractDevices: &zero})
	h, peer := handlerWith(t, srv.URL, store), handlerWith(t, peerSrv.URL, store)
	steps := []struct {
		h      http.Handler
		status string
	}{{h, "firing"}, {peer, "resolved"}, {h, "firing"}}
	for _, s := range steps {
		postWith(t, s.h, "ack=1&retract=1", instanceGroup(s.status, "a"))
	}
	ns := acktest.Notifications(t, testutil.GetCalls(calls, mu))
	if len(ns) != 2 {
		t.Fatalf("%d notifications, want the alert and the alert again", len(ns))
	}
	acktest.AssertAcked(t, ns[1], "relay."+text.SlugHash("grafana", "DiskFull", 6),
		pushward.NotificationAcknowledge{RepeatSeconds: 300, ExpireSeconds: 3600})
}

// ctxStore fails a write on a done context, as PostgresStore does.
type ctxStore struct{ *state.MemoryStore }

func (s ctxStore) Set(ctx context.Context, provider, userKey, key, subKey string, value json.RawMessage, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.MemoryStore.Set(ctx, provider, userKey, key, subKey, value, ttl)
}

// A send cut short by Grafana giving up on the webhook leaves the last push
// unknown too, though the request's context is done by the time that is
// recorded.
func TestRetractAfterASendCutShort(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	h := handlerWith(t, srv.URL, ctxStore{state.NewMemoryStore()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gone := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r.WithContext(ctx)) })
	steps := []struct {
		h      http.Handler
		status string
	}{{h, "firing"}, {h, "resolved"}, {gone, "firing"}, {h, "resolved"}}
	for _, s := range steps {
		postWith(t, s.h, "retract=1", instanceGroup(s.status, "a"))
	}
	ns := acktest.Notifications(t, testutil.GetCalls(calls, mu))
	if len(ns) != 2 || ns[1].Level != pushward.LevelPassive {
		t.Fatalf("notifications = %+v, want the alert and then the Resolved after the cut-short send", ns)
	}
}

// readCtxStore fails a read on a done context as well, as PostgresStore does.
type readCtxStore struct{ ctxStore }

func (s readCtxStore) GetGroup(ctx context.Context, provider, userKey, key string) (map[string]json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.MemoryStore.GetGroup(ctx, provider, userKey, key)
}

// A webhook whose context is done before its alert name is read sends
// nothing, so the name's last push stays as it was: the alert firing again is
// a repeat, and its acknowledged repeats do not start over.
func TestRetractAckAfterAReadCutShort(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	h := handlerWith(t, srv.URL, readCtxStore{ctxStore{state.NewMemoryStore()}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gone := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r.WithContext(ctx)) })
	for _, step := range []http.Handler{h, gone, h} {
		postWith(t, step, "ack=1&retract=1", instanceGroup("firing", "a"))
	}
	if ns := acktest.Notifications(t, testutil.GetCalls(calls, mu)); len(ns) != 1 {
		t.Fatalf("%d notifications, want the alert once", len(ns))
	}
}
