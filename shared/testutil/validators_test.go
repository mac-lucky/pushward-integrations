package testutil_test

import (
	"net/http"
	"strings"
	"testing"

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
