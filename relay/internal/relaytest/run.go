package relaytest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// Harness is a relay API over a mock PushWard server that records every call.
// It has the auth and overrides middlewares of humautil.NewTestAPI and no rate
// limiter.
type Harness struct {
	Mux   *http.ServeMux
	API   huma.API
	Store *state.MemoryStore
	Pool  *client.Pool
	calls *[]testutil.APICall
	mu    *sync.Mutex
}

func newHarness(t *testing.T) *Harness {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServer(t)
	handler, api := humautil.NewTestAPI()
	mux, ok := handler.(*http.ServeMux)
	if !ok {
		t.Fatalf("humautil.NewTestAPI returned a %T, want *http.ServeMux", handler)
	}
	return &Harness{
		Mux:   mux,
		API:   api,
		Store: state.NewMemoryStore(),
		Pool:  client.NewPool(srv.URL, nil),
		calls: calls,
		mu:    mu,
	}
}

func (h *Harness) register(t *testing.T, reg Register) {
	t.Helper()
	if ep, ok := reg(t, h.API, h.Store, h.Pool).(lifecycle.EnderProvider); ok {
		t.Cleanup(ep.Ender().StopAll)
	}
}

// Calls waits for the recorded calls to settle and returns them.
func (h *Harness) Calls() []testutil.APICall {
	return Settle(h.calls, h.mu)
}

// Relay registers every provider once, plus the universal route, on one
// fresh harness, then runs extra against its API.
func Relay(t *testing.T, extra ...func(huma.API)) *Harness {
	t.Helper()
	h := newHarness(t)
	seen := map[string]bool{}
	for _, rt := range Routes() {
		if !seen[rt.Provider] {
			seen[rt.Provider] = true
			h.register(t, rt.Register)
		}
	}
	h.register(t, registerUniversal)
	for _, f := range extra {
		f(h.API)
	}
	return h
}

// Result is what one fixture did: the response to it and the PushWard calls
// made after its prime.
type Result struct {
	Fixture  string // "<dir>/<file>"
	Primed   string // the prime's "<dir>/<file>", if there was one
	Payload  []byte
	Status   int
	Response string
	// Pattern is the mux pattern that served the fixture's request.
	Pattern string
	Calls   []testutil.APICall
}

// RunDedicated sends the fixture file name to rt on a fresh harness with only
// rt's provider registered, after its prime.
func RunDedicated(t *testing.T, rt Route, name string) Result {
	t.Helper()
	h := newHarness(t)
	h.register(t, rt.Register)
	return h.Run(t, h.Mux, rt.Path, rt.Dir, name, nil)
}

// Run posts the fixture file name to target on handler, after its prime.
// headers, when set, adds a sender's headers to each request.
func (h *Harness) Run(t *testing.T, handler http.Handler, target, dir, name string, headers func(dir string, payload []byte) http.Header) Result {
	t.Helper()
	res := Result{Fixture: dir + "/" + filepath.Base(name)}
	send := func(body []byte) (*httptest.ResponseRecorder, *http.Request) {
		var hdr http.Header
		if headers != nil {
			hdr = headers(dir, body)
		}
		req := NewRequest(target, body, hdr)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w, req
	}

	skip := 0
	if res.Primed = Prime(res.Fixture); res.Primed != "" {
		body := ReadFixture(t, filepath.Join(Testdata, res.Primed))
		if w, _ := send(body); w.Code != http.StatusOK {
			t.Fatalf("prime %s: %d %s", res.Primed, w.Code, w.Body.String())
		}
		skip = len(h.Calls())
	}

	res.Payload = ReadFixture(t, name)
	w, req := send(res.Payload)
	res.Status = w.Code
	res.Response = w.Body.String()
	res.Pattern = req.Pattern
	res.Calls = h.Calls()[skip:]
	return res
}

// ReadFixture reads a fixture file.
func ReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name) // #nosec G304 -- fixture path under relay/testdata
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// NewRequest is a webhook POST to target carrying Key and a JSON content
// type, plus hdr. A value in hdr replaces the default.
func NewRequest(target string, body []byte, hdr http.Header) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+Key)
	for k, v := range hdr {
		req.Header[k] = v
	}
	return req
}

// Post sends body to target on h and returns the response.
func Post(h http.Handler, target string, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, NewRequest(target, body, nil))
	return w
}

// Settle waits for the delayed two-phase end: the call list has to stop
// growing for a while before it counts as complete.
func Settle(calls *[]testutil.APICall, mu *sync.Mutex) []testutil.APICall {
	const quiet = 150 * time.Millisecond
	deadline := time.Now().Add(3 * time.Second)
	last, since := -1, time.Now()
	for time.Now().Before(deadline) {
		n := len(testutil.GetCalls(calls, mu))
		if n != last {
			last, since = n, time.Now()
		} else if time.Since(since) >= quiet {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return testutil.GetCalls(calls, mu)
}

// Compact returns b as compact JSON.
func Compact(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return buf.Bytes()
}
