// Package poster turns a provider's artwork URL into the activity image trio
// pushward-server accepts on the generic and steps templates: image_url,
// image_shape and image_thumbhash.
//
// The two halves are deliberately independent. image_url is a syntactic
// decision made here and rendered by the device, which fetches it itself and
// refuses private hosts - so a LAN URL is legal to send but never draws.
// image_thumbhash is fetched by the relay, which usually can reach that same
// LAN host, and is the only tier that survives an unreachable URL. For a
// self-hosted Jellyfin the thumbhash IS the image.
//
// Nothing here ever returns an error. A missing thumbhash degrades the card; it
// must not fail the webhook that carried it.
package poster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// MaxImageURLRunes mirrors the server's cap on content.image_url. It bounds the
// fetch side too: the URL is what keys the cache, so an unbounded one is
// retained memory chosen by whoever sent the webhook.
const MaxImageURLRunes = 2048

const (
	// maxThumbhashChars mirrors the server's cap on content.image_thumbhash.
	maxThumbhashChars = 64
	// fetchConcurrency bounds simultaneous artwork fetches relay-wide. A burst
	// of webhooks for one library scan must not open a socket per event, and
	// each in-flight decode may hold maxDecodeBytes against a pod limited to
	// 64Mi - which is what keeps this number small rather than merely bounded.
	fetchConcurrency = 2
	// maxRedirects caps the redirect chain. Every hop is re-checked by the
	// dialer, so this only bounds the work, not the reachable address space.
	maxRedirects = 3
	userAgent    = "pushward-relay (+https://pushward.app)"
	// maxBytes is the largest artwork response read. A bigger one is dropped
	// rather than truncated - half a JPEG is not a thumbhash.
	maxBytes = 2 << 20 // 2 MiB
	// cacheSize is how many URLs the relay-wide LRU holds.
	cacheSize = 512
	// negativeTTL is how long a URL that failed on its own merits is remembered
	// before it is tried again. It is what stops a broken URL being re-fetched
	// on every webhook while still letting a restarted media server recover.
	negativeTTL = 15 * time.Minute
	// maxQueueWait bounds how long a fill may wait for one of the
	// fetchConcurrency slots before giving up. Waiting is not evidence about
	// the URL, so a fill that ages out here settles as a timeout and is not
	// remembered as a failure.
	maxQueueWait = 30 * time.Second
)

// Fetch outcomes, as reported to Config.OnResult. The set is closed so it can
// be a metric label.
const (
	resultOK      = "ok"
	resultRefused = "refused" // the URL, or what it served, is not usable artwork
	resultTimeout = "timeout" // ran out of time; says nothing about the URL
	resultError   = "error"   // upstream failed to answer
)

// Config tunes a Resolver. The zero value is valid: every field falls back to
// its Default* constant.
type Config struct {
	// AllowPrivateHosts lets the resolver fetch from loopback, RFC 1918, CGNAT
	// and link-local addresses, and is also what permits a cleartext http
	// fetch. It is off by default because the hosted relay is multi-tenant and
	// its webhook payloads are attacker-controlled, which makes an unguarded
	// fetcher an SSRF probe of the cluster. A self-hosted relay that should
	// reach a LAN media server turns it on.
	AllowPrivateHosts bool
	FetchTimeout      time.Duration
	InlineWait        time.Duration
	// OnResult, when set, is called once per settled fetch with its outcome:
	// "ok", "refused", "timeout" or "error". It is the seam that lets the relay
	// count fetches without this package depending on a metrics library, and it
	// runs on the fill goroutine, so it must not block.
	OnResult func(result string)
}

// Resolver defaults.
const (
	DefaultFetchTimeout = 3 * time.Second
	DefaultInlineWait   = 600 * time.Millisecond
)

func (c Config) withDefaults() Config {
	if c.FetchTimeout <= 0 {
		c.FetchTimeout = DefaultFetchTimeout
	}
	if c.InlineWait <= 0 {
		c.InlineWait = DefaultInlineWait
	}
	return c
}

// Source resolves an image URL to a thumbhash.
type Source interface {
	// Thumbhash returns the padded base64 thumbhash for rawURL, or "" when one
	// is not available in time. It blocks for at most the resolver's inline
	// wait and never returns an error.
	Thumbhash(ctx context.Context, rawURL string) string
}

// Disabled is the Source used when poster resolution is turned off. Providers
// hold a Source unconditionally and never branch on whether the feature is on.
type Disabled struct{}

// Thumbhash always returns the empty string.
func (Disabled) Thumbhash(context.Context, string) string { return "" }

// Static is a Source that answers every URL with one canned hash, for tests and
// demos that need the trio on a card without a network fetch.
type Static string

// Thumbhash returns the canned hash.
func (s Static) Thumbhash(context.Context, string) string { return string(s) }

// isOff reports whether src is the off switch. Disabled is the mechanism; a nil
// Source is treated the same way rather than panicking a webhook.
func isOff(src Source) bool {
	if src == nil {
		return true
	}
	_, disabled := src.(Disabled)
	return disabled
}

// ValidImageURL reports whether raw satisfies the server's image_url rules:
// https, a host, no userinfo, at most MaxImageURLRunes.
//
// It deliberately does no IP filtering. A LAN https URL is a legal image_url -
// the device is the one that declines to load it - and dropping it here would
// also drop the thumbhash the card would otherwise still show.
func ValidImageURL(raw string) bool {
	if raw == "" || utf8.RuneCountInString(raw) > MaxImageURLRunes {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && u.Host != "" && u.User == nil
}

// Apply sets the activity image trio on c from one artwork URL. The shape is
// only written when something else was: an image_shape alone renders nothing
// and would still cost a content field on every push.
func Apply(ctx context.Context, src Source, c *pushward.Content, rawURL string, shape pushward.ImageShape) {
	ApplyFetchURL(ctx, src, c, rawURL, rawURL, shape)
}

// ApplyFetchURL is Apply for a provider whose image is better fetched from a
// different URL than the device should render - Jellyfin, where the relay asks
// for a small transcoded JPEG it can certainly decode while the device gets the
// plain item image.
//
// A disabled Source writes nothing at all. Turning the feature off has to mean
// the card carries no image fields: image_url on its own would still publish
// the media server's hostname to every device the activity reaches.
func ApplyFetchURL(ctx context.Context, src Source, c *pushward.Content, displayURL, fetchURL string, shape pushward.ImageShape) {
	if isOff(src) {
		return
	}
	// The trio is owned as a unit, so a field left over from an earlier build of
	// the same content cannot outlive the image it belonged to.
	c.ImageURL, c.ImageShape, c.ImageThumbhash = "", "", ""
	if ValidImageURL(displayURL) {
		c.ImageURL = displayURL
	}
	if fetchURL != "" {
		c.ImageThumbhash = src.Thumbhash(ctx, fetchURL)
	}
	if c.ImageURL != "" || c.ImageThumbhash != "" {
		c.ImageShape = shape
	}
}

// Resolver fetches artwork and caches the resulting thumbhashes relay-wide.
//
// A webhook must be answered promptly, so a cache miss waits only InlineWait
// for the fetch and otherwise returns "" and lets the fill land for the next
// frame. Live Activities are a stream of frames; the placeholder appearing one
// push later is invisible, a webhook that took three seconds is not.
type Resolver struct {
	cfg    Config
	client *http.Client
	sem    chan struct{}

	mu         sync.Mutex
	entries    map[string]*cacheEntry
	head, tail *cacheEntry
}

// cacheEntry is one URL's slot in the LRU. It is pending while a fill is in
// flight (done is closed when it settles), positive with a zero expires, or
// negative until expires.
type cacheEntry struct {
	key        string
	done       chan struct{}
	pending    bool
	hash       string
	expires    time.Time
	prev, next *cacheEntry
}

// NewResolver builds a Resolver. It never fails: an unusable config falls back
// to the defaults rather than refusing to start over a cosmetic feature.
func NewResolver(cfg Config) *Resolver {
	cfg = cfg.withDefaults()

	dialer := &net.Dialer{Timeout: cfg.FetchTimeout, KeepAlive: 30 * time.Second}
	if !cfg.AllowPrivateHosts {
		// The check runs on the resolved address rather than the hostname, so a
		// DNS name that answers with 127.0.0.1 (or rebinds to it between the
		// check and the dial) is refused at connect time.
		dialer.Control = blockPrivateDial
	}

	r := &Resolver{
		cfg:     cfg,
		sem:     make(chan struct{}, fetchConcurrency),
		entries: make(map[string]*cacheEntry),
	}
	r.client = &http.Client{
		Timeout: cfg.FetchTimeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   cfg.FetchTimeout,
			ResponseHeaderTimeout: cfg.FetchTimeout,
			MaxIdleConnsPerHost:   2,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("poster: stopped after %d redirects", maxRedirects)
			}
			// Every hop is held to the rules the first URL was: a redirect is
			// the obvious way to launder a scheme, or a set of credentials,
			// past a check made on the URL that arrived in the payload.
			if !r.fetchable(req.URL) {
				return fmt.Errorf("poster: refusing redirect to scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}
	return r
}

// Thumbhash implements Source.
func (r *Resolver) Thumbhash(ctx context.Context, rawURL string) string {
	// Length is checked before anything retains the URL: it becomes an LRU key,
	// so a payload carrying a megabyte of URL would otherwise buy a megabyte of
	// resident memory per distinct value, up to the size of the cache.
	if rawURL == "" || utf8.RuneCountInString(rawURL) > MaxImageURLRunes || ctx.Err() != nil {
		return ""
	}

	e, done, wait := r.lookup(ctx, rawURL)
	if !wait {
		return r.hashOf(e)
	}

	timer := time.NewTimer(r.inlineWait(ctx))
	defer timer.Stop()
	select {
	case <-done:
		return r.hashOf(e)
	case <-timer.C:
		return ""
	case <-ctx.Done():
		return ""
	}
}

func (r *Resolver) hashOf(e *cacheEntry) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return e.hash
}

// inlineWait is how long a caller may block on a pending fill: the configured
// wait, or whatever is left of the request's own deadline if that is shorter.
func (r *Resolver) inlineWait(ctx context.Context) time.Duration {
	wait := r.cfg.InlineWait
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
	}
	return max(wait, 0)
}

// lookup returns the entry for key, the channel to wait on, and whether a fill
// is in flight. A miss (or an expired negative entry) starts one; a hit on a
// pending entry joins it, so concurrent webhooks for the same artwork share a
// single fetch.
func (r *Resolver) lookup(ctx context.Context, key string) (*cacheEntry, chan struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if e, ok := r.entries[key]; ok {
		r.moveToFront(e)
		switch {
		case e.pending:
			return e, e.done, true
		case e.expires.IsZero() || time.Now().Before(e.expires):
			return e, nil, false
		}
		// Negative entry aged out: refill in place.
		e.pending = true
		e.hash = ""
		e.done = make(chan struct{})
		r.startFill(ctx, e)
		return e, e.done, true
	}

	e := &cacheEntry{key: key, pending: true, done: make(chan struct{})}
	r.entries[key] = e
	r.pushFront(e)
	r.evict()
	r.startFill(ctx, e)
	return e, e.done, true
}

// startFill launches the fetch. The caller must hold r.mu.
func (r *Resolver) startFill(ctx context.Context, e *cacheEntry) {
	// Detached from the webhook's context on purpose: answering the caller
	// (which happens as soon as InlineWait elapses) must not cancel a fetch
	// whose result the next frame will use.
	base := context.WithoutCancel(ctx)
	done := e.done
	go func() {
		hash, result := r.fill(base, e.key)
		if r.cfg.OnResult != nil {
			r.cfg.OnResult(result)
		}
		if result != resultOK {
			// Scheme and host only. The path can carry an access token, and the
			// userinfo a URL might carry is a credential.
			slog.Debug("poster fetch produced no thumbhash", "result", result, "target", targetOf(e.key))
		}

		r.mu.Lock()
		e.hash = hash
		e.pending = false
		switch {
		case hash != "":
			e.expires = time.Time{}
		case result == resultTimeout:
			// Running out of time is a statement about load, not about the URL.
			// Remembering it would let one burst of webhooks blank a whole
			// library's artwork for the negative TTL - and since the cache is
			// keyed by URL alone, a poisoned CDN URL would take every tenant
			// with it. Expired-on-arrival: the next frame tries again.
			e.expires = time.Now()
		default:
			e.expires = time.Now().Add(negativeTTL)
		}
		r.mu.Unlock()

		close(done)
	}()
}

// fill queues for a fetch slot and then runs the fetch under its own timeout.
//
// The slot is taken before the clock starts. Charging a queued fill for the
// time it spent waiting its turn is what turns one burst of webhooks into a
// cache full of failures.
func (r *Resolver) fill(ctx context.Context, rawURL string) (string, string) {
	timer := time.NewTimer(maxQueueWait)
	defer timer.Stop()
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-timer.C:
		return "", resultTimeout
	}

	fetchCtx, cancel := context.WithTimeout(ctx, r.cfg.FetchTimeout)
	defer cancel()
	return r.fetch(fetchCtx, rawURL)
}

// fetchable reports whether u may be fetched at all. https is the whole
// allowlist on the hosted relay: cleartext is only for the self-hosted LAN
// case, and userinfo is refused everywhere because Go forwards it as an
// Authorization header - which would make an attacker-controlled payload a
// credential-spray tool pointed at whatever the relay can reach.
func (r *Resolver) fetchable(u *url.URL) bool {
	if u.Host == "" || u.User != nil {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && r.cfg.AllowPrivateHosts)
}

// fetch downloads rawURL and returns its thumbhash plus the outcome class. Any
// failure at all - unreachable host, refused address, wrong content type,
// oversized body, undecodable format - yields an empty hash.
func (r *Resolver) fetch(ctx context.Context, rawURL string) (string, string) {
	u, err := url.Parse(rawURL)
	if err != nil || !r.fetchable(u) {
		return "", resultRefused
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", resultRefused
	}
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", userAgent)

	resp, err := r.client.Do(req)
	if err != nil {
		if isTimeout(err) {
			return "", resultTimeout
		}
		return "", resultError
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", resultError
	}
	if resp.ContentLength > maxBytes {
		return "", resultRefused
	}
	// One byte past the cap distinguishes "exactly at the limit" from "the
	// server lied about (or omitted) Content-Length".
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		if isTimeout(err) {
			return "", resultTimeout
		}
		return "", resultError
	}
	if int64(len(body)) > maxBytes {
		return "", resultRefused
	}
	if !strings.HasPrefix(http.DetectContentType(body), "image/") {
		return "", resultRefused
	}

	hash := imageThumbhash(body)
	if hash == "" || len(hash) > maxThumbhashChars {
		return "", resultRefused
	}
	return hash, resultOK
}

// isTimeout reports whether err is a deadline rather than a refusal. Both the
// fetch context and the client's own timeouts have to count, which is why this
// is not a bare errors.Is against context.DeadlineExceeded.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// targetOf is the scheme and host of a URL, which is all a log line may carry.
func targetOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// --- LRU bookkeeping (caller holds r.mu) ---

func (r *Resolver) pushFront(e *cacheEntry) {
	e.prev = nil
	e.next = r.head
	if r.head != nil {
		r.head.prev = e
	}
	r.head = e
	if r.tail == nil {
		r.tail = e
	}
}

func (r *Resolver) moveToFront(e *cacheEntry) {
	if r.head == e {
		return
	}
	r.unlink(e)
	r.pushFront(e)
}

func (r *Resolver) unlink(e *cacheEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else if r.head == e {
		r.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else if r.tail == e {
		r.tail = e.prev
	}
	e.prev, e.next = nil, nil
}

// evict drops least-recently-used entries until the cache is back within
// cacheSize. An evicted pending entry keeps its fill goroutine, which still
// closes its channel for whoever is waiting - the result is simply not cached.
func (r *Resolver) evict() {
	for len(r.entries) > cacheSize && r.tail != nil {
		victim := r.tail
		r.unlink(victim)
		delete(r.entries, victim.key)
	}
}

// --- SSRF guard ---

// blockedPrefixes are the special-purpose ranges an artwork fetch must never
// reach. Everything that is simply not a routable address - unspecified,
// loopback, multicast, link-local, the v4 broadcast - is refused by the
// global-unicast test in isBlockedAddr instead, so this list only has to name
// the ranges that would otherwise look like ordinary public addresses.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("10.0.0.0/8"),     // RFC 1918
	netip.MustParsePrefix("100.64.0.0/10"),  // CGNAT, and Tailscale on top of it
	netip.MustParsePrefix("172.16.0.0/12"),  // RFC 1918
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments, incl. NAT64 discovery
	netip.MustParsePrefix("192.168.0.0/16"), // RFC 1918
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, and 255.255.255.255 with it
	netip.MustParsePrefix("fc00::/7"),       // unique local
}

// IPv6 prefixes that carry an IPv4 address inside them. Each is a way to name a
// v4 host with a v6 literal, and so a way past a v4-only block list: ::7f00:1
// and 2002:7f00:1:: are both 127.0.0.1.
var (
	ipv4Compatible = netip.MustParsePrefix("::/96")        // deprecated ::a.b.c.d
	nat64          = netip.MustParsePrefix("64:ff9b::/96") // RFC 6052 well-known prefix
	sixToFour      = netip.MustParsePrefix("2002::/16")    // RFC 3056, v4 in bytes 2-5
)

// blockPrivateDial is the net.Dialer Control hook. address is the resolved
// "ip:port", which is what makes the check DNS-rebinding safe.
func blockPrivateDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("poster: unparseable dial address %q", address)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("poster: unparseable dial address %q", address)
	}
	if isBlockedAddr(ip) {
		return fmt.Errorf("poster: refusing to dial non-public address %s", ip)
	}
	return nil
}

// isBlockedAddr reports whether ip is outside the public internet. Any IPv6
// form that carries an IPv4 address is reduced to that address first, so
// ::ffff:127.0.0.1, ::7f00:1, 64:ff9b::a9fe:a9fe and 2002:7f00:1:: are all
// judged as the v4 host they reach rather than slipping past the v4 prefixes.
func isBlockedAddr(ip netip.Addr) bool {
	if !ip.IsValid() {
		return true
	}
	ip = ip.WithZone("")
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	if v4, ok := embeddedIPv4(ip); ok {
		ip = v4
	}
	if !ip.IsGlobalUnicast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// embeddedIPv4 extracts the IPv4 address an IPv6 address carries, if it carries
// one at all.
func embeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	if !ip.Is6() || ip.Is4In6() {
		return netip.Addr{}, false
	}
	b := ip.As16()
	switch {
	case ipv4Compatible.Contains(ip), nat64.Contains(ip):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case sixToFour.Contains(ip):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}
