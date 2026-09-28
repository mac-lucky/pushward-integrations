package state

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
)

// The memory store stands in for Postgres in the handler tests, so it has to
// keep the same rules; mappings_postgres_test.go runs the same cases against
// the real table.

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newMemMappings() (*MemoryMappingStore, *clock) {
	c := &clock{t: time.Unix(1_790_000_000, 0)}
	s := NewMemoryMappingStore()
	s.now = c.now
	return s, c
}

func memKey(fp byte) MappingKey {
	k := MappingKey{KeyHash: auth.KeyDigest("hlk_mem"), Source: "ci"}
	k.Fingerprint[0] = fp
	return k
}

func memRow(k MappingKey, now time.Time) *MappingRow {
	exp := now.Add(PendingTTL)
	return &MappingRow{
		MappingKey: k,
		Mapping:    json.RawMessage(`{"v":1}`),
		Shape:      json.RawMessage(`{}`),
		Proposal:   json.RawMessage(`{}`),
		Samples:    json.RawMessage(`{"a":"b"}`),
		Proposer:   "heuristic/1",
		ExpiresAt:  &exp,
	}
}

func TestMemoryMappings_Cap(t *testing.T) {
	s, c := newMemMappings()
	ctx := context.Background()
	for i := range MaxPending {
		if ok, err := s.InsertPending(ctx, memRow(memKey(byte(i)), c.t), MaxPending); !ok || err != nil {
			t.Fatalf("insert %d = %v, %v", i, ok, err)
		}
	}
	if ok, _ := s.InsertPending(ctx, memRow(memKey(0), c.t), MaxPending); ok {
		t.Error("a live row was inserted twice")
	}
	if _, err := s.InsertPending(ctx, memRow(memKey(99), c.t), MaxPending); !errors.Is(err, ErrPendingCap) {
		t.Errorf("sixth insert: %v, want ErrPendingCap", err)
	}
	// Past their expiry the rows no longer count, and one can be replaced.
	c.advance(PendingTTL)
	if ok, err := s.InsertPending(ctx, memRow(memKey(0), c.t), MaxPending); !ok || err != nil {
		t.Errorf("insert over an expired row = %v, %v", ok, err)
	}
}

func TestMemoryMappings_DecideAndEvict(t *testing.T) {
	s, c := newMemMappings()
	ctx := context.Background()
	for i := range MaxConfirmed + 1 {
		k := memKey(byte(i))
		if ok, err := s.InsertPending(ctx, memRow(k, c.t), MaxPending); !ok || err != nil {
			t.Fatalf("insert %d = %v, %v", i, ok, err)
		}
		c.advance(time.Second)
		res, err := s.Decide(ctx, k, MappingConfirmed, memExp(s, k), MaxConfirmed)
		if err != nil || res.Outcome != DecideOK {
			t.Fatalf("Decide %d = %+v, %v", i, res, err)
		}
		if want := int64(0); i == MaxConfirmed {
			want = 1
			if res.Evicted != want {
				t.Errorf("Decide %d evicted %d, want %d", i, res.Evicted, want)
			}
		}
	}
	if r, _ := s.Get(ctx, memKey(0)); r != nil {
		t.Error("the least recently used mapping survived")
	}
	if res, _ := s.Decide(ctx, memKey(1), MappingConfirmed, memExp(s, memKey(1)), MaxConfirmed); res.Outcome != DecideIdempotent {
		t.Errorf("repeat = %s", res.Outcome)
	}
	if res, _ := s.Decide(ctx, memKey(1), MappingRejected, memExp(s, memKey(1)), MaxRejected); res.Outcome != DecideConflict {
		t.Errorf("opposite = %s", res.Outcome)
	}
	if res, _ := s.Decide(ctx, memKey(200), MappingRejected, memExp(s, memKey(200)), MaxRejected); res.Outcome != DecideNotFound {
		t.Errorf("no row = %s", res.Outcome)
	}
}

func TestMemoryMappings_Edit(t *testing.T) {
	s, c := newMemMappings()
	ctx := context.Background()
	k := memKey(1)
	_, _ = s.InsertPending(ctx, memRow(k, c.t), MaxPending)
	m := json.RawMessage(`{"v":1,"k":"alert"}`)
	if res, _ := s.Edit(ctx, k, m, MappingConfirmed, 0, MaxConfirmed); res.Outcome != EditOK || res.Rev != 1 {
		t.Errorf("edit = %+v", res)
	}
	if res, _ := s.Edit(ctx, k, json.RawMessage(`{"k":"alert","v":1}`), MappingConfirmed, 0, MaxConfirmed); res.Outcome != EditIdempotent {
		t.Errorf("resubmit = %+v", res)
	}
	if res, _ := s.Edit(ctx, k, json.RawMessage(`{"v":1}`), MappingConfirmed, 0, MaxConfirmed); res.Outcome != EditStale {
		t.Errorf("stale = %+v", res)
	}
	if r, _ := s.Get(ctx, k); r.Proposer != "user-edit" || r.Status != MappingConfirmed {
		t.Errorf("row = %+v", r)
	}
}

func TestMemoryMappings_ReviewAndSweep(t *testing.T) {
	s, c := newMemMappings()
	ctx := context.Background()
	k := memKey(1)
	_, _ = s.InsertPending(ctx, memRow(k, c.t), MaxPending)
	if ok, _ := s.ClaimReview(ctx, k); ok {
		t.Error("the inserter holds the claim")
	}
	c.advance(ReviewClaimTTL + time.Second)
	if ok, _ := s.ClaimReview(ctx, k); !ok {
		t.Error("a stale claim is taken over")
	}
	_ = s.MarkReviewSent(ctx, k)
	_ = s.ReleaseReview(ctx, k)
	if ok, _ := s.ClaimReview(ctx, k); ok {
		t.Error("a sent review is claimed again")
	}

	d := memKey(2)
	_, _ = s.InsertPending(ctx, memRow(d, c.t), MaxPending)
	_, _ = s.Decide(ctx, d, MappingConfirmed, memExp(s, d), MaxConfirmed)
	c.advance(SamplesTTL + time.Second)
	if res, _ := s.Sweep(ctx); res != (SweepResult{Pending: 1, Samples: 1}) {
		t.Errorf("Sweep = %+v", res)
	}
	c.advance(IdleTTL)
	if res, _ := s.Sweep(ctx); res != (SweepResult{Idle: 1}) {
		t.Errorf("Sweep = %+v", res)
	}
	if n := len(s.Rows()); n != 0 {
		t.Errorf("%d rows left", n)
	}
}

// memExp is the expiry a review token for k's current row would carry.
func memExp(s MappingStore, k MappingKey) time.Time {
	r, _ := s.Get(context.Background(), k)
	if r == nil || r.ExpiresAt == nil {
		return time.Time{}
	}
	return *r.ExpiresAt
}
