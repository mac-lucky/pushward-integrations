package client

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/lrumap"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

const maxClients = 1000

// retryBudget caps how long one outbound call may sleep between retries. The
// relay makes these calls from a webhook handler, so a parked retry holds an
// inbound connection and a goroutine. pushward-server rate-limits per IP and
// the whole relay egresses from one IP, so a throttling event hits every tenant
// at once; without a budget each of them would sleep up to maxRetryAfter on
// every remaining attempt.
//
// Sized against the server's 30s WriteTimeout: 20s of sleep plus one in-flight
// request (the client's 10s timeout) lands at the point where the response
// could no longer be written anyway. Normal 5xx backoff tops out around 15s, so
// this only bites the pathological large-Retry-After case.
const retryBudget = 20 * time.Second

// maxIdleConnsPerHost is how many idle connections to PushWard the transport
// keeps. Every tenant client talks to that one host, and http.DefaultTransport
// keeps 2, so a burst of webhooks closed most of its connections afterwards
// and the next burst paid for new TLS handshakes.
const maxIdleConnsPerHost = 64

// NewTransport returns http.DefaultTransport's settings (dial, TLS handshake
// and idle timeouts included) with room for maxIdleConnsPerHost idle
// connections to the PushWard host.
func NewTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = maxIdleConnsPerHost
	return t
}

// Pool manages a pool of pushward.Client instances keyed by hlk_ key hash.
// All clients share the same base URL and HTTP client but use different API
// keys.
type Pool struct {
	baseURL    string
	httpClient *http.Client
	opts       []pushward.ClientOption
	clients    *lrumap.Map[*pooled]

	// Set by WithBreakers. A nil server means the clients carry no breaker.
	server    *serverBreaker
	threshold int
	cooldown  time.Duration
}

// pooled is one tenant's client and the breaker it carries, if any. Both go
// when the LRU evicts the entry.
type pooled struct {
	cl      *pushward.Client
	breaker *tenantBreaker
}

// Option configures a Pool.
type Option func(*Pool)

// WithClientOptions applies opts to every client the pool creates.
func WithClientOptions(opts ...pushward.ClientOption) Option {
	return func(p *Pool) { p.opts = append(p.opts, opts...) }
}

// WithBreakers gives every tenant a circuit breaker of its own, opening after
// threshold consecutive faults for cooldown, in front of one breaker for the
// whole server with the same settings. The server breaker opens only for a
// fault streak that spans several tenants (serverTenants), so one tenant's
// failures leave the others delivering.
func WithBreakers(threshold int, cooldown time.Duration) Option {
	return func(p *Pool) {
		p.server = newServerBreaker(threshold, cooldown)
		p.threshold = threshold
		p.cooldown = cooldown
	}
}

// NewPool creates a new client pool. When httpClient is nil the pool builds
// one on NewTransport with a 10s timeout; either way every client created from
// the pool shares it, and with it the idle connections.
func NewPool(baseURL string, httpClient *http.Client, opts ...Option) *Pool {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second, Transport: NewTransport()}
	}
	p := &Pool{
		baseURL:    baseURL,
		httpClient: httpClient,
		clients:    lrumap.New[*pooled](maxClients),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// SendNotification resolves the client for userKey and sends a notification.
func (p *Pool) SendNotification(ctx context.Context, userKey string, log *slog.Logger, req pushward.SendNotificationRequest) error {
	cl := p.Get(userKey)
	if err := cl.SendNotification(ctx, req); err != nil {
		log.Error("failed to send notification", "source", req.Source, "error", err)
		return err
	}
	log.Info("notification sent", "source", req.Source)
	return nil
}

// Get returns a pushward.Client for the given hlk_ key, creating one if needed.
// Every pooled client carries the retry budget; a caller-supplied option of the
// same kind still wins, since options apply in order. The tenant breaker goes
// last, so it replaces any breaker in the client options.
func (p *Pool) Get(hlkKey string) *pushward.Client {
	hash := lrumap.KeyHash(hlkKey)
	return p.clients.GetOrCreate(hash, func() *pooled {
		opts := []pushward.ClientOption{pushward.WithRetryBudget(retryBudget)}
		opts = append(opts, p.opts...)
		opts = append(opts, pushward.WithHTTPClient(p.httpClient))
		e := &pooled{}
		if p.server != nil {
			e.breaker = &tenantBreaker{
				tenant: hash,
				own:    pushward.NewCircuitBreaker(p.threshold, p.cooldown),
				server: p.server,
			}
			opts = append(opts, pushward.WithBreaker(e.breaker))
		}
		e.cl = pushward.NewClient(p.baseURL, hlkKey, opts...)
		return e
	}).cl
}

// ServerBreakerOpen reports whether the breaker for the whole server refuses
// calls right now: open inside its cooldown, or probing. It is false without
// WithBreakers.
func (p *Pool) ServerBreakerOpen() bool {
	return p.server != nil && p.server.blocking()
}

// OpenTenantBreakers counts the pooled tenants whose own breaker refuses calls
// right now. A tenant that tripped its breaker and then went quiet stays in
// the pool until eviction, so this must not count a breaker whose cooldown is
// over and that nobody has called since.
func (p *Pool) OpenTenantBreakers() int {
	n := 0
	p.clients.Range(func(_ string, e *pooled) {
		if e.breaker != nil && e.breaker.own.Blocking() {
			n++
		}
	})
	return n
}
