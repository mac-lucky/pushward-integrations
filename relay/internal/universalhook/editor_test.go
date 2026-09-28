package universalhook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/ratelimit"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
)

var update = flag.Bool("update", false, "rewrite the golden pages in testdata/editor")

var editorNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type editorHarness struct {
	*harness
	e     *Editor
	clock *testClock
	store *state.MemoryMappingStore
}

// newEditorHarness serves the webhook and the editor on one mux, with the
// handler and the store on one fixed clock. The rate limits are off; the
// tests that exercise them switch them back on.
func newEditorHarness(t *testing.T) *editorHarness {
	t.Helper()
	store := state.NewMemoryMappingStore()
	clock := &testClock{t: editorNow}
	store.SetClock(clock.now)
	hs := newHarness(t, store)
	hs.h.now = clock.now
	e := RegisterEditor(hs.mux.(*http.ServeMux), hs.h)
	e.allowIP = func(string) bool { return true }
	e.allowKey = func(string, string) bool { return true }
	return &editorHarness{harness: hs, e: e, clock: clock, store: store}
}

func (eh *editorHarness) get(t *testing.T, path string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	eh.mux.ServeHTTP(w, req)
	return w
}

// submit posts a form as the page does: form-encoded, from the relay's own
// origin, with no integration key. headers override or add to that.
func (eh *editorHarness) submit(t *testing.T, path string, form url.Values, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", publicURL)
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	w := httptest.NewRecorder()
	eh.mux.ServeHTTP(w, req)
	return w
}

// newRow posts a fixture as the first event of its shape and returns the
// editor path from its review's Edit action.
func (eh *editorHarness) newRow(t *testing.T, source, name string) string {
	t.Helper()
	before := len(eh.snapshot())
	if w := eh.post(t, "/universal?source="+source, fixture(t, name)); w.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", w.Code, w.Body.String())
	}
	rv := reviews(t, eh.snapshot()[before:])
	if len(rv) != 1 {
		t.Fatalf("%d reviews for a new shape", len(rv))
	}
	return strings.TrimPrefix(action(t, rv[0], "edit").URL, publicURL)
}

// open GETs an editor page and reads its form.
func (eh *editorHarness) open(t *testing.T, path string) pageForm {
	t.Helper()
	w := eh.get(t, path)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	return readPage(t, w.Body.String())
}

func tokenOf(path string) string {
	_, tok, _ := strings.Cut(strings.TrimPrefix(path, "/universal/"), "/")
	return tok
}

type option struct {
	value, label string
	selected     bool
}

// pageForm is what a browser would post from a page's form, before a button
// is pressed, and the options of each select.
type pageForm struct {
	action  string
	values  url.Values
	options map[string][]option
}

func readPage(t *testing.T, body string) pageForm {
	t.Helper()
	f := pageForm{values: url.Values{}, options: map[string][]option{}}
	z := html.NewTokenizer(strings.NewReader(body))
	var sel string
	var opts []option
	inOption := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			if !errors.Is(z.Err(), io.EOF) {
				t.Fatal(z.Err())
			}
			return f
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			a := attrMap(tok)
			switch tok.Data {
			case "form":
				f.action = a["action"]
			case "input":
				if a["type"] == "hidden" || a["type"] == "text" {
					f.values.Add(a["name"], a["value"])
				}
			case "select":
				sel, opts = a["name"], nil
			case "option":
				_, selected := a["selected"]
				opts = append(opts, option{value: a["value"], selected: selected})
				inOption = true
			}
		case html.TextToken:
			if inOption {
				opts[len(opts)-1].label += string(z.Text())
			}
		case html.EndTagToken:
			switch z.Token().Data {
			case "option":
				inOption = false
			case "select":
				v := ""
				if len(opts) > 0 {
					v = opts[0].value
				}
				for _, o := range opts {
					if o.selected {
						v = o.value
					}
				}
				f.values.Add(sel, v)
				if _, ok := f.options[sel]; !ok {
					f.options[sel] = opts
				}
			}
		}
	}
}

func attrMap(tok html.Token) map[string]string {
	m := make(map[string]string, len(tok.Attr))
	for _, a := range tok.Attr {
		m[a.Key] = a.Val
	}
	return m
}

// optionFor returns the value of the first option of select name whose label
// starts with prefix.
func (f pageForm) optionFor(t *testing.T, name, prefix string) string {
	t.Helper()
	for _, o := range f.options[name] {
		if strings.HasPrefix(o.label, prefix) {
			return o.value
		}
	}
	t.Fatalf("select %s has no option starting %q", name, prefix)
	return ""
}

func (f pageForm) with(kv ...string) url.Values {
	v := url.Values{}
	for k, vs := range f.values {
		v[k] = slices.Clone(vs)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "editor", name+".html")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) // #nosec G304 -- a golden file under testdata
	if err != nil {
		t.Fatalf("%v (run the test with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("page differs from %s; if the change is intended, rerun with -update\n%s", path, got)
	}
}

func mustMintAt(t *testing.T, c Claims) string {
	t.Helper()
	tok, err := Mint(reviewKey, c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func editClaims(t *testing.T, path string, now time.Time) Claims {
	t.Helper()
	c, err := Parse(reviewKey, tokenOf(path), now, ScopeEdit)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func tenant() [32]byte {
	return auth.UniversalDigest(testKey)
}

func TestEditorGolden(t *testing.T) {
	eh := newEditorHarness(t)
	pending := eh.newRow(t, "alertmanager", "alertmanager_firing.json")
	eh.clock.advance(time.Minute)
	confirmed := eh.newRow(t, "backups", "plain_notify.json")
	rv := reviews(t, eh.snapshot())
	if w := eh.tap(t, action(t, rv[len(rv)-1], "accept")); w.Code != http.StatusOK {
		t.Fatalf("accept: %d", w.Code)
	}
	eh.clock.advance(time.Minute)
	eh.newRow(t, "ci", "ci_progress_running.json")
	rv = reviews(t, eh.snapshot())
	if w := eh.tap(t, action(t, rv[len(rv)-1], "reject")); w.Code != http.StatusOK {
		t.Fatalf("reject: %d", w.Code)
	}

	page := func(path string, want int) []byte {
		t.Helper()
		w := eh.get(t, path)
		if w.Code != want {
			t.Fatalf("GET %s: %d, want %d\n%s", path, w.Code, want, w.Body.String())
		}
		return w.Body.Bytes()
	}

	golden(t, "pending", page(pending, http.StatusOK))

	// A save that fails validation: title and body on one field, a value
	// that is too long and one that is personal data.
	f := readPage(t, string(page(pending, http.StatusOK)))
	form := f.with("op", "save", "p.body", f.values.Get("p.title"))
	form["sv.n"] = []string{strings.Repeat("very-long-value ", 5), "+14155550100", "p1"}
	form["sv.v"] = append(form["sv.v"][:len(form["sv.v"])-3], "critical", "warning", "info")
	w := eh.submit(t, pending, form)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid save: %d %s", w.Code, w.Body.String())
	}
	golden(t, "invalid", w.Body.Bytes())

	if w := eh.submit(t, pending, f.with("op", "save")); w.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	golden(t, "saved", page(pending+"?saved=1", http.StatusOK))

	// Eight days on the samples are gone, and the page shows paths only.
	eh.clock.advance(8 * 24 * time.Hour)
	if res, err := eh.store.Sweep(context.Background()); err != nil || res.Samples == 0 {
		t.Fatalf("sweep = %+v, %v", res, err)
	}
	golden(t, "confirmed", page(confirmed, http.StatusOK))

	golden(t, "notfound", page(EditPath+"not-a-token", http.StatusNotFound))
	c := editClaims(t, confirmed, eh.clock.now())
	c.Expires = eh.clock.now().Add(-time.Hour)
	golden(t, "expired", page(EditPath+mustMintAt(t, c), http.StatusGone))

	eh.newRow(t, "deploys", "plain_notify.json")
	list := mustMintAt(t, Claims{Scope: ScopeList, Expires: eh.clock.now().Add(ListTokenTTL), KeyHash: tenant()})
	golden(t, "list", page(ListPath+list, http.StatusOK))
}

func TestEditorEndToEnd(t *testing.T) {
	eh := newEditorHarness(t)
	path := eh.newRow(t, "backups", "plain_notify.json")
	f := eh.open(t, path)
	if f.action != tokenOf(path) {
		t.Errorf("form action %q, want the token relative to the page", f.action)
	}
	form := f.with("op", "save", "p.title", f.optionFor(t, "p.title", "host"))
	w := eh.submit(t, path, form)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != tokenOf(path)+"?saved=1" {
		t.Fatalf("save: %d %q %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}

	row := mappingRow(t, eh.harness, "backups")
	var m universal.Mapping
	if err := json.Unmarshal(row.Mapping, &m); err != nil {
		t.Fatal(err)
	}
	if row.Status != state.MappingConfirmed || row.Proposer != "user-edit" || m.Paths[universal.RoleTitle] != "host" {
		t.Fatalf("row after save: status %s proposer %q title %q", row.Status, row.Proposer, m.Paths[universal.RoleTitle])
	}
	if m.Classes[universal.RoleTitle] == "" {
		t.Error("the saved mapping has no class for its title")
	}

	before := len(eh.snapshot())
	if w := eh.post(t, "/universal?source=backups", fixture(t, "plain_notify.json")); w.Code != http.StatusOK {
		t.Fatalf("next webhook: %d", w.Code)
	}
	ns := notifications(t, eh.snapshot()[before:])
	if len(ns) != 1 || ns[0].Title != "backup-01" {
		t.Fatalf("next webhook sent %+v, want one notification titled by host", ns)
	}

	w = eh.get(t, path+"?saved=1")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Saved.") {
		t.Errorf("saved page: %d", w.Code)
	}
	if got := readPage(t, w.Body.String()).values.Get("p.title"); got != form.Get("p.title") {
		t.Errorf("page shows title option %q after the save, want %q", got, form.Get("p.title"))
	}
}

// "Send raw instead" rejects the shape and keeps the stored mapping, whatever
// the form's other fields say; saving later maps it again.
func TestEditorSendRaw(t *testing.T) {
	eh := newEditorHarness(t)
	path := eh.newRow(t, "backups", "plain_notify.json")
	f := eh.open(t, path)
	stored := mappingRow(t, eh.harness, "backups").Mapping
	if w := eh.submit(t, path, f.with("op", "raw", "p.body", f.values.Get("p.title"))); w.Code != http.StatusSeeOther {
		t.Fatalf("raw: %d %s", w.Code, w.Body.String())
	}
	row := mappingRow(t, eh.harness, "backups")
	if row.Status != state.MappingRejected || !jsonSame(row.Mapping, stored) {
		t.Fatalf("row after raw: status %s mapping %s", row.Status, row.Mapping)
	}
	before := len(eh.snapshot())
	eh.post(t, "/universal?source=backups", fixture(t, "plain_notify.json"))
	if ns := notifications(t, eh.snapshot()[before:]); len(ns) != 1 || ns[0].Title != "backups" {
		t.Fatalf("a rejected shape sends the raw notification, got %+v", ns)
	}

	f = eh.open(t, path)
	if w := eh.submit(t, path, f.with("op", "save")); w.Code != http.StatusSeeOther {
		t.Fatalf("save after raw: %d", w.Code)
	}
	if row := mappingRow(t, eh.harness, "backups"); row.Status != state.MappingConfirmed {
		t.Errorf("status %s after saving a rejected mapping", row.Status)
	}
}

func jsonSame(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	ja, _ := json.Marshal(va)
	jb, _ := json.Marshal(vb)
	return bytes.Equal(ja, jb)
}

func TestEditorTokens(t *testing.T) {
	eh := newEditorHarness(t)
	path := eh.newRow(t, "backups", "plain_notify.json")
	f := eh.open(t, path)
	raw, err := b64.DecodeString(tokenOf(path))
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		b := bytes.Clone(raw)
		b[i] ^= 0x01
		p := EditPath + b64.EncodeToString(b)
		if w := eh.get(t, p); w.Code != http.StatusNotFound {
			t.Errorf("byte %d flipped: GET %d", i, w.Code)
		}
		if w := eh.submit(t, p, f.with("op", "save")); w.Code != http.StatusNotFound {
			t.Errorf("byte %d flipped: POST %d", i, w.Code)
		}
	}

	c := editClaims(t, path, eh.clock.now())
	other, err := Mint(bytes.Repeat([]byte{0x07}, 32), c)
	if err != nil {
		t.Fatal(err)
	}
	if w := eh.get(t, EditPath+other); w.Code != http.StatusNotFound {
		t.Errorf("token under another key: %d", w.Code)
	}

	old := c
	old.Expires = eh.clock.now().Add(-time.Second)
	if w := eh.get(t, EditPath+mustMintAt(t, old)); w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "/universal/links") {
		t.Errorf("expired edit token: %d", w.Code)
	}
	if w := eh.submit(t, EditPath+mustMintAt(t, old), f.with("op", "save")); w.Code != http.StatusGone {
		t.Errorf("expired edit token, POST: %d", w.Code)
	}

	list := Claims{Scope: ScopeList, Expires: eh.clock.now().Add(time.Hour), KeyHash: c.KeyHash}
	listTok := mustMintAt(t, list)
	if w := eh.get(t, EditPath+listTok); w.Code != http.StatusNotFound {
		t.Errorf("list token on the edit page: %d", w.Code)
	}
	if w := eh.submit(t, EditPath+listTok, f.with("op", "save")); w.Code != http.StatusNotFound {
		t.Errorf("list token posted to the edit page: %d", w.Code)
	}
	if w := eh.get(t, ListPath+tokenOf(path)); w.Code != http.StatusNotFound {
		t.Errorf("edit token on the list page: %d", w.Code)
	}
	for _, s := range []Scope{ScopeAccept, ScopeReject} {
		rc := c
		rc.Scope = s
		if w := eh.get(t, EditPath+mustMintAt(t, rc)); w.Code != http.StatusNotFound {
			t.Errorf("review token %c on the edit page: %d", s, w.Code)
		}
	}
	list.Expires = eh.clock.now().Add(-time.Second)
	if w := eh.get(t, ListPath+mustMintAt(t, list)); w.Code != http.StatusGone {
		t.Errorf("expired list token: %d", w.Code)
	}

	// \r and \n are skipped by the base64 decoder; a token with one in it
	// is another spelling, and no token at all.
	tok := tokenOf(path)
	for _, ins := range []string{"%0A", "%0D", "%0D%0A"} {
		for _, p := range []string{EditPath + tok[:5] + ins + tok[5:], EditPath + tok + ins, ListPath + listTok[:5] + ins + listTok[5:]} {
			if w := eh.get(t, p); w.Code != http.StatusNotFound {
				t.Errorf("GET %s: %d", strings.ReplaceAll(p, tok, "<tok>"), w.Code)
			}
		}
		if w := eh.submit(t, EditPath+tok[:5]+ins+tok[5:], f.with("op", "save")); w.Code != http.StatusNotFound {
			t.Errorf("POST with %s in the token: %d", ins, w.Code)
		}
	}
	if w := eh.get(t, ListPath+listTok); w.Code != http.StatusOK {
		t.Errorf("the list token itself: %d", w.Code)
	}

	// No integration key is needed, and one sent is not what picks the
	// tenant: the token is.
	w := eh.get(t, path, "Authorization", "Bearer hlk_someone_else")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "backups") {
		t.Errorf("GET with another tenant's key: %d", w.Code)
	}
	if w := eh.submit(t, path, f.with("op", "save"), "Authorization", "Bearer hlk_someone_else"); w.Code != http.StatusSeeOther {
		t.Errorf("POST with another tenant's key: %d", w.Code)
	}
}

func TestEditorOtherRoutes(t *testing.T) {
	eh := newEditorHarness(t)
	path := eh.newRow(t, "backups", "plain_notify.json")
	cases := []struct {
		method, path string
		code         int
		allow        string
	}{
		{http.MethodPut, path, http.StatusMethodNotAllowed, "GET, HEAD, POST"},
		{http.MethodDelete, ListPath + "abc", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodGet, EditPath, http.StatusNotFound, ""},
		{http.MethodGet, path + "/more", http.StatusNotFound, ""},
		{http.MethodPost, ListPath + "abc", http.StatusMethodNotAllowed, "GET, HEAD"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		w := httptest.NewRecorder()
		eh.mux.ServeHTTP(w, req)
		if w.Code != c.code || w.Header().Get("Allow") != c.allow || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s %s: %d allow %q type %q", c.method, c.path, w.Code, w.Header().Get("Allow"), w.Header().Get("Content-Type"))
		}
	}
	if w := eh.get(t, path); w.Code != http.StatusOK {
		t.Errorf("GET the page itself: %d", w.Code)
	}
}

func TestEditorRefusesForms(t *testing.T) {
	eh := newEditorHarness(t)
	path := eh.newRow(t, "backups", "plain_notify.json")
	f := eh.open(t, path)
	ok := f.with("op", "save")
	stored := *mappingRow(t, eh.harness, "backups")

	post := func(body, contentType string, headers ...string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Origin", publicURL)
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		w := httptest.NewRecorder()
		eh.mux.ServeHTTP(w, req)
		return w.Code
	}
	big := ok.Encode() + "&pad=" + strings.Repeat("x", maxFormBytes)
	cases := []struct {
		name string
		code int
	}{
		{"json", post(`{"op":"save"}`, "application/json")},
		{"multipart", post(ok.Encode(), "multipart/form-data; boundary=x")},
		{"no type", post(ok.Encode(), "")},
		{"too large", post(big, "application/x-www-form-urlencoded")},
		{"foreign origin", post(ok.Encode(), "application/x-www-form-urlencoded", "Origin", "https://evil.example.com")},
		{"null origin, cross site", post(ok.Encode(), "application/x-www-form-urlencoded", "Origin", "null", "Sec-Fetch-Site", "cross-site")},
		{"same site", post(ok.Encode(), "application/x-www-form-urlencoded", "Sec-Fetch-Site", "same-site")},
		{"no version", post(url.Values{"op": {"save"}}.Encode(), "application/x-www-form-urlencoded")},
		{"bad field index", post(f.with("op", "save", "p.title", "999").Encode(), "application/x-www-form-urlencoded")},
		{"unknown button", post(f.with("op", "delete").Encode(), "application/x-www-form-urlencoded")},
		{"rev past int4", post(f.with("op", "save", "rev", "2147483648").Encode(), "application/x-www-form-urlencoded")},
		{"too many rows", post(rows(f, 20).Encode(), "application/x-www-form-urlencoded")},
	}
	want := map[string]int{
		"json": http.StatusUnsupportedMediaType, "multipart": http.StatusUnsupportedMediaType,
		"no type": http.StatusUnsupportedMediaType, "too large": http.StatusRequestEntityTooLarge,
		"foreign origin": http.StatusForbidden, "null origin, cross site": http.StatusForbidden,
		"same site": http.StatusForbidden, "no version": http.StatusBadRequest,
		"bad field index": http.StatusBadRequest, "unknown button": http.StatusBadRequest,
		"rev past int4": http.StatusBadRequest, "too many rows": http.StatusBadRequest,
	}
	for _, c := range cases {
		if c.code != want[c.name] {
			t.Errorf("%s: %d, want %d", c.name, c.code, want[c.name])
		}
	}
	if row := mappingRow(t, eh.harness, "backups"); row.Rev != stored.Rev || row.Status != stored.Status {
		t.Fatalf("a refused form changed the row: rev %d status %s", row.Rev, row.Status)
	}
	// The largest rev a row can have is only stale; the table the page
	// offers, all of it, is read.
	if w := eh.submit(t, path, f.with("op", "save", "rev", "2147483647")); w.Code != http.StatusConflict {
		t.Errorf("rev at the int4 limit: %d", w.Code)
	}
	if w := eh.submit(t, path, rows(f, universal.MaxTableEntries+blankRows)); w.Code != http.StatusSeeOther {
		t.Errorf("a full table: %d %s", w.Code, w.Body.String())
	}

	// The page's own posts: no-referrer makes Safari send Origin: null, and
	// an older browser may send no Origin at all.
	if w := eh.submit(t, path, ok, "Origin", "null", "Sec-Fetch-Site", "same-origin"); w.Code != http.StatusSeeOther {
		t.Errorf("Origin null from the page: %d", w.Code)
	}
	if w := eh.submit(t, path, ok, "Origin", ""); w.Code != http.StatusSeeOther {
		t.Errorf("no Origin: %d", w.Code)
	}
	if w := eh.submit(t, path, ok, "Origin", strings.ToUpper(publicURL)); w.Code != http.StatusSeeOther {
		t.Errorf("Origin in upper case: %d", w.Code)
	}
}

// rows fills the page's status table with n typed rows.
func rows(f pageForm, n int) url.Values {
	v := f.with("op", "save")
	v["lv.k"], v["lv.n"], v["lv.v"] = nil, nil, nil
	for i := range n {
		v["lv.n"] = append(v["lv.n"], fmt.Sprintf("state%02d", i))
		v["lv.v"] = append(v["lv.v"], universal.LifecycleOngoing)
	}
	return v
}

// A form with every row too long lists 20 problems at most, and its page
// stays small.
func TestEditorErrorCap(t *testing.T) {
	eh := newEditorHarness(t)
	path := eh.newRow(t, "alertmanager", "alertmanager_firing.json")
	f := eh.open(t, path)
	form := f.with("op", "save")
	for _, table := range []string{"sv", "lv"} {
		form[table+".k"], form[table+".n"], form[table+".v"] = nil, nil, nil
		for i := range universal.MaxTableEntries + blankRows {
			form[table+".n"] = append(form[table+".n"], fmt.Sprintf("%02d%s", i, strings.Repeat("x", universal.MaxTableKeyRunes)))
		}
	}
	form["sv.v"] = slices.Repeat([]string{universal.SeverityInfo}, universal.MaxTableEntries+blankRows)
	form["lv.v"] = slices.Repeat([]string{universal.LifecycleEnded}, universal.MaxTableEntries+blankRows)
	if n := len(form.Encode()); n > maxFormBytes {
		t.Fatalf("form is %d bytes", n)
	}
	w := eh.submit(t, path, form)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d", w.Code)
	}
	body := w.Body.String()
	if n := strings.Count(body, "<li>"); n != maxErrorLines {
		t.Errorf("%d problems listed, want %d", n, maxErrorLines)
	}
	if want := fmt.Sprintf("And %d more.", 2*(universal.MaxTableEntries+blankRows)-maxErrorLines+1); !strings.Contains(body, want) {
		t.Errorf("no %q line", want)
	}
	if len(body) > 64<<10 {
		t.Errorf("the re-rendered page is %d bytes", len(body))
	}
}

func TestEditorValidation(t *testing.T) {
	eh := newEditorHarness(t)
	path := eh.newRow(t, "ci", "ci_progress_running.json")
	f := eh.open(t, path)
	stored := *mappingRow(t, eh.harness, "ci")

	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"title is body", f.with("op", "save", "p.body", f.values.Get("p.title")), "Title and body are both"},
		{"text as progress", f.with("op", "save", "p.progress", f.optionFor(t, "p.body", "commit.message")), "cannot be the progress"},
		{"unknown kind", f.with("op", "save", "kind", "banner"), "Unknown kind"},
		{"unknown scale", f.with("op", "save", "ps", "perthousand"), "Unknown progress scale"},
	}
	for _, c := range cases {
		w := eh.submit(t, path, c.form)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(html.UnescapeString(w.Body.String()), c.want) {
			t.Errorf("%s: %d, want 422 saying %q", c.name, w.Code, c.want)
		}
	}

	// Table keys: normalized, capped, never secret-looking, and not listed
	// twice with different meanings; at most 16 per table.
	tableForm := func(keys ...string) url.Values {
		v := f.with("op", "save")
		v["lv.k"] = nil
		v["lv.n"] = keys
		v["lv.v"] = nil
		for i := range keys {
			v["lv.v"] = append(v["lv.v"], []string{universal.LifecycleOngoing, universal.LifecycleEnded}[i%2])
		}
		return v
	}
	var many []string
	for i := range universal.MaxTableEntries + 1 {
		many = append(many, fmt.Sprintf("state%02d", i))
	}
	tables := []struct {
		name string
		keys []string
		want string
	}{
		{"too long", []string{strings.Repeat("x", universal.MaxTableKeyRunes+1)}, "longer than 64 characters"},
		{"credential", []string{"ghp_" + strings.Repeat("Ab1", 12)}, "looks like a secret"},
		{"email", []string{"ops@example.com"}, "looks like a secret"},
		{"card", []string{"4111111111111111"}, "looks like a secret"},
		{"twice", []string{"Done", "done"}, `"done" is listed twice`},
		{"too many", many, "17 entries, at most 16"},
	}
	for _, c := range tables {
		w := eh.submit(t, path, tableForm(c.keys...))
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(html.UnescapeString(w.Body.String()), c.want) {
			t.Errorf("table %s: %d, want 422 saying %q", c.name, w.Code, c.want)
		}
	}
	if row := mappingRow(t, eh.harness, "ci"); row.Rev != stored.Rev {
		t.Fatal("an invalid form was stored")
	}

	// The re-rendered form keeps what was typed, and a fixed form saves.
	w := eh.submit(t, path, tableForm("Queued", strings.Repeat("y", 70)))
	again := readPage(t, w.Body.String())
	if n := again.values["lv.n"]; !slices.Contains(n, "Queued") {
		t.Errorf("re-rendered form lost the typed value: %q", n)
	}
	if again.values.Get("rev") != f.values.Get("rev") || again.values.Get("created") != f.values.Get("created") {
		t.Error("re-rendered form does not carry the version it was made from")
	}
	if w := eh.submit(t, path, tableForm("Queued", "Canceled")); w.Code != http.StatusSeeOther {
		t.Fatalf("valid table: %d %s", w.Code, w.Body.String())
	}
	var m universal.Mapping
	_ = json.Unmarshal(mappingRow(t, eh.harness, "ci").Mapping, &m)
	if m.LifecycleValues["queued"] != universal.LifecycleOngoing || m.LifecycleValues["canceled"] != universal.LifecycleEnded {
		t.Errorf("stored lifecycle table %v", m.LifecycleValues)
	}
	// "default" drops a key.
	f = eh.open(t, path)
	drop := f.with("op", "save")
	for i, k := range drop["lv.k"] {
		if k == "queued" {
			drop["lv.v"][i] = ""
		}
	}
	if w := eh.submit(t, path, drop); w.Code != http.StatusSeeOther {
		t.Fatalf("drop: %d", w.Code)
	}
	m = universal.Mapping{}
	_ = json.Unmarshal(mappingRow(t, eh.harness, "ci").Mapping, &m)
	if _, ok := m.LifecycleValues["queued"]; ok || m.LifecycleValues["canceled"] != universal.LifecycleEnded {
		t.Errorf("lifecycle table after dropping queued: %v", m.LifecycleValues)
	}
}

func TestEditorVersions(t *testing.T) {
	ctx := context.Background()
	stale := func() float64 {
		return counterValue(t, "pushward_relay_universal_editor_requests_total", map[string]string{"op": "save", "result": "stale"})
	}

	t.Run("double post", func(t *testing.T) {
		eh := newEditorHarness(t)
		path := eh.newRow(t, "backups", "plain_notify.json")
		form := eh.open(t, path).with("op", "save")
		// A double tap on Save: the same form twice, at once.
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i := range codes {
			wg.Go(func() { codes[i] = eh.submit(t, path, form).Code })
		}
		wg.Wait()
		if codes[0] != http.StatusSeeOther || codes[1] != http.StatusSeeOther {
			t.Fatalf("double post: %v", codes)
		}
		if w := eh.submit(t, path, form); w.Code != http.StatusSeeOther {
			t.Errorf("third post: %d", w.Code)
		}
		if row := mappingRow(t, eh.harness, "backups"); row.Rev != 1 {
			t.Errorf("rev %d after a double post, want 1", row.Rev)
		}
	})

	t.Run("stale rev", func(t *testing.T) {
		eh := newEditorHarness(t)
		path := eh.newRow(t, "backups", "plain_notify.json")
		f := eh.open(t, path)
		if w := eh.submit(t, path, f.with("op", "save", "p.title", f.optionFor(t, "p.title", "host"))); w.Code != http.StatusSeeOther {
			t.Fatalf("first save: %d", w.Code)
		}
		before := stale()
		w := eh.submit(t, path, f.with("op", "save"))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `href="`+tokenOf(path)+`"`) {
			t.Fatalf("second form from the old rev: %d %s", w.Code, w.Body.String())
		}
		if stale() != before+1 {
			t.Error("stale save not counted")
		}
	})

	t.Run("old incarnation", func(t *testing.T) {
		eh := newEditorHarness(t)
		path := eh.newRow(t, "backups", "plain_notify.json")
		form := eh.open(t, path).with("op", "save")
		// The review goes unanswered, the row is swept, and the same shape
		// is proposed again: rev 0 once more.
		eh.clock.advance(state.PendingTTL + time.Second)
		if res, _ := eh.store.Sweep(ctx); res.Pending != 1 {
			t.Fatalf("sweep = %+v", res)
		}
		eh.newRow(t, "backups", "plain_notify.json")
		if row := mappingRow(t, eh.harness, "backups"); row.Rev != 0 {
			t.Fatalf("new incarnation at rev %d", row.Rev)
		}
		if w := eh.submit(t, path, form); w.Code != http.StatusConflict {
			t.Fatalf("form from the swept row: %d %s", w.Code, w.Body.String())
		}
		if row := mappingRow(t, eh.harness, "backups"); row.Status != state.MappingPending {
			t.Errorf("the new proposal was decided by an old form: %s", row.Status)
		}
	})

	t.Run("swept", func(t *testing.T) {
		eh := newEditorHarness(t)
		path := eh.newRow(t, "backups", "plain_notify.json")
		form := eh.open(t, path).with("op", "save")
		eh.clock.advance(state.PendingTTL + time.Second)
		_, _ = eh.store.Sweep(ctx)
		if w := eh.submit(t, path, form); w.Code != http.StatusGone {
			t.Errorf("save of a swept row: %d", w.Code)
		}
		if w := eh.get(t, path); w.Code != http.StatusGone {
			t.Errorf("GET of a swept row: %d", w.Code)
		}
	})

	t.Run("confirmed cap", func(t *testing.T) {
		eh := newEditorHarness(t)
		for i := range state.MaxConfirmed {
			k := state.MappingKey{KeyHash: tenant(), Source: "bulk"}
			k.Fingerprint[0] = byte(i + 1)
			exp := eh.clock.now().Add(state.PendingTTL)
			row := &state.MappingRow{
				MappingKey: k, Mapping: json.RawMessage(`{"v":1,"k":"notification","p":{}}`),
				Shape: json.RawMessage(`{"v":1,"fields":[]}`), Proposal: json.RawMessage(`{}`), Proposer: "heuristic/1", ExpiresAt: &exp,
			}
			if ok, err := eh.store.InsertPending(ctx, row, state.MaxPending); !ok || err != nil {
				t.Fatalf("insert %d: %v %v", i, ok, err)
			}
			if res, _ := eh.store.Decide(ctx, k, state.MappingConfirmed, exp, state.MaxConfirmed); res.Outcome != state.DecideOK {
				t.Fatalf("decide %d: %+v", i, res)
			}
			eh.clock.advance(time.Second)
		}
		path := eh.newRow(t, "backups", "plain_notify.json")
		hits := counterValue(t, "pushward_relay_universal_cap_hits_total", map[string]string{"cap": "confirmed"})
		if w := eh.submit(t, path, eh.open(t, path).with("op", "save")); w.Code != http.StatusSeeOther {
			t.Fatalf("save: %d", w.Code)
		}
		lru := state.MappingKey{KeyHash: tenant(), Source: "bulk"}
		lru.Fingerprint[0] = 1
		if r, _ := eh.store.Get(ctx, lru); r != nil {
			t.Error("the least recently used confirmed mapping survived the cap")
		}
		if r := mappingRow(t, eh.harness, "backups"); r.Status != state.MappingConfirmed {
			t.Errorf("edited row status %s", r.Status)
		}
		if n, _ := eh.store.CountByStatus(ctx); n[state.MappingConfirmed] != state.MaxConfirmed {
			t.Errorf("%d confirmed rows, want %d", n[state.MappingConfirmed], state.MaxConfirmed)
		}
		if got := counterValue(t, "pushward_relay_universal_cap_hits_total", map[string]string{"cap": "confirmed"}); got != hits+1 {
			t.Errorf("cap hits %v, want %v", got, hits+1)
		}
	})
}

func TestEditorHeaders(t *testing.T) {
	eh := newEditorHarness(t)
	path := eh.newRow(t, "backups", "plain_notify.json")
	f := eh.open(t, path)

	sum := sha256.Sum256([]byte(editorCSS))
	csp := "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
		"'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
	want := map[string]string{
		"Content-Type":               "text/html; charset=utf-8",
		"Cache-Control":              "no-store",
		"Referrer-Policy":            "no-referrer",
		"X-Content-Type-Options":     "nosniff",
		"X-Frame-Options":            "DENY",
		"X-Robots-Tag":               "noindex, nofollow",
		"Cross-Origin-Opener-Policy": "same-origin",
		"Content-Security-Policy":    csp,
	}
	responses := map[string]*httptest.ResponseRecorder{
		"page":     eh.get(t, path),
		"notfound": eh.get(t, EditPath+"nope"),
		"nested":   eh.get(t, EditPath+"a/b"),
		"refused":  eh.submit(t, path, f.with("op", "save"), "Origin", "https://evil.example.com"),
		"saved":    eh.submit(t, path, f.with("op", "save")),
		"list":     eh.get(t, ListPath+mustMintAt(t, Claims{Scope: ScopeList, Expires: eh.clock.now().Add(time.Hour), KeyHash: tenant()})),
	}
	for name, w := range responses {
		for k, v := range want {
			if got := w.Header().Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", name, k, got, v)
			}
		}
	}

	// The CSP hash is of the bytes the page carries, not only of the file.
	body := eh.get(t, path).Body.String()
	_, rest, _ := strings.Cut(body, "<style>")
	css, _, _ := strings.Cut(rest, "</style>")
	emitted := sha256.Sum256([]byte(css))
	if !strings.Contains(csp, base64.StdEncoding.EncodeToString(emitted[:])) {
		t.Error("the CSP hash does not match the emitted stylesheet")
	}
	if !strings.Contains(body, `<meta name="referrer" content="no-referrer">`) {
		t.Error("the page has no referrer meta tag")
	}
}

func TestEditorRateLimits(t *testing.T) {
	eh := newEditorHarness(t)
	path := eh.newRow(t, "backups", "plain_notify.json")
	form := eh.open(t, path).with("op", "save")

	eh.e.allowIP = ratelimit.AllowIP
	limited := false
	for range 30 {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "203.0.113.77:40000"
		w := httptest.NewRecorder()
		eh.mux.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			limited = w.Header().Get("Retry-After") != "" && strings.Contains(w.Body.String(), "Too many requests")
			break
		}
	}
	if !limited {
		t.Error("30 page loads from one IP were never limited")
	}

	eh.e.allowIP = func(string) bool { return true }
	eh.e.allowKey = ratelimit.AllowKey
	limited = false
	for range 15 {
		if w := eh.submit(t, path, form); w.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("15 saves for one tenant were never limited")
	}
}

// Edit links on the list never outlive the list link they came from.
func TestEditorListLinksExpireWithList(t *testing.T) {
	eh := newEditorHarness(t)
	eh.newRow(t, "backups", "plain_notify.json")
	listExp := eh.clock.now().Add(time.Hour)
	w := eh.get(t, ListPath+mustMintAt(t, Claims{Scope: ScopeList, Expires: listExp, KeyHash: tenant()}))
	_, rest, ok := strings.Cut(w.Body.String(), `href="../edit/`)
	if w.Code != http.StatusOK || !ok {
		t.Fatalf("list: %d", w.Code)
	}
	tok, _, _ := strings.Cut(rest, `"`)
	c, err := Parse(reviewKey, tok, eh.clock.now(), ScopeEdit)
	if err != nil || !c.Expires.Equal(listExp) {
		t.Errorf("edit link expires %v (%v), want the list's %v", c.Expires, err, listExp)
	}
}

// A row this relay cannot read (a newer replica wrote it mid-rollout) is a
// 503 to retry, not a 500.
func TestEditorUnreadableRow(t *testing.T) {
	eh := newEditorHarness(t)
	k := state.MappingKey{KeyHash: tenant(), Source: "future"}
	exp := eh.clock.now().Add(state.PendingTTL)
	row := &state.MappingRow{
		MappingKey: k, Mapping: json.RawMessage(`{"v":1,"k":"notification","p":{}}`),
		Shape: json.RawMessage(`{"v":99,"fields":[]}`), Proposal: json.RawMessage(`{}`), Proposer: "heuristic/9", ExpiresAt: &exp,
	}
	if ok, err := eh.store.InsertPending(context.Background(), row, state.MaxPending); !ok || err != nil {
		t.Fatalf("insert: %v %v", ok, err)
	}
	tok := mustMintAt(t, Claims{Scope: ScopeEdit, Expires: exp, KeyHash: k.KeyHash, Fingerprint: k.Fingerprint, Source: k.Source})
	before := counterValue(t, "pushward_relay_universal_editor_requests_total", map[string]string{"op": "view", "result": "error"})
	w := eh.get(t, EditPath+tok)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "30" {
		t.Errorf("GET: %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if w := eh.submit(t, EditPath+tok, url.Values{"op": {"save"}, "rev": {"0"}, "created": {"0"}}); w.Code != http.StatusServiceUnavailable {
		t.Errorf("POST: %d", w.Code)
	}
	if got := counterValue(t, "pushward_relay_universal_editor_requests_total", map[string]string{"op": "view", "result": "error"}); got != before+1 {
		t.Errorf("error count %v, want %v", got, before+1)
	}
}

// panicStore panics on every read, with the token in the panic value.
type panicStore struct {
	state.MappingStore
	tok string
}

func (p panicStore) Get(context.Context, state.MappingKey) (*state.MappingRow, error) {
	panic("store exploded reading " + p.tok)
}

func (p panicStore) List(context.Context, [32]byte) ([]state.MappingRow, error) {
	panic("store exploded listing " + p.tok)
}

func TestEditorPanicGetsFallbackPage(t *testing.T) {
	eh := newEditorHarness(t)
	path := eh.newRow(t, "backups", "plain_notify.json")
	listTok := mustMintAt(t, Claims{Scope: ScopeList, Expires: eh.clock.now().Add(time.Hour), KeyHash: tenant()})

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	for _, p := range []string{path, ListPath + listTok} {
		eh.h.mappings = panicStore{MappingStore: eh.store, tok: tokenOf(p)}
		w := eh.get(t, p)
		if w.Code != http.StatusInternalServerError || w.Body.String() != fallbackPage ||
			w.Header().Get("Content-Security-Policy") != contentSecurityPolicy {
			t.Errorf("GET %s: %d %q", strings.ReplaceAll(p, tokenOf(p), "<tok>"), w.Code, w.Body.String())
		}
		if strings.Contains(logs.String(), tokenOf(p)) {
			t.Error("the panic log carries the token")
		}
	}
	eh.h.mappings = panicStore{MappingStore: eh.store, tok: tokenOf(path)}
	if w := eh.submit(t, path, url.Values{"op": {"save"}, "rev": {"0"}, "created": {"0"}}); w.Code != http.StatusInternalServerError {
		t.Errorf("POST: %d", w.Code)
	}
	if n := strings.Count(logs.String(), "mapping editor panicked"); n != 3 {
		t.Errorf("%d panics logged, want 3:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "[token]") || !strings.Contains(logs.String(), "/universal/edit/{token}") {
		t.Errorf("log does not name the route or mark the cut token:\n%s", logs.String())
	}
}

// allowedElements is every element the editor pages may contain.
var allowedElements = map[string]bool{
	"html": true, "head": true, "meta": true, "title": true, "style": true, "body": true, "main": true,
	"h1": true, "h2": true, "p": true, "span": true, "div": true, "ul": true, "li": true, "a": true,
	"small": true, "code": true, "form": true, "fieldset": true, "legend": true, "label": true,
	"input": true, "select": true, "optgroup": true, "option": true, "button": true,
	"table": true, "thead": true, "tbody": true, "tr": true, "th": true, "td": true,
	"details": true, "summary": true,
}

// checkMarkup walks a page with an HTML tokenizer: only allowed elements, no
// event handler attributes, no script, no javascript: URL, and one style
// element holding exactly the embedded stylesheet.
func checkMarkup(t *testing.T, name, body string) {
	t.Helper()
	z := html.NewTokenizer(strings.NewReader(body))
	styles := 0
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if !errors.Is(z.Err(), io.EOF) {
				t.Fatalf("%s: %v", name, z.Err())
			}
			break
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		tok := z.Token()
		if !allowedElements[tok.Data] {
			t.Errorf("%s: element <%s>", name, tok.Data)
		}
		for _, a := range tok.Attr {
			if strings.HasPrefix(strings.ToLower(a.Key), "on") {
				t.Errorf("%s: <%s %s=%q>", name, tok.Data, a.Key, a.Val)
			}
			switch a.Key {
			case "href", "action", "src", "formaction":
				if strings.Contains(strings.ToLower(a.Val), "javascript:") || strings.Contains(a.Val, "//") {
					t.Errorf("%s: <%s %s=%q>", name, tok.Data, a.Key, a.Val)
				}
			}
		}
		if tok.Data == "style" {
			styles++
			if z.Next() != html.TextToken || string(z.Text()) != string(editorCSS) {
				t.Errorf("%s: a style element that is not the embedded stylesheet", name)
			}
		}
	}
	if styles != 1 {
		t.Errorf("%s: %d style elements", name, styles)
	}
}

var nasty = []string{
	`<script>alert(1)</script>`,
	`"><svg onload=alert(1)>`,
	`</style><script>alert(2)</script>`,
	`' onmouseover='alert(3)`,
}

func TestEditorEscapes(t *testing.T) {
	eh := newEditorHarness(t)
	ctx := context.Background()

	// A row as a hostile sender could make one: paths, samples and table
	// keys that try to leave their context.
	fields := make([]universal.ShapeField, 0, len(nasty)+1)
	samples := map[string]string{}
	for i, p := range nasty {
		fields = append(fields, universal.ShapeField{Path: p, Type: universal.TypeString, Class: universal.ClassText, Runes: 20, Words: 2})
		samples[p] = nasty[(i+1)%len(nasty)]
	}
	fields = append(fields, universal.ShapeField{Path: "link", Type: universal.TypeString, Class: universal.ClassURL, Runes: 30, Words: 1})
	samples["link"] = "javascript:alert(document.cookie)"
	shape, _ := json.Marshal(universal.Shape{V: universal.ShapeVersion, Fields: fields})
	sampleJSON, _ := json.Marshal(samples)
	mapping, _ := json.Marshal(universal.Mapping{
		V: universal.MappingVersion, Kind: universal.KindAlert,
		Paths: map[universal.Role]string{
			universal.RoleTitle: nasty[0], universal.RoleBody: nasty[1], universal.RoleURL: "link",
			universal.RoleSeverity: nasty[2], universal.RoleLifecycle: "missing</option><script>x</script>",
		},
		SeverityValues: map[string]string{`<script>y</script>`: "critical", `"><b>`: "info"},
	})
	cands, _ := json.Marshal(map[universal.Role][]string{universal.RoleTitle: {nasty[3], nasty[0]}})
	k := state.MappingKey{KeyHash: tenant(), Source: "xss"}
	exp := eh.clock.now().Add(state.PendingTTL)
	row := &state.MappingRow{
		MappingKey: k, Mapping: mapping, Shape: shape, Proposal: json.RawMessage(`{}`),
		Samples: sampleJSON, Candidates: cands, Proposer: "heuristic/1", ExpiresAt: &exp,
	}
	if ok, err := eh.store.InsertPending(ctx, row, state.MaxPending); !ok || err != nil {
		t.Fatalf("insert: %v %v", ok, err)
	}
	// And one that came in as a webhook.
	payload := `{"</style><script>alert(4)</script>": "x\"><img src=x onerror=alert(5)>", "title": "<b>hi</b>", "url": "javascript:alert(6)"}`
	if w := eh.post(t, "/universal?source=xss2", []byte(payload)); w.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", w.Code, w.Body.String())
	}
	var webhookPath string
	for _, n := range reviews(t, eh.snapshot()) {
		webhookPath = strings.TrimPrefix(action(t, n, "edit").URL, publicURL)
	}

	tok := mustMintAt(t, Claims{Scope: ScopeEdit, Expires: eh.clock.now().Add(time.Hour), KeyHash: k.KeyHash, Fingerprint: k.Fingerprint, Source: k.Source})
	list := mustMintAt(t, Claims{Scope: ScopeList, Expires: eh.clock.now().Add(time.Hour), KeyHash: tenant()})
	pages := map[string]string{
		"stored":  eh.get(t, EditPath+tok).Body.String(),
		"webhook": eh.get(t, webhookPath).Body.String(),
		"list":    eh.get(t, ListPath+list).Body.String(),
	}
	// A failed save re-renders what was submitted.
	f := readPage(t, pages["stored"])
	form := f.with("op", "save")
	form["sv.n"] = []string{nasty[1], nasty[2], `<script>z</script>`}
	form["sv.v"] = append(form["sv.v"][:len(form["sv.v"])-3], "critical", "warning", "bogus</option>")
	w := eh.submit(t, EditPath+tok, form)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("hostile save: %d", w.Code)
	}
	pages["invalid"] = w.Body.String()

	for name, body := range pages {
		for _, s := range append(slices.Clone(nasty), "<b>", "<img", "</option><script>") {
			if strings.Contains(body, s) {
				t.Errorf("%s page carries %q unescaped", name, s)
			}
		}
		checkMarkup(t, name, body)
	}
	if !strings.Contains(pages["stored"], "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("the hostile path is not shown escaped")
	}
}

// The largest shape the store keeps, every field text a role can take and
// every sample at its cap, still makes a page a phone loads quickly, and its
// form posts well under maxFormBytes.
func TestEditorPageSizeWorstCase(t *testing.T) {
	eh := newEditorHarness(t)
	fields := make([]universal.Field, 0, universal.MaxPaths)
	for i := range universal.MaxPaths {
		fields = append(fields, universal.Field{
			Path:  fmt.Sprintf("%s.f%03d", strings.Repeat("section", 9)[:59], i),
			Value: strings.Repeat("word ", universal.MaxValueRunes/5),
			Type:  universal.TypeString,
		})
	}
	shapes := universal.ShapesOf(fields)
	m := universal.NewMapping(universal.ProposeShapes(shapes), shapes)
	r := &request{pk: state.MappingKey{KeyHash: tenant(), Source: strings.Repeat("s", 32)}, fields: fields, shapes: shapes}
	row, err := eh.h.newRow(r, m, universal.Result{Proposal: universal.ProposeShapes(shapes), By: "heuristic/1"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := eh.store.InsertPending(context.Background(), row, state.MaxPending); !ok || err != nil {
		t.Fatalf("insert: %v %v", ok, err)
	}
	c := Claims{Scope: ScopeEdit, Expires: eh.clock.now().Add(time.Hour), KeyHash: tenant(), Fingerprint: row.Fingerprint, Source: row.Source}
	path := EditPath + mustMintAt(t, c)
	w := eh.get(t, path)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d", w.Code)
	}
	t.Logf("worst-case page: %d bytes, %d shape fields", w.Body.Len(), len(readPage(t, w.Body.String()).options["p.title"]))
	if w.Body.Len() > 150_000 {
		t.Errorf("page is %d bytes, over 150 KB", w.Body.Len())
	}
	form := readPage(t, w.Body.String()).with("op", "save")
	if n := len(form.Encode()); n > maxFormBytes/2 {
		t.Errorf("the page's form posts %d bytes, too close to the %d limit", n, maxFormBytes)
	}
	for _, sel := range []string{"p.title", "p.body", "p.correlation"} {
		if n := len(readPage(t, w.Body.String()).options[sel]); n > 1+candidatesPerRole+maxOtherOptions+1 {
			t.Errorf("%s offers %d options", sel, n)
		}
	}
}
