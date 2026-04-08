package bazarr

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func testConfig() *config.BazarrConfig {
	return &config.BazarrConfig{
		BaseProviderConfig: config.BaseProviderConfig{
			Enabled:        true,
			Priority:       1,
			CleanupDelay:   5 * time.Minute,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
	}
}

func newHandler(t *testing.T, cfg *config.BazarrConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServer(t)
	pool := client.NewPool(srv.URL, nil)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, pool, cfg)

	return mux, calls, mu
}

func send(t *testing.T, h http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/bazarr", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestEpisodeDownloaded(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"version": "1.0",
		"title": "Bazarr notification",
		"message": "Breaking Bad (2008) - S05E14 - Ozymandias : English subtitles downloaded from opensubtitles with a score of 96.0%.",
		"type": "info"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Wait for two-phase end
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	// create + phase1(ONGOING) + phase2(ENDED) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls (create + phase1 + phase2), got %d", len(recorded))
	}

	// Verify create
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if !strings.HasPrefix(createReq.Slug, "bazarr-") {
		t.Errorf("expected slug with bazarr- prefix, got %s", createReq.Slug)
	}
	if createReq.Name != "Breaking Bad (2008) - S05E14 - Ozymandias" {
		t.Errorf("expected media name, got %s", createReq.Name)
	}
	if createReq.Priority != 1 {
		t.Errorf("expected priority 1, got %d", createReq.Priority)
	}

	// Phase 1: ONGOING with "Downloaded"
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != "Downloaded" {
		t.Errorf("expected state 'Downloaded', got %s", phase1.Content.State)
	}
	if phase1.Content.Icon != "mdi:download" {
		t.Errorf("expected mdi:download icon, got %s", phase1.Content.Icon)
	}
	if phase1.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}
	if !strings.Contains(phase1.Content.Subtitle, "English") {
		t.Errorf("expected subtitle to contain language, got %s", phase1.Content.Subtitle)
	}
	if !strings.Contains(phase1.Content.Subtitle, "96.0%") {
		t.Errorf("expected subtitle to contain score, got %s", phase1.Content.Subtitle)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestMovieUpgraded(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"version": "1.0",
		"title": "Bazarr notification",
		"message": "Dune: Part Two (2024) : French forced subtitles upgraded from subscene with a score of 100.0%.",
		"type": "info"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}

	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "Dune: Part Two (2024)" {
		t.Errorf("expected movie name with colon preserved, got %s", createReq.Name)
	}

	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &phase1)
	if phase1.Content.State != "Upgraded" {
		t.Errorf("expected state 'Upgraded', got %s", phase1.Content.State)
	}
	if !strings.Contains(phase1.Content.Subtitle, "French forced") {
		t.Errorf("expected subtitle with forced modifier, got %s", phase1.Content.Subtitle)
	}
}

func TestTestNotification(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"version": "1.0",
		"title": "Bazarr notification",
		"message": "This is a test notification",
		"type": "info"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls (create + update), got %d", len(recorded))
	}

	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if create.Slug != "relay-test-bazarr" {
		t.Errorf("expected slug relay-test-bazarr, got %s", create.Slug)
	}
}

func TestParseMessage(t *testing.T) {
	tests := []struct {
		name    string
		msg     string
		want    *subtitleEvent
		wantNil bool
	}{
		{
			name: "episode",
			msg:  "Breaking Bad (2008) - S05E14 - Ozymandias : English subtitles downloaded from opensubtitles with a score of 96.0%.",
			want: &subtitleEvent{
				media:    "Breaking Bad (2008) - S05E14 - Ozymandias",
				language: "English",
				action:   "downloaded",
				provider: "opensubtitles",
				score:    "96.0",
			},
		},
		{
			name: "movie with colon",
			msg:  "Dune: Part Two (2024) : French forced subtitles upgraded from subscene with a score of 100.0%.",
			want: &subtitleEvent{
				media:    "Dune: Part Two (2024)",
				language: "French forced",
				action:   "upgraded",
				provider: "subscene",
				score:    "100.0",
			},
		},
		{
			name: "HI subtitles",
			msg:  "The Office (2005) - S02E01 - The Dundies : English HI subtitles manually downloaded from addic7ed with a score of 88.5%.",
			want: &subtitleEvent{
				media:    "The Office (2005) - S02E01 - The Dundies",
				language: "English HI",
				action:   "manually downloaded",
				provider: "addic7ed",
				score:    "88.5",
			},
		},
		{
			name:    "unrecognized",
			msg:     "This is a test notification",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseMessage(tt.msg)
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil result")
			}
			if got.media != tt.want.media {
				t.Errorf("media: got %q, want %q", got.media, tt.want.media)
			}
			if got.language != tt.want.language {
				t.Errorf("language: got %q, want %q", got.language, tt.want.language)
			}
			if got.action != tt.want.action {
				t.Errorf("action: got %q, want %q", got.action, tt.want.action)
			}
			if got.provider != tt.want.provider {
				t.Errorf("provider: got %q, want %q", got.provider, tt.want.provider)
			}
			if got.score != tt.want.score {
				t.Errorf("score: got %q, want %q", got.score, tt.want.score)
			}
		})
	}
}
