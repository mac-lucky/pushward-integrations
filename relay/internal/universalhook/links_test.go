package universalhook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func TestLinks(t *testing.T) {
	eh := newEditorHarness(t)
	eh.newRow(t, "backups", "plain_notify.json")
	before := len(eh.snapshot())

	w := eh.post(t, LinksPath, nil)
	if w.Code != http.StatusAccepted || strings.TrimSpace(w.Body.String()) != `{"status":"sent"}` {
		t.Fatalf("links: %d %s", w.Code, w.Body.String())
	}
	ns := notifications(t, eh.snapshot()[before:])
	if len(ns) != 1 {
		t.Fatalf("%d notifications, want 1", len(ns))
	}
	n := ns[0]
	if n.Title != "Webhook mappings" || len(n.Actions) != 1 {
		t.Fatalf("notification %q with %d actions", n.Title, len(n.Actions))
	}
	a := n.Actions[0]
	if a.Title != "Open editor" || !a.Foreground || a.Method != "" || !strings.HasPrefix(a.URL, publicURL+ListPath) {
		t.Errorf("action = %+v", a)
	}
	c, err := Parse(reviewKey, strings.TrimPrefix(a.URL, publicURL+ListPath), eh.clock.now(), ScopeList)
	if err != nil || c.KeyHash != tenant() || !c.Expires.Equal(eh.clock.now().Add(ListTokenTTL)) {
		t.Errorf("list token: %+v, %v", c, err)
	}
	if b, _ := json.Marshal(n); len(b) > 1024 {
		t.Errorf("the push is %d bytes", len(b))
	}

	// The list opens with a working edit link for the tenant's mapping.
	w = eh.get(t, strings.TrimPrefix(a.URL, publicURL))
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	var href string
	for _, s := range strings.Split(w.Body.String(), `href="`)[1:] {
		href, _, _ = strings.Cut(s, `"`)
	}
	ref, err := url.Parse(href)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := url.Parse(strings.TrimPrefix(a.URL, publicURL))
	edit := list.ResolveReference(ref).Path
	if !strings.HasPrefix(edit, EditPath) {
		t.Fatalf("list link %q", href)
	}
	if w := eh.get(t, edit); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "backups") {
		t.Errorf("edit link from the list: %d", w.Code)
	}
	ec, _ := Parse(reviewKey, tokenOf(edit), eh.clock.now(), ScopeEdit)
	if !ec.Expires.Equal(eh.clock.now().Add(ListEditTokenTTL)) {
		t.Errorf("list edit link expires %v", ec.Expires)
	}

	// One a minute.
	w = eh.post(t, LinksPath, nil)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Errorf("second call: %d", w.Code)
	}
	eh.clock.advance(time.Minute)
	if w := eh.post(t, LinksPath, nil); w.Code != http.StatusAccepted {
		t.Errorf("a minute later: %d", w.Code)
	}
	if n := len(notifications(t, eh.snapshot()[before:])); n != 2 {
		t.Errorf("%d notifications after the refused call, want 2", n)
	}

	// Another tenant has its own limit, and no key is no link.
	req := httptest.NewRequest(http.MethodPost, LinksPath, nil)
	req.Header.Set("Authorization", "Bearer hlk_other_tenant")
	rec := httptest.NewRecorder()
	eh.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Errorf("another tenant: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	eh.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, LinksPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: %d", rec.Code)
	}
}

// A send that fails gives the minute back: the tenant can ask again at once.
func TestLinksFailedSendKeepsQuota(t *testing.T) {
	var mu sync.Mutex
	var calls []testutil.APICall
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, testutil.APICall{Method: r.Method, Path: r.URL.Path})
		if fail {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	t.Cleanup(srv.Close)
	hs := newHarnessAt(t, srv.URL, &calls, &mu, state.NewMemoryStore(), state.NewMemoryMappingStore())

	if w := hs.post(t, LinksPath, nil); w.Code == http.StatusAccepted || w.Code == http.StatusTooManyRequests {
		t.Fatalf("failed send: %d", w.Code)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if w := hs.post(t, LinksPath, nil); w.Code != http.StatusAccepted {
		t.Fatalf("retry after a failed send: %d %s", w.Code, w.Body.String())
	}
	if w := hs.post(t, LinksPath, nil); w.Code != http.StatusTooManyRequests {
		t.Errorf("third call: %d", w.Code)
	}
	if hs.h.links.Len() != 1 || hs.h.SweepLinkLimiters() != 0 {
		t.Error("a limiter in use was swept")
	}
}
