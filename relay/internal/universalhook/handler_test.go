package universalhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overrides"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const testKey = "hlk_universal_test"

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
	}
}

type harness struct {
	mux   http.Handler
	calls *[]testutil.APICall
	mu    *sync.Mutex
	store state.Store
	h     *Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServer(t)
	return newHarnessAt(t, srv.URL, calls, mu, nil)
}

// newHarnessAt registers the route against a PushWard server at url, with
// the relay_state store under the strict key hashing main applies. A nil
// proposer is the heuristic.
func newHarnessAt(t *testing.T, url string, calls *[]testutil.APICall, mu *sync.Mutex, proposer universal.Proposer) *harness {
	t.Helper()
	mux, api := humautil.NewTestAPI()
	store := state.NewMemoryStore()
	h := RegisterRoutes(api, state.KeyHashing(store, state.KeyModeStrict), client.NewPool(url, nil), testConfig(), proposer)
	t.Cleanup(func() {
		h.ender.StopAll()
		h.ender.Wait()
	})
	return &harness{mux: mux, calls: calls, mu: mu, store: store, h: h}
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

// deliverAs sends a payload through the heuristic's whole mapping, its kind
// included, the way a preset of that kind is delivered. The route itself
// sends a payload it does not know as a plain notification, so the card path
// is driven from here.
func (hs *harness) deliverAs(t *testing.T, source, channels string, body []byte) error {
	t.Helper()
	fields, truncated, err := universal.Flatten(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	shapes := universal.ShapesOf(fields)
	m := universal.NewMapping(universal.ProposeShapes(shapes), shapes)
	ctx := context.Background()
	if channels != "" {
		ov, err := overrides.Parse(url.Values{"channels": {channels}})
		if err != nil {
			t.Fatal(err)
		}
		ctx = context.WithValue(ctx, overrides.ContextKey(), ov)
	}
	r := &request{key: testKey, source: source, fields: fields, shapes: shapes, truncated: truncated, log: slog.Default(), sendLog: slog.Default()}
	_, err = hs.h.deliver(ctx, r, m, viaPreset)
	return err
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

// only is the single call a webhook made, which must be a notification.
func only(t *testing.T, hs *harness) pushward.SendNotificationRequest {
	t.Helper()
	calls := hs.snapshot()
	ns := notifications(t, calls)
	if len(calls) != 1 || len(ns) != 1 {
		t.Fatalf("got %d calls, want one notification: %+v", len(calls), calls)
	}
	return ns[0]
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

func TestPlainNotification(t *testing.T) {
	events := func() float64 {
		return counterValue(t, "pushward_relay_universal_events_total", map[string]string{"via": "proposer", "kind": "notification"})
	}
	start := events()
	hs := newHarness(t)
	if w := hs.post(t, "/universal?source=backups", fixture(t, "plain_notify.json")); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	n := only(t, hs)
	if n.Title != "Nightly backup finished" || n.Body != "Backed up 14 volumes to the offsite bucket in 4m12s." {
		t.Errorf("title/body = %q / %q", n.Title, n.Body)
	}
	if n.URL != "https://backups.example.com/runs/2026-09-28" || n.Source != "backups" || n.ThreadID != "universal-backups" {
		t.Errorf("url/source/thread = %q %q %q", n.URL, n.Source, n.ThreadID)
	}
	if n.Level != pushward.LevelActive || len(n.Actions) != 0 || n.CollapseID != "" || n.ActivitySlug != "" {
		t.Errorf("level %q, %d actions, collapse %q, slug %q", n.Level, len(n.Actions), n.CollapseID, n.ActivitySlug)
	}
	if got := events() - start; got != 1 {
		t.Errorf("events_total grew by %v, want 1", got)
	}
	if rows := hs.store.(*state.MemoryStore).Rows(); len(rows) != 0 {
		t.Errorf("a plain notification left %d relay_state rows", len(rows))
	}

	// The same payload again is just another notification.
	hs.post(t, "/universal?source=backups", fixture(t, "plain_notify.json"))
	if n := len(notifications(t, hs.snapshot())); n != 2 {
		t.Errorf("%d notifications after a second event, want 2", n)
	}
}

// The "notification sent" line names the source once: the client pool adds
// it, so the handler's logger must not carry it too.
func TestNotificationLogNamesSourceOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	hs := newHarness(t)
	if w := hs.post(t, "/universal?source=backups", fixture(t, "plain_notify.json")); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	var line string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, `"msg":"notification sent"`) {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no notification sent line in %q", buf.String())
	}
	if n := strings.Count(line, `"source":`); n != 1 {
		t.Errorf("source appears %d times in %s", n, line)
	}
	if !strings.Contains(line, `"source":"backups"`) || !strings.Contains(line, `"tenant":`) {
		t.Errorf("line lacks source or tenant: %s", line)
	}
}

// A payload the heuristic reads as an alert or a CI run still makes one
// notification per event, at the default level: no card, no end.
func TestUnknownShapesOpenNoCard(t *testing.T) {
	hs := newHarness(t)
	for _, name := range []string{"alertmanager_firing.json", "alertmanager_resolved.json", "ci_progress_running.json", "ci_progress_done.json"} {
		before := len(hs.snapshot())
		if w := hs.post(t, "/universal?source=am", fixture(t, name)); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		calls := testutil.WaitForCalls(t, hs.calls, hs.mu, before+1, time.Second)[before:]
		ns := notifications(t, calls)
		if len(calls) != 1 || len(ns) != 1 {
			t.Fatalf("%s: %d calls, want one notification: %+v", name, len(calls), calls)
		}
		if n := ns[0]; n.Level != pushward.LevelActive || n.CollapseID != "" || n.ActivitySlug != "" || n.Title == "" {
			t.Errorf("%s: %+v", name, n)
		}
	}
	firing := notifications(t, hs.snapshot())[0]
	if firing.Title != "DiskAlmostFull" || firing.Body != "Only 41 GiB left on /srv, filling at about 6 GiB a day." ||
		firing.URL != "https://alertmanager.example.com" {
		t.Errorf("firing = %q / %q / %q", firing.Title, firing.Body, firing.URL)
	}
	time.Sleep(50 * time.Millisecond) // past EndDelay and EndDisplayTime
	if n := activityCalls(hs.snapshot()); n != 0 {
		t.Errorf("%d activity calls", n)
	}
}

func TestTitleFallsBackToSource(t *testing.T) {
	body := []byte(`{"id":"8d3f5a1e-27c4-4b9e-a0f6-5c2d9e7b1a43","value":42,"ok":true}`)
	for _, tt := range []struct{ target, title string }{
		{"/universal?source=my-app", "My app"},
		{"/universal?source=nas2", "Nas2"},
		{"/universal", "Webhook"},
	} {
		t.Run(tt.target, func(t *testing.T) {
			hs := newHarness(t)
			if w := hs.post(t, tt.target, body); w.Code != http.StatusOK {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
			n := only(t, hs)
			// The id says nothing to a person and is left out.
			if n.Title != tt.title || n.Body != "Value: 42\nOk: true" {
				t.Errorf("title/body = %q / %q", n.Title, n.Body)
			}
		})
	}
}

// With no body field, the body lists the payload's most readable fields: text
// the heuristic ranks as body-like first, the rest in payload order, four at
// most. Ids, times and links are left out.
func TestDetailLines(t *testing.T) {
	hs := newHarness(t)
	body := []byte(`{
		"name": "Disk check",
		"status": "warning",
		"host": "nas-01",
		"mount": "/srv",
		"used_pct": 93,
		"checked_at": "2026-09-28T03:04:12Z",
		"check_id": "8d3f5a1e-27c4-4b9e-a0f6-5c2d9e7b1a43",
		"url": "https://grafana.example.com/d/disk",
		"spare": "unused"
	}`)
	if w := hs.post(t, "/universal", body); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	n := only(t, hs)
	if n.Title != "Disk check" || n.URL != "https://grafana.example.com/d/disk" {
		t.Errorf("title/url = %q %q", n.Title, n.URL)
	}
	if want := "Mount: /srv\nStatus: warning\nHost: nas-01\nSpare: unused"; n.Body != want {
		t.Errorf("body = %q, want %q", n.Body, want)
	}
}

// A body that only repeats the title is replaced by the detail lines, which
// leave out both fields.
func TestBodyRepeatingTitle(t *testing.T) {
	hs := newHarness(t)
	hs.post(t, "/universal", []byte(`{"title":"Backup done","message":"backup done","host":"nas-01"}`))
	if n := only(t, hs); n.Title != "Backup done" || n.Body != "Host: nas-01" {
		t.Errorf("title/body = %q / %q", n.Title, n.Body)
	}
}

// A payload with nothing readable keeps Apply's own body.
func TestNothingReadable(t *testing.T) {
	hs := newHarness(t)
	hs.post(t, "/universal?source=pinger", []byte(`{"id":"8d3f5a1e-27c4-4b9e-a0f6-5c2d9e7b1a43","at":"2026-09-28T03:04:12Z"}`))
	if n := only(t, hs); n.Title != "Pinger" || n.Body != "Event from Pinger" {
		t.Errorf("title/body = %q / %q", n.Title, n.Body)
	}
}

// Detail lines show values through Display: a field whose key names a
// secret is left out, and secrets, emails, URL credentials and queries
// inside text are masked. None of the synthetic secrets below may reach the
// notification.
func TestDetailLinesRedact(t *testing.T) {
	const (
		jwt    = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1bml2ZXJzYWwtdGVzdCJ9.c2lnbmF0dXJlLW5vdC1yZWFs"
		apiKey = "k9Fq2LmZ7xRw4TnB8vYc1HsJ6dPe3GaU" // #nosec G101 -- synthetic
		pass   = "correct-horse-battery-staple"
	)
	body := fmt.Appendf(nil, `{
		"title": "Sync failed",
		"api_key": %q,
		"dsn": "postgres://admin:%s@db:5432/app",
		"contact": "ops bob@example.com",
		"mirror": "from https://sync:%s@mirror.example.com/repo?token=%s",
		"seen": "jwt %s expired"
	}`, apiKey, pass, pass, apiKey, jwt)
	hs := newHarness(t)
	if w := hs.post(t, "/universal?source=sync", body); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	n := only(t, hs)
	want := "Contact: ops [email]\nMirror: from https://mirror.example.com/repo\nSeen: jwt [redacted] expired"
	if n.Title != "Sync failed" || n.Body != want {
		t.Errorf("title/body = %q / %q, want body %q", n.Title, n.Body, want)
	}
	b, _ := json.Marshal(n)
	for _, s := range []string{jwt, apiKey, pass, "eyJhbGci", "bob@", "sync:", "token", testKey} {
		if bytes.Contains(b, []byte(s)) {
			t.Errorf("notification carries %q: %s", s, b)
		}
	}
}

// A payload of long keys and values that escape to six bytes of JSON, the
// longest source and the longest link still makes a request inside the push
// budget, with labels cut to size.
func TestDetailLinesWorstCase(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"link":"https://example.com/` + strings.Repeat("p", 2000) + `"`)
	for i := range 250 {
		fmt.Fprintf(&b, `,"section_%03d_with_a_rather_long_descriptive_name":%q`, i, strings.Repeat("<>&", 80))
	}
	b.WriteString(`}`)
	source := strings.Repeat("s", 32)

	hs := newHarness(t)
	if w := hs.post(t, "/universal?source="+source, []byte(b.String())); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	calls := hs.snapshot()
	n := only(t, hs)
	if len(calls[0].Body) > maxNotificationBytes {
		t.Errorf("request is %d bytes, over %d", len(calls[0].Body), maxNotificationBytes)
	}
	if !strings.HasPrefix(n.URL, "https://example.com/ppp") || n.Title != "S"+strings.Repeat("s", 31) {
		t.Errorf("title/url = %q %q", n.Title, n.URL)
	}
	for line := range strings.SplitSeq(n.Body, "\n") {
		lbl, _, ok := strings.Cut(line, ": ")
		if !ok || utf8.RuneCountInString(lbl) > detailLabelRunes {
			t.Errorf("line %q", line)
		}
	}
}

// The lines stay inside any budget even when every character of their values
// escapes to six bytes of JSON.
func TestDetailLinesBudget(t *testing.T) {
	var fields []universal.Field
	for i := range 10 {
		path := strings.Repeat(string(rune('a'+i)), universal.MaxPathBytes)
		value := strings.Repeat("<>&", universal.MaxValueRunes/3)
		fields = append(fields, universal.Field{Path: path, Value: value, Type: universal.TypeString})
	}
	shapes := universal.ShapesOf(fields)
	for _, budget := range []int{0, 100, 700, maxNotificationBytes} {
		body := detailLines(fields, shapes, universal.Mapping{}, budget)
		if n := jsonLen(body); n > budget {
			t.Errorf("budget %d: body is %d bytes of JSON", budget, n)
		}
		if lines := strings.Count(body, "\n") + 1; body != "" && lines > maxDetailLines {
			t.Errorf("budget %d: %d lines", budget, lines)
		}
	}
	if got := detailLines(fields, shapes, universal.Mapping{}, maxNotificationBytes); strings.Count(got, "\n") != maxDetailLines-1 {
		t.Errorf("a full budget should hold %d lines: %q", maxDetailLines, got)
	}
	if got := detailLines(nil, nil, universal.Mapping{}, maxNotificationBytes); got != "" {
		t.Errorf("detailLines(nil) = %q", got)
	}
}

func TestHumanize(t *testing.T) {
	for in, want := range map[string]string{
		"my-app":            "My app",
		"disk_used":         "Disk used",
		"diskUsed":          "Disk used",
		"alerts[].labels.x": "X",
		"a.b.*.status_text": "Status text",
		"":                  "",
		"[]":                "",
	} {
		if got := humanize(in); got != want {
			t.Errorf("humanize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sourceTitle(""); got != "Webhook" {
		t.Errorf("sourceTitle(\"\") = %q", got)
	}
}

type stubProposer struct {
	res universal.Result
	err error
}

func (p stubProposer) Propose(context.Context, universal.Input) (universal.Result, error) {
	return p.res, p.err
}

// Only the proposer's title, body and link are used: a kind, a severity or a
// correlation id it picks changes nothing, so a notification is never
// time-sensitive and never collapses into another.
func TestProposerPicksTextOnly(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	hs := newHarnessAt(t, srv.URL, calls, mu, stubProposer{res: universal.Result{Proposal: universal.Proposal{
		Title:       "title",
		Body:        "message",
		Correlation: "host",
		Severity:    "level",
		Lifecycle:   "status",
		Kind:        universal.KindAlert,
	}}})
	hs.post(t, "/universal", []byte(`{"title":"Disk full","message":"Only 41 GiB left","host":"nas-01","level":"critical","status":"firing"}`))
	n := only(t, hs)
	if n.Title != "Disk full" || n.Body != "Only 41 GiB left" || n.Level != pushward.LevelActive || n.CollapseID != "" {
		t.Errorf("notification = %+v", n)
	}
}

// A proposer that fails is replaced by the heuristic.
func TestProposerErrorUsesHeuristic(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	hs := newHarnessAt(t, srv.URL, calls, mu, stubProposer{err: errors.New("model down")})
	hs.post(t, "/universal", fixture(t, "plain_notify.json"))
	if n := only(t, hs); n.Title != "Nightly backup finished" {
		t.Errorf("title = %q", n.Title)
	}
}

func TestChannelsActivitySendsNothing(t *testing.T) {
	hs := newHarness(t)
	if w := hs.post(t, "/universal?channels=activity", fixture(t, "plain_notify.json")); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if calls := hs.snapshot(); len(calls) != 0 {
		t.Errorf("calls = %+v, want none", calls)
	}
}

func TestBadBodies(t *testing.T) {
	hs := newHarness(t)
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
	hs := newHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/universal", strings.NewReader(`{"a":1}`))
	w := httptest.NewRecorder()
	hs.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", w.Code)
	}
}

func TestUpstreamRefusalSurfacesUnchanged(t *testing.T) {
	testutil.AssertUpstreamRefusalSurfaces(t, func(t *testing.T, status int) *httptest.ResponseRecorder {
		srv, calls, mu := testutil.MockPushWardServerRejecting(t, status, status)
		hs := newHarnessAt(t, srv.URL, calls, mu, nil)
		return hs.post(t, "/universal", fixture(t, "plain_notify.json"))
	})
}

// A delivery the server refuses fails the request, so the sender retries.
func TestFailedDeliveryFailsRequest(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServerRejecting(t, http.StatusUnprocessableEntity, 0)
	hs := newHarnessAt(t, srv.URL, calls, mu, nil)
	if w := hs.post(t, "/universal", fixture(t, "plain_notify.json")); w.Code == http.StatusOK {
		t.Errorf("a refused notification answered %d", w.Code)
	}
}

func TestAlertFiringThenResolved(t *testing.T) {
	hs := newHarness(t)
	if err := hs.deliverAs(t, "alertmanager", "", fixture(t, "alertmanager_firing.json")); err != nil {
		t.Fatalf("firing: %v", err)
	}
	calls := hs.snapshot()
	// create, ongoing frame, the alert notification
	if len(calls) != 3 || calls[0].Path != "/activities" || calls[1].Method != http.MethodPatch {
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
	if ns[0].Level != pushward.LevelTimeSensitive || ns[0].ActivitySlug != create.Slug || ns[0].CollapseID != create.Slug {
		t.Errorf("alert notification level/slug/collapse = %q %q %q", ns[0].Level, ns[0].ActivitySlug, ns[0].CollapseID)
	}

	if err := hs.deliverAs(t, "alertmanager", "", fixture(t, "alertmanager_resolved.json")); err != nil {
		t.Fatalf("resolved: %v", err)
	}
	// + the resolved notification, then the two end phases
	calls = testutil.WaitForCalls(t, hs.calls, hs.mu, 6, 5*time.Second)
	if len(calls) != 6 {
		t.Fatalf("got %d calls, want 6: %+v", len(calls), calls)
	}
	var end pushward.UpdateRequest
	testutil.UnmarshalBody(t, calls[len(calls)-1].Body, &end)
	if calls[len(calls)-1].Path != "/activities/"+create.Slug || end.State != pushward.StateEnded || end.Content.State != "Resolved" ||
		end.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("last call %s %s = %+v", calls[len(calls)-1].Method, calls[len(calls)-1].Path, end)
	}
	resolved := notifications(t, calls)[1]
	if resolved.Level != pushward.LevelPassive || !strings.HasPrefix(resolved.Body, "Resolved"+text.SepDot) {
		t.Errorf("resolved notification = %q %q", resolved.Level, resolved.Body)
	}
}

func TestProgressRunThenDone(t *testing.T) {
	hs := newHarness(t)
	if err := hs.deliverAs(t, "ci", "", fixture(t, "ci_progress_running.json")); err != nil {
		t.Fatal(err)
	}
	calls := hs.snapshot()
	// create and frame: a progress start notifies nobody
	if len(calls) != 2 || len(notifications(t, calls)) != 0 {
		t.Fatalf("running calls = %+v", calls)
	}
	c := testutil.LastActivityUpdate(t, calls)
	if c.Template != pushward.TemplateGeneric || c.Progress != 0.45 || c.State != "Running" {
		t.Errorf("running content = %+v", c)
	}

	if err := hs.deliverAs(t, "ci", "", fixture(t, "ci_progress_done.json")); err != nil {
		t.Fatal(err)
	}
	calls = testutil.WaitForCalls(t, hs.calls, hs.mu, 5, 5*time.Second)
	if len(calls) != 5 {
		t.Fatalf("got %d calls, want 5: %+v", len(calls), calls)
	}
	end := testutil.LastActivityUpdate(t, calls)
	if end.State != "Done" || end.Progress != 1 || end.AccentColor != pushward.ColorGreen {
		t.Errorf("done content = %+v", end)
	}
	done := notifications(t, calls)[0]
	if done.Level != pushward.LevelPassive || !strings.HasPrefix(done.Body, "Done"+text.SepDot) {
		t.Errorf("done notification = %q %q", done.Level, done.Body)
	}
}

// A progress frame without a value keeps the bar where the last one left it.
func TestProgressWithoutValueKeepsLast(t *testing.T) {
	hs := newHarness(t)
	if err := hs.deliverAs(t, "ci", "", fixture(t, "ci_progress_running.json")); err != nil {
		t.Fatal(err)
	}
	noValue := bytes.Replace(fixture(t, "ci_progress_running.json"), []byte(`"progress": 45,`), nil, 1)
	if err := hs.deliverAs(t, "ci", "", noValue); err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if c := testutil.LastActivityUpdate(t, hs.snapshot()); c.Progress != 0.45 {
		t.Errorf("progress after a frame without a value = %v, want 0.45", c.Progress)
	}
}

// A card the server refuses for good still reaches the user as a notification.
func TestActivityRefusedFallsBackToNotification(t *testing.T) {
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServerRejecting(t, 0, http.StatusConflict)
	hs := newHarnessAt(t, srv.URL, calls, mu, nil)
	if err := hs.deliverAs(t, "am", "", fixture(t, "alertmanager_firing.json")); err != nil {
		t.Fatalf("firing: %v", err)
	}
	if n := len(notifications(t, hs.snapshot())); n != 1 {
		t.Errorf("%d fallback notifications, want 1", n)
	}
}

func TestChannelsNotificationMakesNoActivityCalls(t *testing.T) {
	hs := newHarness(t)
	for _, name := range []string{"alertmanager_firing.json", "alertmanager_resolved.json", "ci_progress_running.json", "ci_progress_done.json"} {
		if err := hs.deliverAs(t, "", "notification", fixture(t, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	calls := hs.snapshot()
	if n := activityCalls(calls); n != 0 {
		t.Errorf("%d activity calls under channels=notification", n)
	}
	// firing, resolved, progress start and end
	if n := len(notifications(t, calls)); n != 4 {
		t.Errorf("%d notifications, want 4", n)
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

// A body with nothing to add past the title, and no readable field to fill
// it, says where the event came from; the server needs a body.
func TestBodyFallback(t *testing.T) {
	const id = `"id":"8d3f5a1e-27c4-4b9e-a0f6-5c2d9e7b1a43"`
	for _, c := range []struct{ target, body, title, text string }{
		{"/universal", `{"title":"Backup done","message":"backup done",` + id + `}`, "Backup done", "Webhook event"},
		{"/universal?source=nas-backup", `{"title":"Event",` + id + `}`, "Event", "Event from Nas backup"},
		{"/universal?source=-", `{` + id + `}`, "Webhook", "Webhook event"},
	} {
		hs := newHarness(t)
		hs.post(t, c.target, []byte(c.body))
		if n := only(t, hs); n.Title != c.title || n.Body != c.text {
			t.Errorf("%s %s: title/body = %q / %q, want %q / %q", c.target, c.body, n.Title, n.Body, c.title, c.text)
		}
	}
}

// A value's own line breaks are folded, so each field is one labelled line
// and the line cap holds.
func TestDetailLinesOneLineEach(t *testing.T) {
	hs := newHarness(t)
	hs.post(t, "/universal", []byte(`{"title":"Build finished","stage":"a\nb\nc\nd","env":"x\r\nZone: eu","host":"p\tq"}`))
	n := only(t, hs)
	want := "Stage: a b c d\nEnv: x Zone: eu\nHost: p q"
	if n.Body != want {
		t.Errorf("body = %q, want %q", n.Body, want)
	}
}

// Filled tight, the request the client sends stays inside the push budget,
// display name included. A title, link and values that escape to six bytes of
// JSON leave the four lines about the room they take, and the value length
// steps a rune at a time, so some run ends within a few bytes of the budget.
func TestDetailLinesBudgetSweep(t *testing.T) {
	source := strings.Repeat("s", 32)
	link := "https://example.com/?" + strings.Repeat("a&", 117)
	for n := 95; n <= 100; n++ {
		for k := 40; k <= 60; k++ {
			title := strings.Repeat("<<a", 34)[:n]
			var b strings.Builder
			fmt.Fprintf(&b, `{"title":%q,"link":%q`, title, link)
			for i := range 4 {
				fmt.Fprintf(&b, `,"remarks_about_the_nightly_sync_part_%02d":%q`, i, strings.Repeat("<>&", 20)[:k])
			}
			b.WriteString(`}`)
			hs := newHarness(t)
			if w := hs.post(t, "/universal?source="+source, []byte(b.String())); w.Code != http.StatusOK {
				t.Fatalf("title %d: got %d %s", n, w.Code, w.Body.String())
			}
			calls := hs.snapshot()
			if len(calls) != 1 {
				t.Fatalf("title %d: %d calls", n, len(calls))
			}
			if size := len(calls[0].Body); size > maxNotificationBytes {
				t.Errorf("title %d: request is %d bytes, over %d", n, size, maxNotificationBytes)
			}
			if !bytes.Contains(calls[0].Body, []byte(`"source_display_name"`)) {
				t.Fatalf("title %d: the request has no display name, so the sweep proves nothing", n)
			}
		}
	}
}

func newPresetHarness(t *testing.T) *harness {
	t.Helper()
	hs := newHarness(t)
	hs.h.config.Presets = true
	return hs
}

func presetFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../universal/presets/testdata", name)) // #nosec G304 -- fixture under the presets package
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An Alertmanager payload maps through its preset: a card that opens on
// firing and ends on resolved, keyed on the group, with no source set.
func TestPresetOpensAndEndsACard(t *testing.T) {
	hs := newPresetHarness(t)
	hits := counterValue(t, "pushward_relay_universal_preset_hits_total", map[string]string{"preset": "alertmanager"})
	if w := hs.post(t, "/universal", fixture(t, "alertmanager_firing.json")); w.Code != http.StatusOK {
		t.Fatalf("firing: %d %s", w.Code, w.Body.String())
	}
	calls := hs.snapshot()
	if len(calls) != 3 || calls[0].Path != "/activities" {
		t.Fatalf("firing calls = %+v", calls)
	}
	var create struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	testutil.UnmarshalBody(t, calls[0].Body, &create)
	if create.Name != "DiskAlmostFull" {
		t.Errorf("card name = %q", create.Name)
	}
	if c := testutil.LastActivityUpdate(t, calls); c.Severity != "critical" || c.State != "Disk /srv on nas-01 is 93% full" {
		t.Errorf("firing content = %+v", c)
	}
	if w := hs.post(t, "/universal", fixture(t, "alertmanager_resolved.json")); w.Code != http.StatusOK {
		t.Fatalf("resolved: %d %s", w.Code, w.Body.String())
	}
	calls = testutil.WaitForCalls(t, hs.calls, hs.mu, 6, 5*time.Second)
	last := calls[len(calls)-1]
	var end pushward.UpdateRequest
	testutil.UnmarshalBody(t, last.Body, &end)
	if last.Path != "/activities/"+create.Slug || end.State != pushward.StateEnded {
		t.Errorf("last call %s %s = %+v", last.Method, last.Path, end)
	}
	got := counterValue(t, "pushward_relay_universal_preset_hits_total", map[string]string{"preset": "alertmanager"}) - hits
	if got != 2 {
		t.Errorf("preset_hits_total{preset=alertmanager} grew by %v, want 2", got)
	}
}

// A notification preset sends its own title and body, not the proposer's
// picks.
func TestPresetNotification(t *testing.T) {
	hs := newPresetHarness(t)
	hs.post(t, "/universal", presetFixture(t, "graylog_event.json"))
	if n := only(t, hs); n.Title != "Failed SSH logins" || n.Body != "Failed SSH logins: count()=25.0" {
		t.Errorf("title/body = %q / %q", n.Title, n.Body)
	}
	if n := activityCalls(hs.snapshot()); n != 0 {
		t.Errorf("%d activity calls", n)
	}
}

// With presets off, a payload a preset knows is sent the way an unknown one
// is: one plain notification.
func TestPresetsOff(t *testing.T) {
	hs := newHarness(t)
	hs.post(t, "/universal", fixture(t, "alertmanager_firing.json"))
	calls := hs.snapshot()
	if n := activityCalls(calls); n != 0 || len(notifications(t, calls)) != 1 {
		t.Errorf("calls = %+v", calls)
	}
}

// A payload no preset knows goes out as a plain notification with presets
// on, counted as the proposer's.
func TestUnknownWithPresetsOn(t *testing.T) {
	hs := newPresetHarness(t)
	before := counterValue(t, "pushward_relay_universal_events_total", map[string]string{"via": viaProposer, "kind": "notification"})
	hs.post(t, "/universal?source=backup", []byte(`{"title":"Backup finished","details":"Copied 12 files to the NAS in 3 minutes"}`))
	if n := only(t, hs); n.Title != "Backup finished" || n.Body != "Copied 12 files to the NAS in 3 minutes" {
		t.Errorf("title/body = %q / %q", n.Title, n.Body)
	}
	if got := counterValue(t, "pushward_relay_universal_events_total", map[string]string{"via": viaProposer, "kind": "notification"}) - before; got != 1 {
		t.Errorf("events_total{via=proposer} grew by %v, want 1", got)
	}
}

// A GitHub check run keys its end on the conclusion, so a failed run ends red
// and says so, where the run's status alone would read "completed".
func TestPresetFailedRunEndsRed(t *testing.T) {
	hs := newPresetHarness(t)
	if w := hs.post(t, "/universal", presetFixture(t, "github-check-run_created.json")); w.Code != http.StatusOK {
		t.Fatalf("created: %d %s", w.Code, w.Body.String())
	}
	if n := activityCalls(hs.snapshot()); n == 0 {
		t.Fatal("the running check opened no card")
	}
	if w := hs.post(t, "/universal", presetFixture(t, "github-check-run_completed.json")); w.Code != http.StatusOK {
		t.Fatalf("completed: %d %s", w.Code, w.Body.String())
	}
	calls := testutil.WaitForCalls(t, hs.calls, hs.mu, 5, 5*time.Second)
	var end pushward.UpdateRequest
	testutil.UnmarshalBody(t, calls[len(calls)-1].Body, &end)
	if end.State != pushward.StateEnded || end.Content.AccentColor != pushward.ColorRed || end.Content.State != "Failure" {
		t.Errorf("end = %+v", end)
	}
}

// failure reads the end value first, then a one-word status body when the
// end value only says the run is over; never a branch, a sentence, or a
// body after an end value that already says the run went well.
func TestFailure(t *testing.T) {
	for _, c := range []struct{ raw, body, want string }{
		{"failed", "Deploy to prod", "failed"},
		{"killed", "", "killed"},
		{"declined", "", "declined"},
		{"COMPLETED", "FAILURE", "FAILURE"},
		{"FINALIZED", "ABORTED", "ABORTED"},
		{"COMPLETED", "SUCCESS", ""},
		{"COMPLETED", "UNSTABLE", "UNSTABLE"},
		{"skipped", "cancellation", ""},
		{"not_run", "timeout", ""},
		{"neutral", "canceled", ""},
		{"completed", "fix-failure", ""},
		{"completed", "Retry the failed upload", ""},
		{"success", "failure", ""},
		{"", "", ""},
	} {
		if got := failure(universal.Event{LifecycleRaw: c.raw, Body: c.body}); got != c.want {
			t.Errorf("failure(%q, %q) = %q, want %q", c.raw, c.body, got, c.want)
		}
	}
}

// A Jenkins build ends on its phase, which only says it is over; the status
// in its body makes the failed build's card end red.
func TestPresetJenkinsFailureEndsRed(t *testing.T) {
	hs := newPresetHarness(t)
	for _, name := range []string{"jenkins-notification_started.json", "jenkins-notification_completed.json"} {
		if w := hs.post(t, "/universal", presetFixture(t, name)); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	calls := testutil.WaitForCalls(t, hs.calls, hs.mu, 5, 5*time.Second)
	var end pushward.UpdateRequest
	testutil.UnmarshalBody(t, calls[len(calls)-1].Body, &end)
	if end.State != pushward.StateEnded || end.Content.AccentColor != pushward.ColorRed || end.Content.State != "Failure" {
		t.Errorf("end = %+v", end)
	}
}

// withValue returns a preset fixture with one top-level or nested string
// replaced, for events the fixtures have no file of their own for.
func withValue(t *testing.T, name, from, to string) []byte {
	t.Helper()
	b := presetFixture(t, name)
	out := bytes.Replace(b, []byte(from), []byte(to), 1)
	if bytes.Equal(out, b) {
		t.Fatalf("%s has no %s", name, from)
	}
	return out
}

// A note on an Opsgenie alert changes its open card and never opens one:
// after the close it is dropped instead of reopening the alert loudly.
func TestPresetUpdateNeverOpensACard(t *testing.T) {
	hs := newPresetHarness(t)
	note := withValue(t, "opsgenie_create.json", `"action": "Create"`, `"action": "AddNote"`)
	w := hs.post(t, "/universal", note)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "not open") {
		t.Fatalf("note on no card: %d %s", w.Code, w.Body.String())
	}
	if calls := hs.snapshot(); len(calls) != 0 {
		t.Fatalf("a note on no card made calls: %+v", calls)
	}

	hs.post(t, "/universal", presetFixture(t, "opsgenie_create.json"))
	opened := len(hs.snapshot())
	if w := hs.post(t, "/universal", note); w.Code != http.StatusOK || strings.Contains(w.Body.String(), "ignored") {
		t.Fatalf("note on an open card: %d %s", w.Code, w.Body.String())
	}
	calls := hs.snapshot()
	if len(calls) != opened+1 || calls[len(calls)-1].Method != http.MethodPatch {
		t.Errorf("a note on an open card should make one update, got %+v", calls[opened:])
	}
}

// Jenkins's FINALIZED after COMPLETED ends a card that is already ending: no
// second end, no second push.
func TestPresetSecondEndIsDropped(t *testing.T) {
	hs := newPresetHarness(t)
	for _, b := range [][]byte{
		presetFixture(t, "jenkins-notification_started.json"),
		presetFixture(t, "jenkins-notification_completed.json"),
		withValue(t, "jenkins-notification_completed.json", `"phase": "COMPLETED"`, `"phase": "FINALIZED"`),
	} {
		if w := hs.post(t, "/universal", b); w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(notifications(t, hs.snapshot())); n != 1 {
		t.Errorf("%d notifications, want the one end push", n)
	}
}
