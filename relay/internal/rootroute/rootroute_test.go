package rootroute

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/ratelimit"
	"github.com/mac-lucky/pushward-integrations/relay/internal/relaytest"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// tripwire is a body that fails the test if anything reads it.
type tripwire struct{ t *testing.T }

func (tw tripwire) Read([]byte) (int, error) {
	tw.t.Error("the body was read")
	return 0, io.EOF
}

// seen is what a stub route got. in is the body as handed over, before the
// stub reads it: the request's own when the router left it unread.
type seen struct {
	path, query, pattern string
	in                   io.ReadCloser
	body                 []byte
	err                  error
}

// stubMux serves routes with handlers that record what reached them and
// answer 200. Only the routes named are registered.
func stubMux(routes ...string) (*http.ServeMux, *seen) {
	s := &seen{}
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.HandleFunc("POST "+route, func(w http.ResponseWriter, r *http.Request) {
			s.path, s.query, s.pattern, s.in = r.URL.Path, r.URL.RawQuery, r.Pattern, r.Body
			s.body, s.err = io.ReadAll(r.Body)
			if s.err != nil {
				http.Error(w, s.err.Error(), http.StatusRequestTimeout)
			}
		})
	}
	return mux, s
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	return relaytest.ReadFixture(t, filepath.Join(relaytest.Testdata, name))
}

func dispatched(route, via string) float64 {
	return promtest.ToFloat64(metrics.RootDispatchTotal.WithLabelValues(route, via))
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestOtherMethodsOnRootAre404(t *testing.T) {
	h := relaytest.Relay(t)
	root := New(h.Mux, Options{})
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequest(method, "/", nil)
		req.Header.Set("Authorization", "Bearer "+relaytest.Key)
		if w := serve(root, req); w.Code != http.StatusNotFound {
			t.Errorf("%s /: %d, want 404", method, w.Code)
		}
	}
}

func TestOtherPathsPassThrough(t *testing.T) {
	var got *http.Request
	mux := http.NewServeMux()
	mux.HandleFunc("POST /x", func(_ http.ResponseWriter, r *http.Request) { got = r })
	req := httptest.NewRequest(http.MethodPost, "/x?a=1", tripwire{t})
	req.Header.Set("Authorization", "Bearer "+relaytest.Key)
	req.Header.Set("Content-Type", "application/json")
	body := req.Body
	serve(New(mux, Options{}), req)
	if got != req || got.Body != body || got.URL.Path != "/x" || got.URL.RawQuery != "a=1" {
		t.Error("POST /x did not reach its route as sent")
	}
}

func TestNoKeyIsUnauthorizedUnread(t *testing.T) {
	h := relaytest.Relay(t)
	root := New(h.Mux, Options{})
	before := dispatched(Universal, viaSkipped)
	req := httptest.NewRequest(http.MethodPost, "/", tripwire{t})
	req.Header.Set("Content-Type", "application/json")
	w := serve(root, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401: %s", w.Code, w.Body.String())
	}
	if req.Pattern != "POST "+Universal {
		t.Errorf("pattern %q, want POST %s", req.Pattern, Universal)
	}
	if got := dispatched(Universal, viaSkipped) - before; got != 1 {
		t.Errorf("root_dispatch_total{route=/universal,via=skipped} grew by %v, want 1", got)
	}
}

func TestNotJSONIsNotRead(t *testing.T) {
	mux, s := stubMux(Universal)
	root := New(mux, Options{})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("payload=%7B%7D"))
	req.Header.Set("Authorization", "Bearer "+relaytest.Key)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	body := req.Body
	serve(root, req)
	if s.path != Universal || s.in != body {
		t.Errorf("reached %q with the body replaced: %v; want %s, unread", s.path, s.in != body, Universal)
	}
}

func TestJSONMediaTypes(t *testing.T) {
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON", "application/vnd.grafana+json"} {
		mux, s := stubMux("/grafana", Universal)
		req := relaytest.NewRequest("/", fixture(t, "grafana/firing_single.json"), http.Header{"Content-Type": {ct}})
		serve(New(mux, Options{}), req)
		if s.path != "/grafana" {
			t.Errorf("Content-Type %q reached %q, want /grafana", ct, s.path)
		}
	}
}

func TestOversizedBodyIs413(t *testing.T) {
	h := relaytest.Relay(t)
	root := New(h.Mux, Options{})
	big := append([]byte(`{"pad":"`), bytes.Repeat([]byte("x"), humautil.MaxWebhookBytes)...)
	big = append(big, `"}`...)

	for _, tt := range []struct {
		name   string
		length int64
	}{
		{"declared length", int64(len(big))},
		{"unknown length", -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := relaytest.NewRequest("/", big, nil)
			req.ContentLength = tt.length
			w := serve(root, req)
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Errorf("status %d, want 413", w.Code)
			}
			if req.Pattern != "POST "+Universal {
				t.Errorf("pattern %q, want POST %s", req.Pattern, Universal)
			}
		})
	}

	// Exactly the limit is too large too, as huma counts it.
	exact := append([]byte(`{"pad":"`), bytes.Repeat([]byte("x"), humautil.MaxWebhookBytes-10)...)
	exact = append(exact, `"}`...)
	req := relaytest.NewRequest("/", exact, nil)
	req.ContentLength = -1
	if w := serve(root, req); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body of exactly %d bytes: status %d, want 413", len(exact), w.Code)
	}
}

func TestDisabledProviderGoesToUniversal(t *testing.T) {
	srv, _, _ := testutil.MockPushWardServer(t)
	handler, api := humautil.NewTestAPI()
	mux := handler.(*http.ServeMux)
	u := relaytest.Universal("", "").Register(t, api, state.NewMemoryStore(), client.NewPool(srv.URL, nil))
	t.Cleanup(u.(lifecycle.EnderProvider).Ender().StopAll)

	before := dispatched(Universal, viaDisabled)
	req := relaytest.NewRequest("/", fixture(t, "grafana/firing_single.json"), nil)
	w := serve(New(mux, Options{}), req)
	if w.Code != http.StatusOK || req.Pattern != "POST "+Universal {
		t.Errorf("status %d pattern %q, want 200 from POST %s: %s", w.Code, req.Pattern, Universal, w.Body.String())
	}
	if got := dispatched(Universal, viaDisabled) - before; got != 1 {
		t.Errorf("root_dispatch_total{via=disabled} grew by %v, want 1", got)
	}
}

func TestNoUniversalServesRootAs404(t *testing.T) {
	mux, _ := stubMux("/grafana")
	root := New(mux, Options{})
	before := dispatched("/", viaNone)
	if w := serve(root, relaytest.NewRequest("/", []byte(`{"title":"x"}`), nil)); w.Code != http.StatusNotFound {
		t.Errorf("undetected payload: %d, want 404", w.Code)
	}
	if got := dispatched("/", viaNone) - before; got != 1 {
		t.Errorf("root_dispatch_total{route=/,via=none} grew by %v, want 1", got)
	}
	// A detected one still reaches its provider.
	req := relaytest.NewRequest("/", fixture(t, "grafana/firing_single.json"), nil)
	if w := serve(root, req); w.Code != http.StatusOK || req.Pattern != "POST /grafana" {
		t.Errorf("grafana payload: %d via %q", w.Code, req.Pattern)
	}
}

func TestVetoGoesToUniversal(t *testing.T) {
	mux, s := stubMux("/gitea", Universal)
	before := dispatched(Universal, "veto")
	req := relaytest.NewRequest("/", fixture(t, "gitea/run_requested.json"), http.Header{"X-Github-Event": {"workflow_run"}})
	serve(New(mux, Options{}), req)
	if s.path != Universal {
		t.Errorf("reached %q, want %s", s.path, Universal)
	}
	if got := dispatched(Universal, "veto") - before; got != 1 {
		t.Errorf("root_dispatch_total{via=veto} grew by %v, want 1", got)
	}
}

func TestQueryIsKept(t *testing.T) {
	for _, tt := range []struct {
		name, fixture, route string
	}{
		{"universal", "universal/plain_notify.json", Universal},
		{"provider", "grafana/firing_single.json", "/grafana"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mux, s := stubMux("/grafana", Universal)
			serve(New(mux, Options{}), relaytest.NewRequest("/?source=x&channels=notification", fixture(t, tt.fixture), nil))
			if s.path != tt.route || s.query != "source=x&channels=notification" {
				t.Errorf("reached %s?%s, want %s?source=x&channels=notification", s.path, s.query, tt.route)
			}
		})
	}
}

// TestUptimeKumaTestNotification posts Uptime Kuma's Test button payload to
// /: it is detected, and the handler answers with its self-test card.
func TestUptimeKumaTestNotification(t *testing.T) {
	h := relaytest.Relay(t)
	req := relaytest.NewRequest("/", []byte(`{"heartbeat":null,"monitor":null,"msg":"Uptime Kuma Test"}`),
		http.Header{"User-Agent": {"axios/1.7.7"}})
	w := serve(New(h.Mux, Options{}), req)
	if w.Code != http.StatusOK || req.Pattern != "POST /uptimekuma" {
		t.Fatalf("status %d via %q, want 200 from POST /uptimekuma: %s", w.Code, req.Pattern, w.Body.String())
	}
	calls := h.Calls()
	if len(calls) == 0 || calls[0].Path != "/activities" || !bytes.Contains(calls[0].Body, []byte(`"relay-test-uptimekuma"`)) {
		t.Errorf("calls %v, want the uptimekuma self-test card", relaytest.Calls(calls))
	}
}

// TestProxmoxSystemTypeIsNotDetected: "system" is a self-test alias of the
// relay's Proxmox handler, not a type Proxmox sends, so a payload carrying it
// is somebody else's and belongs on the universal route.
func TestProxmoxSystemTypeIsNotDetected(t *testing.T) {
	mux, s := stubMux("/proxmox", Universal)
	body := []byte(`{"type":"system","title":"Disk failing","message":"SMART error on /dev/sda","severity":"error","hostname":"nas"}`)
	serve(New(mux, Options{}), relaytest.NewRequest("/", body, nil))
	if s.path != Universal {
		t.Errorf("reached %q, want %s", s.path, Universal)
	}
}

// TestPatternVisibleOutside checks the rewrite happens on the request the
// wrappers outside hold, which is what metrics and tracing read.
func TestPatternVisibleOutside(t *testing.T) {
	h := relaytest.Relay(t)
	root := New(h.Mux, Options{})
	var pattern, path string
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		root.ServeHTTP(w, r)
		pattern, path = r.Pattern, r.URL.Path
	})
	req := relaytest.NewRequest("/", fixture(t, "gatus/triggered.json"), nil)
	if w := serve(outer, req); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if pattern != "POST /gatus" || path != "/gatus" {
		t.Errorf("outer wrapper saw pattern %q path %q, want POST /gatus and /gatus", pattern, path)
	}
}

func TestReplayedBytesAreIdentical(t *testing.T) {
	bodies := map[string][]byte{
		"detected":       fixture(t, "backrest/snapshot_start.json"),
		"undetected":     []byte(" \n{\"title\": \"x\",\t\"message\": \"y\"}\n\n"),
		"not json":       []byte("{\"title\": "),
		"over the limit": bytes.Repeat([]byte("a"), humautil.MaxWebhookBytes+4096),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			mux, s := stubMux("/backrest", Universal)
			req := relaytest.NewRequest("/", body, nil)
			req.ContentLength = -1
			serve(New(mux, Options{}), req)
			if s.err != nil || !bytes.Equal(s.body, body) {
				t.Errorf("route read %d bytes (err %v), want the %d sent", len(s.body), s.err, len(body))
			}
		})
	}
}

// failingBody returns data, then err.
type failingBody struct {
	data []byte
	err  error
}

func (f *failingBody) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func TestReadErrorIsReplayed(t *testing.T) {
	mux, s := stubMux(Universal, "/grafana")
	boom := errors.New("connection reset")
	req := relaytest.NewRequest("/", nil, nil)
	req.Body = io.NopCloser(&failingBody{data: []byte(`{"alerts":[],"groupKey":"{}","version":"1"`), err: boom})
	req.ContentLength = -1
	serve(New(mux, Options{}), req)
	if s.path != Universal {
		t.Errorf("reached %q, want %s: an unread body is not detected", s.path, Universal)
	}
	if !errors.Is(s.err, boom) || string(s.body) != `{"alerts":[],"groupKey":"{}","version":"1"` {
		t.Errorf("route read %q, %v; want the bytes before the error, then the error", s.body, s.err)
	}
}

// TestReadDeadline stalls a body on a real connection: the router gives up
// after ReadTimeout and the route sees the timeout.
func TestReadDeadline(t *testing.T) {
	mux, s := stubMux(Universal, "/grafana")
	srv := httptest.NewServer(New(mux, Options{ReadTimeout: 100 * time.Millisecond}))
	t.Cleanup(srv.Close)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	partial := `{"alerts":[],"groupKey":"{}",`
	_, err = fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: relay\r\nAuthorization: Bearer %s\r\n"+
		"Content-Type: application/json\r\nContent-Length: 200\r\n\r\n%s", relaytest.Key, partial)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestTimeout || s.path != Universal || string(s.body) != partial {
		t.Errorf("status %d at %q, route read %q; want 408 at %s after %q", resp.StatusCode, s.path, s.body, Universal, partial)
	}
	var ne net.Error
	if !errors.As(s.err, &ne) || !ne.Timeout() {
		t.Errorf("route saw %v, want a timeout", s.err)
	}
}

// ipSeq gives each run of TestIPCheckSpendsNothing an IP of its own, since
// the limiter it drains outlives the test under -count.
var ipSeq atomic.Int32

// TestIPCheckSpendsNothing posts through the root route to a stub that
// spends no tokens: the IP keeps its whole burst for the route's own limiter.
func TestIPCheckSpendsNothing(t *testing.T) {
	mux, s := stubMux("/grafana", Universal)
	root := New(mux, Options{})
	ip := fmt.Sprintf("198.51.100.%d", ipSeq.Add(1))
	// post returns the body it sent, as it was before the router saw it.
	post := func(body io.Reader) io.ReadCloser {
		req := relaytest.NewRequest("/", nil, nil)
		req.Body = io.NopCloser(body)
		req.RemoteAddr = ip + ":40000"
		sent := req.Body
		serve(root, req)
		return sent
	}
	for range 50 {
		post(bytes.NewReader(fixture(t, "grafana/firing_single.json")))
	}
	for i := range 20 {
		if !ratelimit.AllowIP(ip) {
			t.Fatalf("request %d over the limit: the root route spent tokens", i+1)
		}
	}
	// Now exhausted: the body goes on unread, for the route's limiter to 429.
	s.path = ""
	sent := post(bytes.NewReader(fixture(t, "grafana/firing_single.json")))
	if s.path != Universal || s.in != sent {
		t.Errorf("an exhausted IP reached %q, body replaced %v; want %s, unread", s.path, s.in != sent, Universal)
	}
}

// TestStalledBodiesDoNotBlock stalls more bodies than there are detector
// slots on real connections, then sends a normal webhook: it must be answered
// right away, not after the stalled reads time out.
func TestStalledBodiesDoNotBlock(t *testing.T) {
	const readTimeout = 3 * time.Second
	mux := http.NewServeMux()
	mux.HandleFunc("POST /grafana", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("POST "+Universal, func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestTimeout)
		}
	})
	srv := httptest.NewServer(New(mux, Options{ReadTimeout: readTimeout}))
	t.Cleanup(srv.Close)

	for range defaultDetectors + 1 {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		// Registered after srv.Close, so it runs first: the stalled
		// handlers see their connections go and finish.
		t.Cleanup(func() { _ = conn.Close() })
		_, err = fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: relay\r\nAuthorization: Bearer %s\r\n"+
			"Content-Type: application/json\r\nContent-Length: 200\r\n\r\n{\"alerts\":", relaytest.Key)
		if err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)

	req := relaytest.NewRequest(srv.URL+"/", fixture(t, "grafana/firing_single.json"), nil)
	req.RequestURI = ""
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if took := time.Since(start); resp.StatusCode != http.StatusOK || took > time.Second {
		t.Errorf("status %d after %v behind %d stalled bodies; want 200 well inside the %v read timeout",
			resp.StatusCode, took, defaultDetectors+1, readTimeout)
	}
}

// TestDocument checks POST / is in the OpenAPI document and nowhere else:
// registering it on the mux would shadow the Router.
func TestDocument(t *testing.T) {
	h := relaytest.Relay(t, Document)
	item := h.API.OpenAPI().Paths["/"]
	if item == nil || item.Post == nil || item.Post.OperationID != "post-root-webhook" {
		t.Fatalf("POST / is not documented: %+v", item)
	}
	if _, pattern := h.Mux.Handler(httptest.NewRequest(http.MethodPost, "/", nil)); pattern != "" {
		t.Errorf("POST / is registered on the mux as %q", pattern)
	}
	resp := item.Post.Responses["200"].Content["application/json"].Schema
	if resp == nil || resp.Ref == "" {
		t.Errorf("the 200 response has no schema ref: %+v", resp)
	}
}
