package starr

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

const tmdbPoster = "https://image.tmdb.org/t/p/original/poster.jpg"

// Every Radarr and Sonarr event that opens or closes a download card carries
// the artwork. Download is the terminal event, so its frame goes out through
// the two-phase end and is the one that has to still have it.
func TestStarrEventsCarryPosterImage(t *testing.T) {
	tests := []struct {
		name      string
		send      func(t *testing.T, mux http.Handler, payload string) *httptest.ResponseRecorder
		payload   string
		waitCalls int // 0 when the frame is already recorded by the time the webhook returns
	}{
		{
			name: "radarr grab",
			send: sendRadarr,
			payload: `{
				"eventType": "Grab",
				"movie": {"id": 1, "title": "Inception", "year": 2010, "tmdbId": 27205,
					"images": [{"coverType": "poster", "remoteUrl": "` + tmdbPoster + `"}]},
				"release": {"quality": "Bluray-1080p"},
				"downloadId": "SABnzbd_nzo_img1"
			}`,
		},
		{
			name: "radarr download",
			send: sendRadarr,
			payload: `{
				"eventType": "Download",
				"movie": {"id": 1, "title": "Inception", "year": 2010, "tmdbId": 27205,
					"images": [{"coverType": "poster", "remoteUrl": "` + tmdbPoster + `"}]},
				"movieFile": {"quality": "Bluray-1080p"},
				"downloadId": "SABnzbd_nzo_img2"
			}`,
			waitCalls: 3,
		},
		{
			name: "sonarr grab",
			send: sendSonarr,
			payload: `{
				"eventType": "Grab",
				"series": {"id": 1, "title": "Breaking Bad", "titleSlug": "breaking-bad", "tvdbId": 81189,
					"images": [{"coverType": "poster", "remoteUrl": "` + tmdbPoster + `"}]},
				"episodes": [{"id": 10, "seasonNumber": 1, "episodeNumber": 1, "title": "Pilot"}],
				"release": {"quality": "WEBDL-1080p"},
				"downloadId": "SABnzbd_nzo_img3"
			}`,
		},
		{
			name: "sonarr download",
			send: sendSonarr,
			payload: `{
				"eventType": "Download",
				"series": {"id": 1, "title": "Breaking Bad", "titleSlug": "breaking-bad", "tvdbId": 81189,
					"images": [{"coverType": "poster", "remoteUrl": "` + tmdbPoster + `"}]},
				"episodes": [{"id": 10, "seasonNumber": 1, "episodeNumber": 1, "title": "Pilot"}],
				"episodeFile": {"quality": "WEBDL-1080p"},
				"downloadId": "SABnzbd_nzo_img4"
			}`,
			waitCalls: 3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux, _, calls, mu := newHandler(t, testConfig())

			if w := tc.send(t, mux, tc.payload); w.Code != 200 {
				t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
			}

			recorded := testutil.GetCalls(calls, mu)
			if tc.waitCalls > 0 {
				recorded = testutil.WaitForCalls(t, calls, mu, tc.waitCalls, 2*time.Second)
			}
			testutil.AssertImageTrio(t, testutil.LastActivityUpdate(t, recorded),
				tmdbPoster, testutil.Thumbhash, pushward.ImageShapePoster)
		})
	}
}

// No artwork in the payload means no image fields at all, not an empty URL and
// a shape that renders a blank frame.
func TestRadarrGrabWithoutImagesOmitsTheTrio(t *testing.T) {
	mux, _, calls, mu := newHandler(t, testConfig())

	w := sendRadarr(t, mux, `{
		"eventType": "Grab",
		"movie": {"id": 1, "title": "Inception", "year": 2010, "tmdbId": 27205},
		"release": {"quality": "Bluray-1080p"},
		"downloadId": "SABnzbd_nzo_img6"
	}`)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	testutil.AssertImageTrio(t, testutil.LastActivityUpdate(t, testutil.GetCalls(calls, mu)), "", "", "")
}

// A non-poster cover type is not artwork for this card.
func TestPosterURLPicksThePosterCoverType(t *testing.T) {
	images := []StarrImage{
		{CoverType: "banner", RemoteURL: "https://example.com/banner.jpg"},
		{CoverType: "Poster", RemoteURL: tmdbPoster},
	}
	if got := posterURL(images); got != tmdbPoster {
		t.Errorf("posterURL = %q, want %q", got, tmdbPoster)
	}
	if got := posterURL([]StarrImage{{CoverType: "fanart", RemoteURL: "https://example.com/f.jpg"}}); got != "" {
		t.Errorf("expected no poster, got %q", got)
	}
}
