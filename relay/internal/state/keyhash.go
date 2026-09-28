package state

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
)

// KeyMode selects which form of the tenant key KeyHashing writes, and whether
// it still reads and cleans up rows keyed the other way.
type KeyMode string

const (
	// KeyModeCompat writes the raw key, as releases before hashing did, and
	// reads the raw row first, then the hashed one. It is the first step of a
	// rollout: replicas still on the old release keep finding every row.
	KeyModeCompat KeyMode = "compat"

	// KeyModeHashed writes the digest and reads the hashed row first, then the
	// raw one, so rows written by a compat replica stay readable until their
	// TTL runs out.
	KeyModeHashed KeyMode = "hashed"

	// KeyModeStrict writes and reads the digest only. For state that never
	// existed under a raw key.
	KeyModeStrict KeyMode = "strict"
)

// HashKey returns the hex SHA-256 of a tenant key, the form KeyHashing stores.
func HashKey(raw string) string {
	d := auth.KeyDigest(raw)
	return hex.EncodeToString(d[:])
}

// KeyHashing wraps inner and picks the form of userKey that reaches it, per
// mode. In compat and hashed mode each row may also exist under the other form
// of the key (its twin):
// a write deletes the twin, a read falls back to it, Exists and GetGroup see
// both, and Delete and DeleteGroup remove both. Cleanup passes through.
func KeyHashing(inner Store, mode KeyMode) Store {
	switch mode {
	case KeyModeCompat, KeyModeHashed, KeyModeStrict:
	default:
		panic("state: KeyHashing: unknown key mode " + string(mode))
	}
	return &keyHashing{inner: inner, mode: mode}
}

type keyHashing struct {
	inner Store
	mode  KeyMode
}

// keys returns the key a row is written under and its twin, "" when the mode
// keeps no twin.
func (s *keyHashing) keys(userKey string) (primary, twin string) {
	hashed := HashKey(userKey)
	switch s.mode {
	case KeyModeCompat:
		return userKey, hashed
	case KeyModeHashed:
		return hashed, userKey
	default:
		return hashed, ""
	}
}

// Set deletes the twin before it writes. A compat and a hashed replica writing
// the same row at once could otherwise each delete the other's fresh write and
// lose both; this order leaves at worst both rows, a duplicate that expires on
// its TTL. A failed twin delete does not fail the write: the stale twin is
// shadowed for readers in this mode, but a reader in the other mode reads it
// first, so it is logged at Warn.
func (s *keyHashing) Set(ctx context.Context, provider, userKey, key, subKey string, value json.RawMessage, ttl time.Duration) error {
	primary, twin := s.keys(userKey)
	if twin != "" {
		if err := s.inner.Delete(ctx, provider, twin, key, subKey); err != nil {
			slog.Warn("state twin delete failed", "provider", provider, "error", err)
		}
	}
	return s.inner.Set(ctx, provider, primary, key, subKey, value, ttl)
}

func (s *keyHashing) Get(ctx context.Context, provider, userKey, key, subKey string) (json.RawMessage, error) {
	primary, twin := s.keys(userKey)
	v, err := s.inner.Get(ctx, provider, primary, key, subKey)
	if err != nil || v != nil || twin == "" {
		return v, err
	}
	return s.inner.Get(ctx, provider, twin, key, subKey)
}

func (s *keyHashing) GetGroup(ctx context.Context, provider, userKey, key string) (map[string]json.RawMessage, error) {
	primary, twin := s.keys(userKey)
	group, err := s.inner.GetGroup(ctx, provider, primary, key)
	if err != nil || twin == "" {
		return group, err
	}
	old, err := s.inner.GetGroup(ctx, provider, twin, key)
	if err != nil {
		return nil, err
	}
	if len(old) == 0 {
		return group, nil
	}
	// The primary row wins a sub-key present under both keys: it is the newer
	// write, since writing it deleted the twin.
	maps.Copy(old, group)
	return old, nil
}

// Delete and DeleteGroup report a failed twin delete, unlike Set: after an
// explicit delete there is no primary row left to shadow the twin, so a
// surviving twin would read back as live state.
func (s *keyHashing) Delete(ctx context.Context, provider, userKey, key, subKey string) error {
	primary, twin := s.keys(userKey)
	err := s.inner.Delete(ctx, provider, primary, key, subKey)
	if twin != "" {
		err = errors.Join(err, s.inner.Delete(ctx, provider, twin, key, subKey))
	}
	return err
}

func (s *keyHashing) DeleteGroup(ctx context.Context, provider, userKey, key string) error {
	primary, twin := s.keys(userKey)
	err := s.inner.DeleteGroup(ctx, provider, primary, key)
	if twin != "" {
		err = errors.Join(err, s.inner.DeleteGroup(ctx, provider, twin, key))
	}
	return err
}

func (s *keyHashing) Exists(ctx context.Context, provider, userKey, key, subKey string) (bool, error) {
	primary, twin := s.keys(userKey)
	ok, err := s.inner.Exists(ctx, provider, primary, key, subKey)
	if err != nil || ok || twin == "" {
		return ok, err
	}
	return s.inner.Exists(ctx, provider, twin, key, subKey)
}

func (s *keyHashing) Cleanup(ctx context.Context) (int64, error) {
	return s.inner.Cleanup(ctx)
}
