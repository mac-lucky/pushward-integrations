package client

import (
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

// Pool manages a pool of pushward.Client instances keyed by hlk_ key hash.
// All clients share the same base URL but use different API keys.
type Pool struct {
	baseURL string
	mu      sync.RWMutex
	clients map[string]*pushward.Client // key hash → client
}

// NewPool creates a new client pool.
func NewPool(baseURL string) *Pool {
	return &Pool{
		baseURL: baseURL,
		clients: make(map[string]*pushward.Client),
	}
}

// Get returns a pushward.Client for the given hlk_ key, creating one if needed.
func (p *Pool) Get(hlkKey string) *pushward.Client {
	hash := keyHash(hlkKey)

	p.mu.RLock()
	if c, ok := p.clients[hash]; ok {
		p.mu.RUnlock()
		return c
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after acquiring write lock
	if c, ok := p.clients[hash]; ok {
		return c
	}

	c := pushward.NewClient(p.baseURL, hlkKey)
	p.clients[hash] = c
	return c
}

func keyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h[:16])
}
