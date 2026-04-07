package selftest

import (
	"strings"
	"testing"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func TestSendTest(t *testing.T) {
	t.Run("valid provider", func(t *testing.T) {
		srv, calls, mu := testutil.MockPushWardServer(t)
		cl := pushward.NewClient(srv.URL, "hlk_test")

		err := SendTest(t.Context(), cl, "radarr")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		recorded := testutil.GetCalls(calls, mu)
		if len(recorded) != 2 {
			t.Fatalf("expected 2 calls, got %d", len(recorded))
		}

		var create pushward.CreateActivityRequest
		testutil.UnmarshalBody(t, recorded[0].Body, &create)
		if create.Slug != "relay-test-radarr" {
			t.Errorf("expected slug relay-test-radarr, got %s", create.Slug)
		}
		if create.Priority != 1 {
			t.Errorf("expected priority 1, got %d", create.Priority)
		}
		if create.EndedTTL != 300 {
			t.Errorf("expected ended_ttl 300, got %d", create.EndedTTL)
		}
		if create.StaleTTL != 120 {
			t.Errorf("expected stale_ttl 120, got %d", create.StaleTTL)
		}

		var update pushward.UpdateRequest
		testutil.UnmarshalBody(t, recorded[1].Body, &update)
		if update.Content.Template != "steps" {
			t.Errorf("expected template steps, got %s", update.Content.Template)
		}
	})

	t.Run("unknown provider", func(t *testing.T) {
		srv, _, _ := testutil.MockPushWardServer(t)
		cl := pushward.NewClient(srv.URL, "hlk_test")

		err := SendTest(t.Context(), cl, "unknown")
		if err == nil {
			t.Fatal("expected error for unknown provider")
		}
		if !strings.Contains(err.Error(), "unknown provider") {
			t.Errorf("expected 'unknown provider' in error, got %s", err.Error())
		}
	})
}
