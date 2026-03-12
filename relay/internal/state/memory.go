package state

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

type memEntry struct {
	value     json.RawMessage
	expiresAt *time.Time
}

func (e *memEntry) expired() bool {
	return e.expiresAt != nil && time.Now().After(*e.expiresAt)
}

// MemoryStore implements Store using an in-memory map. Used for tests.
type MemoryStore struct {
	mu      sync.Mutex
	entries map[string]*memEntry // composite key → entry
}

// NewMemoryStore creates a new in-memory state store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		entries: make(map[string]*memEntry),
	}
}

func compositeKey(provider, userKey, key, subKey string) string {
	return provider + "\x00" + userKey + "\x00" + key + "\x00" + subKey
}

func groupPrefix(provider, userKey, key string) string {
	return provider + "\x00" + userKey + "\x00" + key + "\x00"
}

func (s *MemoryStore) Set(_ context.Context, provider, userKey, key, subKey string, value json.RawMessage, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	e := &memEntry{value: value}
	if ttl > 0 {
		t := time.Now().Add(ttl)
		e.expiresAt = &t
	}
	s.entries[compositeKey(provider, userKey, key, subKey)] = e
	return nil
}

func (s *MemoryStore) Get(_ context.Context, provider, userKey, key, subKey string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[compositeKey(provider, userKey, key, subKey)]
	if !ok || e.expired() {
		return nil, nil
	}
	return e.value, nil
}

func (s *MemoryStore) GetGroup(_ context.Context, provider, userKey, key string) (map[string]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prefix := groupPrefix(provider, userKey, key)
	result := make(map[string]json.RawMessage)
	for k, e := range s.entries {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix && !e.expired() {
			subKey := k[len(prefix):]
			result[subKey] = e.value
		}
	}
	return result, nil
}

func (s *MemoryStore) Delete(_ context.Context, provider, userKey, key, subKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.entries, compositeKey(provider, userKey, key, subKey))
	return nil
}

func (s *MemoryStore) DeleteGroup(_ context.Context, provider, userKey, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	prefix := groupPrefix(provider, userKey, key)
	for k := range s.entries {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(s.entries, k)
		}
	}
	return nil
}

func (s *MemoryStore) Exists(_ context.Context, provider, userKey, key, subKey string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[compositeKey(provider, userKey, key, subKey)]
	return ok && !e.expired(), nil
}

func (s *MemoryStore) Cleanup(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var count int64
	for k, e := range s.entries {
		if e.expired() {
			delete(s.entries, k)
			count++
		}
	}
	return count, nil
}
