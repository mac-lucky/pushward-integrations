package state

import (
	"context"
	"encoding/json"
	"time"
)

// Entry represents a single state store entry returned by ListByProvider.
type Entry struct {
	UserKey string
	Key     string
	SubKey  string
	Value   json.RawMessage
}

// Store provides multi-tenant ephemeral state for relay handlers.
// Each method is keyed by (provider, userKey) where userKey is the hlk_ integration key hash.
type Store interface {
	// Set upserts a state entry with optional TTL (0 = no expiry).
	Set(ctx context.Context, provider, userKey, key, subKey string, value json.RawMessage, ttl time.Duration) error

	// Get retrieves a single state entry. Returns nil if not found.
	Get(ctx context.Context, provider, userKey, key, subKey string) (json.RawMessage, error)

	// GetGroup retrieves all entries for a (provider, userKey, key) group.
	GetGroup(ctx context.Context, provider, userKey, key string) (map[string]json.RawMessage, error)

	// Delete removes a single state entry.
	Delete(ctx context.Context, provider, userKey, key, subKey string) error

	// DeleteGroup removes all entries for a (provider, userKey, key) group.
	DeleteGroup(ctx context.Context, provider, userKey, key string) error

	// Exists checks if an entry exists and has not expired.
	Exists(ctx context.Context, provider, userKey, key, subKey string) (bool, error)

	// ListByProvider returns all non-expired entries for a given provider.
	// Used for startup recovery (e.g., finding pending ArgoCD entries).
	ListByProvider(ctx context.Context, provider string) ([]Entry, error)

	// Cleanup removes all expired entries. Called by the background sweep goroutine.
	Cleanup(ctx context.Context) (int64, error)
}
