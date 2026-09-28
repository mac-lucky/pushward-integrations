package state

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MappingStatus is where a universal webhook mapping stands in its review.
type MappingStatus string

const (
	// MappingPending is a proposal waiting for the user. It expires after
	// PendingTTL, like the review links that decide it.
	MappingPending MappingStatus = "pending"
	// MappingConfirmed is a mapping the user accepted or edited.
	MappingConfirmed MappingStatus = "confirmed"
	// MappingRejected sends the shape as a raw notification instead.
	MappingRejected MappingStatus = "rejected"
)

// Caps and lifetimes of universal_mappings rows. They bound what one tenant can
// make the relay store, so they are constants rather than config: a tenant has
// at most MaxPending+MaxConfirmed+MaxRejected rows.
const (
	MaxPending   = 5
	MaxConfirmed = 32
	MaxRejected  = 32

	// PendingTTL is how long a proposal waits for a decision.
	PendingTTL = 7 * 24 * time.Hour
	// IdleTTL is how long a decided mapping survives without a webhook using it.
	IdleTTL = 180 * 24 * time.Hour
	// SamplesTTL is how long the redacted sample values are kept for the editor.
	SamplesTTL = 7 * 24 * time.Hour

	// ReviewClaimTTL is how long a claim on sending the review holds. A replica
	// that claimed and then died leaves the review to the next event after it.
	ReviewClaimTTL = 5 * time.Minute
	// TouchInterval is how stale last_used_at gets before Touch writes it, so a
	// busy shape costs one write an hour rather than one per webhook.
	TouchInterval = time.Hour
)

// ErrPendingCap is InsertPending refusing a proposal because the tenant already
// has MaxPending of them waiting.
var ErrPendingCap = errors.New("state: pending mapping cap reached")

// MappingKey identifies a mapping: one tenant (auth.UniversalDigest of its
// key), one ?source= value and one payload shape.
type MappingKey struct {
	KeyHash     [32]byte
	Source      string
	Fingerprint [32]byte
}

// MappingRow is one stored mapping. Mapping, Shape, Proposal and Candidates
// hold paths and vocabulary words only. Samples holds a display value for
// every field of the payload the mapping was proposed from, correlation
// included, each redacted by universal.Display, and is dropped after
// SamplesTTL.
type MappingRow struct {
	MappingKey
	Status MappingStatus
	// Mapping is the universal.Mapping in force, Shape the universal.Shape it
	// validates against, and Proposal the universal.Proposal first made for
	// the shape, kept to measure how often proposals are accepted as they are.
	Mapping    json.RawMessage
	Shape      json.RawMessage
	Proposal   json.RawMessage
	Samples    json.RawMessage
	Candidates json.RawMessage
	// Proposer names what made the mapping: "heuristic/1", a ranker, or
	// "user-edit".
	Proposer string
	// Rev counts proposals, decisions and edits over the life of the row; it
	// never restarts while the row exists, even when a proposal replaces an
	// expired or unreadable one, so a stale editor form cannot overwrite a
	// newer one. A deleted row's successor starts again at 0; see RowVersion.
	Rev             int
	ReviewClaimedAt *time.Time
	ReviewSentAt    *time.Time
	CreatedAt       time.Time
	DecidedAt       *time.Time
	LastUsedAt      time.Time
	// ExpiresAt is set exactly while the row is pending.
	ExpiresAt *time.Time
}

// RowVersion names one state of a row, as an editor form carries it. Rev
// alone is not enough: a row that is deleted (swept, evicted) and proposed
// again starts over at rev 0, so CreatedAt tells the incarnations apart.
// CreatedAt counts to the microsecond, the precision Postgres keeps.
type RowVersion struct {
	Rev       int
	CreatedAt time.Time
}

// Version returns the row's current version.
func (r *MappingRow) Version() RowVersion {
	return RowVersion{Rev: r.Rev, CreatedAt: r.CreatedAt}
}

// sameIncarnation compares created_at to the microsecond.
func sameIncarnation(a, b time.Time) bool {
	return a.UnixMicro() == b.UnixMicro()
}

// DecideOutcome is what Decide made of a review tap.
type DecideOutcome string

const (
	DecideOK DecideOutcome = "ok"
	// DecideIdempotent is the same decision again, a double tap.
	DecideIdempotent DecideOutcome = "idempotent"
	// DecideConflict is the opposite of a decision already made. Only the
	// first one counts; changing it is the editor's job.
	DecideConflict DecideOutcome = "conflict"
	// DecideNotFound is a row that is gone, or pending past its expiry.
	DecideNotFound DecideOutcome = "notfound"
)

// DecideResult reports a Decide call. Evicted counts the least recently used
// rows removed to keep the decided status under its cap.
type DecideResult struct {
	Outcome DecideOutcome
	Evicted int64
}

// EditOutcome is what Edit made of a saved editor form.
type EditOutcome string

const (
	EditOK EditOutcome = "ok"
	// EditIdempotent is a stale form whose content is already stored, a
	// resubmitted save.
	EditIdempotent EditOutcome = "idempotent"
	// EditStale is a form made from an older revision than the stored one,
	// or from an earlier incarnation of the row.
	EditStale    EditOutcome = "stale"
	EditNotFound EditOutcome = "notfound"
)

// EditResult reports an Edit call. Rev is the stored revision afterwards.
type EditResult struct {
	Outcome EditOutcome
	Rev     int
	Evicted int64
}

// SweepResult counts what one Sweep removed.
type SweepResult struct {
	// Pending rows that expired undecided.
	Pending int64
	// Idle decided rows unused for IdleTTL.
	Idle int64
	// Samples dropped from rows older than SamplesTTL.
	Samples int64
}

// MappingStore keeps universal webhook mappings. Writes that change which rows
// exist for a tenant (InsertPending, ReplaceStale, Decide, Edit) serialize per
// tenant, so the caps hold across replicas.
type MappingStore interface {
	// Get returns the row for k, or nil. A pending row past its expiry reads
	// as absent.
	Get(ctx context.Context, k MappingKey) (*MappingRow, error)

	// GetForDelivery is Get for the webhook path: it fills only Status,
	// Mapping, Rev, ReviewSentAt, CreatedAt and ExpiresAt, and leaves out
	// the large columns.
	GetForDelivery(ctx context.Context, k MappingKey) (*MappingRow, error)

	// InsertPending stores row as a new pending proposal, claimed for review
	// by the caller. It reports false when a live row for the key already
	// exists, which is another replica winning the race, and ErrPendingCap
	// when the tenant has maxPending proposals waiting. It may replace an
	// expired pending row.
	InsertPending(ctx context.Context, row *MappingRow, maxPending int) (bool, error)

	// ReplaceStale stores row as a new pending proposal in place of the live
	// row for its key, one the caller could not read, if that row is still at
	// staleRev. The pending cap applies as for InsertPending, not counting
	// the row replaced. It reports false when the row changed or is gone.
	ReplaceStale(ctx context.Context, row *MappingRow, staleRev, maxPending int) (bool, error)

	// ClaimReview claims sending the review of a pending row that has none
	// sent and no claim younger than ReviewClaimTTL.
	ClaimReview(ctx context.Context, k MappingKey) (bool, error)
	// MarkReviewSent records that the review went out.
	MarkReviewSent(ctx context.Context, k MappingKey) error
	// ReleaseReview drops a claim so the next event can try again.
	ReleaseReview(ctx context.Context, k MappingKey) error

	// Decide confirms or rejects the pending row that expires at expires, the
	// expiry its review link carries, then evicts the least recently used
	// rows of that status past maxDecided. A link for an earlier proposal of
	// the same shape, which expired at another time, finds nothing.
	Decide(ctx context.Context, k MappingKey, status MappingStatus, expires time.Time, maxDecided int) (DecideResult, error)

	// Edit stores a user's mapping with status confirmed or rejected, when the
	// stored row is still at version at, then evicts as Decide does. A form
	// from another incarnation of the row is stale even when its content
	// matches.
	Edit(ctx context.Context, k MappingKey, mapping json.RawMessage, status MappingStatus, at RowVersion, maxDecided int) (EditResult, error)

	// List returns a tenant's live rows, pending first, then by last use. It
	// leaves out Shape, Samples and Candidates; Get a row for those. The caps
	// bound it to MaxPending+MaxConfirmed+MaxRejected rows.
	List(ctx context.Context, keyHash [32]byte) ([]MappingRow, error)

	// Touch records a use of the row, at most once per TouchInterval.
	Touch(ctx context.Context, k MappingKey) error

	// Sweep deletes expired pending rows and decided rows idle past IdleTTL,
	// and drops samples older than SamplesTTL.
	Sweep(ctx context.Context) (SweepResult, error)

	// CountByStatus counts live rows across all tenants.
	CountByStatus(ctx context.Context) (map[MappingStatus]int64, error)
}

// DecidedCap returns the row cap of a decided status.
func DecidedCap(s MappingStatus) int {
	if s == MappingRejected {
		return MaxRejected
	}
	return MaxConfirmed
}

func checkDecided(s MappingStatus) error {
	if s != MappingConfirmed && s != MappingRejected {
		return fmt.Errorf("state: status %q is not a decision", s)
	}
	return nil
}

// keyLockID is the advisory lock object for a tenant: the first four bytes of
// its key hash. Tenants that share them only share a lock.
func keyLockID(keyHash [32]byte) int32 {
	return int32(binary.BigEndian.Uint32(keyHash[:4])) // #nosec G115 -- a bit pattern, not a quantity
}
