package state

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"
)

// MemoryMappingStore implements MappingStore in memory, with the Postgres
// store's semantics and caps. Used for tests.
type MemoryMappingStore struct {
	mu   sync.Mutex
	rows map[MappingKey]*MappingRow
	now  func() time.Time
}

var _ MappingStore = (*MemoryMappingStore)(nil)

// NewMemoryMappingStore creates an empty in-memory mapping store.
func NewMemoryMappingStore() *MemoryMappingStore {
	return &MemoryMappingStore{rows: make(map[MappingKey]*MappingRow), now: time.Now}
}

func (s *MemoryMappingStore) live(r *MappingRow, now time.Time) bool {
	return r.Status != MappingPending || r.ExpiresAt != nil && r.ExpiresAt.After(now)
}

func (s *MemoryMappingStore) Get(_ context.Context, k MappingKey) (*MappingRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[k]
	if !ok || !s.live(r, s.now()) {
		return nil, nil
	}
	return cloneRow(r), nil
}

// GetForDelivery returns the whole row; there is nothing to save in memory.
func (s *MemoryMappingStore) GetForDelivery(ctx context.Context, k MappingKey) (*MappingRow, error) {
	return s.Get(ctx, k)
}

func (s *MemoryMappingStore) InsertPending(_ context.Context, row *MappingRow, maxPending int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	old, ok := s.rows[row.MappingKey]
	if ok && s.live(old, now) {
		return false, nil
	}
	if s.pending(row.MappingKey, now) >= maxPending {
		return false, ErrPendingCap
	}
	rev := 0
	if ok {
		rev = old.Rev + 1
	}
	s.propose(row, rev, now)
	return true, nil
}

func (s *MemoryMappingStore) ReplaceStale(_ context.Context, row *MappingRow, staleRev, maxPending int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	old, ok := s.rows[row.MappingKey]
	if !ok || !s.live(old, now) || old.Rev != staleRev {
		return false, nil
	}
	if s.pending(row.MappingKey, now) >= maxPending {
		return false, ErrPendingCap
	}
	s.propose(row, old.Rev+1, now)
	return true, nil
}

// pending counts the tenant's live pending rows other than k.
func (s *MemoryMappingStore) pending(k MappingKey, now time.Time) int {
	n := 0
	for _, r := range s.rows {
		if r.KeyHash == k.KeyHash && r.MappingKey != k && r.Status == MappingPending && s.live(r, now) {
			n++
		}
	}
	return n
}

// propose stores row as a fresh pending proposal at rev, claimed for review.
func (s *MemoryMappingStore) propose(row *MappingRow, rev int, now time.Time) {
	r := cloneRow(row)
	r.Status = MappingPending
	r.Rev = rev
	r.ReviewClaimedAt = &now
	r.ReviewSentAt = nil
	r.CreatedAt = now
	r.DecidedAt = nil
	r.LastUsedAt = now
	s.rows[row.MappingKey] = r
}

func (s *MemoryMappingStore) ClaimReview(_ context.Context, k MappingKey) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	r, ok := s.rows[k]
	if !ok || r.Status != MappingPending || !s.live(r, now) || r.ReviewSentAt != nil {
		return false, nil
	}
	if r.ReviewClaimedAt != nil && !r.ReviewClaimedAt.Before(now.Add(-ReviewClaimTTL)) {
		return false, nil
	}
	r.ReviewClaimedAt = &now
	return true, nil
}

func (s *MemoryMappingStore) MarkReviewSent(_ context.Context, k MappingKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[k]; ok {
		now := s.now()
		r.ReviewSentAt = &now
	}
	return nil
}

func (s *MemoryMappingStore) ReleaseReview(_ context.Context, k MappingKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[k]; ok && r.ReviewSentAt == nil {
		r.ReviewClaimedAt = nil
	}
	return nil
}

func (s *MemoryMappingStore) Decide(_ context.Context, k MappingKey, status MappingStatus, expires time.Time, maxDecided int) (DecideResult, error) {
	if err := checkDecided(status); err != nil {
		return DecideResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	r, ok := s.rows[k]
	switch {
	case !ok || !s.live(r, now), r.Status == MappingPending && !r.ExpiresAt.Equal(expires):
		return DecideResult{Outcome: DecideNotFound}, nil
	case r.Status == status:
		return DecideResult{Outcome: DecideIdempotent}, nil
	case r.Status != MappingPending:
		return DecideResult{Outcome: DecideConflict}, nil
	}
	s.decide(r, status, now)
	return DecideResult{Outcome: DecideOK, Evicted: s.evict(k.KeyHash, status, maxDecided)}, nil
}

func (s *MemoryMappingStore) Edit(_ context.Context, k MappingKey, mapping json.RawMessage, status MappingStatus, rev, maxDecided int) (EditResult, error) {
	if err := checkDecided(status); err != nil {
		return EditResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	r, ok := s.rows[k]
	if !ok || !s.live(r, now) {
		return EditResult{Outcome: EditNotFound}, nil
	}
	if r.Rev != rev {
		out := EditStale
		if r.Status == status && jsonEqual(r.Mapping, mapping) {
			out = EditIdempotent
		}
		return EditResult{Outcome: out, Rev: r.Rev}, nil
	}
	r.Mapping = bytes.Clone(mapping)
	r.Proposer = "user-edit"
	s.decide(r, status, now)
	return EditResult{Outcome: EditOK, Rev: r.Rev, Evicted: s.evict(k.KeyHash, status, maxDecided)}, nil
}

func (s *MemoryMappingStore) decide(r *MappingRow, status MappingStatus, now time.Time) {
	r.Status = status
	r.DecidedAt = &now
	r.LastUsedAt = now
	r.ExpiresAt = nil
	r.Rev++
}

// evict drops the least recently used rows of a status past maxDecided.
func (s *MemoryMappingStore) evict(keyHash [32]byte, status MappingStatus, maxDecided int) int64 {
	var rows []*MappingRow
	for _, r := range s.rows {
		if r.KeyHash == keyHash && r.Status == status {
			rows = append(rows, r)
		}
	}
	if len(rows) <= maxDecided {
		return 0
	}
	slices.SortFunc(rows, func(a, b *MappingRow) int {
		return cmp.Or(b.LastUsedAt.Compare(a.LastUsedAt), b.CreatedAt.Compare(a.CreatedAt))
	})
	for _, r := range rows[maxDecided:] {
		delete(s.rows, r.MappingKey)
	}
	return int64(len(rows) - maxDecided)
}

func (s *MemoryMappingStore) List(_ context.Context, keyHash [32]byte) ([]MappingRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var out []MappingRow
	for _, r := range s.rows {
		if r.KeyHash == keyHash && s.live(r, now) {
			c := cloneRow(r)
			c.Shape, c.Samples, c.Candidates = nil, nil, nil
			out = append(out, *c)
		}
	}
	decided := func(r MappingRow) bool { return r.Status != MappingPending }
	slices.SortFunc(out, func(a, b MappingRow) int {
		if decided(a) != decided(b) {
			if decided(a) {
				return 1
			}
			return -1
		}
		return b.LastUsedAt.Compare(a.LastUsedAt)
	})
	return out, nil
}

func (s *MemoryMappingStore) Touch(_ context.Context, k MappingKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if r, ok := s.rows[k]; ok && r.LastUsedAt.Before(now.Add(-TouchInterval)) {
		r.LastUsedAt = now
	}
	return nil
}

func (s *MemoryMappingStore) Sweep(context.Context) (SweepResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var res SweepResult
	for k, r := range s.rows {
		switch {
		case r.Status == MappingPending && !s.live(r, now):
			delete(s.rows, k)
			res.Pending++
			continue
		case r.Status != MappingPending && r.LastUsedAt.Before(now.Add(-IdleTTL)):
			delete(s.rows, k)
			res.Idle++
			continue
		}
		if r.Samples != nil && r.CreatedAt.Before(now.Add(-SamplesTTL)) {
			r.Samples = nil
			res.Samples++
		}
	}
	return res, nil
}

func (s *MemoryMappingStore) CountByStatus(context.Context) (map[MappingStatus]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := map[MappingStatus]int64{}
	for _, r := range s.rows {
		if s.live(r, now) {
			out[r.Status]++
		}
	}
	return out, nil
}

// Rows returns every stored row, expired ones included, with all columns. It
// is for tests that assert on what was written.
func (s *MemoryMappingStore) Rows() []MappingRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MappingRow, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, *cloneRow(r))
	}
	return out
}

func cloneRow(r *MappingRow) *MappingRow {
	c := *r
	c.Mapping = bytes.Clone(r.Mapping)
	c.Shape = bytes.Clone(r.Shape)
	c.Proposal = bytes.Clone(r.Proposal)
	c.Samples = bytes.Clone(r.Samples)
	c.Candidates = bytes.Clone(r.Candidates)
	return &c
}

// jsonEqual compares two JSON documents by value, as jsonb equality does.
func jsonEqual(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	ja, _ := json.Marshal(va)
	jb, _ := json.Marshal(vb)
	return bytes.Equal(ja, jb)
}
