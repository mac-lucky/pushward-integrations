package starr

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/poster"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

// notifyHandler runs in notify mode, so a Download is one notification and no
// activity.
func notifyHandler(t *testing.T, opts testutil.MockOptions) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServerWith(t, opts)
	cfg := testConfig()
	cfg.Mode = config.ModeNotify
	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, state.NewMemoryStore(), client.NewPool(srv.URL, nil), cfg, poster.Disabled{})
	return mux, calls, mu
}

func postStarr(t *testing.T, h http.Handler, target, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s: %d %s", target, w.Code, w.Body.String())
	}
}

func notificationsIn(t *testing.T, calls []testutil.APICall) []pushward.SendNotificationRequest {
	t.Helper()
	var out []pushward.SendNotificationRequest
	for _, c := range testutil.CallsTo(calls, http.MethodPost, "/notifications") {
		var n pushward.SendNotificationRequest
		testutil.UnmarshalBody(t, c.Body, &n)
		out = append(out, n)
	}
	return out
}

func retractsIn(t *testing.T, calls []testutil.APICall) []string {
	t.Helper()
	var out []string
	for _, c := range testutil.CallsTo(calls, http.MethodPost, "/notifications/retract") {
		var body struct {
			CollapseID string `json:"collapse_id"`
		}
		testutil.UnmarshalBody(t, c.Body, &body)
		out = append(out, body.CollapseID)
	}
	return out
}

const (
	healthIndexer  = `{"eventType":"Health","level":"warning","message":"Indexer down","type":"IndexerStatusCheck"}`
	healthDisk     = `{"eventType":"Health","level":"error","message":"Disk low","type":"DiskSpaceCheck"}`
	restoreIndexer = `{"eventType":"HealthRestored","level":"warning","message":"Indexer back","type":"IndexerStatusCheck"}`
)

func TestHealthCollapseIDPerType(t *testing.T) {
	zero := 0
	for _, tc := range []struct {
		name, query string
		opts        testutil.MockOptions
		retracted   bool
	}{
		{"restored replaces its own alert", "", testutil.MockOptions{}, false},
		{"retract=1 takes it back", "?retract=1", testutil.MockOptions{}, true},
		{"retract reaching no device falls back", "?retract=1", testutil.MockOptions{RetractDevices: &zero}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, calls, mu := notifyHandler(t, tc.opts)
			for _, body := range []string{healthIndexer, healthDisk, restoreIndexer} {
				postStarr(t, h, "/radarr"+tc.query, body)
			}
			recorded := testutil.GetCalls(calls, mu)
			ns := notificationsIn(t, recorded)
			const indexer, disk = "radarr-health-indexerstatuscheck", "radarr-health-diskspacecheck"
			if len(ns) < 2 || ns[0].CollapseID != indexer || ns[1].CollapseID != disk {
				t.Fatalf("health notifications = %+v, want one id per type", ns)
			}
			rs := retractsIn(t, recorded)
			if tc.query == "" && len(rs) != 0 {
				t.Errorf("retracts %q without ?retract=1", rs)
			}
			if tc.query != "" && (len(rs) != 1 || rs[0] != indexer) {
				t.Errorf("retracts %q, want only %s", rs, indexer)
			}
			if tc.retracted {
				if len(ns) != 2 {
					t.Errorf("%d notifications, want no Restored after a retract that reached", len(ns))
				}
				return
			}
			if len(ns) != 3 || ns[2].CollapseID != indexer || ns[2].Level != pushward.LevelPassive {
				t.Errorf("restored = %+v, want a passive one under %s", ns[2:], indexer)
			}
		})
	}
}

func TestHealthCollapseIDBounds(t *testing.T) {
	if got := healthCollapseID("sonarr", ""); got != "sonarr-health" {
		t.Errorf("no type: %q", got)
	}
	long := strings.Repeat("VeryLongHealthCheckName", 4)
	if got := healthCollapseID("prowlarr", long); len(got) > 64 || !strings.HasPrefix(got, "prowlarr-health-") || got != healthCollapseID("prowlarr", long) {
		t.Errorf("long type: %q", got)
	}
}

func manualInteraction(downloadID string) string {
	return `{"eventType":"ManualInteractionRequired","downloadId":"` + downloadID + `",
		"downloadInfo":{"quality":"x","title":"Inception.1080p","status":"Warning","statusMessages":[]}}`
}

func radarrDownload(downloadID string) string {
	return `{"eventType":"Download","movie":{"id":1,"title":"Inception","year":2010,"tmdbId":27205},
		"movieFile":{"quality":"Bluray-1080p","size":1},"downloadId":"` + downloadID + `"}`
}

func TestManualInteractionIDPerDownload(t *testing.T) {
	h, calls, mu := notifyHandler(t, testutil.MockOptions{})
	for _, body := range []string{manualInteraction("dl-A"), manualInteraction("dl-B"), manualInteraction(""), radarrDownload("dl-A")} {
		postStarr(t, h, "/radarr", body)
	}
	recorded := testutil.GetCalls(calls, mu)
	ns := notificationsIn(t, recorded)
	if len(ns) != 4 {
		t.Fatalf("%d notifications, want 4", len(ns))
	}
	a, b := ns[0].CollapseID, ns[1].CollapseID
	if a == b || !strings.HasPrefix(a, "radarr-manual-interaction-") || len(a) > 64 {
		t.Errorf("ids %q and %q, want one per download", a, b)
	}
	if ns[2].CollapseID != "radarr-manual-interaction" {
		t.Errorf("no downloadId: %q, want the old id", ns[2].CollapseID)
	}
	if rs := retractsIn(t, recorded); len(rs) != 0 {
		t.Errorf("retracts %q without ?retract=1", rs)
	}
}

func TestManualInteractionRetractedOnImport(t *testing.T) {
	h, calls, mu := notifyHandler(t, testutil.MockOptions{})
	postStarr(t, h, "/radarr?retract=1", manualInteraction("dl-A"))
	postStarr(t, h, "/radarr?retract=1", manualInteraction("dl-B"))
	postStarr(t, h, "/radarr?retract=1", radarrDownload("dl-A"))
	// The record is gone, and another download never had one.
	postStarr(t, h, "/radarr?retract=1", radarrDownload("dl-A"))
	postStarr(t, h, "/radarr?retract=1", radarrDownload("dl-C"))

	recorded := testutil.GetCalls(calls, mu)
	ns := notificationsIn(t, recorded)
	if rs := retractsIn(t, recorded); len(rs) != 1 || rs[0] != ns[0].CollapseID {
		t.Errorf("retracts %q, want one for %s", rs, ns[0].CollapseID)
	}

	// Sonarr keeps records of its own.
	postStarr(t, h, "/sonarr?retract=1", strings.Replace(manualInteraction("dl-S"), "Inception", "Breaking.Bad", 1))
	postStarr(t, h, "/sonarr?retract=1", `{"eventType":"Download","series":{"id":1,"title":"Breaking Bad"},
		"episodes":[{"episodeNumber":5,"seasonNumber":2}],"episodeFile":{"quality":"1080p","size":1},"downloadId":"dl-S"}`)
	rs := retractsIn(t, testutil.GetCalls(calls, mu))
	if len(rs) != 2 || rs[1] != manualInteractionCollapseID("sonarr", "dl-S") {
		t.Errorf("retracts %q, want the sonarr one last", rs)
	}
}
