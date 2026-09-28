//go:build integration

package universalhook

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// TestPostgresEndToEnd runs a shape through proposal, review and a decision
// on the real tables: the review link has to match the expiry Postgres hands
// back, and neither table may end up holding the integration key.
func TestPostgresEndToEnd(t *testing.T) {
	ctx := context.Background()
	ctr, err := postgres.Run(ctx, "postgres:16-alpine",
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
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store, err := state.NewPostgresStore(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	mappings, err := state.NewMappingStore(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}

	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServer(t)
	hs := newHarnessAt(t, srv.URL, calls, mu, store, mappings)

	if w := hs.post(t, "/universal?source=alertmanager", fixture(t, "alertmanager_firing.json")); w.Code != http.StatusOK {
		t.Fatalf("firing: %d %s", w.Code, w.Body.String())
	}
	rv := reviews(t, hs.snapshot())
	if len(rv) != 1 {
		t.Fatalf("%d reviews", len(rv))
	}
	if w := hs.tap(t, action(t, rv[0], "accept")); w.Code != http.StatusOK {
		t.Fatalf("accept: %d %s", w.Code, w.Body.String())
	}
	if w := hs.post(t, "/universal?source=alertmanager", fixture(t, "alertmanager_resolved.json")); w.Code != http.StatusOK {
		t.Fatalf("resolved: %d %s", w.Code, w.Body.String())
	}
	if got := testutil.WaitForCalls(t, calls, mu, 7, 5*time.Second); len(got) != 7 {
		t.Errorf("got %d calls, want 7", len(got))
	}

	var status string
	var rev int
	if err := pool.QueryRow(ctx, `SELECT status, rev FROM universal_mappings`).Scan(&status, &rev); err != nil {
		t.Fatal(err)
	}
	if status != "confirmed" || rev != 1 {
		t.Errorf("mapping row status %s rev %d", status, rev)
	}
	var leaks int
	err = pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM relay_state WHERE position('hlk_' in user_key || key || value::text) > 0)
		     + (SELECT count(*) FROM universal_mappings
		        WHERE position('hlk_' in source || mapping::text || shape::text || proposal::text
		              || coalesce(samples::text, '') || coalesce(candidates::text, '')) > 0)`).Scan(&leaks)
	if err != nil || leaks != 0 {
		t.Errorf("%d rows carry the integration key (%v)", leaks, err)
	}
}
