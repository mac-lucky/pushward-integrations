package selftest

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func newTestHandler(t *testing.T) (*ProviderTestHandler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	pool := client.NewPool(srv.URL)
	h := NewProviderTestHandler(pool)
	return h, calls, mu
}

func sendTest(t *testing.T, h *ProviderTestHandler, provider string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/test/"+provider, nil)
	req.SetPathValue("provider", provider)
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	auth.Middleware(h).ServeHTTP(w, req)
	return w
}

func TestProviderTestHandler_ValidProvider(t *testing.T) {
	expectedTemplates := map[string]string{
		"grafana":         "alert",
		"argocd":          "pipeline",
		"radarr":          "pipeline",
		"sonarr":          "pipeline",
		"jellyfin":        "generic",
		"paperless":       "generic",
		"changedetection": "alert",
		"unmanic":         "generic",
		"proxmox":         "pipeline",
		"overseerr":       "pipeline",
		"uptimekuma":      "alert",
		"gatus":           "alert",
		"backrest":        "generic",
	}

	for provider, expectedTemplate := range expectedTemplates {
		t.Run(provider, func(t *testing.T) {
			h, calls, mu := newTestHandler(t)

			w := sendTest(t, h, provider)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}

			recorded := testutil.GetCalls(calls, mu)
			if len(recorded) != 2 {
				t.Fatalf("expected 2 calls (create + update), got %d", len(recorded))
			}

			// Create activity
			if recorded[0].Method != http.MethodPost || recorded[0].Path != "/activities" {
				t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
			}
			var create pushward.CreateActivityRequest
			testutil.UnmarshalBody(t, recorded[0].Body, &create)
			if create.Slug != "relay-test-"+provider {
				t.Errorf("expected slug relay-test-%s, got %s", provider, create.Slug)
			}
			if create.Priority != 1 {
				t.Errorf("expected priority 1, got %d", create.Priority)
			}
			if create.StaleTTL != 25 {
				t.Errorf("expected stale_ttl 25, got %d", create.StaleTTL)
			}
			if create.EndedTTL != 30 {
				t.Errorf("expected ended_ttl 30, got %d", create.EndedTTL)
			}

			// ONGOING update
			if recorded[1].Method != http.MethodPatch || recorded[1].Path != "/activity/relay-test-"+provider {
				t.Errorf("expected PATCH /activity/relay-test-%s, got %s %s", provider, recorded[1].Method, recorded[1].Path)
			}
			var update pushward.UpdateRequest
			testutil.UnmarshalBody(t, recorded[1].Body, &update)
			if update.State != pushward.StateOngoing {
				t.Errorf("expected ONGOING, got %s", update.State)
			}
			if update.Content.Template != expectedTemplate {
				t.Errorf("expected template %s, got %s", expectedTemplate, update.Content.Template)
			}
		})
	}
}

func TestProviderTestHandler_UnknownProvider(t *testing.T) {
	h, _, _ := newTestHandler(t)

	w := sendTest(t, h, "doesnotexist")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "unknown provider") {
		t.Errorf("expected body to contain 'unknown provider', got %s", body)
	}
	if !strings.Contains(body, "valid_providers") {
		t.Errorf("expected body to contain 'valid_providers', got %s", body)
	}
}

func TestProviderTestHandler_AllProvidersRegistered(t *testing.T) {
	got := ValidProviders()
	expected := []string{
		"argocd", "backrest", "changedetection", "gatus", "grafana",
		"jellyfin", "overseerr", "paperless", "proxmox", "radarr",
		"sonarr", "unmanic", "uptimekuma",
	}

	if len(got) != 13 {
		t.Fatalf("expected 13 providers, got %d: %v", len(got), got)
	}

	sort.Strings(got)
	for i, name := range expected {
		if got[i] != name {
			t.Errorf("expected provider[%d]=%s, got %s", i, name, got[i])
		}
	}
}

func TestProviderTestHandler_AlertFiredAtIsFresh(t *testing.T) {
	h, calls, mu := newTestHandler(t)

	// First request
	w1 := sendTest(t, h, "grafana")
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", w1.Code)
	}

	time.Sleep(100 * time.Millisecond)

	// Second request (new handler to get fresh call list)
	h2, calls2, mu2 := newTestHandler(t)
	w2 := sendTest(t, h2, "grafana")
	if w2.Code != http.StatusOK {
		t.Fatalf("second request: expected 200, got %d", w2.Code)
	}

	recorded1 := testutil.GetCalls(calls, mu)
	recorded2 := testutil.GetCalls(calls2, mu2)

	if len(recorded1) != 2 || len(recorded2) != 2 {
		t.Fatalf("expected 2 calls each, got %d and %d", len(recorded1), len(recorded2))
	}

	var update1, update2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded1[1].Body, &update1)
	testutil.UnmarshalBody(t, recorded2[1].Body, &update2)

	if update1.Content.FiredAt == nil {
		t.Fatal("first FiredAt is nil")
	}
	if update2.Content.FiredAt == nil {
		t.Fatal("second FiredAt is nil")
	}
	if *update2.Content.FiredAt < *update1.Content.FiredAt {
		t.Errorf("second FiredAt (%d) should be >= first (%d)", *update2.Content.FiredAt, *update1.Content.FiredAt)
	}
}

func TestSendTest(t *testing.T) {
	t.Run("valid provider", func(t *testing.T) {
		srv, calls, mu := testutil.MockPushWardServer(t)
		cl := pushward.NewClient(srv.URL, "hlk_test")

		err := SendTest(t.Context(), cl, "grafana")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		recorded := testutil.GetCalls(calls, mu)
		if len(recorded) != 2 {
			t.Fatalf("expected 2 calls, got %d", len(recorded))
		}

		var create pushward.CreateActivityRequest
		testutil.UnmarshalBody(t, recorded[0].Body, &create)
		if create.Slug != "relay-test-grafana" {
			t.Errorf("expected slug relay-test-grafana, got %s", create.Slug)
		}

		var update pushward.UpdateRequest
		testutil.UnmarshalBody(t, recorded[1].Body, &update)
		if update.Content.Template != "alert" {
			t.Errorf("expected template alert, got %s", update.Content.Template)
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
