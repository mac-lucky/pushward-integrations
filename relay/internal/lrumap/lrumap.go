// Package lrumap provides a concurrency-safe map with LRU eviction at capacity.
package lrumap

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type entry[V any] struct {
	value      V
	lastAccess atomic.Int64 // UnixNano
}

// Map is a concurrency-safe map with LRU eviction when the entry count
// reaches maxSize. It uses double-checked locking to minimise write-lock
// contention on the hot path.
type Map[V any] struct {
	mu      sync.RWMutex
	entries map[string]*entry[V]
	maxSize int
	onEvict func(key string, value V)
}

// SetOnEvict registers a callback that is invoked when an entry is evicted
// to make room for a new one. The callback runs under the write lock, so it
// must not call back into the Map.
func (m *Map[V]) SetOnEvict(fn func(key string, value V)) {
	m.mu.Lock()
	m.onEvict = fn
	m.mu.Unlock()
}

// New creates a Map that evicts the least-recently-used entry when
// the number of entries reaches maxSize.
func New[V any](maxSize int) *Map[V] {
	return &Map[V]{
		entries: make(map[string]*entry[V]),
		maxSize: maxSize,
	}
}

// GetOrCreate returns the value for key, creating it via create() if absent.
// The create function is called at most once per key and is invoked under
// the write lock, so it must not call back into the Map.
func (m *Map[V]) GetOrCreate(key string, create func() V) V {
	m.mu.RLock()
	if e, ok := m.entries[key]; ok {
		e.lastAccess.Store(time.Now().UnixNano())
		m.mu.RUnlock()
		return e.value
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock.
	if e, ok := m.entries[key]; ok {
		e.lastAccess.Store(time.Now().UnixNano())
		return e.value
	}

	// Evict LRU if at capacity.
	if len(m.entries) >= m.maxSize {
		var oldestKey string
		var oldestTime int64
		first := true
		for k, e := range m.entries {
			access := e.lastAccess.Load()
			if first || access < oldestTime {
				oldestKey = k
				oldestTime = access
				first = false
			}
		}
		if m.onEvict != nil {
			m.onEvict(oldestKey, m.entries[oldestKey].value)
		}
		delete(m.entries, oldestKey)
	}

	v := create()
	e := &entry[V]{value: v}
	e.lastAccess.Store(time.Now().UnixNano())
	m.entries[key] = e
	return v
}

// Delete removes the entry for key, if present.
func (m *Map[V]) Delete(key string) {
	m.mu.Lock()
	delete(m.entries, key)
	m.mu.Unlock()
}

// Len returns the number of entries in the map.
func (m *Map[V]) Len() int {
	m.mu.RLock()
	n := len(m.entries)
	m.mu.RUnlock()
	return n
}

// KeyHash returns a truncated hex-encoded SHA-256 hash of key.
// This is useful for deriving safe map keys from sensitive API tokens.
func KeyHash(key string) string {
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h[:16])
}
