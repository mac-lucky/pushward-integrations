//go:build integration

package state_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
)

func setupMappings(t *testing.T) (*state.PostgresMappingStore, *pgxpool.Pool) {
	t.Helper()
	_, pool := setupPostgresPool(t)
	ms, err := state.NewMappingStore(context.Background(), pool)
	if err != nil {
		t.Fatal("new mapping store:", err)
	}
	// A second migration, as a second replica starting, is a no-op.
	if _, err := state.NewMappingStore(context.Background(), pool); err != nil {
		t.Fatal("second migration:", err)
	}
	return ms, pool
}

func mappingKey(tenant string, fp byte) state.MappingKey {
	k := state.MappingKey{KeyHash: auth.KeyDigest(tenant), Source: "ci"}
	k.Fingerprint[0] = fp
	return k
}

func pendingRow(k state.MappingKey) *state.MappingRow {
	exp := time.Unix(time.Now().Add(state.PendingTTL).Unix(), 0)
	return &state.MappingRow{
		MappingKey: k,
		Mapping:    json.RawMessage(`{"v":1,"k":"notification","p":{"title":"title"}}`),
		Shape:      json.RawMessage(`{"v":1,"fields":[{"p":"title","t":"string","c":"text","n":5,"w":1}]}`),
		Proposal:   json.RawMessage(`{"title":"title","kind":"notification"}`),
		Samples:    json.RawMessage(`{"title":"Hello"}`),
		Candidates: json.RawMessage(`{"title":["title"]}`),
		Proposer:   "heuristic/1",
		ExpiresAt:  &exp,
	}
}

func mustInsert(t *testing.T, ms state.MappingStore, k state.MappingKey) {
	t.Helper()
	ok, err := ms.InsertPending(context.Background(), pendingRow(k), state.MaxPending)
	if err != nil || !ok {
		t.Fatalf("InsertPending = %v, %v", ok, err)
	}
}

// sameJSON compares documents by value; jsonb reorders object keys.
func sameJSON(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("got %s, want %s", got, want)
	}
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func TestMappings_RoundTrip(t *testing.T) {
	ms, _ := setupMappings(t)
	ctx := context.Background()
	k := mappingKey("hlk_a", 1)
	want := pendingRow(k)
	mustInsert(t, ms, k)

	got, err := ms.Get(ctx, k)
	if err != nil || got == nil {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if got.MappingKey != k || got.Status != state.MappingPending || got.Rev != 0 || got.Proposer != "heuristic/1" {
		t.Errorf("row = %+v", got)
	}
	if !got.ExpiresAt.Equal(*want.ExpiresAt) {
		t.Errorf("expires_at = %s, want %s exactly: review tokens carry it", got.ExpiresAt, want.ExpiresAt)
	}
	if got.ReviewClaimedAt == nil || got.ReviewSentAt != nil || got.DecidedAt != nil {
		t.Errorf("an inserted row is claimed for review and nothing else: %+v", got)
	}
	sameJSON(t, got.Samples, want.Samples)
	sameJSON(t, got.Candidates, want.Candidates)

	if r, _ := ms.Get(ctx, mappingKey("hlk_a", 2)); r != nil {
		t.Error("Get of another fingerprint found a row")
	}
	if r, _ := ms.Get(ctx, mappingKey("hlk_b", 1)); r != nil {
		t.Error("Get of another tenant found a row")
	}
	list, err := ms.List(ctx, k.KeyHash)
	if err != nil || len(list) != 1 || list[0].Shape != nil || list[0].Samples != nil || list[0].Mapping == nil {
		t.Errorf("List = %+v, %v", list, err)
	}
}

func TestMappings_InsertPendingRace(t *testing.T) {
	ms, _ := setupMappings(t)
	k := mappingKey("hlk_race", 1)

	const n = 10
	var wg sync.WaitGroup
	results := make(chan bool, n)
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			ok, err := ms.InsertPending(context.Background(), pendingRow(k), state.MaxPending)
			results <- ok
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	won := 0
	for ok := range results {
		if ok {
			won++
		}
	}
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if won != 1 {
		t.Errorf("%d inserts won, want exactly 1", won)
	}
}

func TestMappings_PendingCapUnderConcurrency(t *testing.T) {
	ms, _ := setupMappings(t)

	const n = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	inserted, capped := 0, 0
	for i := range n {
		wg.Go(func() {
			ok, err := ms.InsertPending(context.Background(), pendingRow(mappingKey("hlk_cap", byte(i))), state.MaxPending) // #nosec G115 -- i < 10
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, state.ErrPendingCap):
				capped++
			case err != nil:
				t.Error(err)
			case ok:
				inserted++
			}
		})
	}
	wg.Wait()
	if inserted != state.MaxPending || capped != n-state.MaxPending {
		t.Errorf("%d inserted, %d capped; want %d and %d", inserted, capped, state.MaxPending, n-state.MaxPending)
	}
	counts, err := ms.CountByStatus(context.Background())
	if err != nil || counts[state.MappingPending] != state.MaxPending {
		t.Errorf("CountByStatus = %v, %v", counts, err)
	}
	// Another tenant has a cap of its own.
	mustInsert(t, ms, mappingKey("hlk_other", 1))
}

func TestMappings_ExpiredPendingIsReplaced(t *testing.T) {
	ms, pool := setupMappings(t)
	ctx := context.Background()
	k := mappingKey("hlk_exp", 1)
	mustInsert(t, ms, k)
	if _, err := ms.Decide(ctx, k, state.MappingConfirmed, rowExp(ms, k), state.MaxConfirmed); err != nil {
		t.Fatal(err)
	}
	// A decided row is never replaced.
	if ok, err := ms.InsertPending(ctx, pendingRow(k), state.MaxPending); ok || err != nil {
		t.Errorf("InsertPending over a decided row = %v, %v", ok, err)
	}

	k2 := mappingKey("hlk_exp", 2)
	mustInsert(t, ms, k2)
	exec(t, pool, `UPDATE universal_mappings SET expires_at = now() - interval '1 second', review_sent_at = now() WHERE fingerprint = $1`, k2.Fingerprint[:])
	if r, _ := ms.Get(ctx, k2); r != nil {
		t.Fatal("an expired pending row still reads")
	}
	if res, _ := ms.Decide(ctx, k2, state.MappingConfirmed, rowExp(ms, k2), state.MaxConfirmed); res.Outcome != state.DecideNotFound {
		t.Errorf("Decide on an expired row = %s", res.Outcome)
	}
	mustInsert(t, ms, k2)
	r, _ := ms.Get(ctx, k2)
	// rev keeps counting across the replacement, so an edit form opened on
	// the expired proposal cannot save over this one.
	if r == nil || r.ReviewSentAt != nil || r.Rev != 1 {
		t.Errorf("replaced row = %+v", r)
	}
}

func TestMappings_Review(t *testing.T) {
	ms, pool := setupMappings(t)
	ctx := context.Background()
	k := mappingKey("hlk_review", 1)
	mustInsert(t, ms, k)

	if ok, _ := ms.ClaimReview(ctx, k); ok {
		t.Error("the inserter holds the claim; nobody else may take it")
	}
	exec(t, pool, `UPDATE universal_mappings SET review_claimed_at = now() - interval '6 minutes'`)
	if ok, err := ms.ClaimReview(ctx, k); !ok || err != nil {
		t.Errorf("a stale claim is taken over: %v, %v", ok, err)
	}
	if err := ms.ReleaseReview(ctx, k); err != nil {
		t.Fatal(err)
	}
	if ok, _ := ms.ClaimReview(ctx, k); !ok {
		t.Error("a released claim is free")
	}
	if err := ms.MarkReviewSent(ctx, k); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `UPDATE universal_mappings SET review_claimed_at = NULL`)
	if ok, _ := ms.ClaimReview(ctx, k); ok {
		t.Error("a sent review is never claimed again")
	}
}

func TestMappings_Decide(t *testing.T) {
	ms, _ := setupMappings(t)
	ctx := context.Background()
	k := mappingKey("hlk_decide", 1)
	mustInsert(t, ms, k)

	steps := []struct {
		status state.MappingStatus
		want   state.DecideOutcome
	}{
		{state.MappingConfirmed, state.DecideOK},
		{state.MappingConfirmed, state.DecideIdempotent},
		{state.MappingRejected, state.DecideConflict},
	}
	for _, s := range steps {
		res, err := ms.Decide(ctx, k, s.status, rowExp(ms, k), state.DecidedCap(s.status))
		if err != nil || res.Outcome != s.want {
			t.Errorf("Decide(%s) = %+v, %v; want %s", s.status, res, err, s.want)
		}
	}
	r, _ := ms.Get(ctx, k)
	if r.Status != state.MappingConfirmed || r.Rev != 1 || r.ExpiresAt != nil || r.DecidedAt == nil {
		t.Errorf("decided row = %+v", r)
	}
	if res, _ := ms.Decide(ctx, mappingKey("hlk_decide", 9), state.MappingConfirmed, time.Time{}, state.MaxConfirmed); res.Outcome != state.DecideNotFound {
		t.Errorf("Decide on no row = %s", res.Outcome)
	}
	if _, err := ms.Decide(ctx, k, state.MappingPending, rowExp(ms, k), state.MaxConfirmed); err == nil {
		t.Error("pending is not a decision")
	}
}

// The 33rd confirmed mapping evicts the least recently used one.
func TestMappings_ConfirmedEviction(t *testing.T) {
	ms, pool := setupMappings(t)
	ctx := context.Background()
	for i := range state.MaxConfirmed {
		k := mappingKey("hlk_lru", byte(i))
		mustInsert(t, ms, k)
		res, err := ms.Decide(ctx, k, state.MappingConfirmed, rowExp(ms, k), state.MaxConfirmed)
		if err != nil || res.Outcome != state.DecideOK || res.Evicted != 0 {
			t.Fatalf("Decide %d = %+v, %v", i, res, err)
		}
	}
	oldest := mappingKey("hlk_lru", 7)
	exec(t, pool, `UPDATE universal_mappings SET last_used_at = now() - interval '1 day' WHERE fingerprint = $1`, oldest.Fingerprint[:])

	k := mappingKey("hlk_lru", 200)
	mustInsert(t, ms, k)
	res, err := ms.Decide(ctx, k, state.MappingConfirmed, rowExp(ms, k), state.MaxConfirmed)
	if err != nil || res.Evicted != 1 {
		t.Fatalf("Decide = %+v, %v; want one eviction", res, err)
	}
	if r, _ := ms.Get(ctx, oldest); r != nil {
		t.Error("the least recently used mapping survived")
	}
	if r, _ := ms.Get(ctx, k); r == nil {
		t.Error("the new mapping was evicted")
	}
	counts, _ := ms.CountByStatus(ctx)
	if counts[state.MappingConfirmed] != state.MaxConfirmed {
		t.Errorf("%d confirmed, want %d", counts[state.MappingConfirmed], state.MaxConfirmed)
	}
	list, _ := ms.List(ctx, k.KeyHash)
	if len(list) != state.MaxConfirmed {
		t.Errorf("List has %d rows", len(list))
	}
	// Rejected rows have their own cap.
	r := mappingKey("hlk_lru", 201)
	mustInsert(t, ms, r)
	if res, _ := ms.Decide(ctx, r, state.MappingRejected, rowExp(ms, r), state.MaxRejected); res.Evicted != 0 {
		t.Errorf("a rejection evicted %d confirmed rows", res.Evicted)
	}
}

func TestMappings_Edit(t *testing.T) {
	ms, pool := setupMappings(t)
	ctx := context.Background()
	k := mappingKey("hlk_edit", 1)
	mustInsert(t, ms, k)
	edited := json.RawMessage(`{"v":1,"k":"alert","p":{"title":"title"}}`)
	other := json.RawMessage(`{"v":1,"k":"progress","p":{"title":"title"}}`)
	row, _ := ms.Get(ctx, k)
	v0 := row.Version()
	// A form keeps created_at to the microsecond; what is below it is noise.
	v0.CreatedAt = v0.CreatedAt.Add(999 * time.Nanosecond)

	res, err := ms.Edit(ctx, k, edited, state.MappingConfirmed, v0, state.MaxConfirmed)
	if err != nil || res.Outcome != state.EditOK || res.Rev != 1 {
		t.Fatalf("Edit = %+v, %v", res, err)
	}
	// The same form saved twice.
	if res, _ := ms.Edit(ctx, k, json.RawMessage(`{"k": "alert", "v": 1, "p": {"title": "title"}}`), state.MappingConfirmed, v0, state.MaxConfirmed); res.Outcome != state.EditIdempotent || res.Rev != 1 {
		t.Errorf("resubmit = %+v", res)
	}
	// Another form made from revision 0.
	if res, _ := ms.Edit(ctx, k, other, state.MappingConfirmed, v0, state.MaxConfirmed); res.Outcome != state.EditStale || res.Rev != 1 {
		t.Errorf("stale form = %+v", res)
	}
	v1 := state.RowVersion{Rev: 1, CreatedAt: v0.CreatedAt}
	if res, _ := ms.Edit(ctx, k, other, state.MappingRejected, v1, state.MaxRejected); res.Outcome != state.EditOK || res.Rev != 2 {
		t.Errorf("edit at the current rev = %+v", res)
	}
	r, _ := ms.Get(ctx, k)
	if r.Status != state.MappingRejected || r.Proposer != "user-edit" || r.Rev != 2 {
		t.Errorf("edited row = %+v", r)
	}
	sameJSON(t, r.Mapping, other)
	if res, _ := ms.Edit(ctx, mappingKey("hlk_edit", 2), edited, state.MappingConfirmed, v0, state.MaxConfirmed); res.Outcome != state.EditNotFound {
		t.Errorf("Edit on no row = %+v", res)
	}

	// Deleted (swept, evicted) and proposed again, the row starts over at
	// rev 0. A form from the first one is stale, even with the stored
	// content.
	exec(t, pool, `DELETE FROM universal_mappings WHERE key_hash = $1`, k.KeyHash[:])
	mustInsert(t, ms, k)
	again, _ := ms.Get(ctx, k)
	if again.Rev != 0 || again.CreatedAt.Equal(row.CreatedAt) {
		t.Fatalf("second incarnation rev %d created %v, first created %v", again.Rev, again.CreatedAt, row.CreatedAt)
	}
	if res, _ := ms.Edit(ctx, k, again.Mapping, state.MappingConfirmed, row.Version(), state.MaxConfirmed); res.Outcome != state.EditStale {
		t.Errorf("form from the first incarnation = %+v", res)
	}
	if res, _ := ms.Edit(ctx, k, again.Mapping, state.MappingConfirmed, again.Version(), state.MaxConfirmed); res.Outcome != state.EditOK {
		t.Errorf("form from the second incarnation = %+v", res)
	}
	if res, _ := ms.Edit(ctx, k, again.Mapping, state.MappingConfirmed, row.Version(), state.MaxConfirmed); res.Outcome != state.EditStale {
		t.Errorf("form from the first incarnation after a save = %+v", res)
	}
}

func TestMappings_TouchAndSweep(t *testing.T) {
	ms, pool := setupMappings(t)
	ctx := context.Background()
	expired, idle, old, fresh := mappingKey("hlk_sweep", 1), mappingKey("hlk_sweep", 2), mappingKey("hlk_sweep", 3), mappingKey("hlk_sweep", 4)
	for _, k := range []state.MappingKey{expired, idle, old, fresh} {
		mustInsert(t, ms, k)
	}
	for _, k := range []state.MappingKey{idle, old} {
		if _, err := ms.Decide(ctx, k, state.MappingConfirmed, rowExp(ms, k), state.MaxConfirmed); err != nil {
			t.Fatal(err)
		}
	}
	exec(t, pool, `UPDATE universal_mappings SET expires_at = now() - interval '1 minute' WHERE fingerprint = $1`, expired.Fingerprint[:])
	exec(t, pool, `UPDATE universal_mappings SET last_used_at = now() - interval '181 days' WHERE fingerprint = $1`, idle.Fingerprint[:])
	exec(t, pool, `UPDATE universal_mappings SET created_at = now() - interval '8 days' WHERE fingerprint = $1`, old.Fingerprint[:])

	// Touch within the hour writes nothing; an hour on, it does.
	var before time.Time
	_ = pool.QueryRow(ctx, `SELECT last_used_at FROM universal_mappings WHERE fingerprint = $1`, fresh.Fingerprint[:]).Scan(&before)
	if err := ms.Touch(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	_ = pool.QueryRow(ctx, `SELECT last_used_at FROM universal_mappings WHERE fingerprint = $1`, fresh.Fingerprint[:]).Scan(&after)
	if !after.Equal(before) {
		t.Error("Touch wrote a row used within the hour")
	}
	exec(t, pool, `UPDATE universal_mappings SET last_used_at = now() - interval '2 hours' WHERE fingerprint = $1`, fresh.Fingerprint[:])
	if err := ms.Touch(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `SELECT last_used_at FROM universal_mappings WHERE fingerprint = $1`, fresh.Fingerprint[:]).Scan(&after)
	if time.Since(after) > time.Minute {
		t.Error("Touch did not record a use after an hour")
	}

	res, err := ms.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res != (state.SweepResult{Pending: 1, Idle: 1, Samples: 1}) {
		t.Errorf("Sweep = %+v, want one of each", res)
	}
	if r, _ := ms.Get(ctx, old); r == nil || r.Samples != nil {
		t.Errorf("old row = %+v, want it kept without samples", r)
	}
	if r, _ := ms.Get(ctx, fresh); r == nil || r.Samples == nil {
		t.Error("the fresh row lost its samples")
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM universal_mappings`).Scan(&n)
	if n != 2 {
		t.Errorf("%d rows left, want 2", n)
	}
	if res, _ := ms.Sweep(ctx); res != (state.SweepResult{}) {
		t.Errorf("a second Sweep = %+v, want nothing", res)
	}
}

func TestMappings_NoRawKeys(t *testing.T) {
	ms, pool := setupMappings(t)
	mustInsert(t, ms, mappingKey("hlk_secretkey", 1))
	var n int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM universal_mappings
		WHERE position('hlk_' in encode(key_hash, 'escape') || source || encode(fingerprint, 'escape') || mapping::text
			|| shape::text || proposal::text || proposer || coalesce(samples::text, '') || coalesce(candidates::text, '')) > 0`).Scan(&n)
	if err != nil || n != 0 {
		t.Errorf("%d rows carry hlk_ (%v)", n, err)
	}
}

// The table's own checks back the Go ones.
func TestMappings_Constraints(t *testing.T) {
	_, pool := setupMappings(t)
	ctx := context.Background()
	bad := []string{
		// pending without an expiry
		`INSERT INTO universal_mappings (key_hash, source, fingerprint, status, mapping, shape, proposal, proposer)
		 VALUES (decode(repeat('00', 32), 'hex'), '', decode(repeat('00', 32), 'hex'), 'pending', '{}', '{}', '{}', 'x')`,
		// a source outside the pattern
		`INSERT INTO universal_mappings (key_hash, source, fingerprint, status, mapping, shape, proposal, proposer)
		 VALUES (decode(repeat('00', 32), 'hex'), 'Bad Source', decode(repeat('00', 32), 'hex'), 'confirmed', '{}', '{}', '{}', 'x')`,
		// a short key hash
		`INSERT INTO universal_mappings (key_hash, source, fingerprint, status, mapping, shape, proposal, proposer)
		 VALUES (decode('00', 'hex'), '', decode(repeat('00', 32), 'hex'), 'confirmed', '{}', '{}', '{}', 'x')`,
	}
	for _, sql := range bad {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Errorf("accepted: %s", sql)
		}
	}
}

// rowExp is the expiry a review token for k's current row would carry.
func rowExp(ms state.MappingStore, k state.MappingKey) time.Time {
	r, _ := ms.Get(context.Background(), k)
	if r == nil || r.ExpiresAt == nil {
		return time.Time{}
	}
	return *r.ExpiresAt
}
