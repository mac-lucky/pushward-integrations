package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Advisory lock classes, as the first key of pg_advisory_xact_lock(int, int).
// The pooler runs in transaction mode, so only transaction-scoped locks are
// safe: a session lock would stay with whichever server connection the pooler
// handed out.
const (
	// mappingLockClass serializes one tenant's writes; the second key is
	// keyLockID. 0x554E49 is "UNI".
	mappingLockClass int32 = 0x554E49
	// mappingMigrateLockClass serializes the migration of replicas that start
	// together. 0x554E4D is "UNM".
	mappingMigrateLockClass int32 = 0x554E4D
)

const mappingsMigrationSQL = `
CREATE TABLE IF NOT EXISTS universal_mappings (
    key_hash          BYTEA NOT NULL CHECK (octet_length(key_hash) = 32),
    source            TEXT NOT NULL CHECK (source ~ '^[a-z0-9-]{0,32}$'),
    fingerprint       BYTEA NOT NULL CHECK (octet_length(fingerprint) = 32),
    status            TEXT NOT NULL CHECK (status IN ('pending', 'confirmed', 'rejected')),
    mapping           JSONB NOT NULL,
    shape             JSONB NOT NULL,
    proposal          JSONB NOT NULL,
    proposer          TEXT NOT NULL,
    -- every field's display value, redacted by universal.Display, the
    -- correlation field included; NULL after SamplesTTL
    samples           JSONB,
    candidates        JSONB,
    rev               INT NOT NULL DEFAULT 0,
    review_claimed_at TIMESTAMPTZ,
    review_sent_at    TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at        TIMESTAMPTZ,
    last_used_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ,

    PRIMARY KEY (key_hash, source, fingerprint),
    CHECK ((status = 'pending') = (expires_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_universal_mappings_key_status
    ON universal_mappings (key_hash, status);

CREATE INDEX IF NOT EXISTS idx_universal_mappings_pending_expires
    ON universal_mappings (expires_at) WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_universal_mappings_decided_used
    ON universal_mappings (last_used_at) WHERE status <> 'pending';

CREATE INDEX IF NOT EXISTS idx_universal_mappings_samples_created
    ON universal_mappings (created_at) WHERE samples IS NOT NULL;
`

// liveRow is the WHERE clause that hides pending rows past their expiry.
const liveRow = `(status <> 'pending' OR expires_at > now())`

// byKey matches one row by primary key, as $1, $2, $3.
const byKey = `key_hash = $1 AND source = $2 AND fingerprint = $3`

const rowColumns = `key_hash, source, fingerprint, status, mapping, shape, proposal, samples, candidates,
	proposer, rev, review_claimed_at, review_sent_at, created_at, decided_at, last_used_at, expires_at`

// PostgresMappingStore implements MappingStore on the universal_mappings table.
type PostgresMappingStore struct {
	pool *pgxpool.Pool
}

var _ MappingStore = (*PostgresMappingStore)(nil)

// lockTimeout bounds every wait on an advisory lock. A tenant's writes are
// short, so a longer wait means a stuck transaction, and the webhook is better
// served by an error (it still delivers) than by hanging on the lock.
const lockTimeout = `SET LOCAL lock_timeout = '5s'`

// NewMappingStore creates the universal_mappings table if needed and returns a
// store on it. The migration runs in one transaction under an advisory lock, so
// replicas starting together do not race on CREATE.
func NewMappingStore(ctx context.Context, pool *pgxpool.Pool) (*PostgresMappingStore, error) {
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, lockTimeout); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::int4, 0)`, mappingMigrateLockClass); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, mappingsMigrationSQL)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("mappings migrate: %w", err)
	}
	return &PostgresMappingStore{pool: pool}, nil
}

func keyArgs(k MappingKey) []any {
	return []any{k.KeyHash[:], k.Source, k.Fingerprint[:]}
}

// inKeyTx runs fn in a transaction holding the tenant's advisory lock.
func (s *PostgresMappingStore) inKeyTx(ctx context.Context, keyHash [32]byte, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, lockTimeout); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::int4, $2::int4)`, mappingLockClass, keyLockID(keyHash)); err != nil {
			return err
		}
		return fn(tx)
	})
}

func scanRow(row pgx.Row) (*MappingRow, error) {
	var (
		r            MappingRow
		keyHash, fp  []byte
		status       string
		shape, cands []byte
	)
	err := row.Scan(&keyHash, &r.Source, &fp, &status, &r.Mapping, &shape, &r.Proposal, &r.Samples, &cands,
		&r.Proposer, &r.Rev, &r.ReviewClaimedAt, &r.ReviewSentAt, &r.CreatedAt, &r.DecidedAt, &r.LastUsedAt, &r.ExpiresAt)
	if err != nil {
		return nil, err
	}
	if len(keyHash) != len(r.KeyHash) || len(fp) != len(r.Fingerprint) {
		return nil, errors.New("key hash or fingerprint is not 32 bytes")
	}
	copy(r.KeyHash[:], keyHash)
	copy(r.Fingerprint[:], fp)
	r.Status = MappingStatus(status)
	r.Shape = shape
	r.Candidates = cands
	return &r, nil
}

// GetForDelivery reads the columns the webhook path uses, with the large ones
// selected as NULL.
func (s *PostgresMappingStore) GetForDelivery(ctx context.Context, k MappingKey) (*MappingRow, error) {
	r, err := scanRow(s.pool.QueryRow(ctx, `
		SELECT key_hash, source, fingerprint, status, mapping, NULL::jsonb, NULL::jsonb, NULL::jsonb, NULL::jsonb,
			'', rev, NULL::timestamptz, review_sent_at, created_at, NULL::timestamptz, last_used_at, expires_at
		FROM universal_mappings WHERE `+byKey+` AND `+liveRow, keyArgs(k)...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mappings get for delivery: %w", err)
	}
	return r, nil
}

func (s *PostgresMappingStore) Get(ctx context.Context, k MappingKey) (*MappingRow, error) {
	r, err := scanRow(s.pool.QueryRow(ctx, `SELECT `+rowColumns+` FROM universal_mappings WHERE `+byKey+` AND `+liveRow, keyArgs(k)...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mappings get: %w", err)
	}
	return r, nil
}

// liveCounts reports, under the tenant lock, whether k has a live row and how
// many of the tenant's other live rows are pending.
func liveCounts(ctx context.Context, tx pgx.Tx, k MappingKey) (exists bool, pending int, err error) {
	err = tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE source = $2 AND fingerprint = $3) > 0,
		       count(*) FILTER (WHERE status = 'pending' AND NOT (source = $2 AND fingerprint = $3))
		FROM universal_mappings
		WHERE key_hash = $1 AND `+liveRow, keyArgs(k)...).Scan(&exists, &pending)
	return exists, pending, err
}

// InsertPending checks for a live row and the pending cap under the tenant
// lock, so of two replicas proposing the same shape one inserts and the other
// reads false. The ON CONFLICT clause only ever replaces an expired pending
// row, carrying its rev forward; a live one makes RETURNING come back empty,
// which is the same answer.
func (s *PostgresMappingStore) InsertPending(ctx context.Context, row *MappingRow, maxPending int) (bool, error) {
	if row.ExpiresAt == nil {
		return false, errors.New("mappings insert: a pending row needs expires_at")
	}
	var inserted bool
	err := s.inKeyTx(ctx, row.KeyHash, func(tx pgx.Tx) error {
		exists, pending, err := liveCounts(ctx, tx, row.MappingKey)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
		if pending >= maxPending {
			return ErrPendingCap
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO universal_mappings (key_hash, source, fingerprint, status, mapping, shape, proposal,
				proposer, samples, candidates, review_claimed_at, expires_at)
			VALUES ($1, $2, $3, 'pending', $4, $5, $6, $7, $8, $9, now(), $10)
			ON CONFLICT (key_hash, source, fingerprint) DO UPDATE SET
				status = 'pending', mapping = EXCLUDED.mapping, shape = EXCLUDED.shape,
				proposal = EXCLUDED.proposal, proposer = EXCLUDED.proposer, samples = EXCLUDED.samples,
				candidates = EXCLUDED.candidates, rev = universal_mappings.rev + 1, review_claimed_at = now(),
				review_sent_at = NULL, created_at = now(), decided_at = NULL, last_used_at = now(),
				expires_at = EXCLUDED.expires_at
			WHERE universal_mappings.status = 'pending' AND universal_mappings.expires_at <= now()
			RETURNING true`,
			row.KeyHash[:], row.Source, row.Fingerprint[:], row.Mapping, row.Shape, row.Proposal,
			row.Proposer, nullJSON(row.Samples), nullJSON(row.Candidates), *row.ExpiresAt).Scan(&inserted)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if errors.Is(err, ErrPendingCap) {
		return false, err
	}
	if err != nil {
		return false, fmt.Errorf("mappings insert: %w", err)
	}
	return inserted, nil
}

func (s *PostgresMappingStore) ReplaceStale(ctx context.Context, row *MappingRow, staleRev, maxPending int) (bool, error) {
	if row.ExpiresAt == nil {
		return false, errors.New("mappings replace: a pending row needs expires_at")
	}
	var replaced bool
	err := s.inKeyTx(ctx, row.KeyHash, func(tx pgx.Tx) error {
		_, pending, err := liveCounts(ctx, tx, row.MappingKey)
		if err != nil {
			return err
		}
		if pending >= maxPending {
			return ErrPendingCap
		}
		err = tx.QueryRow(ctx, `
			UPDATE universal_mappings SET
				status = 'pending', mapping = $4, shape = $5, proposal = $6, proposer = $7, samples = $8,
				candidates = $9, rev = rev + 1, review_claimed_at = now(), review_sent_at = NULL,
				created_at = now(), decided_at = NULL, last_used_at = now(), expires_at = $10
			WHERE `+byKey+` AND rev = $11 AND `+liveRow+`
			RETURNING true`,
			row.KeyHash[:], row.Source, row.Fingerprint[:], row.Mapping, row.Shape, row.Proposal,
			row.Proposer, nullJSON(row.Samples), nullJSON(row.Candidates), *row.ExpiresAt, staleRev).Scan(&replaced)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if errors.Is(err, ErrPendingCap) {
		return false, err
	}
	if err != nil {
		return false, fmt.Errorf("mappings replace: %w", err)
	}
	return replaced, nil
}

// nullJSON sends a nil document as SQL NULL rather than JSON null.
func nullJSON(v json.RawMessage) any {
	if v == nil {
		return nil
	}
	return v
}

func (s *PostgresMappingStore) ClaimReview(ctx context.Context, k MappingKey) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE universal_mappings SET review_claimed_at = now()
		WHERE `+byKey+` AND status = 'pending' AND expires_at > now() AND review_sent_at IS NULL
		  AND (review_claimed_at IS NULL OR review_claimed_at < now() - $4::interval)`,
		append(keyArgs(k), ReviewClaimTTL)...)
	if err != nil {
		return false, fmt.Errorf("mappings claim review: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PostgresMappingStore) MarkReviewSent(ctx context.Context, k MappingKey) error {
	if _, err := s.pool.Exec(ctx, `UPDATE universal_mappings SET review_sent_at = now() WHERE `+byKey, keyArgs(k)...); err != nil {
		return fmt.Errorf("mappings mark review sent: %w", err)
	}
	return nil
}

func (s *PostgresMappingStore) ReleaseReview(ctx context.Context, k MappingKey) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE universal_mappings SET review_claimed_at = NULL
		WHERE `+byKey+` AND review_sent_at IS NULL`, keyArgs(k)...)
	if err != nil {
		return fmt.Errorf("mappings release review: %w", err)
	}
	return nil
}

// Decide only moves a live pending row with the expected expiry. When nothing
// moved, the row is read back to tell a repeat of the decision already made
// from its opposite; a pending row with another expiry is not the one the
// link was made for.
func (s *PostgresMappingStore) Decide(ctx context.Context, k MappingKey, status MappingStatus, expires time.Time, maxDecided int) (DecideResult, error) {
	if err := checkDecided(status); err != nil {
		return DecideResult{}, err
	}
	var res DecideResult
	err := s.inKeyTx(ctx, k.KeyHash, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE universal_mappings
			SET status = $4, decided_at = now(), last_used_at = now(), expires_at = NULL, rev = rev + 1
			WHERE `+byKey+` AND status = 'pending' AND expires_at > now() AND expires_at = $5`,
			append(keyArgs(k), string(status), expires)...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var cur string
			err := tx.QueryRow(ctx, `
				SELECT status FROM universal_mappings
				WHERE `+byKey+` AND `+liveRow+` AND (status <> 'pending' OR expires_at = $4)`,
				append(keyArgs(k), expires)...).Scan(&cur)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				res.Outcome = DecideNotFound
				return nil
			case err != nil:
				return err
			case MappingStatus(cur) == status:
				res.Outcome = DecideIdempotent
			default:
				res.Outcome = DecideConflict
			}
			return nil
		}
		res.Outcome = DecideOK
		res.Evicted, err = evictLRU(ctx, tx, k.KeyHash, status, maxDecided)
		return err
	})
	if err != nil {
		return DecideResult{}, fmt.Errorf("mappings decide: %w", err)
	}
	return res, nil
}

func (s *PostgresMappingStore) Edit(ctx context.Context, k MappingKey, mapping json.RawMessage, status MappingStatus, rev, maxDecided int) (EditResult, error) {
	if err := checkDecided(status); err != nil {
		return EditResult{}, err
	}
	var res EditResult
	err := s.inKeyTx(ctx, k.KeyHash, func(tx pgx.Tx) error {
		args := append(keyArgs(k), mapping, string(status))
		err := tx.QueryRow(ctx, `
			UPDATE universal_mappings
			SET mapping = $4, status = $5, proposer = 'user-edit', decided_at = now(), last_used_at = now(),
				expires_at = NULL, rev = rev + 1
			WHERE `+byKey+` AND rev = $6 AND `+liveRow+`
			RETURNING rev`, append(args, rev)...).Scan(&res.Rev)
		if errors.Is(err, pgx.ErrNoRows) {
			var same bool
			err := tx.QueryRow(ctx, `
				SELECT rev, mapping = $4::jsonb AND status = $5
				FROM universal_mappings WHERE `+byKey+` AND `+liveRow, args...).Scan(&res.Rev, &same)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				res.Outcome = EditNotFound
				return nil
			case err != nil:
				return err
			case same:
				res.Outcome = EditIdempotent
			default:
				res.Outcome = EditStale
			}
			return nil
		}
		if err != nil {
			return err
		}
		res.Outcome = EditOK
		res.Evicted, err = evictLRU(ctx, tx, k.KeyHash, status, maxDecided)
		return err
	})
	if err != nil {
		return EditResult{}, fmt.Errorf("mappings edit: %w", err)
	}
	return res, nil
}

// evictLRU deletes a tenant's least recently used rows of a status past max.
func evictLRU(ctx context.Context, tx pgx.Tx, keyHash [32]byte, status MappingStatus, maxRows int) (int64, error) {
	tag, err := tx.Exec(ctx, `
		DELETE FROM universal_mappings
		WHERE key_hash = $1 AND (source, fingerprint) IN (
			SELECT source, fingerprint FROM universal_mappings
			WHERE key_hash = $1 AND status = $2
			ORDER BY last_used_at DESC, created_at DESC
			OFFSET $3)`, keyHash[:], string(status), maxRows)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *PostgresMappingStore) List(ctx context.Context, keyHash [32]byte) ([]MappingRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key_hash, source, fingerprint, status, mapping, NULL::jsonb, proposal, NULL::jsonb, NULL::jsonb,
			proposer, rev, review_claimed_at, review_sent_at, created_at, decided_at, last_used_at, expires_at
		FROM universal_mappings
		WHERE key_hash = $1 AND `+liveRow+`
		ORDER BY status = 'pending' DESC, last_used_at DESC`, keyHash[:])
	if err != nil {
		return nil, fmt.Errorf("mappings list: %w", err)
	}
	defer rows.Close()
	var out []MappingRow
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, fmt.Errorf("mappings list scan: %w", err)
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mappings list rows: %w", err)
	}
	return out, nil
}

func (s *PostgresMappingStore) Touch(ctx context.Context, k MappingKey) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE universal_mappings SET last_used_at = now()
		WHERE `+byKey+` AND last_used_at < now() - $4::interval`, append(keyArgs(k), TouchInterval)...)
	if err != nil {
		return fmt.Errorf("mappings touch: %w", err)
	}
	return nil
}

// Sweep takes no lock: every statement is idempotent, and a row it deletes
// under a concurrent InsertPending is either an expired one that the insert
// replaces anyway or an idle decided one the insert never looks at.
func (s *PostgresMappingStore) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	tag, err := s.pool.Exec(ctx, `DELETE FROM universal_mappings WHERE status = 'pending' AND expires_at <= now()`)
	if err != nil {
		return res, fmt.Errorf("mappings sweep pending: %w", err)
	}
	res.Pending = tag.RowsAffected()

	tag, err = s.pool.Exec(ctx, `
		DELETE FROM universal_mappings WHERE status <> 'pending' AND last_used_at < now() - $1::interval`, IdleTTL)
	if err != nil {
		return res, fmt.Errorf("mappings sweep idle: %w", err)
	}
	res.Idle = tag.RowsAffected()

	tag, err = s.pool.Exec(ctx, `
		UPDATE universal_mappings SET samples = NULL
		WHERE samples IS NOT NULL AND created_at < now() - $1::interval`, SamplesTTL)
	if err != nil {
		return res, fmt.Errorf("mappings sweep samples: %w", err)
	}
	res.Samples = tag.RowsAffected()
	return res, nil
}

func (s *PostgresMappingStore) CountByStatus(ctx context.Context) (map[MappingStatus]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM universal_mappings WHERE `+liveRow+` GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("mappings count: %w", err)
	}
	defer rows.Close()
	out := map[MappingStatus]int64{}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("mappings count scan: %w", err)
		}
		out[MappingStatus(status)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mappings count rows: %w", err)
	}
	return out, nil
}
