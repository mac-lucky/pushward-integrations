package state

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
)

// HashKey returns the hex SHA-256 of a tenant key, the form KeyHashing stores.
func HashKey(raw string) string {
	d := auth.KeyDigest(raw)
	return hex.EncodeToString(d[:])
}

// KeyHashing wraps inner so every row is written and looked up under
// HashKey(userKey), never the raw hlk_ key. A row an older release stored
// under the raw key is invisible through it: reads miss it, deletes leave it,
// and Cleanup removes it once it expires. Cleanup passes through.
func KeyHashing(inner Store) Store {
	return &keyHashing{inner: inner}
}

type keyHashing struct {
	inner Store
}

func (s *keyHashing) Set(ctx context.Context, provider, userKey, key, subKey string, value json.RawMessage, ttl time.Duration) error {
	return s.inner.Set(ctx, provider, HashKey(userKey), key, subKey, value, ttl)
}

func (s *keyHashing) Get(ctx context.Context, provider, userKey, key, subKey string) (json.RawMessage, error) {
	return s.inner.Get(ctx, provider, HashKey(userKey), key, subKey)
}

func (s *keyHashing) GetGroup(ctx context.Context, provider, userKey, key string) (map[string]json.RawMessage, error) {
	return s.inner.GetGroup(ctx, provider, HashKey(userKey), key)
}

func (s *keyHashing) Delete(ctx context.Context, provider, userKey, key, subKey string) error {
	return s.inner.Delete(ctx, provider, HashKey(userKey), key, subKey)
}

func (s *keyHashing) DeleteGroup(ctx context.Context, provider, userKey, key string) error {
	return s.inner.DeleteGroup(ctx, provider, HashKey(userKey), key)
}

func (s *keyHashing) Exists(ctx context.Context, provider, userKey, key, subKey string) (bool, error) {
	return s.inner.Exists(ctx, provider, HashKey(userKey), key, subKey)
}

func (s *keyHashing) Cleanup(ctx context.Context) (int64, error) {
	return s.inner.Cleanup(ctx)
}
