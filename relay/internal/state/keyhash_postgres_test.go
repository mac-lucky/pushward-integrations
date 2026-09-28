//go:build integration

package state_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
)

// countRows counts relay_state rows, and those whose user_key is a raw key.
func countRows(t *testing.T, pool *pgxpool.Pool) (total, raw int) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT count(*), count(*) FILTER (WHERE starts_with(user_key, 'hlk_'))
		FROM relay_state
	`).Scan(&total, &raw)
	if err != nil {
		t.Fatal("count rows:", err)
	}
	return total, raw
}

// writeTenants writes a single row and a two-row group for each tenant.
func writeTenants(t *testing.T, s state.Store, tenants []string) {
	t.Helper()
	ctx := context.Background()
	for _, k := range tenants {
		if err := s.Set(ctx, "argocd", k, "app", "", json.RawMessage(`{"step":1}`), time.Hour); err != nil {
			t.Fatal(err)
		}
		for _, sub := range []string{"run", "job:1"} {
			if err := s.Set(ctx, "gitea", k, "slug", sub, json.RawMessage(`{"sub":"`+sub+`"}`), time.Hour); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestPostgres_HashedModeStoresNoRawKeys(t *testing.T) {
	pg, pool := setupPostgresPool(t)
	ctx := context.Background()
	s := state.KeyHashing(pg, state.KeyModeHashed)
	tenants := []string{"hlk_one", "hlk_two"}

	writeTenants(t, s, tenants)
	if err := s.Delete(ctx, "argocd", "hlk_two", "app", ""); err != nil {
		t.Fatal(err)
	}

	total, raw := countRows(t, pool)
	if raw != 0 {
		t.Fatalf("%d of %d rows carry a raw hlk_ key", raw, total)
	}
	if total != 5 {
		t.Fatalf("expected 5 rows (3 for hlk_one, 2 for hlk_two), got %d", total)
	}

	got, err := s.Get(ctx, "argocd", "hlk_one", "app", "")
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, got, json.RawMessage(`{"step":1}`))
	group, err := s.GetGroup(ctx, "gitea", "hlk_two", "slug")
	if err != nil {
		t.Fatal(err)
	}
	if len(group) != 2 {
		t.Fatalf("expected a 2-row group, got %d", len(group))
	}
}

// The rollout's second step: rows a compat replica wrote under the raw key stay
// readable to a hashed replica, and each hashed write moves a row over.
func TestPostgres_CompatToHashedTransition(t *testing.T) {
	pg, pool := setupPostgresPool(t)
	ctx := context.Background()
	compat := state.KeyHashing(pg, state.KeyModeCompat)
	hashed := state.KeyHashing(pg, state.KeyModeHashed)
	tenants := []string{"hlk_one", "hlk_two"}

	writeTenants(t, compat, tenants)
	if total, raw := countRows(t, pool); raw != total || total != 6 {
		t.Fatalf("compat should write 6 raw rows, got %d raw of %d", raw, total)
	}

	for _, k := range tenants {
		got, err := hashed.Get(ctx, "argocd", k, "app", "")
		if err != nil {
			t.Fatal(err)
		}
		jsonEq(t, got, json.RawMessage(`{"step":1}`))
		ok, err := hashed.Exists(ctx, "argocd", k, "app", "")
		if err != nil || !ok {
			t.Fatalf("hashed Exists(%s) = %v, %v", k, ok, err)
		}
		group, err := hashed.GetGroup(ctx, "gitea", k, "slug")
		if err != nil {
			t.Fatal(err)
		}
		if len(group) != 2 {
			t.Fatalf("hashed GetGroup(%s) = %d rows, want 2", k, len(group))
		}
	}

	// Half a group moved over: GetGroup still returns it whole, with the
	// hashed write winning.
	if err := hashed.Set(ctx, "gitea", "hlk_one", "slug", "run", json.RawMessage(`{"v":2}`), time.Hour); err != nil {
		t.Fatal(err)
	}
	group, err := hashed.GetGroup(ctx, "gitea", "hlk_one", "slug")
	if err != nil {
		t.Fatal(err)
	}
	if len(group) != 2 {
		t.Fatalf("mixed group has %d rows, want 2", len(group))
	}
	jsonEq(t, group["run"], json.RawMessage(`{"v":2}`))

	// Rewriting everything through the hashed store leaves no raw row.
	writeTenants(t, hashed, tenants)
	if total, raw := countRows(t, pool); raw != 0 || total != 6 {
		t.Fatalf("after the hashed rewrite: %d raw of %d rows, want 0 of 6", raw, total)
	}
	got, err := compat.Get(ctx, "argocd", "hlk_two", "app", "")
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, got, json.RawMessage(`{"step":1}`))
}
