package changedetection

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func testConfig() *config.ChangedetectionConfig {
	return &config.ChangedetectionConfig{
		Enabled:      true,
		Priority:     2,
		CleanupDelay: 5 * time.Minute,
		StaleTimeout: 1 * time.Hour,
	}
}

func newHandler(t *testing.T, cfg *config.ChangedetectionConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	pool := client.NewPool(srv.URL)
	h := NewHandler(pool, cfg)
	return auth.Middleware(h), calls, mu
}

func send(t *testing.T, h http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/changedetection", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestPageChanged(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"url": "https://example.com/product/widget",
		"title": "Widget Product Page",
		"tag": "prices",
		"diff_url": "https://cd.example.com/diff/550e8400",
		"preview_url": "https://cd.example.com/preview/550e8400",
		"triggered_text": "Price: $29.99",
		"timestamp": "2024-01-15T10:30:00Z"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls (create + ONGOING + ENDED), got %d", len(recorded))
	}

	// Verify create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	expectedSlug := slugForURL("https://example.com/product/widget")
	if createReq.Slug != expectedSlug {
		t.Errorf("expected slug %s, got %s", expectedSlug, createReq.Slug)
	}
	if createReq.Name != "Widget Product Page" {
		t.Errorf("expected name 'Widget Product Page', got %s", createReq.Name)
	}
	if createReq.Priority != 2 {
		t.Errorf("expected priority 2, got %d", createReq.Priority)
	}

	// Verify ONGOING update
	var ongoing pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &ongoing)
	if ongoing.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", ongoing.State)
	}
	if ongoing.Content.Template != "alert" {
		t.Errorf("expected template alert, got %s", ongoing.Content.Template)
	}
	if ongoing.Content.State != "Price: $29.99" {
		t.Errorf("expected state 'Price: $29.99', got %s", ongoing.Content.State)
	}
	if ongoing.Content.Icon != "eye.fill" {
		t.Errorf("expected icon eye.fill, got %s", ongoing.Content.Icon)
	}
	if ongoing.Content.AccentColor != "#FF9500" {
		t.Errorf("expected accent color #FF9500, got %s", ongoing.Content.AccentColor)
	}
	if ongoing.Content.Subtitle != "Changedetection \u00b7 prices" {
		t.Errorf("expected subtitle 'Changedetection \u00b7 prices', got %s", ongoing.Content.Subtitle)
	}
	if ongoing.Content.Severity != "info" {
		t.Errorf("expected severity info, got %s", ongoing.Content.Severity)
	}
	if ongoing.Content.URL != "https://cd.example.com/diff/550e8400" {
		t.Errorf("expected URL 'https://cd.example.com/diff/550e8400', got %s", ongoing.Content.URL)
	}
	if ongoing.Content.SecondaryURL != "https://cd.example.com/preview/550e8400" {
		t.Errorf("expected secondary URL 'https://cd.example.com/preview/550e8400', got %s", ongoing.Content.SecondaryURL)
	}
	if ongoing.Content.FiredAt == nil {
		t.Fatal("expected FiredAt to be set")
	}
	expectedFiredAt := int64(1705314600)
	if *ongoing.Content.FiredAt != expectedFiredAt {
		t.Errorf("expected FiredAt %d, got %d", expectedFiredAt, *ongoing.Content.FiredAt)
	}

	// Verify ENDED update
	var ended pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &ended)
	if ended.State != pushward.StateEnded {
		t.Errorf("expected ENDED, got %s", ended.State)
	}
	if ended.Content.State != "Price: $29.99" {
		t.Errorf("expected state 'Price: $29.99', got %s", ended.Content.State)
	}
}

func TestPageChangedNoTitle(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"url": "https://example.com/status",
		"title": "",
		"tag": "",
		"diff_url": "https://cd.example.com/diff/abc",
		"preview_url": "",
		"triggered_text": "Status: OK",
		"timestamp": "2024-01-15T10:30:00Z"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}

	// Verify name falls back to URL
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "https://example.com/status" {
		t.Errorf("expected name to fall back to URL, got %s", createReq.Name)
	}

	// Verify subtitle without tag
	var ongoing pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &ongoing)
	if ongoing.Content.Subtitle != "Changedetection" {
		t.Errorf("expected subtitle 'Changedetection', got %s", ongoing.Content.Subtitle)
	}
}

func TestPageChangedNoTriggeredText(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"url": "https://example.com/page",
		"title": "My Page",
		"tag": "monitoring",
		"diff_url": "https://cd.example.com/diff/xyz",
		"preview_url": "https://cd.example.com/preview/xyz",
		"triggered_text": "",
		"timestamp": "2024-01-15T10:30:00Z"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}

	// Verify state falls back to "Page changed"
	var ongoing pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &ongoing)
	if ongoing.Content.State != "Page changed" {
		t.Errorf("expected state 'Page changed', got %s", ongoing.Content.State)
	}
}
