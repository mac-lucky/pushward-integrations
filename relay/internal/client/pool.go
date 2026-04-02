package client

import (
	"net/http"

	"github.com/mac-lucky/pushward-integrations/relay/internal/lrumap"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

const maxClients = 1000

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

// Get returns a pushward.Client for the given hlk_ key, creating one if needed.
func (p *Pool) Get(hlkKey string) *pushward.Client {
	hash := lrumap.KeyHash(hlkKey)
	return p.clients.GetOrCreate(hash, func() *pushward.Client {
		opts := append([]pushward.ClientOption{}, p.opts...)
		if p.httpClient != nil {
			opts = append(opts, pushward.WithHTTPClient(p.httpClient))
		}
		return pushward.NewClient(p.baseURL, hlkKey, opts...)
	})
}
