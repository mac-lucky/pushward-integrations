//go:build integration

package state_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
)

// jsonEq compares two JSON values semantically (ignoring whitespace differences
// introduced by PostgreSQL JSONB normalization).
func jsonEq(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	var gb, wb bytes.Buffer
	if err := json.Compact(&gb, got); err != nil {
		t.Fatalf("compact got: %v", err)
	}
	if err := json.Compact(&wb, want); err != nil {
		t.Fatalf("compact want: %v", err)
	}
	if gb.String() != wb.String() {
		t.Fatalf("got %s, want %s", gb.String(), wb.String())
	}
}

func setupPostgres(t *testing.T) *state.PostgresStore {
	t.Helper()
	ctx := context.Background()

	ctr, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("relay_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		postgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatal("start postgres container:", err)
	}

	connStr, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal("connection string:", err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatal("pgxpool:", err)
	}
	t.Cleanup(pool.Close)

	store, err := state.NewPostgresStore(ctx, pool)
	if err != nil {
		t.Fatal("new postgres store:", err)
	}

	return store
}

func TestPostgres_SetAndGet(t *testing.T) {
	store := setupPostgres(t)
	ctx := context.Background()

	val := json.RawMessage(`{"slug":"test-1"}`)
	if err := store.Set(ctx, "grafana", "hlk_abc", "alert1", "", val, 0); err != nil {
		t.Fatal("set:", err)
	}

	got, err := store.Get(ctx, "grafana", "hlk_abc", "alert1", "")
	if err != nil {
		t.Fatal("get:", err)
	}
	jsonEq(t, got, val)
}

func TestPostgres_GetNotFound(t *testing.T) {
	store := setupPostgres(t)
	ctx := context.Background()

	got, err := store.Get(ctx, "grafana", "hlk_abc", "nonexistent", "")
	if err != nil {
		t.Fatal("get:", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %s", got)
	}
}

func TestPostgres_Upsert(t *testing.T) {
	store := setupPostgres(t)
	ctx := context.Background()

	v1 := json.RawMessage(`{"v":1}`)
	v2 := json.RawMessage(`{"v":2}`)

	if err := store.Set(ctx, "argocd", "hlk_x", "app1", "", v1, 0); err != nil {
		t.Fatal("set v1:", err)
	}
	if err := store.Set(ctx, "argocd", "hlk_x", "app1", "", v2, 0); err != nil {
		t.Fatal("set v2:", err)
	}

	got, err := store.Get(ctx, "argocd", "hlk_x", "app1", "")
	if err != nil {
		t.Fatal("get:", err)
	}
	jsonEq(t, got, v2)
}

func TestPostgres_TTLExpiry(t *testing.T) {
	store := setupPostgres(t)
	ctx := context.Background()

	val := json.RawMessage(`{"slug":"ephemeral"}`)
	if err := store.Set(ctx, "starr", "hlk_ttl", "dl1", "", val, 1*time.Second); err != nil {
		t.Fatal("set:", err)
	}

	// Should be visible immediately
	got, err := store.Get(ctx, "starr", "hlk_ttl", "dl1", "")
	if err != nil {
		t.Fatal("get before expiry:", err)
	}
	if got == nil {
		t.Fatal("expected value before expiry, got nil")
	}

	time.Sleep(1500 * time.Millisecond)

	// Should be expired now
	got, err = store.Get(ctx, "starr", "hlk_ttl", "dl1", "")
	if err != nil {
		t.Fatal("get after expiry:", err)
	}
	if got != nil {
		t.Fatalf("expected nil after expiry, got %s", got)
	}
}

func TestPostgres_Delete(t *testing.T) {
	store := setupPostgres(t)
	ctx := context.Background()

	val := json.RawMessage(`{"slug":"del"}`)
	if err := store.Set(ctx, "proxmox", "hlk_d", "k1", "", val, 0); err != nil {
		t.Fatal("set:", err)
	}
	if err := store.Delete(ctx, "proxmox", "hlk_d", "k1", ""); err != nil {
		t.Fatal("delete:", err)
	}

	got, err := store.Get(ctx, "proxmox", "hlk_d", "k1", "")
	if err != nil {
		t.Fatal("get after delete:", err)
	}
	if got != nil {
		t.Fatalf("expected nil after delete, got %s", got)
	}
}

func TestPostgres_GetGroup(t *testing.T) {
	store := setupPostgres(t)
	ctx := context.Background()

	if err := store.Set(ctx, "grafana", "hlk_g", "alert1", "fp1", json.RawMessage(`{"i":1}`), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, "grafana", "hlk_g", "alert1", "fp2", json.RawMessage(`{"i":2}`), 0); err != nil {
		t.Fatal(err)
	}
	// Different key — should not appear
	if err := store.Set(ctx, "grafana", "hlk_g", "alert2", "fp3", json.RawMessage(`{"i":3}`), 0); err != nil {
		t.Fatal(err)
	}

	group, err := store.GetGroup(ctx, "grafana", "hlk_g", "alert1")
	if err != nil {
		t.Fatal("get group:", err)
	}
	if len(group) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(group))
	}
	jsonEq(t, group["fp1"], json.RawMessage(`{"i":1}`))
	jsonEq(t, group["fp2"], json.RawMessage(`{"i":2}`))
}

func TestPostgres_GetGroupExcludesExpired(t *testing.T) {
	store := setupPostgres(t)
	ctx := context.Background()

	if err := store.Set(ctx, "grafana", "hlk_ge", "a", "live", json.RawMessage(`{"live":true}`), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, "grafana", "hlk_ge", "a", "dead", json.RawMessage(`{"live":false}`), 1*time.Second); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond)

	group, err := store.GetGroup(ctx, "grafana", "hlk_ge", "a")
	if err != nil {
		t.Fatal("get group:", err)
	}
	if len(group) != 1 {
		t.Fatalf("expected 1 live entry, got %d", len(group))
	}
	if _, ok := group["live"]; !ok {
		t.Fatal("expected 'live' entry to survive")
	}
}

func TestPostgres_DeleteGroup(t *testing.T) {
	store := setupPostgres(t)
	ctx := context.Background()

	if err := store.Set(ctx, "starr", "hlk_dg", "k", "a", json.RawMessage(`{}`), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, "starr", "hlk_dg", "k", "b", json.RawMessage(`{}`), 0); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteGroup(ctx, "starr", "hlk_dg", "k"); err != nil {
		t.Fatal("delete group:", err)
	}

	group, err := store.GetGroup(ctx, "starr", "hlk_dg", "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(group) != 0 {
		t.Fatalf("expected empty group after delete, got %d", len(group))
	}
}

func TestPostgres_Exists(t *testing.T) {
	store := setupPostgres(t)
	ctx := context.Background()

	exists, err := store.Exists(ctx, "uptimekuma", "hlk_e", "m1", "")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expected false for non-existent key")
	}

	if err := store.Set(ctx, "uptimekuma", "hlk_e", "m1", "", json.RawMessage(`{}`), 0); err != nil {
		t.Fatal(err)
	}

	exists, err = store.Exists(ctx, "uptimekuma", "hlk_e", "m1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected true after set")
	}
}

func TestPostgres_ExistsExpired(t *testing.T) {
	store := setupPostgres(t)
	ctx := context.Background()

	if err := store.Set(ctx, "backrest", "hlk_ee", "k", "", json.RawMessage(`{}`), 1*time.Second); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond)

	exists, err := store.Exists(ctx, "backrest", "hlk_ee", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("expected false for expired key")
	}
}

func TestPostgres_Cleanup(t *testing.T) {
	store := setupPostgres(t)
	ctx := context.Background()

	// Insert 3 entries: 2 expired, 1 live
	if err := store.Set(ctx, "p", "u", "expired1", "", json.RawMessage(`{}`), 1*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, "p", "u", "expired2", "", json.RawMessage(`{}`), 1*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, "p", "u", "live", "", json.RawMessage(`{}`), 0); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond)

	cleaned, err := store.Cleanup(ctx)
	if err != nil {
		t.Fatal("cleanup:", err)
	}
	if cleaned != 2 {
		t.Fatalf("expected 2 cleaned, got %d", cleaned)
	}

	// Live entry should still exist
	got, err := store.Get(ctx, "p", "u", "live", "")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("live entry was incorrectly cleaned up")
	}
}

func TestPostgres_TenantIsolation(t *testing.T) {
	store := setupPostgres(t)
	ctx := context.Background()

	v1 := json.RawMessage(`{"tenant":"a"}`)
	v2 := json.RawMessage(`{"tenant":"b"}`)

	if err := store.Set(ctx, "grafana", "hlk_a", "alert", "", v1, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, "grafana", "hlk_b", "alert", "", v2, 0); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, "grafana", "hlk_a", "alert", "")
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, got, v1)

	got, err = store.Get(ctx, "grafana", "hlk_b", "alert", "")
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, got, v2)
}

func TestPostgres_ProviderIsolation(t *testing.T) {
	store := setupPostgres(t)
	ctx := context.Background()

	v1 := json.RawMessage(`{"p":"grafana"}`)
	v2 := json.RawMessage(`{"p":"argocd"}`)

	if err := store.Set(ctx, "grafana", "hlk_x", "key1", "", v1, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, "argocd", "hlk_x", "key1", "", v2, 0); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, "grafana", "hlk_x", "key1", "")
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, got, v1)
}

func TestPostgres_UpsertUpdatesTTL(t *testing.T) {
	store := setupPostgres(t)
	ctx := context.Background()

	// Set with short TTL
	if err := store.Set(ctx, "p", "u", "k", "", json.RawMessage(`{}`), 1*time.Second); err != nil {
		t.Fatal(err)
	}

	// Upsert with no TTL (permanent)
	if err := store.Set(ctx, "p", "u", "k", "", json.RawMessage(`{"updated":true}`), 0); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1500 * time.Millisecond)

	// Should still exist — TTL was cleared by upsert
	got, err := store.Get(ctx, "p", "u", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("entry expired despite upsert clearing TTL")
	}
	jsonEq(t, got, json.RawMessage(`{"updated":true}`))
}
