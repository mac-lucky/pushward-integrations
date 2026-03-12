package client

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
)

const maxClients = 1000

type poolEntry struct {
	client     *pushward.Client
	lastAccess time.Time
}

// Pool manages a pool of pushward.Client instances keyed by hlk_ key hash.
// All clients share the same base URL but use different API keys.
type Pool struct {
	baseURL string
	mu      sync.RWMutex
	clients map[string]*poolEntry // key hash → entry
}

// NewPool creates a new client pool.
func NewPool(baseURL string) *Pool {
	return &Pool{
		baseURL: baseURL,
		clients: make(map[string]*poolEntry),
	}
}

// Get returns a pushward.Client for the given hlk_ key, creating one if needed.
func (p *Pool) Get(hlkKey string) *pushward.Client {
	hash := keyHash(hlkKey)

	p.mu.RLock()
	if e, ok := p.clients[hash]; ok {
		p.mu.RUnlock()
		return e.client
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after acquiring write lock
	if e, ok := p.clients[hash]; ok {
		e.lastAccess = time.Now()
		return e.client
	}

	// Evict LRU if at capacity
	if len(p.clients) >= maxClients {
		var oldestKey string
		var oldestTime time.Time
		first := true
		for k, e := range p.clients {
			if first || e.lastAccess.Before(oldestTime) {
				oldestKey = k
				oldestTime = e.lastAccess
				first = false
			}
		}
		delete(p.clients, oldestKey)
	}

	c := pushward.NewClient(p.baseURL, hlkKey)
	p.clients[hash] = &poolEntry{client: c, lastAccess: time.Now()}
	return c
}

func keyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h[:16])
}
