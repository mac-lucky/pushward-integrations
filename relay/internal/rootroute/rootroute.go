// Package rootroute serves POST /, the one URL any webhook can be pointed at.
// A payload recognisably from one of the relay's providers is handed to that
// provider's route as if it had been posted there; anything else goes to the
// universal route. Every other method and path passes through untouched.
package rootroute

import (
	"bytes"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/detect"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/ratelimit"
)

// Universal is the route a payload no provider claims goes to.
const Universal = "/universal"

// Why a request went where it did, as root_dispatch_total's via label.
const (
	viaDisabled = "disabled"
	viaNone     = "none"
	viaSkipped  = "skipped"
)

// Defaults for Options.
const (
	defaultReadTimeout = 5 * time.Second
	defaultDetectors   = 8
)

// Mux is what the root route needs of an *http.ServeMux: serving, and
// telling which routes are registered.
type Mux interface {
	http.Handler
	Handler(r *http.Request) (h http.Handler, pattern string)
}

// Options tunes the root route. Zero values take the defaults.
type Options struct {
	// ReadTimeout bounds reading the body. 5 s by default.
	ReadTimeout time.Duration
	// Detectors caps how many bodies are inspected at once. 8 by default.
	Detectors int
}

// Router is the root route in front of a mux.
type Router struct {
	mux         Mux
	enabled     map[string]bool
	readTimeout time.Duration
	detectors   chan struct{}
}

// New returns the root route in front of mux. It reads which routes mux
// serves once, here, so every provider must be registered before the call.
func New(mux Mux, opts Options) *Router {
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = defaultReadTimeout
	}
	if opts.Detectors <= 0 {
		opts.Detectors = defaultDetectors
	}
	rt := &Router{
		mux:         mux,
		enabled:     map[string]bool{},
		readTimeout: opts.ReadTimeout,
		detectors:   make(chan struct{}, opts.Detectors),
	}
	for _, route := range append(detect.Routes(), Universal) {
		_, pattern := mux.Handler(&http.Request{Method: http.MethodPost, URL: &url.URL{Path: route}})
		rt.enabled[route] = pattern != ""
	}
	return rt
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/" {
		rt.mux.ServeHTTP(w, r)
		return
	}
	route, via, rule := rt.route(w, r)
	metrics.RootDispatchTotal.WithLabelValues(route, via).Inc()
	attrs := []attribute.KeyValue{
		attribute.String("pushward.root.route", route),
		attribute.String("pushward.root.via", via),
	}
	if rule != "" {
		attrs = append(attrs, attribute.String("pushward.root.rule", rule))
	}
	trace.SpanFromContext(r.Context()).SetAttributes(attrs...)
	slog.Debug("root route", "route", route, "via", via, "rule", rule)
	if route != "/" {
		// In place: the mux sets r.Pattern on this request, and the
		// wrappers outside hold the same pointer for metrics and traces.
		u := *r.URL
		u.Path, u.RawPath = route, ""
		r.URL = &u
	}
	rt.mux.ServeHTTP(w, r)
}

// fallback is the universal route when it is on. Without it the request is
// served as posted, and gets the 404 it always has.
func (rt *Router) fallback() string {
	if rt.enabled[Universal] {
		return Universal
	}
	return "/"
}

// route picks where r goes. A request that fails a check made before the
// body is read goes to the fallback unread, and the route there answers it:
// 401 without a key, 429 over the IP limit, 413 when too large.
func (rt *Router) route(w http.ResponseWriter, r *http.Request) (route, via, rule string) {
	if !rt.readable(r) {
		return rt.fallback(), viaSkipped, ""
	}
	body, err := rt.read(w, r)
	if err != nil || len(body) >= humautil.MaxWebhookBytes {
		return rt.fallback(), viaSkipped, ""
	}
	m, ok, err := rt.detect(r, body)
	if err != nil {
		return rt.fallback(), viaSkipped, ""
	}
	switch {
	case ok && rt.enabled[m.Route]:
		return m.Route, m.Via, m.Rule
	case ok:
		return rt.fallback(), viaDisabled, m.Rule
	case m.Via == detect.ViaVeto:
		return rt.fallback(), detect.ViaVeto, m.Rule
	}
	return rt.fallback(), viaNone, ""
}

// readable reports whether the body is worth reading: the request carries a
// key, says it is JSON, is not over the size limit, and its IP has a request
// left. The IP check spends nothing; the route's own limiter does that.
func (rt *Router) readable(r *http.Request) bool {
	if auth.ExtractKey(r.Header.Get("Authorization")) == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mt != "application/json" && !strings.HasSuffix(mt, "+json")) {
		return false
	}
	if r.ContentLength >= humautil.MaxWebhookBytes {
		return false
	}
	return !ratelimit.IPExhausted(ratelimit.ClientIP(r.RemoteAddr, r.Header.Get))
}

// detect runs detect.Detect under the Detectors cap. The body is already in
// memory by then; the cap bounds how many are decoded at once. A request
// whose client went away while it waited gets err.
func (rt *Router) detect(r *http.Request, body []byte) (detect.Match, bool, error) {
	select {
	case rt.detectors <- struct{}{}:
	case <-r.Context().Done():
		return detect.Match{}, false, r.Context().Err()
	}
	defer func() { <-rt.detectors }()
	m, ok := detect.Detect(r.Header, body)
	return m, ok, nil
}

// read reads up to MaxWebhookBytes of the body and puts it back, so the
// route that serves the request reads it again from the start: the bytes
// read, then the rest, or the read's error. It takes no shared slot: the
// deadline and the size cap bound a slow sender here as they do on every
// route huma serves, and one stalled body holds up no other request.
func (rt *Router) read(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	// Not every ResponseWriter supports deadlines; the read then has none.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(rt.readTimeout))

	orig := r.Body
	if orig == nil {
		orig = http.NoBody
	}
	var buf bytes.Buffer
	if r.ContentLength > 0 {
		buf.Grow(int(min(r.ContentLength, humautil.MaxWebhookBytes)) + bytes.MinRead)
	}
	_, err := buf.ReadFrom(io.LimitReader(orig, humautil.MaxWebhookBytes))
	rest := io.Reader(orig)
	if err != nil {
		rest = errReader{err}
	} else {
		// As huma does after its own read: the deadline was for this read,
		// and a deadline left on the connection would cancel the request's
		// context mid-handler once it passed.
		_ = rc.SetReadDeadline(time.Time{})
	}
	r.Body = replayed{Reader: io.MultiReader(bytes.NewReader(buf.Bytes()), rest), Closer: orig}
	return buf.Bytes(), err
}

// replayed is a request body read once already.
type replayed struct {
	io.Reader
	io.Closer
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
