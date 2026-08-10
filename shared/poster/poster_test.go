package poster

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

const gradientHash = "WfcJhRqPdTeXeIhXiXiYd3BmB/eH"

// permissiveConfig is what every reachability test needs: httptest listens on
// 127.0.0.1 over plain http, which the default config refuses on both counts.
func permissiveConfig() Config {
	return Config{AllowPrivateHosts: true, InlineWait: 2 * time.Second, FetchTimeout: 5 * time.Second}
}

// imageServer serves body for every request and counts the requests it got.
func imageServer(t *testing.T, body []byte, contentType string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// --- URL rules ---

func TestValidImageURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"public https", "https://image.tmdb.org/t/p/w500/abc.jpg", true},
		{"lan https is legal to send", "https://jellyfin.lan:8096/Items/1/Images/Primary", true},
		{"https with query", "https://example.com/poster?size=large", true},
		{"empty", "", false},
		{"http", "http://example.com/poster.jpg", false},
		{"userinfo", "https://user:pass@example.com/poster.jpg", false},
		{"no host", "https:///poster.jpg", false},
		{"relative", "/poster.jpg", false},
		{"custom scheme", "jellyfin://poster.jpg", false},
		{"data uri", "data:image/png;base64,iVBORw0KGgo=", false},
		{"2048 runes", "https://example.com/" + strings.Repeat("a", 2048-20), true},
		{"2049 runes", "https://example.com/" + strings.Repeat("a", 2049-20), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidImageURL(tc.raw); got != tc.want {
				t.Errorf("ValidImageURL(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestValidImageURLRuneCap(t *testing.T) {
	// The cap is runes, not bytes: a multi-byte path well under the limit must
	// pass, where a byte-counting cap would start rejecting at half the length.
	// The rune is built rather than written literally to keep this file ASCII.
	multiByte := strings.Repeat(string(rune(0x00E9)), 1200) // e-acute, 2 bytes each
	raw := "https://example.com/" + multiByte
	if len(raw) <= MaxImageURLRunes {
		t.Fatalf("fixture is only %d bytes; it has to exceed the cap in bytes to be meaningful", len(raw))
	}
	if !ValidImageURL(raw) {
		t.Error("expected a 1220-rune URL of multi-byte characters to be valid")
	}
}

// --- Source plumbing ---

func TestDisabledSource(t *testing.T) {
	if got := (Disabled{}).Thumbhash(context.Background(), "https://example.com/a.jpg"); got != "" {
		t.Errorf("expected empty hash from Disabled, got %q", got)
	}
}

// Turning posters off has to suppress the whole trio. An image_url on its own
// still publishes the media server's hostname to every device the activity
// reaches, which is exactly what an operator setting enabled: false is asking
// not to happen.
func TestApplyDisabledWritesNothing(t *testing.T) {
	for _, src := range []Source{Disabled{}, nil} {
		var c pushward.Content
		Apply(context.Background(), src, &c, "https://image.tmdb.org/t/p/w500/x.jpg", pushward.ImageShapePoster)
		if c.ImageURL != "" || c.ImageThumbhash != "" || c.ImageShape != "" {
			t.Errorf("source %T: expected no image fields, got url=%q hash=%q shape=%q",
				src, c.ImageURL, c.ImageThumbhash, c.ImageShape)
		}
	}
}

func TestApply(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		hash      string
		wantURL   string
		wantHash  string
		wantShape pushward.ImageShape
	}{
		{
			name: "https url and hash", url: "https://cdn.example.com/a.jpg", hash: gradientHash,
			wantURL: "https://cdn.example.com/a.jpg", wantHash: gradientHash, wantShape: pushward.ImageShapePoster,
		},
		{
			name: "http url is dropped but the hash survives", url: "http://jellyfin.lan:8096/a.jpg", hash: gradientHash,
			wantURL: "", wantHash: gradientHash, wantShape: pushward.ImageShapePoster,
		},
		{
			name: "url without a hash still carries the shape", url: "https://cdn.example.com/a.jpg", hash: "",
			wantURL: "https://cdn.example.com/a.jpg", wantHash: "", wantShape: pushward.ImageShapePoster,
		},
		{
			name: "nothing resolvable leaves the shape unset", url: "http://jellyfin.lan/a.jpg", hash: "",
			wantURL: "", wantHash: "", wantShape: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var c pushward.Content
			Apply(context.Background(), Static(tc.hash), &c, tc.url, pushward.ImageShapePoster)
			if c.ImageURL != tc.wantURL {
				t.Errorf("image_url = %q, want %q", c.ImageURL, tc.wantURL)
			}
			if c.ImageThumbhash != tc.wantHash {
				t.Errorf("image_thumbhash = %q, want %q", c.ImageThumbhash, tc.wantHash)
			}
			if c.ImageShape != tc.wantShape {
				t.Errorf("image_shape = %q, want %q", c.ImageShape, tc.wantShape)
			}
		})
	}
}

// recordingSource is Static with a memory of what it was asked for.
type recordingSource struct{ urls []string }

func (s *recordingSource) Thumbhash(_ context.Context, rawURL string) string {
	s.urls = append(s.urls, rawURL)
	return gradientHash
}

func TestApplyFetchURLUsesTheFetchURL(t *testing.T) {
	src := &recordingSource{}
	var c pushward.Content
	display := "https://jellyfin.example.com/Items/7/Images/Primary"
	fetch := display + "?format=Jpg&maxWidth=256&quality=90"

	ApplyFetchURL(context.Background(), src, &c, display, fetch, pushward.ImageShapeSquare)

	if c.ImageURL != display {
		t.Errorf("image_url = %q, want the display URL %q", c.ImageURL, display)
	}
	if len(src.urls) != 1 || src.urls[0] != fetch {
		t.Errorf("resolver was asked for %v, want the fetch URL %q", src.urls, fetch)
	}
	if c.ImageShape != pushward.ImageShapeSquare {
		t.Errorf("image_shape = %q, want square", c.ImageShape)
	}
}

// --- Resolver ---

func TestResolverFetchesAndHashes(t *testing.T) {
	srv, hits := imageServer(t, readFixture(t, "gradient.png"), "image/png")
	r := NewResolver(permissiveConfig())

	got := r.Thumbhash(context.Background(), srv.URL+"/poster.png")
	if got != gradientHash {
		t.Errorf("hash = %q, want %q", got, gradientHash)
	}
	if hits.Load() != 1 {
		t.Errorf("expected 1 upstream request, got %d", hits.Load())
	}
}

func TestResolverCachesPositiveResult(t *testing.T) {
	srv, hits := imageServer(t, readFixture(t, "gradient.png"), "image/png")
	r := NewResolver(permissiveConfig())
	url := srv.URL + "/poster.png"

	first := r.Thumbhash(context.Background(), url)
	second := r.Thumbhash(context.Background(), url)

	if first != second || first == "" {
		t.Errorf("expected a stable non-empty hash, got %q then %q", first, second)
	}
	if hits.Load() != 1 {
		t.Errorf("expected the second call to be served from cache, got %d upstream requests", hits.Load())
	}
}

// A URL past the server's own image_url cap is refused before it is fetched or
// cached: the URL is the cache key, so accepting an unbounded one would let a
// webhook payload choose how much memory the relay retains.
func TestResolverRefusesOverlongURL(t *testing.T) {
	srv, hits := imageServer(t, readFixture(t, "gradient.png"), "image/png")
	r := NewResolver(permissiveConfig())
	long := srv.URL + "/poster.png?pad=" + strings.Repeat("a", MaxImageURLRunes)

	if got := r.Thumbhash(context.Background(), long); got != "" {
		t.Errorf("expected an overlong URL to be refused, got %q", got)
	}
	if hits.Load() != 0 {
		t.Errorf("expected no upstream request, got %d", hits.Load())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) != 0 {
		t.Errorf("expected nothing cached, got %d entries", len(r.entries))
	}
}

func TestResolverRejectsOversizeBody(t *testing.T) {
	srv, _ := imageServer(t, bytes.Repeat([]byte{0x41}, maxBytes+1), "image/png")
	r := NewResolver(permissiveConfig())

	if got := r.Thumbhash(context.Background(), srv.URL+"/poster.png"); got != "" {
		t.Errorf("expected an oversize body to be refused, got %q", got)
	}
}

// A server that streams without declaring a length gets past the
// Content-Length check, so the read itself has to enforce the cap.
func TestResolverRejectsOversizeChunkedBody(t *testing.T) {
	chunk := bytes.Repeat([]byte{0x41}, 256<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		for range (maxBytes / len(chunk)) + 2 {
			_, _ = w.Write(chunk)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	r := NewResolver(permissiveConfig())

	if got := r.Thumbhash(context.Background(), srv.URL+"/poster.png"); got != "" {
		t.Errorf("expected a chunked oversize body to be refused, got %q", got)
	}
}

func TestResolverRejectsNonImage(t *testing.T) {
	srv, _ := imageServer(t, []byte("<html><body>login required</body></html>"), "text/html")
	r := NewResolver(permissiveConfig())

	if got := r.Thumbhash(context.Background(), srv.URL+"/poster.png"); got != "" {
		t.Errorf("expected an HTML body to be refused, got %q", got)
	}
}

func TestResolverRejectsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	r := NewResolver(permissiveConfig())

	if got := r.Thumbhash(context.Background(), srv.URL+"/poster.png"); got != "" {
		t.Errorf("expected a 404 to be refused, got %q", got)
	}
}

func TestResolverRejectsNonHTTPScheme(t *testing.T) {
	r := NewResolver(permissiveConfig())
	for _, raw := range []string{"ftp://example.com/a.jpg", "file:///etc/passwd", "://broken", "https://"} {
		if got := r.Thumbhash(context.Background(), raw); got != "" {
			t.Errorf("Thumbhash(%q) = %q, want empty", raw, got)
		}
	}
}

// Go forwards userinfo as an Authorization header, so a payload carrying
// https://user:pass@host would spray whatever credentials it likes at whatever
// the relay can reach. It is refused whether or not private hosts are allowed.
func TestResolverRejectsUserinfo(t *testing.T) {
	srv, hits := imageServer(t, readFixture(t, "gradient.png"), "image/png")
	withCreds := strings.Replace(srv.URL, "http://", "http://user:pass@", 1) + "/poster.png"

	r := NewResolver(permissiveConfig())
	if got := r.Thumbhash(context.Background(), withCreds); got != "" {
		t.Errorf("expected a URL with userinfo to be refused, got %q", got)
	}
	if hits.Load() != 0 {
		t.Errorf("expected the request never to reach the server, got %d hits", hits.Load())
	}
}

// Cleartext is the self-hosted LAN concession. On the hosted relay it would be
// a port prober that reports back through the thumbhash, so https is the whole
// allowlist when private hosts are off.
func TestResolverRejectsCleartextWhenPrivateHostsOff(t *testing.T) {
	srv, hits := imageServer(t, readFixture(t, "gradient.png"), "image/png")
	r := NewResolver(Config{InlineWait: 2 * time.Second, FetchTimeout: 5 * time.Second})

	if got := r.Thumbhash(context.Background(), srv.URL+"/poster.png"); got != "" {
		t.Errorf("expected a cleartext fetch to be refused, got %q", got)
	}
	if hits.Load() != 0 {
		t.Errorf("expected the request never to reach the server, got %d hits", hits.Load())
	}
}

// The webhook must be answered on time. A slow upstream returns nothing inline
// and lands in the cache for the next frame instead.
func TestResolverInlineWaitTimesOutThenFillsInBackground(t *testing.T) {
	body := readFixture(t, "gradient.png")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := permissiveConfig()
	cfg.InlineWait = 5 * time.Millisecond
	r := NewResolver(cfg)
	url := srv.URL + "/poster.png"

	start := time.Now()
	if got := r.Thumbhash(context.Background(), url); got != "" {
		t.Errorf("expected an empty hash inside the inline wait, got %q", got)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("call blocked for %s, well past the 5ms inline wait", elapsed)
	}

	if got := waitForHash(t, r, url); got != gradientHash {
		t.Errorf("background fill produced %q, want %q", got, gradientHash)
	}
}

// The fill must outlive the request that started it: cancelling the webhook's
// context is not a reason to throw away a fetch already in flight.
func TestResolverFillSurvivesCallerCancellation(t *testing.T) {
	body := readFixture(t, "gradient.png")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := permissiveConfig()
	cfg.InlineWait = 5 * time.Millisecond
	r := NewResolver(cfg)
	url := srv.URL + "/poster.png"

	ctx, cancel := context.WithCancel(context.Background())
	if got := r.Thumbhash(ctx, url); got != "" {
		t.Fatalf("expected an empty hash inside the inline wait, got %q", got)
	}
	cancel()

	if got := waitForHash(t, r, url); got != gradientHash {
		t.Errorf("fill did not survive cancellation: got %q, want %q", got, gradientHash)
	}
}

func TestResolverCachesFailureThenRetriesAfterTTL(t *testing.T) {
	body := readFixture(t, "gradient.png")
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	r := NewResolver(permissiveConfig())
	url := srv.URL + "/poster.png"

	if got := r.Thumbhash(context.Background(), url); got != "" {
		t.Fatalf("expected the 500 to produce an empty hash, got %q", got)
	}
	// Inside the TTL the failure is remembered rather than re-fetched.
	if got := r.Thumbhash(context.Background(), url); got != "" {
		t.Errorf("expected the cached failure to be reused, got %q", got)
	}
	if hits.Load() != 1 {
		t.Errorf("expected 1 upstream request inside the negative TTL, got %d", hits.Load())
	}

	// Age the entry out by hand: the real TTL is 15 minutes.
	r.mu.Lock()
	r.entries[url].expires = time.Now().Add(-time.Second)
	r.mu.Unlock()

	if got := r.Thumbhash(context.Background(), url); got != gradientHash {
		t.Errorf("expected a retry after the negative TTL, got %q", got)
	}
}

// A fetch that ran out of time says nothing about the URL. Remembering it would
// let one burst of webhooks blank a library's artwork for the whole negative
// TTL, so the entry has to be retried on the next frame instead.
func TestResolverDoesNotCacheTimeouts(t *testing.T) {
	body := readFixture(t, "gradient.png")
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			time.Sleep(300 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := permissiveConfig()
	cfg.FetchTimeout = 50 * time.Millisecond
	cfg.InlineWait = 20 * time.Millisecond
	r := NewResolver(cfg)
	url := srv.URL + "/poster.png"

	if got := r.Thumbhash(context.Background(), url); got != "" {
		t.Fatalf("expected the slow first fetch to produce an empty hash, got %q", got)
	}
	if got := waitForHash(t, r, url); got != gradientHash {
		t.Errorf("expected the timed-out URL to be retried, got %q", got)
	}
}

// Concurrent webhooks for the same artwork share one fetch.
func TestResolverDedupesInFlightFetches(t *testing.T) {
	body := readFixture(t, "gradient.png")
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	r := NewResolver(permissiveConfig())
	url := srv.URL + "/poster.png"

	results := make(chan string, 8)
	for range cap(results) {
		go func() { results <- r.Thumbhash(context.Background(), url) }()
	}
	for range cap(results) {
		if got := <-results; got != gradientHash {
			t.Errorf("concurrent call returned %q, want %q", got, gradientHash)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("expected the concurrent callers to share one fetch, got %d requests", hits.Load())
	}
}

// The semaphore is what stops one library scan opening a socket per event, and
// with each in-flight decode allowed to hold tens of megabytes it is also the
// memory bound.
func TestResolverBoundsConcurrentFetches(t *testing.T) {
	body := readFixture(t, "gradient.png")
	var inFlight, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	cfg := permissiveConfig()
	cfg.InlineWait = time.Millisecond
	r := NewResolver(cfg)

	urls := make([]string, 12)
	var wg sync.WaitGroup
	for i := range urls {
		urls[i] = fmt.Sprintf("%s/poster%d.png", srv.URL, i)
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			r.Thumbhash(context.Background(), u)
		}(urls[i])
	}
	wg.Wait()
	for _, u := range urls {
		if got := waitForHash(t, r, u); got != gradientHash {
			t.Fatalf("%s: got %q", u, got)
		}
	}

	if peak.Load() > fetchConcurrency {
		t.Errorf("%d fetches were in flight at once, cap is %d", peak.Load(), fetchConcurrency)
	}
}

// A negative entry is refilled in place, which is the one path that mutates an
// entry other goroutines are already holding. Run it from many at once so -race
// has something to say about it.
func TestResolverConcurrentNegativeRefill(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	cfg := permissiveConfig()
	cfg.InlineWait = 5 * time.Millisecond
	r := NewResolver(cfg)
	url := srv.URL + "/poster.png"

	for range 5 {
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if got := r.Thumbhash(context.Background(), url); got != "" {
					t.Errorf("expected an empty hash from a failing URL, got %q", got)
				}
			}()
		}
		wg.Wait()
		r.mu.Lock()
		if e, ok := r.entries[url]; ok {
			e.expires = time.Now().Add(-time.Second)
		}
		r.mu.Unlock()
	}
}

// --- LRU ---

// Eviction is by least-recently-used, not by insertion order: touching an entry
// has to save it from the next eviction, or a hot poster is thrown away while a
// one-off library scan fills the cache around it. The list is driven directly
// here rather than through cacheSize real fetches.
func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	r := NewResolver(Config{})
	key := func(i int) string { return fmt.Sprintf("https://cdn.example.com/%d.png", i) }

	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range cacheSize {
		e := &cacheEntry{key: key(i), hash: gradientHash}
		r.entries[e.key] = e
		r.pushFront(e)
	}
	// The oldest entry is used again, so the second-oldest becomes the victim.
	r.moveToFront(r.entries[key(0)])

	newest := &cacheEntry{key: "https://cdn.example.com/new.png", hash: gradientHash}
	r.entries[newest.key] = newest
	r.pushFront(newest)
	r.evict()

	if len(r.entries) != cacheSize {
		t.Errorf("cache holds %d entries, cap is %d", len(r.entries), cacheSize)
	}
	if _, ok := r.entries[key(0)]; !ok {
		t.Error("the recently used entry was evicted")
	}
	if _, ok := r.entries[key(1)]; ok {
		t.Error("the least recently used entry survived")
	}
	if _, ok := r.entries[newest.key]; !ok {
		t.Error("the newest entry was evicted")
	}
}

// --- SSRF guard ---

func TestResolverBlocksPrivateHostsByDefault(t *testing.T) {
	body := readFixture(t, "gradient.png")
	var hits atomic.Int64
	// TLS, because with private hosts off nothing but https is fetched at all,
	// and the subject here is the dial guard rather than the scheme check.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	r := NewResolver(Config{InlineWait: 2 * time.Second, FetchTimeout: 5 * time.Second})
	if got := r.Thumbhash(context.Background(), srv.URL+"/poster.png"); got != "" {
		t.Errorf("expected a loopback fetch to be refused, got %q", got)
	}
	if hits.Load() != 0 {
		t.Errorf("expected the request never to reach the server, got %d hits", hits.Load())
	}

	// Control: the same URL with the guard off and the test server's own CA
	// trusted does fetch, which is what proves the guard - and not certificate
	// verification - refused it above.
	allowed := NewResolver(permissiveConfig())
	allowed.client.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	if got := allowed.Thumbhash(context.Background(), srv.URL+"/poster.png"); got != gradientHash {
		t.Errorf("with the guard off: got %q, want %q", got, gradientHash)
	}
}

func TestResolverAllowsPrivateHostsWhenConfigured(t *testing.T) {
	srv, hits := imageServer(t, readFixture(t, "gradient.png"), "image/png")
	r := NewResolver(permissiveConfig())

	if got := r.Thumbhash(context.Background(), srv.URL+"/poster.png"); got != gradientHash {
		t.Errorf("expected the loopback fetch to succeed, got %q", got)
	}
	if hits.Load() != 1 {
		t.Errorf("expected 1 request, got %d", hits.Load())
	}
}

// The guard runs per dial, not per URL, so a public host that redirects to a
// private one is refused on the second hop. This is the bypass a URL-time check
// would miss entirely.
func TestResolverRefusesRedirectToPrivateHost(t *testing.T) {
	body := readFixture(t, "gradient.png")
	var hits atomic.Int64
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(private.Close)

	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, private.URL+"/poster.png", http.StatusFound)
	}))
	t.Cleanup(public.Close)

	// Both servers listen on loopback, so the first hop is waved through by
	// hand to model a genuinely public host; every later address goes to the
	// real guard.
	firstHop := strings.TrimPrefix(public.URL, "http://")
	var dialMu sync.Mutex
	var dials []string
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	dialer.Control = func(network, address string, c syscall.RawConn) error {
		dialMu.Lock()
		dials = append(dials, address)
		dialMu.Unlock()
		if address == firstHop {
			return nil
		}
		return blockPrivateDial(network, address, c)
	}
	r := NewResolver(permissiveConfig())
	r.client.Transport.(*http.Transport).DialContext = dialer.DialContext

	if got := r.Thumbhash(context.Background(), public.URL+"/start"); got != "" {
		t.Errorf("expected the redirect to a private host to be refused, got %q", got)
	}
	if hits.Load() != 0 {
		t.Errorf("expected the private host never to be reached, got %d hits", hits.Load())
	}
	// Two dials: the hop that was allowed, and the redirect target that was
	// refused. One dial would mean the redirect was never followed and this
	// test proved nothing about the second hop.
	dialMu.Lock()
	defer dialMu.Unlock()
	if len(dials) != 2 {
		t.Errorf("dial attempts = %v, want the hop and the redirect target", dials)
	}
}

func TestIsBlockedAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"1.1.1.1", false},
		{"93.184.216.34", false},
		{"2606:2800:220:1:248:1893:25c8:1946", false},
		{"127.0.0.1", true},
		{"127.1.2.3", true},
		{"10.0.0.5", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"172.32.0.1", false}, // just outside 172.16/12
		{"192.168.1.10", true},
		{"100.64.0.1", true},   // CGNAT / Tailscale
		{"100.128.0.1", false}, // just outside 100.64/10
		{"169.254.169.254", true},
		{"0.0.0.0", true},
		{"0.1.2.3", true},
		{"224.0.0.1", true}, // multicast
		{"192.0.0.170", true},
		{"198.18.0.1", true},
		{"240.0.0.1", true},
		{"255.255.255.255", true},
		{"::1", true},
		{"fc00::1", true},
		{"fd12:3456::1", true},
		{"fe80::1", true},
		{"ff02::1", true},
		{"::ffff:127.0.0.1", true}, // IPv4-mapped loopback
		{"::ffff:10.0.0.1", true},
		{"::ffff:a9fe:a9fe", true}, // IPv4-mapped, written in hex
		{"::", true},
		// Every remaining way to name a v4 host with a v6 literal.
		{"::7f00:1", true},           // IPv4-compatible 127.0.0.1
		{"::127.0.0.1", true},        // the same address, dotted
		{"::a9fe:a9fe", true},        // IPv4-compatible 169.254.169.254
		{"64:ff9b::a9fe:a9fe", true}, // NAT64 to the metadata address
		{"64:ff9b::7f00:1", true},    // NAT64 to loopback
		{"2002:7f00:1::", true},      // 6to4 encoding of 127.0.0.1
		{"2002:c0a8:1::1", true},     // 6to4 encoding of 192.168.0.1
		{"2002:101:101::", false},    // 6to4 encoding of a public address
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			ip, err := netip.ParseAddr(tc.addr)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.addr, err)
			}
			if got := isBlockedAddr(ip); got != tc.want {
				t.Errorf("isBlockedAddr(%s) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
	if !isBlockedAddr(netip.Addr{}) {
		t.Error("expected the zero Addr to be blocked")
	}
}

func TestBlockPrivateDial(t *testing.T) {
	if err := blockPrivateDial("tcp", "127.0.0.1:8096", nil); err == nil {
		t.Error("expected loopback to be refused")
	}
	if err := blockPrivateDial("tcp", "1.1.1.1:443", nil); err != nil {
		t.Errorf("expected a public address to be allowed, got %v", err)
	}
	if err := blockPrivateDial("tcp", "not-an-address", nil); err == nil {
		t.Error("expected an unparseable address to be refused")
	}
}

// --- config defaults ---

func TestConfigDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	if got.FetchTimeout != DefaultFetchTimeout || got.InlineWait != DefaultInlineWait {
		t.Errorf("zero Config did not fall back to the defaults: %+v", got)
	}
	if got.AllowPrivateHosts {
		t.Error("private hosts must stay off unless asked for")
	}
}

// waitForHash polls until the background fill lands, so the test does not race
// a fixed sleep against a loaded machine.
func waitForHash(t *testing.T, r *Resolver, url string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got := r.Thumbhash(context.Background(), url); got != "" {
			return got
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(5 * time.Millisecond)
	}
}
