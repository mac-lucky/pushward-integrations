package overseerr

import (
	"net/http"
	"testing"

	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

const overseerrPoster = "https://image.tmdb.org/t/p/w600_and_h900_bestv2/inception.jpg"

func TestMediaPendingCarriesPosterImage(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"notification_type": "MEDIA_PENDING",
		"event": "media.pending",
		"subject": "Inception (2010)",
		"image": "`+overseerrPoster+`",
		"media": {"media_type": "movie", "tmdbId": "27205"},
		"request": {"request_id": "1", "requestedBy_username": "admin"}
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	testutil.AssertImageTrio(t, testutil.LastActivityUpdate(t, testutil.GetCalls(calls, mu)),
		overseerrPoster, testutil.Thumbhash, pushward.ImageShapePoster)
}

// Overseerr sends `"image": ""` whenever the webhook template omits it, which
// must leave the card with no image fields at all rather than an empty URL.
func TestMediaPendingWithoutImageOmitsTheTrio(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"notification_type": "MEDIA_PENDING",
		"event": "media.pending",
		"subject": "Inception (2010)",
		"image": "",
		"media": {"media_type": "movie", "tmdbId": "27205"},
		"request": {"request_id": "1", "requestedBy_username": "admin"}
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	testutil.AssertImageTrio(t, testutil.LastActivityUpdate(t, testutil.GetCalls(calls, mu)), "", "", "")
}
