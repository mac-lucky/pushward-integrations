package bazarr

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func testConfig() *config.BazarrConfig {
	return &config.BazarrConfig{
		BaseProviderConfig: config.BaseProviderConfig{
			Enabled: true,
		},
	}
}

func newHandler(t *testing.T, cfg *config.BazarrConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
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

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call (notification), got %d", len(recorded))
	}

	if recorded[0].Method != http.MethodPost || recorded[0].Path != "/notifications" {
		t.Errorf("expected POST /notifications, got %s %s", recorded[0].Method, recorded[0].Path)
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Subtitle != "Breaking Bad (2008) - S05E14 - Ozymandias" {
		t.Errorf("expected media name in subtitle, got %s", req.Subtitle)
	}
	if !strings.Contains(req.Title, "Downloaded") {
		t.Errorf("expected title to contain 'Downloaded', got %s", req.Title)
	}
	if !strings.Contains(req.Title, "English") {
		t.Errorf("expected title to contain language, got %s", req.Title)
	}
	if req.ThreadID != "bazarr-breaking-bad-2008-s05e14-ozymandias" {
		t.Errorf("expected per-media thread_id, got %s", req.ThreadID)
	}
	if !strings.Contains(req.Body, "96.0%") {
		t.Errorf("expected body to contain score, got %s", req.Body)
	}
	if !strings.HasPrefix(req.Body, "Breaking Bad") {
		t.Errorf("expected body to start with media name, got %s", req.Body)
	}
	if req.Source != "bazarr" {
		t.Errorf("expected source bazarr, got %s", req.Source)
	}
	if !req.Push {
		t.Error("expected push=true")
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

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 1 {
		t.Fatalf("expected 1 call, got %d", len(recorded))
	}

	var req pushward.SendNotificationRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &req)
	if req.Subtitle != "Dune: Part Two (2024)" {
		t.Errorf("expected movie name with colon preserved in subtitle, got %s", req.Subtitle)
	}
	if !strings.Contains(req.Title, "Upgraded") {
		t.Errorf("expected title with 'Upgraded', got %s", req.Title)
	}
	if !strings.Contains(req.Title, "French forced") {
		t.Errorf("expected title with forced modifier, got %s", req.Title)
	}
	if req.ThreadID != "bazarr-dune-part-two-2024" {
		t.Errorf("expected per-media thread_id, got %s", req.ThreadID)
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
