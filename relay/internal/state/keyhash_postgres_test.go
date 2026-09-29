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
func writeTenants(t *testing.T, s state.Store, tenants []string, ttl time.Duration) {
	t.Helper()
	ctx := context.Background()
	for _, k := range tenants {
		if err := s.Set(ctx, "argocd", k, "app", "", json.RawMessage(`{"step":1}`), ttl); err != nil {
			t.Fatal(err)
		}
		for _, sub := range []string{"run", "job:1"} {
			if err := s.Set(ctx, "gitea", k, "slug", sub, json.RawMessage(`{"sub":"`+sub+`"}`), ttl); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestPostgres_KeyHashingStoresNoRawKeys(t *testing.T) {
	pg, pool := setupPostgresPool(t)
	ctx := context.Background()
	s := state.KeyHashing(pg)
	tenants := []string{"hlk_one", "hlk_two"}

	writeTenants(t, s, tenants, time.Hour)
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

// Rows 0.14 wrote under the raw key are not read or deleted through
// KeyHashing; only their TTL removes them.
func TestPostgres_KeyHashingIgnoresRawRows(t *testing.T) {
	pg, pool := setupPostgresPool(t)
	ctx := context.Background()
	s := state.KeyHashing(pg)
	tenants := []string{"hlk_one", "hlk_two"}

	writeTenants(t, pg, tenants, time.Second)

	for _, k := range tenants {
		got, err := s.Get(ctx, "argocd", k, "app", "")
		if err != nil || got != nil {
			t.Fatalf("Get(%s) = %s, %v, want nothing", k, got, err)
		}
		ok, err := s.Exists(ctx, "argocd", k, "app", "")
		if err != nil || ok {
			t.Fatalf("Exists(%s) = %v, %v, want false", k, ok, err)
		}
		group, err := s.GetGroup(ctx, "gitea", k, "slug")
		if err != nil || len(group) != 0 {
			t.Fatalf("GetGroup(%s) = %v, %v, want empty", k, group, err)
		}
		if err := s.Delete(ctx, "argocd", k, "app", ""); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteGroup(ctx, "gitea", k, "slug"); err != nil {
			t.Fatal(err)
		}
	}
	if total, raw := countRows(t, pool); raw != 6 || total != 6 {
		t.Fatalf("after deleting through KeyHashing: %d raw of %d rows, want 6 of 6", raw, total)
	}

	// Writing the same rows again adds hashed ones next to the raw ones.
	writeTenants(t, s, tenants, time.Hour)
	if total, raw := countRows(t, pool); raw != 6 || total != 12 {
		t.Fatalf("after the hashed write: %d raw of %d rows, want 6 of 12", raw, total)
	}

	time.Sleep(1500 * time.Millisecond)
	n, err := s.Cleanup(ctx)
	if err != nil {
		t.Fatal("cleanup:", err)
	}
	if total, raw := countRows(t, pool); n != 6 || raw != 0 || total != 6 {
		t.Fatalf("cleanup removed %d rows, leaving %d raw of %d, want 6 removed and 0 of 6 left", n, raw, total)
	}
}
