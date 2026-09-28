package universalhook

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state/statetest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const (
	testKey   = "hlk_universal_test"
	publicURL = "https://relay.example.com"
)

var reviewKey = bytes.Repeat([]byte{0x42}, 32)

func testConfig() *config.UniversalConfig {
	return &config.UniversalConfig{
		BaseProviderConfig: config.BaseProviderConfig{
			Enabled:        true,
			Priority:       3,
			CleanupDelay:   time.Hour,
			StaleTimeout:   time.Hour,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
		PublicURL: publicURL,
		ReviewKey: base64.StdEncoding.EncodeToString(reviewKey),
	}
}

type harness struct {
	mux      http.Handler
	calls    *[]testutil.APICall
	mu       *sync.Mutex
	store    state.Store
	mappings state.MappingStore
	h        *Handler
}

func newHarness(t *testing.T, mappings state.MappingStore) *harness {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServer(t)
	return newHarnessAt(t, srv.URL, calls, mu, state.NewMemoryStore(), mappings)
}

// newHarnessAt registers the routes against a PushWard server at url, with
// store as the relay_state store under the strict key hashing main applies.
func newHarnessAt(t *testing.T, url string, calls *[]testutil.APICall, mu *sync.Mutex, store state.Store, mappings state.MappingStore) *harness {
	t.Helper()
	mux, api := humautil.NewTestAPI()
	h, err := RegisterRoutes(api, state.KeyHashing(store, state.KeyModeStrict), mappings, client.NewPool(url, nil), testConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.ender.StopAll()
		h.ender.Wait()
	})
	return &harness{mux: mux, calls: calls, mu: mu, store: store, mappings: mappings, h: h}
}

func (hs *harness) post(t *testing.T, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)
	w := httptest.NewRecorder()
	hs.mux.ServeHTTP(w, req)
	return w
}

// tap does what the phone does for a silent action: a bare POST to its URL,
// with no integration key.
func (hs *harness) tap(t *testing.T, action pushward.NotificationAction) *httptest.ResponseRecorder {
	t.Helper()
	path, ok := strings.CutPrefix(action.URL, publicURL)
	if !ok {
		t.Fatalf("action %s URL %q is not under the public URL", action.ID, action.URL)
	}
	req := httptest.NewRequest(action.Method, path, nil)
	w := httptest.NewRecorder()
	hs.mux.ServeHTTP(w, req)
	return w
}

func (hs *harness) snapshot() []testutil.APICall {
	return testutil.GetCalls(hs.calls, hs.mu)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../../testdata/universal", name)) // #nosec G304 -- fixture under relay/testdata
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func notifications(t *testing.T, calls []testutil.APICall) []pushward.SendNotificationRequest {
	t.Helper()
	var out []pushward.SendNotificationRequest
	for _, c := range calls {
		if c.Method == http.MethodPost && c.Path == "/notifications" {
			var n pushward.SendNotificationRequest
			testutil.UnmarshalBody(t, c.Body, &n)
			out = append(out, n)
		}
	}
	return out
}

func reviews(t *testing.T, calls []testutil.APICall) []pushward.SendNotificationRequest {
	t.Helper()
	var out []pushward.SendNotificationRequest
	for _, n := range notifications(t, calls) {
		if n.ThreadID == "universal-review" {
			out = append(out, n)
		}
	}
	return out
}

func activityCalls(calls []testutil.APICall) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c.Path, "/activities") {
			n++
		}
	}
	return n
}

func action(t *testing.T, n pushward.SendNotificationRequest, id string) pushward.NotificationAction {
	t.Helper()
	for _, a := range n.Actions {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("notification %q has no %q action", n.Title, id)
	return pushward.NotificationAction{}
}

func mappingRow(t *testing.T, hs *harness, source string) *state.MappingRow {
	t.Helper()
	rows := hs.mappings.(*state.MemoryMappingStore).Rows()
	for i := range rows {
		if rows[i].Source == source {
			return &rows[i]
		}
	}
	t.Fatalf("no mapping row for source %q (%d rows)", source, len(rows))
	return nil
}

// counterValue reads a counter from the default registry, matching the given
// labels.
func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metrics:
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if v, ok := labels[lp.GetName()]; ok && v != lp.GetValue() {
					continue metrics
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

func TestNewShapeDeliversAndReviewsOnce(t *testing.T) {
	hs := newHarness(t, state.NewMemoryMappingStore())
	body := fixture(t, "plain_notify.json")

	if w := hs.post(t, "/universal?source=backups", body); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	ns := notifications(t, hs.snapshot())
	if len(ns) != 2 {
		t.Fatalf("got %d notifications, want the event and its review", len(ns))
	}
	ev, rv := ns[0], ns[1]
	if ev.Title != "Nightly backup finished" || !strings.HasPrefix(ev.Body, "Backed up 14 volumes") {
		t.Errorf("event notification = %q / %q", ev.Title, ev.Body)
	}
	if ev.Source != "backups" || ev.ThreadID != "universal-backups" || ev.URL != "https://backups.example.com/runs/2026-09-28" {
		t.Errorf("event notification source/thread/url = %q %q %q", ev.Source, ev.ThreadID, ev.URL)
	}

	if rv.Title != "Check mapping: backups" || rv.ThreadID != "universal-review" || rv.Level != pushward.LevelActive {
		t.Errorf("review title/thread/level = %q %q %q", rv.Title, rv.ThreadID, rv.Level)
	}
	if !strings.HasPrefix(rv.Body, "Sent as a notification.\nTitle <- title = \"Nightly backup finished\"") {
		t.Errorf("review body = %q", rv.Body)
	}
	if rv.Metadata["kind"] != "notification" || len(rv.Metadata["fp"]) != 16 {
		t.Errorf("review metadata = %v", rv.Metadata)
	}
	if len(rv.Actions) != 3 {
		t.Fatalf("review has %d actions, want 3", len(rv.Actions))
	}
	accept, reject, edit := action(t, rv, "accept"), action(t, rv, "reject"), action(t, rv, "edit")
	checks := []struct {
		a                              pushward.NotificationAction
		title, prefix, method          string
		foreground, destructive, authn bool
	}{
		{accept, "Looks right", publicURL + ReviewPath, http.MethodPost, false, false, true},
		{reject, "Send raw instead", publicURL + ReviewPath, http.MethodPost, false, true, true},
		{edit, "Edit", publicURL + EditPath, "", true, false, true},
	}
	for _, c := range checks {
		if c.a.Title != c.title || !strings.HasPrefix(c.a.URL, c.prefix) || c.a.Method != c.method ||
			c.a.Foreground != c.foreground || c.a.Destructive != c.destructive || c.a.AuthenticationRequired != c.authn {
			t.Errorf("action %s = %+v", c.a.ID, c.a)
		}
	}

	row := mappingRow(t, hs, "backups")
	if row.Status != state.MappingPending || row.ReviewSentAt == nil || row.Proposer != "heuristic/1" {
		t.Errorf("row status %s, review sent %v, proposer %q", row.Status, row.ReviewSentAt, row.Proposer)
	}

	// The same shape again: delivered, not reviewed a second time.
	if w := hs.post(t, "/universal?source=backups", body); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	calls := hs.snapshot()
	if n, r := len(notifications(t, calls)), len(reviews(t, calls)); n != 3 || r != 1 {
		t.Errorf("after a second event: %d notifications, %d reviews; want 3 and 1", n, r)
	}
}

func TestAcceptWithoutKey(t *testing.T) {
	hs := newHarness(t, state.NewMemoryMappingStore())
	body := fixture(t, "plain_notify.json")
	hs.post(t, "/universal", body)
	rv := reviews(t, hs.snapshot())[0]

	w := hs.tap(t, action(t, rv, "accept"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatalf("accept: %d %s", w.Code, w.Body.String())
	}
	if w := hs.tap(t, action(t, rv, "accept")); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"idempotent"`) {
		t.Errorf("second accept: %d %s", w.Code, w.Body.String())
	}
	if w := hs.tap(t, action(t, rv, "reject")); w.Code != http.StatusConflict {
		t.Errorf("reject after accept: %d %s", w.Code, w.Body.String())
	}
	if row := mappingRow(t, hs, ""); row.Status != state.MappingConfirmed || row.Rev != 1 || row.ExpiresAt != nil {
		t.Errorf("row after accept: status %s rev %d expires %v", row.Status, row.Rev, row.ExpiresAt)
	}

	before := len(hs.snapshot())
	hs.post(t, "/universal", body)
	calls := hs.snapshot()[before:]
	ns := notifications(t, calls)
	if len(ns) != 1 || ns[0].Title != "Nightly backup finished" {
		t.Fatalf("a confirmed shape sends its event only, got %+v", ns)
	}
}

func TestRejectSendsRaw(t *testing.T) {
	hs := newHarness(t, state.NewMemoryMappingStore())
	body := fixture(t, "plain_notify.json")
	hs.post(t, "/universal?source=backups", body)
	rv := reviews(t, hs.snapshot())[0]
	if w := hs.tap(t, action(t, rv, "reject")); w.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", w.Code, w.Body.String())
	}

	before := len(hs.snapshot())
	if w := hs.post(t, "/universal?source=backups", body); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	ns := notifications(t, hs.snapshot()[before:])
	if len(ns) != 1 {
		t.Fatalf("got %d notifications, want the raw one", len(ns))
	}
	raw := ns[0]
	if raw.Title != "backups" || raw.Level != pushward.LevelPassive {
		t.Errorf("raw title/level = %q %q", raw.Title, raw.Level)
	}
	if !strings.HasPrefix(raw.Body, "title: Nightly backup finished"+text.SepDot+"message: Backed up") {
		t.Errorf("raw body = %q", raw.Body)
	}
	if strings.Count(raw.Body, text.SepDot) > rawLines-1 {
		t.Errorf("raw body has more than %d lines: %q", rawLines, raw.Body)
	}
	if len(raw.Actions) != 1 {
		t.Fatalf("raw notification has %d actions, want Edit only", len(raw.Actions))
	}
	if e := raw.Actions[0]; e.ID != "edit" || e.Title != "Edit" || !e.Foreground || !strings.HasPrefix(e.URL, publicURL+EditPath) {
		t.Errorf("edit action = %+v", e)
	}
}

func TestPendingCap(t *testing.T) {
	hs := newHarness(t, state.NewMemoryMappingStore())
	capHits := func() float64 {
		return counterValue(t, "pushward_relay_universal_cap_hits_total", map[string]string{"cap": "pending"})
	}
	start := capHits()
	for i := range state.MaxPending + 1 {
		body := fmt.Appendf(nil, `{"title":"Job %d","message":"finished","extra_%d":"x"}`, i, i)
		if w := hs.post(t, "/universal", body); w.Code != http.StatusOK {
			t.Fatalf("shape %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	calls := hs.snapshot()
	if n, r := len(notifications(t, calls)), len(reviews(t, calls)); n != 2*state.MaxPending+1 || r != state.MaxPending {
		t.Errorf("%d notifications and %d reviews, want every event and %d reviews", n, r, state.MaxPending)
	}
	if got := capHits() - start; got != 1 {
		t.Errorf("cap hits grew by %v, want 1", got)
	}
	if n := len(hs.mappings.(*state.MemoryMappingStore).Rows()); n != state.MaxPending {
		t.Errorf("%d rows stored, want %d", n, state.MaxPending)
	}
}

func TestAlertFiringThenResolved(t *testing.T) {
	hs := newHarness(t, state.NewMemoryMappingStore())
	if w := hs.post(t, "/universal?source=alertmanager", fixture(t, "alertmanager_firing.json")); w.Code != http.StatusOK {
		t.Fatalf("firing: %d %s", w.Code, w.Body.String())
	}
	calls := hs.snapshot()
	// create, ongoing frame, the alert notification, the review
	if len(calls) != 4 || calls[0].Path != "/activities" || calls[1].Method != http.MethodPatch {
		t.Fatalf("firing calls = %+v", calls)
	}
	var create struct {
		Slug     string `json:"slug"`
		Name     string `json:"name"`
		Priority int    `json:"priority"`
	}
	testutil.UnmarshalBody(t, calls[0].Body, &create)
	if !strings.HasPrefix(create.Slug, "u-alertmanager-") || create.Name != "DiskAlmostFull" || create.Priority != 3 {
		t.Errorf("create = %+v", create)
	}
	c := testutil.LastActivityUpdate(t, calls)
	if c.Template != pushward.TemplateAlert || c.Severity != "critical" || c.AccentColor != pushward.ColorRed ||
		c.Subtitle != "alertmanager" || c.FiredAt == nil || !strings.HasPrefix(c.State, "Only 41 GiB left") {
		t.Errorf("firing content = %+v", c)
	}
	ns := notifications(t, calls)
	if rv := reviews(t, calls); len(rv) != 1 || !strings.Contains(rv[0].Body, "Matched on <- alerts[].fingerprint\n") || strings.Contains(rv[0].Body, "4f1c2d9e8a7b6c5d") {
		t.Errorf("the review must name the correlation field without its value: %+v", rv)
	}
	if ns[0].Level != pushward.LevelTimeSensitive || ns[0].ActivitySlug != create.Slug || ns[0].CollapseID != create.Slug {
		t.Errorf("alert notification level/slug/collapse = %q %q %q", ns[0].Level, ns[0].ActivitySlug, ns[0].CollapseID)
	}

	if w := hs.post(t, "/universal?source=alertmanager", fixture(t, "alertmanager_resolved.json")); w.Code != http.StatusOK {
		t.Fatalf("resolved: %d %s", w.Code, w.Body.String())
	}
	// + the resolved notification, then the two end phases
	calls = testutil.WaitForCalls(t, hs.calls, hs.mu, 7, 5*time.Second)
	if len(calls) != 7 {
		t.Fatalf("got %d calls, want 7: %+v", len(calls), calls)
	}
	var end pushward.UpdateRequest
	testutil.UnmarshalBody(t, calls[len(calls)-1].Body, &end)
	if calls[len(calls)-1].Path != "/activities/"+create.Slug || end.State != pushward.StateEnded || end.Content.State != "Resolved" ||
		end.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("last call %s %s = %+v", calls[len(calls)-1].Method, calls[len(calls)-1].Path, end)
	}
	resolved := notifications(t, calls)[2]
	if resolved.Level != pushward.LevelPassive || !strings.HasPrefix(resolved.Body, "Resolved"+text.SepDot) {
		t.Errorf("resolved notification = %q %q", resolved.Level, resolved.Body)
	}
	if n := len(reviews(t, calls)); n != 1 {
		t.Errorf("%d reviews for one shape", n)
	}
}

func TestProgressRunThenDone(t *testing.T) {
	hs := newHarness(t, state.NewMemoryMappingStore())
	hs.post(t, "/universal?source=ci", fixture(t, "ci_progress_running.json"))
	calls := hs.snapshot()
	// create, frame, review: a progress start notifies nobody
	if len(calls) != 3 || len(notifications(t, calls)) != 1 {
		t.Fatalf("running calls = %+v", calls)
	}
	c := testutil.LastActivityUpdate(t, calls)
	if c.Template != pushward.TemplateGeneric || c.Progress != 0.45 || c.State != "Running" {
		t.Errorf("running content = %+v", c)
	}

	hs.post(t, "/universal?source=ci", fixture(t, "ci_progress_done.json"))
	calls = testutil.WaitForCalls(t, hs.calls, hs.mu, 6, 5*time.Second)
	if len(calls) != 6 {
		t.Fatalf("got %d calls, want 6: %+v", len(calls), calls)
	}
	end := testutil.LastActivityUpdate(t, calls)
	if end.State != "Done" || end.Progress != 1 || end.AccentColor != pushward.ColorGreen {
		t.Errorf("done content = %+v", end)
	}
	done := notifications(t, calls)[1]
	if done.Level != pushward.LevelPassive || !strings.HasPrefix(done.Body, "Done"+text.SepDot) {
		t.Errorf("done notification = %q %q", done.Level, done.Body)
	}
}

// A progress frame without a value keeps the bar where the last one left it.
func TestProgressWithoutValueKeepsLast(t *testing.T) {
	hs := newHarness(t, state.NewMemoryMappingStore())
	hs.post(t, "/universal?source=ci", fixture(t, "ci_progress_running.json"))
	noValue := bytes.Replace(fixture(t, "ci_progress_running.json"), []byte(`"progress": 45,`), nil, 1)
	if w := hs.post(t, "/universal?source=ci", noValue); w.Code != http.StatusOK {
		t.Fatalf("second frame: %d %s", w.Code, w.Body.String())
	}
	if c := testutil.LastActivityUpdate(t, hs.snapshot()); c.Progress != 0.45 {
		t.Errorf("progress after a frame without a value = %v, want 0.45", c.Progress)
	}
}

// A card the server refuses for good still reaches the user as a notification.
func TestActivityRefusedFallsBackToNotification(t *testing.T) {
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServerRejecting(t, 0, http.StatusConflict)
	hs := newHarnessAt(t, srv.URL, calls, mu, state.NewMemoryStore(), state.NewMemoryMappingStore())
	if w := hs.post(t, "/universal?source=am", fixture(t, "alertmanager_firing.json")); w.Code != http.StatusOK {
		t.Fatalf("firing: %d %s", w.Code, w.Body.String())
	}
	var delivered int
	for _, n := range notifications(t, hs.snapshot()) {
		if n.ThreadID != "universal-review" {
			delivered++
		}
	}
	if delivered != 1 {
		t.Errorf("%d fallback notifications, want 1", delivered)
	}
}

func TestChannelsNotificationMakesNoActivityCalls(t *testing.T) {
	hs := newHarness(t, state.NewMemoryMappingStore())
	for _, name := range []string{"alertmanager_firing.json", "alertmanager_resolved.json", "ci_progress_running.json", "ci_progress_done.json"} {
		if w := hs.post(t, "/universal?channels=notification", fixture(t, name)); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	calls := hs.snapshot()
	if n := activityCalls(calls); n != 0 {
		t.Errorf("%d activity calls under channels=notification", n)
	}
	// firing, resolved, progress start and end, and two reviews
	if n := len(notifications(t, calls)); n != 6 {
		t.Errorf("%d notifications, want 6", n)
	}
}

// channels=activity suppresses the event's notification but not the review:
// the review is how the user fixes the mapping at all.
func TestChannelsActivityStillReviews(t *testing.T) {
	hs := newHarness(t, state.NewMemoryMappingStore())
	hs.post(t, "/universal?channels=activity", fixture(t, "plain_notify.json"))
	ns := notifications(t, hs.snapshot())
	if len(ns) != 1 || ns[0].ThreadID != "universal-review" {
		t.Errorf("notifications = %+v, want the review only", ns)
	}
}

func TestBadBodies(t *testing.T) {
	hs := newHarness(t, state.NewMemoryMappingStore())
	for _, body := range []string{`{"a":`, `"hello"`, `42`, `true`, `null`, `not json`, ``} {
		if w := hs.post(t, "/universal", []byte(body)); w.Code != http.StatusBadRequest {
			t.Errorf("body %q: got %d, want 400", body, w.Code)
		}
	}
	w := hs.post(t, "/universal", []byte(`{}`))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"ignored"`) {
		t.Errorf("empty object: %d %s", w.Code, w.Body.String())
	}
	if w := hs.post(t, "/universal?source=Bad_Source", []byte(`{"a":1}`)); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid source: got %d, want 422", w.Code)
	}
	if n := len(hs.snapshot()); n != 0 {
		t.Errorf("%d upstream calls for bodies that were refused", n)
	}
}

func TestNoKeyIsUnauthorized(t *testing.T) {
	hs := newHarness(t, state.NewMemoryMappingStore())
	req := httptest.NewRequest(http.MethodPost, "/universal", strings.NewReader(`{"a":1}`))
	w := httptest.NewRecorder()
	hs.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", w.Code)
	}
}

func TestMappingStoreDownStillDelivers(t *testing.T) {
	hs := newHarness(t, statetest.FailingMappingStore{})
	if w := hs.post(t, "/universal", fixture(t, "plain_notify.json")); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	ns := notifications(t, hs.snapshot())
	if len(ns) != 1 || ns[0].Title != "Nightly backup finished" {
		t.Errorf("notifications = %+v, want the event and no review", ns)
	}

	hs.post(t, "/universal", fixture(t, "alertmanager_firing.json"))
	if activityCalls(hs.snapshot()) != 2 {
		t.Error("an alert must still open its card with the mapping store down")
	}
}

// No review for a delivery that failed in a way a retry can fix (an unknown
// key here; the client does not retry it, so the test stays fast); the retry
// that gets through sends it.
func TestReviewWaitsForDelivery(t *testing.T) {
	var mu sync.Mutex
	fail := true
	var calls []testutil.APICall
	var cmu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		cmu.Lock()
		calls = append(calls, testutil.APICall{Method: r.Method, Path: r.URL.Path, Body: body.Bytes()})
		cmu.Unlock()
		mu.Lock()
		defer mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	t.Cleanup(srv.Close)
	hs := newHarnessAt(t, srv.URL, &calls, &cmu, state.NewMemoryStore(), state.NewMemoryMappingStore())
	body := fixture(t, "plain_notify.json")

	if w := hs.post(t, "/universal", body); w.Code == http.StatusOK {
		t.Fatal("a failed delivery must fail the request")
	}
	if n := len(reviews(t, hs.snapshot())); n != 0 {
		t.Fatalf("%d reviews after a failed delivery", n)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if w := hs.post(t, "/universal", body); w.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	if n := len(reviews(t, hs.snapshot())); n != 1 {
		t.Errorf("%d reviews after the retry, want 1", n)
	}
}

// A delivery the server refuses for good still gets its review, with the
// refusal in it: every retry would be refused the same way, and the review is
// the user's only way to fix the mapping.
func TestPermanentRefusalStillReviewed(t *testing.T) {
	var mu sync.Mutex
	var calls []testutil.APICall
	refused := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, testutil.APICall{Method: r.Method, Path: r.URL.Path, Body: body.Bytes()})
		if !refused {
			refused = true
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	t.Cleanup(srv.Close)
	hs := newHarnessAt(t, srv.URL, &calls, &mu, state.NewMemoryStore(), state.NewMemoryMappingStore())

	if w := hs.post(t, "/universal", fixture(t, "plain_notify.json")); w.Code == http.StatusOK {
		t.Fatal("a refused delivery must fail the request")
	}
	rs := reviews(t, hs.snapshot())
	if len(rs) != 1 {
		t.Fatalf("%d reviews after a refused delivery, want 1", len(rs))
	}
	if !strings.Contains(rs[0].Body, "Delivery was refused") {
		t.Errorf("review body does not mention the refusal: %q", rs[0].Body)
	}
}

// Synthetic secrets: none of them may reach a review, a notification's text
// or any stored column. The delivered notification keeps its link whole: it
// is the sender's own URL, going to the sender's own device, and a signed
// link with its query cut off does not open.
func TestSecretsStayOut(t *testing.T) {
	const (
		jwt    = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1bml2ZXJzYWwtdGVzdCJ9.c2lnbmF0dXJlLW5vdC1yZWFs"
		apiKey = "k9Fq2LmZ7xRw4TnB8vYc1HsJ6dPe3GaU" // #nosec G101 -- synthetic
		pass   = "correct-horse-battery-staple"
	)
	body := fmt.Appendf(nil, `{
		"title": "Deploy finished",
		"message": "Version 2.4.1 is live on web-01",
		"token": %q,
		"api_key": %q,
		"auth": {"password": %q, "user": "deploy"},
		"link": "https://deploy.example.com/runs/77?token=%s"
	}`, jwt, apiKey, pass, jwt)
	hs := newHarness(t, state.NewMemoryMappingStore())
	if w := hs.post(t, "/universal?source=deploys", body); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	rv := reviews(t, hs.snapshot())
	if len(rv) != 1 {
		t.Fatalf("%d reviews", len(rv))
	}
	secrets := []string{jwt, apiKey, pass, "eyJhbGci", testKey}
	for _, n := range notifications(t, hs.snapshot()) {
		if n.ThreadID != "universal-review" {
			n.URL = ""
		}
		b, _ := json.Marshal(n)
		for _, s := range secrets {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("notification %q carries %q: %s", n.Title, s, b)
			}
		}
	}
	for _, row := range hs.mappings.(*state.MemoryMappingStore).Rows() {
		for name, col := range map[string][]byte{"mapping": row.Mapping, "shape": row.Shape, "proposal": row.Proposal, "samples": row.Samples, "candidates": row.Candidates} {
			for _, s := range secrets {
				if bytes.Contains(col, []byte(s)) {
					t.Errorf("stored %s carries %q: %s", name, s, col)
				}
			}
		}
		var samples map[string]string
		if err := json.Unmarshal(row.Samples, &samples); err != nil {
			t.Fatal(err)
		}
		if samples["token"] != "[redacted]" || samples["api_key"] != "[redacted]" || samples["auth.password"] != "[redacted]" {
			t.Errorf("secret samples = %q %q %q", samples["token"], samples["api_key"], samples["auth.password"])
		}
	}
	for _, r := range hs.store.(*state.MemoryStore).Rows() {
		if strings.Contains(r.UserKey, testKey) {
			t.Errorf("relay_state row under the raw key: %+v", r)
		}
	}
}

// A 256-field payload with long keys and values, and the longest source,
// still makes a review that fits the push.
func TestReviewSizeWorstCase(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{`)
	long := strings.Repeat("word ", 60)
	names := []string{"title", "message", "url", "id", "progress", "severity", "status"}
	for i := range 256 {
		if i > 0 {
			b.WriteString(",")
		}
		key := fmt.Sprintf("section_%03d_with_a_rather_long_descriptive_name.%s", i, names[i%len(names)])
		switch names[i%len(names)] {
		case "url":
			fmt.Fprintf(&b, `%q:%q`, key, "https://example.com/"+strings.Repeat("p", 200))
		case "progress":
			fmt.Fprintf(&b, `%q:%d`, key, i%100)
		case "severity":
			fmt.Fprintf(&b, `%q:"critical"`, key)
		case "status":
			fmt.Fprintf(&b, `%q:"firing"`, key)
		default:
			fmt.Fprintf(&b, `%q:%q`, key, long)
		}
	}
	b.WriteString(`}`)
	source := strings.Repeat("s", maxSourceLen)

	hs := newHarness(t, state.NewMemoryMappingStore())
	if w := hs.post(t, "/universal?source="+source, []byte(b.String())); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	var found bool
	for _, c := range hs.snapshot() {
		var n pushward.SendNotificationRequest
		if c.Path != "/notifications" {
			continue
		}
		testutil.UnmarshalBody(t, c.Body, &n)
		if n.ThreadID != "universal-review" {
			continue
		}
		found = true
		if len(c.Body) > maxReviewBytes {
			t.Errorf("review request is %d bytes, over %d", len(c.Body), maxReviewBytes)
		}
	}
	if !found {
		t.Fatal("no review sent")
	}
	row := mappingRow(t, hs, source)
	if len(row.Samples) > maxSamplesBytes || len(row.Shape) > 16<<10+1024 {
		t.Errorf("stored samples %d bytes, shape %d bytes", len(row.Samples), len(row.Shape))
	}
}

// The body budget holds for any mapping, not just what the heuristic
// proposes: every role on a 256-byte path with full value tables.
func TestReviewRequestBudget(t *testing.T) {
	hs := newHarness(t, state.NewMemoryMappingStore())
	m, fields := worstMapping()
	r := &request{
		pk:     state.MappingKey{Source: strings.Repeat("s", maxSourceLen)},
		fields: fields,
	}
	req, err := hs.h.reviewRequest(r, m, hs.h.reviewExpiry())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > maxReviewBytes {
		t.Errorf("review request is %d bytes, over %d", len(b), maxReviewBytes)
	}
	if !strings.HasPrefix(req.Body, "Sent as an alert card.\nTitle <- ...") {
		t.Errorf("body = %q", req.Body)
	}
}

func TestReviewLinks(t *testing.T) {
	hs := newHarness(t, state.NewMemoryMappingStore())
	body := fixture(t, "plain_notify.json")
	hs.post(t, "/universal", body)
	row := mappingRow(t, hs, "")

	mint := func(c Claims) pushward.NotificationAction {
		tok, err := Mint(reviewKey, c)
		if err != nil {
			t.Fatal(err)
		}
		return pushward.NotificationAction{ID: "x", URL: publicURL + ReviewPath + tok, Method: http.MethodPost}
	}
	claims := Claims{Scope: ScopeAccept, Expires: *row.ExpiresAt, KeyHash: row.KeyHash, Fingerprint: row.Fingerprint}

	// A link for an earlier proposal of this shape carries another expiry.
	older := claims
	older.Expires = row.ExpiresAt.Add(-time.Hour)
	if w := hs.tap(t, mint(older)); w.Code != http.StatusNotFound {
		t.Errorf("link for another proposal: %d", w.Code)
	}
	// An edit link is not a review link.
	edit := claims
	edit.Scope = ScopeEdit
	if w := hs.tap(t, mint(edit)); w.Code != http.StatusNotFound {
		t.Errorf("edit token on the review route: %d", w.Code)
	}
	if w := hs.tap(t, pushward.NotificationAction{URL: publicURL + ReviewPath + "bm90LWEtdG9rZW4", Method: http.MethodPost}); w.Code != http.StatusNotFound {
		t.Errorf("garbage token: %d", w.Code)
	}
	// An authentic link past its expiry says so.
	hs.h.now = func() time.Time { return row.ExpiresAt.Add(time.Second) }
	if w := hs.tap(t, mint(claims)); w.Code != http.StatusGone {
		t.Errorf("expired link: %d", w.Code)
	}
	hs.h.now = time.Now
	if w := hs.tap(t, mint(claims)); w.Code != http.StatusOK {
		t.Errorf("the real link: %d %s", w.Code, w.Body.String())
	}
	// GET is not a route.
	req := httptest.NewRequest(http.MethodGet, ReviewPath+"x", nil)
	w := httptest.NewRecorder()
	hs.mux.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Error("GET on a review link must not decide anything")
	}
}

// A review token with \r or \n in it is another spelling of the same bytes
// to the base64 decoder, and must not decide anything.
func TestReviewLinkOneSpelling(t *testing.T) {
	hs := newHarness(t, state.NewMemoryMappingStore())
	hs.post(t, "/universal", fixture(t, "plain_notify.json"))
	accept := action(t, reviews(t, hs.snapshot())[0], "accept")
	prefix := publicURL + ReviewPath
	tok := strings.TrimPrefix(accept.URL, prefix)
	for _, ins := range []string{"%0A", "%0D", "%0D%0A"} {
		bad := accept
		bad.URL = prefix + tok[:7] + ins + tok[7:]
		if w := hs.tap(t, bad); w.Code != http.StatusNotFound {
			t.Errorf("token with %s: %d", ins, w.Code)
		}
	}
	if row := mappingRow(t, hs, ""); row.Status != state.MappingPending {
		t.Fatalf("a respelled token decided the row: %s", row.Status)
	}
	if w := hs.tap(t, accept); w.Code != http.StatusOK {
		t.Errorf("the token itself: %d", w.Code)
	}
}

func TestUpstreamRefusalSurfacesUnchanged(t *testing.T) {
	testutil.AssertUpstreamRefusalSurfaces(t, func(t *testing.T, status int) *httptest.ResponseRecorder {
		srv, calls, mu := testutil.MockPushWardServerRejecting(t, status, status)
		hs := newHarnessAt(t, srv.URL, calls, mu, state.NewMemoryStore(), state.NewMemoryMappingStore())
		return hs.post(t, "/universal", fixture(t, "plain_notify.json"))
	})
}

func TestIsCapabilityPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/universal/review/abc": true,
		"/universal/edit/abc":   true,
		"/universal/list/abc":   true,
		"/universal":            false,
		"/universal/review":     false,
		"/grafana":              false,
	} {
		if got := IsCapabilityPath(path); got != want {
			t.Errorf("IsCapabilityPath(%q) = %v", path, got)
		}
	}
}

func TestFailed(t *testing.T) {
	for raw, want := range map[string]bool{
		"failed": true, "FAILURE": true, "build.error": true, "TIMED_OUT": true, "timed out": true,
		"cancelled": true, "canceled": true, "Aborted": true, "success": false, "done": false,
		"completed": false, "": false, "passed": false,
	} {
		if got := failed(raw); got != want {
			t.Errorf("failed(%q) = %v", raw, got)
		}
	}
}

// worstMapping maps every role to a path of the longest length Flatten
// keeps, with full value tables of the longest keys, over 256-rune values.
func worstMapping() (universal.Mapping, []universal.Field) {
	m := universal.Mapping{
		V:               universal.MappingVersion,
		Kind:            universal.KindAlert,
		Paths:           map[universal.Role]string{},
		SeverityValues:  map[string]string{},
		LifecycleValues: map[string]string{},
	}
	var fields []universal.Field
	for i, role := range universal.Roles {
		path := strings.Repeat(string(rune('a'+i)), universal.MaxPathBytes)
		m.Paths[role] = path
		fields = append(fields, universal.Field{Path: path, Value: strings.Repeat("value ", 50)[:universal.MaxValueRunes], Type: universal.TypeString})
	}
	for i := range universal.MaxTableEntries {
		k := fmt.Sprintf("%02d", i) + strings.Repeat("k", universal.MaxTableKeyRunes-2)
		m.SeverityValues[k] = universal.SeverityCritical
		m.LifecycleValues[k] = universal.LifecycleEnded
	}
	return m, fields
}

func TestLevelOf(t *testing.T) {
	sev := universal.Mapping{Paths: map[universal.Role]string{universal.RoleSeverity: "level"}}
	tests := []struct {
		m    universal.Mapping
		ev   universal.Event
		want string
	}{
		{universal.Mapping{}, universal.Event{Severity: "critical", SevFrom: universal.FromTable}, pushward.LevelActive},
		{sev, universal.Event{Severity: "critical", SevFrom: universal.FromTable}, pushward.LevelTimeSensitive},
		{sev, universal.Event{Severity: "warning", SevFrom: universal.FromHeuristic}, pushward.LevelActive},
		{sev, universal.Event{Severity: "info", SevFrom: universal.FromTable}, pushward.LevelPassive},
		{sev, universal.Event{Severity: "info", SevFrom: universal.FromDefault}, pushward.LevelActive},
	}
	for _, tt := range tests {
		if got := levelOf(tt.m, tt.ev); got != tt.want {
			t.Errorf("levelOf(%v, %+v) = %q, want %q", tt.m.Paths, tt.ev, got, tt.want)
		}
	}
}

// The raw notification for a rejected shape stays inside the push budget even
// when every character of its values escapes to six bytes of JSON.
func TestRawBodyBudget(t *testing.T) {
	var fields []universal.Field
	for i := range 10 {
		path := strings.Repeat(string(rune('a'+i)), universal.MaxPathBytes)
		value := strings.Repeat("<>&", universal.MaxValueRunes/3)
		fields = append(fields, universal.Field{Path: path, Value: value, Type: universal.TypeString})
	}
	for _, budget := range []int{0, 100, 700, maxReviewBytes} {
		body := rawBody(fields, budget)
		if body == "No values" {
			continue
		}
		if n := jsonLen(body); n > budget {
			t.Errorf("budget %d: body is %d bytes of JSON", budget, n)
		}
	}
	if got := rawBody(nil, maxReviewBytes); got != "No values" {
		t.Errorf("rawBody(nil) = %q", got)
	}
}
