package jellyfin

import (
	"net/http"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// The common Jellyfin setup is a http:// LAN server. That URL can never be the
// card's image_url (the server rejects http, and the phone would refuse the LAN
// host anyway), but the relay sits on the same network, so the thumbhash is
// what actually shows the artwork.
func TestPlaybackStartLANServerKeepsOnlyTheThumbhash(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"NotificationType": "PlaybackStart",
		"ServerUrl": "http://jellyfin.lan:8096",
		"ItemId": "abc123",
		"ItemType": "Episode",
		"Name": "Pilot",
		"SeriesName": "Breaking Bad",
		"SeasonNumber": 1,
		"EpisodeNumber": 1,
		"RunTimeTicks": 27630000000,
		"PlaybackPositionTicks": 0,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"IsPaused": false
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	// Episode art is a 16:9 still, so it is framed square rather than as a poster.
	testutil.AssertImageTrio(t, testutil.LastActivityUpdate(t, testutil.GetCalls(calls, mu)),
		"", testutil.Thumbhash, pushward.ImageShapeSquare)
}

// A movie has portrait art, and an https server is reachable by the phone, so
// the card carries the URL as well.
func TestPlaybackStartMovieOnHTTPSServerCarriesTheTrio(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"NotificationType": "PlaybackStart",
		"ServerUrl": "https://jellyfin.example.com/",
		"ItemId": "1f7de2a0",
		"ItemType": "Movie",
		"Name": "Inception",
		"ProductionYear": 2010,
		"RunTimeTicks": 88800000000,
		"PlaybackPositionTicks": 0,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"IsPaused": false
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	testutil.AssertImageTrio(t, testutil.LastActivityUpdate(t, testutil.GetCalls(calls, mu)),
		"https://jellyfin.example.com/Items/1f7de2a0/Images/Primary", testutil.Thumbhash, pushward.ImageShapePoster)
}

// The terminal "Watched" frame is the one that stays on screen after playback,
// so it has to keep the artwork rather than drop to a blank card.
func TestPlaybackStopKeepsTheImage(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	start := `{
		"NotificationType": "PlaybackStart",
		"ServerUrl": "https://jellyfin.example.com",
		"ItemId": "1f7de2a0",
		"ItemType": "Movie",
		"Name": "Inception",
		"RunTimeTicks": 88800000000,
		"PlaybackPositionTicks": 0,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"IsPaused": false
	}`
	if w := send(t, h, start); w.Code != http.StatusOK {
		t.Fatalf("start: expected 200, got %d", w.Code)
	}

	stop := `{
		"NotificationType": "PlaybackStop",
		"ServerUrl": "https://jellyfin.example.com",
		"ItemId": "1f7de2a0",
		"ItemType": "Movie",
		"Name": "Inception",
		"RunTimeTicks": 88800000000,
		"PlaybackPositionTicks": 88800000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"PlayedToCompletion": true
	}`
	if w := send(t, h, stop); w.Code != http.StatusOK {
		t.Fatalf("stop: expected 200, got %d", w.Code)
	}

	// create + start update + the two-phase end frames.
	recorded := testutil.WaitForCalls(t, calls, mu, 4, 2*time.Second)
	testutil.AssertImageTrio(t, testutil.LastActivityUpdate(t, recorded),
		"https://jellyfin.example.com/Items/1f7de2a0/Images/Primary", testutil.Thumbhash, pushward.ImageShapePoster)
}

// The pause auto-end fires from a timer, long after the payload that armed it
// was answered and discarded. Its artwork therefore has to come from what was
// captured at arming time - which is the only reason playbackImage exists.
func TestPauseAutoEndKeepsTheImage(t *testing.T) {
	cfg := testConfig()
	cfg.PauseTimeout = 20 * time.Millisecond
	h, calls, mu := newHandler(t, cfg)

	start := `{
		"NotificationType": "PlaybackStart",
		"ServerUrl": "https://jellyfin.example.com",
		"ItemId": "1f7de2a0",
		"ItemType": "Movie",
		"Name": "Inception",
		"RunTimeTicks": 88800000000,
		"PlaybackPositionTicks": 0,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"IsPaused": false
	}`
	if w := send(t, h, start); w.Code != http.StatusOK {
		t.Fatalf("start: expected 200, got %d", w.Code)
	}

	paused := `{
		"NotificationType": "PlaybackProgress",
		"ServerUrl": "https://jellyfin.example.com",
		"ItemId": "1f7de2a0",
		"ItemType": "Movie",
		"Name": "Inception",
		"RunTimeTicks": 88800000000,
		"PlaybackPositionTicks": 44400000000,
		"NotificationUsername": "john",
		"DeviceName": "Apple TV",
		"IsPaused": true
	}`
	if w := send(t, h, paused); w.Code != http.StatusOK {
		t.Fatalf("pause: expected 200, got %d", w.Code)
	}

	// create + start update + paused update + the two-phase end frames.
	recorded := testutil.WaitForCalls(t, calls, mu, 5, 2*time.Second)
	last := testutil.LastActivityUpdate(t, recorded)
	if last.State != "Paused on Apple TV by john" {
		t.Fatalf("last frame is %q, want the pause auto-end frame", last.State)
	}
	testutil.AssertImageTrio(t, last,
		"https://jellyfin.example.com/Items/1f7de2a0/Images/Primary", testutil.Thumbhash, pushward.ImageShapePoster)
}

func TestPrimaryImageURL(t *testing.T) {
	tests := []struct {
		name      string
		payload   jellyfinPayload
		wantURL   string
		wantFetch string
	}{
		{
			name:      "trailing slash is trimmed",
			payload:   jellyfinPayload{ServerURL: "https://jf.example.com/", ItemID: "7"},
			wantURL:   "https://jf.example.com/Items/7/Images/Primary",
			wantFetch: "https://jf.example.com/Items/7/Images/Primary?format=Jpg&maxWidth=256&quality=90",
		},
		{
			name:      "dashed guid",
			payload:   jellyfinPayload{ServerURL: "https://jf.example.com", ItemID: "a1b2c3d4-0000-4000-8000-abcdefabcdef"},
			wantURL:   "https://jf.example.com/Items/a1b2c3d4-0000-4000-8000-abcdefabcdef/Images/Primary",
			wantFetch: "https://jf.example.com/Items/a1b2c3d4-0000-4000-8000-abcdefabcdef/Images/Primary?format=Jpg&maxWidth=256&quality=90",
		},
		{
			name:    "no server url",
			payload: jellyfinPayload{ItemID: "7"},
		},
		{
			name:    "no item id",
			payload: jellyfinPayload{ServerURL: "https://jf.example.com"},
		},
		// An item id is pasted into a URL the relay then fetches, so anything
		// that could steer that URL somewhere else is refused outright: a "?"
		// or "#" would cut off the path and the query the relay meant to send,
		// and "../" or a "@" would move the request to another path or host.
		{
			name:    "query in the item id",
			payload: jellyfinPayload{ServerURL: "https://jf.example.com", ItemID: "7?x=1"},
		},
		{
			name:    "fragment in the item id",
			payload: jellyfinPayload{ServerURL: "https://jf.example.com", ItemID: "7#frag"},
		},
		{
			name:    "path traversal in the item id",
			payload: jellyfinPayload{ServerURL: "https://jf.example.com", ItemID: "../../admin"},
		},
		{
			name:    "authority in the item id",
			payload: jellyfinPayload{ServerURL: "https://jf.example.com", ItemID: "7@evil.example.com"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := primaryImageURL(&tc.payload); got != tc.wantURL {
				t.Errorf("primaryImageURL = %q, want %q", got, tc.wantURL)
			}
			if got := primaryImageFetchURL(&tc.payload); got != tc.wantFetch {
				t.Errorf("primaryImageFetchURL = %q, want %q", got, tc.wantFetch)
			}
		})
	}
}

func TestPrimaryImageShape(t *testing.T) {
	if got := primaryImageShape(&jellyfinPayload{Name: "Inception"}); got != pushward.ImageShapePoster {
		t.Errorf("movie shape = %q, want poster", got)
	}
	if got := primaryImageShape(&jellyfinPayload{Name: "Pilot", SeriesName: "Breaking Bad"}); got != pushward.ImageShapeSquare {
		t.Errorf("episode shape = %q, want square", got)
	}
}
