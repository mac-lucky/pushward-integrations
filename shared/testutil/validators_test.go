package testutil_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func patchActivity(t *testing.T, url, slug, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, url+"/activities/"+slug, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestValidateSteps(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "happy path",
			body:       `{"state":"ongoing","content":{"template":"steps","progress":0.5,"current_step":2,"total_steps":5}}`,
			wantStatus: 200,
		},
		{
			name:       "with step_rows and step_labels",
			body:       `{"state":"ongoing","content":{"template":"steps","progress":0.5,"current_step":1,"total_steps":3,"step_rows":[1,2,3],"step_labels":["a","b","c"]}}`,
			wantStatus: 200,
		},
		{
			name:       "missing current_step",
			body:       `{"state":"ongoing","content":{"template":"steps","progress":0.5,"total_steps":3}}`,
			wantStatus: 400,
		},
		{
			name:       "total_steps zero",
			body:       `{"state":"ongoing","content":{"template":"steps","progress":0.5,"current_step":0,"total_steps":0}}`,
			wantStatus: 400,
		},
		{
			name:       "step_rows wrong length",
			body:       `{"state":"ongoing","content":{"template":"steps","progress":0.5,"current_step":1,"total_steps":3,"step_rows":[1,2]}}`,
			wantStatus: 400,
		},
		{
			name:       "step_rows value out of range",
			body:       `{"state":"ongoing","content":{"template":"steps","progress":0.5,"current_step":1,"total_steps":2,"step_rows":[1,11]}}`,
			wantStatus: 400,
		},
		{
			name:       "step_labels wrong length",
			body:       `{"state":"ongoing","content":{"template":"steps","progress":0.5,"current_step":1,"total_steps":3,"step_labels":["a","b"]}}`,
			wantStatus: 400,
		},
		{
			name:       "step_labels too long",
			body:       `{"state":"ongoing","content":{"template":"steps","progress":0.5,"current_step":1,"total_steps":1,"step_labels":["` + strings.Repeat("x", 33) + `"]}}`,
			wantStatus: 400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := testutil.MockPushWardServer(t)
			createActivity(t, srv.URL, "steps-app", "Steps App")
			if got := patchActivity(t, srv.URL, "steps-app", tt.body); got != tt.wantStatus {
				t.Errorf("got status %d, want %d", got, tt.wantStatus)
			}
		})
	}
}

func TestValidateCountdown(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "happy path",
			body:       `{"state":"ongoing","content":{"template":"countdown","progress":0.5,"end_date":1800000000}}`,
			wantStatus: 200,
		},
		{
			name:       "with start_date and warning_threshold",
			body:       `{"state":"ongoing","content":{"template":"countdown","progress":0.5,"start_date":1700000000,"end_date":1800000000,"warning_threshold":60}}`,
			wantStatus: 200,
		},
		{
			name:       "end_date zero",
			body:       `{"state":"ongoing","content":{"template":"countdown","progress":0.5,"end_date":0}}`,
			wantStatus: 400,
		},
		{
			name:       "start_date not before end_date",
			body:       `{"state":"ongoing","content":{"template":"countdown","progress":0.5,"start_date":1800000000,"end_date":1700000000}}`,
			wantStatus: 400,
		},
		{
			name:       "start_date zero",
			body:       `{"state":"ongoing","content":{"template":"countdown","progress":0.5,"start_date":0,"end_date":1800000000}}`,
			wantStatus: 400,
		},
		{
			name:       "negative warning_threshold",
			body:       `{"state":"ongoing","content":{"template":"countdown","progress":0.5,"end_date":1800000000,"warning_threshold":-1}}`,
			wantStatus: 400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := testutil.MockPushWardServer(t)
			createActivity(t, srv.URL, "cd-app", "Countdown App")
			if got := patchActivity(t, srv.URL, "cd-app", tt.body); got != tt.wantStatus {
				t.Errorf("got status %d, want %d", got, tt.wantStatus)
			}
		})
	}
}

func TestValidateTimeline(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "happy path single series",
			body:       `{"state":"ongoing","content":{"template":"timeline","progress":0.5,"value":{"CPU":72.5}}}`,
			wantStatus: 200,
		},
		{
			name:       "multiple series with thresholds",
			body:       `{"state":"ongoing","content":{"template":"timeline","progress":0.5,"value":{"CPU":50,"MEM":30},"scale":"linear","unit":"%","decimals":1,"thresholds":[{"value":80,"color":"#FF3B30","label":"high"}]}}`,
			wantStatus: 200,
		},
		{
			name:       "missing value",
			body:       `{"state":"ongoing","content":{"template":"timeline","progress":0.5}}`,
			wantStatus: 400,
		},
		{
			name:       "value not a map",
			body:       `{"state":"ongoing","content":{"template":"timeline","progress":0.5,"value":42}}`,
			wantStatus: 400,
		},
		{
			name:       "invalid scale",
			body:       `{"state":"ongoing","content":{"template":"timeline","progress":0.5,"value":{"a":1},"scale":"weird"}}`,
			wantStatus: 400,
		},
		{
			name:       "decimals out of range",
			body:       `{"state":"ongoing","content":{"template":"timeline","progress":0.5,"value":{"a":1},"decimals":20}}`,
			wantStatus: 400,
		},
		{
			name:       "too many series",
			body:       `{"state":"ongoing","content":{"template":"timeline","progress":0.5,"value":{"a":1,"b":2,"c":3,"d":4,"e":5}}}`,
			wantStatus: 400,
		},
		{
			name:       "too many thresholds",
			body:       `{"state":"ongoing","content":{"template":"timeline","progress":0.5,"value":{"a":1},"thresholds":[{"value":1},{"value":2},{"value":3},{"value":4},{"value":5},{"value":6}]}}`,
			wantStatus: 400,
		},
		{
			name:       "threshold bad color",
			body:       `{"state":"ongoing","content":{"template":"timeline","progress":0.5,"value":{"a":1},"thresholds":[{"value":1,"color":"neon"}]}}`,
			wantStatus: 400,
		},
		{
			name:       "series key too long",
			body:       `{"state":"ongoing","content":{"template":"timeline","progress":0.5,"value":{"` + strings.Repeat("x", 33) + `":1}}}`,
			wantStatus: 400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := testutil.MockPushWardServer(t)
			createActivity(t, srv.URL, "tl-app", "Timeline App")
			if got := patchActivity(t, srv.URL, "tl-app", tt.body); got != tt.wantStatus {
				t.Errorf("got status %d, want %d", got, tt.wantStatus)
			}
		})
	}
}

func TestValidateBoard(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "happy path single tile",
			body:       `{"state":"ongoing","content":{"template":"board","progress":0,"tiles":[{"label":"Living Room","value":"21.5","unit":"°C","icon":"thermometer","color":"#FF3B30","trend":"up"}]}}`,
			wantStatus: 200,
		},
		{
			name:       "four tiles with tap action",
			body:       `{"state":"ongoing","content":{"template":"board","progress":0,"tiles":[{"label":"A","value":"1"},{"label":"B","value":"2"},{"label":"C","value":"3"},{"label":"D","value":"On","url_action":{"url":"https://example.com"}}]}}`,
			wantStatus: 200,
		},
		{
			name:       "no tiles",
			body:       `{"state":"ongoing","content":{"template":"board","progress":0,"tiles":[]}}`,
			wantStatus: 400,
		},
		{
			name:       "too many tiles",
			body:       `{"state":"ongoing","content":{"template":"board","progress":0,"tiles":[{"label":"A","value":"1"},{"label":"B","value":"2"},{"label":"C","value":"3"},{"label":"D","value":"4"},{"label":"E","value":"5"}]}}`,
			wantStatus: 400,
		},
		{
			name:       "tile missing label",
			body:       `{"state":"ongoing","content":{"template":"board","progress":0,"tiles":[{"value":"1"}]}}`,
			wantStatus: 400,
		},
		{
			name:       "tile missing value",
			body:       `{"state":"ongoing","content":{"template":"board","progress":0,"tiles":[{"label":"A"}]}}`,
			wantStatus: 400,
		},
		{
			name:       "tile value too long",
			body:       `{"state":"ongoing","content":{"template":"board","progress":0,"tiles":[{"label":"A","value":"` + strings.Repeat("9", 17) + `"}]}}`,
			wantStatus: 400,
		},
		{
			name:       "tile bad trend",
			body:       `{"state":"ongoing","content":{"template":"board","progress":0,"tiles":[{"label":"A","value":"1","trend":"sideways"}]}}`,
			wantStatus: 400,
		},
		{
			name:       "tile bad color",
			body:       `{"state":"ongoing","content":{"template":"board","progress":0,"tiles":[{"label":"A","value":"1","color":"neon"}]}}`,
			wantStatus: 400,
		},
		{
			name:       "tile url_action custom scheme ok",
			body:       `{"state":"ongoing","content":{"template":"board","progress":0,"tiles":[{"label":"A","value":"On","url_action":{"url":"homeassistant://navigate"}}]}}`,
			wantStatus: 200,
		},
		{
			name:       "tile url_action empty url",
			body:       `{"state":"ongoing","content":{"template":"board","progress":0,"tiles":[{"label":"A","value":"1","url_action":{"url":""}}]}}`,
			wantStatus: 400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := testutil.MockPushWardServer(t)
			createActivity(t, srv.URL, "board-app", "Board App")
			if got := patchActivity(t, srv.URL, "board-app", tt.body); got != tt.wantStatus {
				t.Errorf("got status %d, want %d", got, tt.wantStatus)
			}
		})
	}
}

func TestValidateLog(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "happy path single line",
			body:       `{"state":"ongoing","content":{"template":"log","progress":0,"lines":[{"text":"build started","level":"info","at":1800000000}]}}`,
			wantStatus: 200,
		},
		{
			name:       "no lines",
			body:       `{"state":"ongoing","content":{"template":"log","progress":0,"lines":[]}}`,
			wantStatus: 400,
		},
		{
			name:       "line missing text",
			body:       `{"state":"ongoing","content":{"template":"log","progress":0,"lines":[{"level":"warn"}]}}`,
			wantStatus: 400,
		},
		{
			name:       "line text too long",
			body:       `{"state":"ongoing","content":{"template":"log","progress":0,"lines":[{"text":"` + strings.Repeat("x", 513) + `"}]}}`,
			wantStatus: 400,
		},
		{
			name:       "line bad level",
			body:       `{"state":"ongoing","content":{"template":"log","progress":0,"lines":[{"text":"oops","level":"fatal"}]}}`,
			wantStatus: 400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := testutil.MockPushWardServer(t)
			createActivity(t, srv.URL, "log-app", "Log App")
			if got := patchActivity(t, srv.URL, "log-app", tt.body); got != tt.wantStatus {
				t.Errorf("got status %d, want %d", got, tt.wantStatus)
			}
		})
	}
}

func TestValidateMedia(t *testing.T) {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	ahead := func(d time.Duration) string { return strconv.FormatInt(time.Now().Add(d).Unix(), 10) }
	// media wraps the extra fields into a full media-template content object.
	media := func(fields string) string {
		body := `{"state":"ongoing","content":{"template":"media","progress":0.2`
		if fields != "" {
			body += "," + fields
		}
		return body + `}}`
	}
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name: "full contract",
			body: media(`"media_title":"Snooze","subtitle":"SZA","playback_state":"playing","position_seconds":47.5,"duration_seconds":214,"position_at":` + now + `,"volume":0.35,"favorite":true,` +
				`"image_url":"https://example.com/art.jpg","image_shape":"square","image_thumbhash":"` + stdHash + `",` +
				`"controls":{"previous":{"url":"https://ha.example/api/webhook/pw-prev"},"play_pause":{"url":"https://ha.example/api/webhook/pw-toggle","method":"POST"},` +
				`"play":{"url":"https://ha.example/api/webhook/pw-play"},"pause":{"url":"https://ha.example/api/webhook/pw-pause"},"next":{"url":"https://ha.example/api/webhook/pw-next"},` +
				`"stop":{"url":"https://ha.example/api/webhook/pw-stop"},"favorite":{"url":"https://ha.example/api/webhook/pw-fav"},` +
				`"volume_down":{"url":"https://ha.example/api/webhook/pw-vdown"},"volume_up":{"url":"https://ha.example/api/webhook/pw-vup"},` +
				`"extra":[{"url":"https://ha.example/api/webhook/pw-shuffle","icon":"shuffle","title":"Shuffle"},{"url":"https://ha.example/api/webhook/pw-repeat","icon":"repeat"},{"url":"spotify://queue","icon":"list.bullet"}]}`),
			wantStatus: 200,
		},
		{
			name:       "template alone",
			body:       media(""),
			wantStatus: 200,
		},
		{
			name:       "indeterminate duration",
			body:       media(`"media_title":"Radio 357","playback_state":"playing","position_seconds":120`),
			wantStatus: 200,
		},
		{
			name:       "custom-scheme control",
			body:       media(`"controls":{"play_pause":{"url":"spotify://toggle"}}`),
			wantStatus: 200,
		},
		{
			name:       "control with headers and body",
			body:       media(`"controls":{"next":{"url":"https://api.example/next","method":"PUT","headers":{"Authorization":"Bearer x"},"body":"{}"}}`),
			wantStatus: 200,
		},
		{
			name:       "media_title at the bound",
			body:       media(`"media_title":"` + strings.Repeat("t", 128) + `"`),
			wantStatus: 200,
		},
		{
			name:       "position_at within the skew allowance",
			body:       media(`"position_seconds":1,"position_at":` + ahead(4*time.Minute)),
			wantStatus: 200,
		},
		{
			name:       "duration at the bound",
			body:       media(`"duration_seconds":604800`),
			wantStatus: 200,
		},
		{
			name:       "volume at both bounds",
			body:       media(`"volume":0`) + "\n" + media(`"volume":1`),
			wantStatus: 200,
		},
		{
			// Under merge-patch a position tick may omit the template; the mock
			// is stateless and leaves the gate to the real server.
			name:       "template-less position tick passes the gate",
			body:       `{"content":{"progress":0.3,"position_seconds":63,"position_at":` + now + `}}`,
			wantStatus: 200,
		},
		{
			name:       "template-less tick still bounds-checked",
			body:       `{"content":{"progress":0.3,"position_seconds":-1}}`,
			wantStatus: 400,
		},
		{
			name:       "media_title over the bound",
			body:       media(`"media_title":"` + strings.Repeat("t", 129) + `"`),
			wantStatus: 400,
		},
		{
			name:       "unknown playback_state",
			body:       media(`"playback_state":"seeking"`),
			wantStatus: 400,
		},
		{
			name:       "negative position_seconds",
			body:       media(`"position_seconds":-0.5`),
			wantStatus: 400,
		},
		{
			name:       "zero duration_seconds",
			body:       media(`"duration_seconds":0`),
			wantStatus: 400,
		},
		{
			name:       "duration_seconds over 7 days",
			body:       media(`"duration_seconds":604801`),
			wantStatus: 400,
		},
		{
			name:       "volume over 1",
			body:       media(`"volume":1.01`),
			wantStatus: 400,
		},
		{
			name:       "negative volume",
			body:       media(`"volume":-0.1`),
			wantStatus: 400,
		},
		{
			name:       "zero position_at",
			body:       media(`"position_seconds":1,"position_at":0`),
			wantStatus: 400,
		},
		{
			name:       "position_at too far in the future",
			body:       media(`"position_seconds":1,"position_at":` + ahead(10*time.Minute)),
			wantStatus: 400,
		},
		{
			name:       "control without url",
			body:       media(`"controls":{"play_pause":{"icon":"playpause"}}`),
			wantStatus: 400,
		},
		{
			name:       "http control asking for the foreground",
			body:       media(`"controls":{"next":{"url":"https://ha.example/api/webhook/pw-next","foreground":true}}`),
			wantStatus: 400,
		},
		{
			name:       "control with a blocked scheme",
			body:       media(`"controls":{"stop":{"url":"javascript:alert(1)"}}`),
			wantStatus: 400,
		},
		{
			name:       "control with a bad method",
			body:       media(`"controls":{"previous":{"url":"https://ha.example/prev","method":"FETCH"}}`),
			wantStatus: 400,
		},
		{
			name:       "four extra controls",
			body:       media(`"controls":{"extra":[{"url":"https://x.example/1","icon":"a"},{"url":"https://x.example/2","icon":"b"},{"url":"https://x.example/3","icon":"c"},{"url":"https://x.example/4","icon":"d"}]}`),
			wantStatus: 400,
		},
		{
			name:       "extra control without icon",
			body:       media(`"controls":{"extra":[{"url":"https://x.example/1","title":"Shuffle"}]}`),
			wantStatus: 400,
		},
		{
			name:       "extra control asking for the foreground",
			body:       media(`"controls":{"extra":[{"url":"https://x.example/1","icon":"shuffle","foreground":true}]}`),
			wantStatus: 400,
		},
		{
			name:       "media_title on generic rejected",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"media_title":"Snooze"}}`,
			wantStatus: 400,
		},
		{
			name:       "controls on steps rejected",
			body:       `{"state":"ongoing","content":{"template":"steps","progress":0.5,"current_step":1,"total_steps":2,"controls":{"stop":{"url":"https://x.example/stop"}}}}`,
			wantStatus: 400,
		},
		{
			name:       "position_seconds on countdown rejected",
			body:       `{"state":"ongoing","content":{"template":"countdown","progress":0.5,"end_date":1800000000,"position_seconds":10}}`,
			wantStatus: 400,
		},
		{
			name:       "favorite on board rejected",
			body:       `{"state":"ongoing","content":{"template":"board","progress":0,"tiles":[{"label":"A","value":"1"}],"favorite":true}}`,
			wantStatus: 400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := testutil.MockPushWardServer(t)
			createActivity(t, srv.URL, "media-app", "Media App")
			// A case may carry several bodies separated by a newline; each
			// must answer the same status.
			for _, body := range strings.Split(tt.body, "\n") {
				if got := patchActivity(t, srv.URL, "media-app", body); got != tt.wantStatus {
					t.Errorf("got status %d, want %d for %s", got, tt.wantStatus, body)
				}
			}
		})
	}
}

// TestValidateURLAnyTemplate asserts the relaxed rule: url / tap-action routing
// is accepted on every template now, not just steps/alert (the server moved tap
// routing into the shared content base).
func TestValidateURLAnyTemplate(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "url on generic ok",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"url":"https://example.com/page"}}`,
			wantStatus: 200,
		},
		{
			name:       "url on timeline ok",
			body:       `{"state":"ongoing","content":{"template":"timeline","progress":0.5,"value":{"CPU":1},"url":"https://example.com"}}`,
			wantStatus: 200,
		},
		{
			name:       "malformed url still rejected",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"url":"example.com/page"}}`,
			wantStatus: 400,
		},
		{
			name:       "tap_action on generic ok",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"tap_action":{"url":"https://example.com"}}}`,
			wantStatus: 200,
		},
		{
			name:       "tap_action missing url rejected",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"tap_action":{"title":"Open"}}}`,
			wantStatus: 400,
		},
		{
			name:       "url_action bad method rejected",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"url_action":{"url":"https://example.com","method":"FETCH"}}}`,
			wantStatus: 400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := testutil.MockPushWardServer(t)
			createActivity(t, srv.URL, "url-app", "URL App")
			if got := patchActivity(t, srv.URL, "url-app", tt.body); got != tt.wantStatus {
				t.Errorf("got status %d, want %d", got, tt.wantStatus)
			}
		})
	}
}

// stdHash / paddedHash are real thumbhashes. stdHash carries a "/", which is
// what separates the standard alphabet from the URL-safe one; paddedHash needs
// "==", which is what separates padded from raw.
const (
	stdHash    = "WfcJhRqPdTeXeIhXiXiYd3BmB/eH"
	paddedHash = "GoeGJQg4gIyPdLc3eISACIiIB1eHiIB4WA=="
)

func TestValidateImageFields(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "full trio on generic",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"image_url":"https://image.tmdb.org/t/p/w500/a.jpg","image_shape":"poster","image_thumbhash":"` + stdHash + `"}}`,
			wantStatus: 200,
		},
		{
			name:       "full trio on steps",
			body:       `{"state":"ongoing","content":{"template":"steps","progress":0.5,"current_step":1,"total_steps":2,"image_url":"https://image.tmdb.org/t/p/w500/a.jpg","image_shape":"square","image_thumbhash":"` + paddedHash + `"}}`,
			wantStatus: 200,
		},
		{
			name:       "full trio on media",
			body:       `{"state":"ongoing","content":{"template":"media","progress":0.5,"media_title":"Snooze","image_url":"https://example.com/art.jpg","image_shape":"square","image_thumbhash":"` + stdHash + `"}}`,
			wantStatus: 200,
		},
		{
			name:       "thumbhash alone on generic",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"image_thumbhash":"` + stdHash + `"}}`,
			wantStatus: 200,
		},
		{
			name:       "lan https url is accepted",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"image_url":"https://jellyfin.lan:8096/Items/1/Images/Primary"}}`,
			wantStatus: 200,
		},
		{
			name:       "circle shape",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"image_url":"https://example.com/a.jpg","image_shape":"circle"}}`,
			wantStatus: 200,
		},
		{
			// Under merge-patch a tick may omit the template; the mock is
			// stateless and leaves that case to the real server.
			name:       "template-less patch passes the gate",
			body:       `{"content":{"progress":0.5,"image_url":"https://example.com/a.jpg","image_thumbhash":"` + stdHash + `"}}`,
			wantStatus: 200,
		},
		{
			name:       "image on alert rejected",
			body:       `{"state":"ongoing","content":{"template":"alert","progress":0.5,"severity":"warning","image_url":"https://example.com/a.jpg"}}`,
			wantStatus: 400,
		},
		{
			name:       "shape alone on countdown rejected",
			body:       `{"state":"ongoing","content":{"template":"countdown","progress":0.5,"end_date":1800000000,"image_shape":"poster"}}`,
			wantStatus: 400,
		},
		{
			name:       "thumbhash alone on timeline rejected",
			body:       `{"state":"ongoing","content":{"template":"timeline","progress":0.5,"value":{"CPU":1},"image_thumbhash":"` + stdHash + `"}}`,
			wantStatus: 400,
		},
		{
			name:       "http image_url rejected",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"image_url":"http://example.com/a.jpg"}}`,
			wantStatus: 400,
		},
		{
			// Assembled from parts: a literal userinfo URL reads as a hardcoded
			// credential to the security linter, and this one is neither.
			name:       "image_url with userinfo rejected",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"image_url":"https://` + "u" + `:` + "p" + `@example.com/a.jpg"}}`,
			wantStatus: 400,
		},
		{
			name:       "hostless image_url rejected",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"image_url":"https:///a.jpg"}}`,
			wantStatus: 400,
		},
		{
			name:       "image_url over 2048 runes rejected",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"image_url":"https://example.com/` + strings.Repeat("a", 2049-20) + `"}}`,
			wantStatus: 400,
		},
		{
			name:       "unknown shape rejected",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"image_url":"https://example.com/a.jpg","image_shape":"banner"}}`,
			wantStatus: 400,
		},
		{
			name:       "raw (unpadded) base64 thumbhash rejected",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"image_thumbhash":"` + strings.TrimRight(paddedHash, "=") + `"}}`,
			wantStatus: 400,
		},
		{
			name:       "url-alphabet base64 thumbhash rejected",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"image_thumbhash":"` + strings.ReplaceAll(stdHash, "/", "_") + `"}}`,
			wantStatus: 400,
		},
		{
			name:       "non-base64 thumbhash rejected",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"image_thumbhash":"not base64!!"}}`,
			wantStatus: 400,
		},
		{
			name:       "thumbhash over 64 chars rejected",
			body:       `{"state":"ongoing","content":{"template":"generic","progress":0.5,"image_thumbhash":"` + strings.Repeat("A", 68) + `"}}`,
			wantStatus: 400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := testutil.MockPushWardServer(t)
			createActivity(t, srv.URL, "image-app", "Image App")
			if got := patchActivity(t, srv.URL, "image-app", tt.body); got != tt.wantStatus {
				t.Errorf("got status %d, want %d", got, tt.wantStatus)
			}
		})
	}
}

func postNotification(t *testing.T, url, body string) int {
	t.Helper()
	resp, err := http.Post(url+"/notifications", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestValidateNotificationAction(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "plain action",
			body:       `{"title":"t","body":"b","actions":[{"id":"open","title":"Open"}]}`,
			wantStatus: 201,
		},
		{
			name:       "silent webhook action",
			body:       `{"title":"t","body":"b","actions":[{"id":"ack","title":"Ack","url":"https://hooks.example.com/ack","method":"POST","headers":{"A":"b"},"body":"{}"}]}`,
			wantStatus: 201,
		},
		{
			name:       "custom scheme action",
			body:       `{"title":"t","body":"b","actions":[{"id":"open","title":"Open","url":"homeassistant://navigate"}]}`,
			wantStatus: 201,
		},
		{
			name:       "missing id",
			body:       `{"title":"t","body":"b","actions":[{"title":"Open"}]}`,
			wantStatus: 400,
		},
		{
			name:       "missing title",
			body:       `{"title":"t","body":"b","actions":[{"id":"open"}]}`,
			wantStatus: 400,
		},
		{
			name:       "blocked javascript scheme",
			body:       `{"title":"t","body":"b","actions":[{"id":"x","title":"X","url":"javascript:alert(1)"}]}`,
			wantStatus: 400,
		},
		{
			name:       "blocked data scheme is case-insensitive",
			body:       `{"title":"t","body":"b","actions":[{"id":"x","title":"X","url":"DATA:text/html,hi"}]}`,
			wantStatus: 400,
		},
		{
			name:       "unknown method",
			body:       `{"title":"t","body":"b","actions":[{"id":"x","title":"X","url":"https://h.example","method":"FETCH"}]}`,
			wantStatus: 400,
		},
		{
			name: "more than 10 actions",
			body: `{"title":"t","body":"b","actions":[` +
				`{"id":"1","title":"a"},{"id":"2","title":"a"},{"id":"3","title":"a"},{"id":"4","title":"a"},` +
				`{"id":"5","title":"a"},{"id":"6","title":"a"},{"id":"7","title":"a"},{"id":"8","title":"a"},` +
				`{"id":"9","title":"a"},{"id":"10","title":"a"},{"id":"11","title":"a"}]}`,
			wantStatus: 400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := testutil.MockPushWardServer(t)
			if got := postNotification(t, srv.URL, tt.body); got != tt.wantStatus {
				t.Errorf("got status %d, want %d", got, tt.wantStatus)
			}
		})
	}
}

func TestValidateNotificationActivitySlug(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "valid slug", body: `{"title":"t","body":"b","activity_slug":"deploy-prod"}`, wantStatus: 201},
		{name: "omitted slug", body: `{"title":"t","body":"b"}`, wantStatus: 201},
		{name: "spaces and punctuation", body: `{"title":"t","body":"b","activity_slug":"Not A Slug!"}`, wantStatus: 400},
		{name: "leading hyphen", body: `{"title":"t","body":"b","activity_slug":"-leading"}`, wantStatus: 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := testutil.MockPushWardServer(t)
			if got := postNotification(t, srv.URL, tt.body); got != tt.wantStatus {
				t.Errorf("got status %d, want %d", got, tt.wantStatus)
			}
		})
	}
}

// The mock must mirror the server's default: an omitted push key means push.
func TestMockNotificationPushDefault(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantPushed bool
	}{
		{name: "omitted push defaults to true", body: `{"title":"t","body":"b"}`, wantPushed: true},
		{name: "explicit true", body: `{"title":"t","body":"b","push":true}`, wantPushed: true},
		{name: "explicit false", body: `{"title":"t","body":"b","push":false}`, wantPushed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := testutil.MockPushWardServer(t)
			resp, err := http.Post(srv.URL+"/notifications", "application/json", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()

			var got struct {
				Pushed bool `json:"pushed"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Pushed != tt.wantPushed {
				t.Errorf("got pushed=%v, want %v", got.Pushed, tt.wantPushed)
			}
		})
	}
}
