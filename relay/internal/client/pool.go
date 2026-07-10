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

// Pool manages a pool of pushward.Client instances keyed by hlk_ key hash.
// All clients share the same base URL but use different API keys.
type Pool struct {
	baseURL    string
	httpClient *http.Client
	opts       []pushward.ClientOption
	clients    *lrumap.Map[*pushward.Client]
}

// NewPool creates a new client pool. When httpClient is non-nil it is shared
// by every client created from the pool (e.g. for trace-propagating transports).
func NewPool(baseURL string, httpClient *http.Client, opts ...pushward.ClientOption) *Pool {
	return &Pool{
		baseURL:    baseURL,
		httpClient: httpClient,
		opts:       opts,
		clients:    lrumap.New[*pushward.Client](maxClients),
	}
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
// same kind still wins, since options apply in order.
func (p *Pool) Get(hlkKey string) *pushward.Client {
	hash := lrumap.KeyHash(hlkKey)
	return p.clients.GetOrCreate(hash, func() *pushward.Client {
		opts := []pushward.ClientOption{pushward.WithRetryBudget(retryBudget)}
		opts = append(opts, p.opts...)
		if p.httpClient != nil {
			opts = append(opts, pushward.WithHTTPClient(p.httpClient))
		}
		return pushward.NewClient(p.baseURL, hlkKey, opts...)
	})
}
