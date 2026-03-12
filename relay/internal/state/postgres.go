package state

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const migrationSQL = `
CREATE TABLE IF NOT EXISTS relay_state (
    provider    TEXT NOT NULL,
    user_key    TEXT NOT NULL,
    key         TEXT NOT NULL,
    sub_key     TEXT NOT NULL DEFAULT '',
    value       JSONB NOT NULL,
    expires_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (provider, user_key, key, sub_key)
);

CREATE INDEX IF NOT EXISTS idx_relay_state_expires
    ON relay_state (expires_at) WHERE expires_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_relay_state_provider_user
    ON relay_state (provider, user_key);
`

// PostgresStore implements Store using a PostgreSQL relay_state table.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore creates a new PostgresStore and runs the migration.
func NewPostgresStore(ctx context.Context, pool *pgxpool.Pool) (*PostgresStore, error) {
	if _, err := pool.Exec(ctx, migrationSQL); err != nil {
		return nil, err
	}
	return &PostgresStore{pool: pool}, nil
}

func (s *PostgresStore) Set(ctx context.Context, provider, userKey, key, subKey string, value json.RawMessage, ttl time.Duration) error {
	var expiresAt *time.Time
	if ttl > 0 {
		t := time.Now().Add(ttl)
		expiresAt = &t
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO relay_state (provider, user_key, key, sub_key, value, expires_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (provider, user_key, key, sub_key)
		DO UPDATE SET value = EXCLUDED.value, expires_at = EXCLUDED.expires_at, updated_at = now()
	`, provider, userKey, key, subKey, value, expiresAt)
	return err
}

func (s *PostgresStore) Get(ctx context.Context, provider, userKey, key, subKey string) (json.RawMessage, error) {
	var value json.RawMessage
	err := s.pool.QueryRow(ctx, `
		SELECT value FROM relay_state
		WHERE provider = $1 AND user_key = $2 AND key = $3 AND sub_key = $4
		  AND (expires_at IS NULL OR expires_at > now())
	`, provider, userKey, key, subKey).Scan(&value)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return value, err
}

func (s *PostgresStore) GetGroup(ctx context.Context, provider, userKey, key string) (map[string]json.RawMessage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sub_key, value FROM relay_state
		WHERE provider = $1 AND user_key = $2 AND key = $3
		  AND (expires_at IS NULL OR expires_at > now())
	`, provider, userKey, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]json.RawMessage)
	for rows.Next() {
		var subKey string
		var value json.RawMessage
		if err := rows.Scan(&subKey, &value); err != nil {
			return nil, err
		}
		result[subKey] = value
	}
	return result, rows.Err()
}

func (s *PostgresStore) Delete(ctx context.Context, provider, userKey, key, subKey string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM relay_state
		WHERE provider = $1 AND user_key = $2 AND key = $3 AND sub_key = $4
	`, provider, userKey, key, subKey)
	return err
}

func (s *PostgresStore) DeleteGroup(ctx context.Context, provider, userKey, key string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM relay_state
		WHERE provider = $1 AND user_key = $2 AND key = $3
	`, provider, userKey, key)
	return err
}

func (s *PostgresStore) Exists(ctx context.Context, provider, userKey, key, subKey string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM relay_state
			WHERE provider = $1 AND user_key = $2 AND key = $3 AND sub_key = $4
			  AND (expires_at IS NULL OR expires_at > now())
		)
	`, provider, userKey, key, subKey).Scan(&exists)
	return exists, err
}

func (s *PostgresStore) Cleanup(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM relay_state WHERE expires_at IS NOT NULL AND expires_at <= now()
	`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
