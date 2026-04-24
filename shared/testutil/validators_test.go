package testutil_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func patchActivity(t *testing.T, url, slug, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, url+"/activity/"+slug, strings.NewReader(body))
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
			body:       `{"state":"ONGOING","content":{"template":"steps","progress":0.5,"current_step":2,"total_steps":5}}`,
			wantStatus: 200,
		},
		{
			name:       "with step_rows and step_labels",
			body:       `{"state":"ONGOING","content":{"template":"steps","progress":0.5,"current_step":1,"total_steps":3,"step_rows":[1,2,3],"step_labels":["a","b","c"]}}`,
			wantStatus: 200,
		},
		{
			name:       "missing current_step",
			body:       `{"state":"ONGOING","content":{"template":"steps","progress":0.5,"total_steps":3}}`,
			wantStatus: 400,
		},
		{
			name:       "total_steps zero",
			body:       `{"state":"ONGOING","content":{"template":"steps","progress":0.5,"current_step":0,"total_steps":0}}`,
			wantStatus: 400,
		},
		{
			name:       "step_rows wrong length",
			body:       `{"state":"ONGOING","content":{"template":"steps","progress":0.5,"current_step":1,"total_steps":3,"step_rows":[1,2]}}`,
			wantStatus: 400,
		},
		{
			name:       "step_rows value out of range",
			body:       `{"state":"ONGOING","content":{"template":"steps","progress":0.5,"current_step":1,"total_steps":2,"step_rows":[1,11]}}`,
			wantStatus: 400,
		},
		{
			name:       "step_labels wrong length",
			body:       `{"state":"ONGOING","content":{"template":"steps","progress":0.5,"current_step":1,"total_steps":3,"step_labels":["a","b"]}}`,
			wantStatus: 400,
		},
		{
			name:       "step_labels too long",
			body:       `{"state":"ONGOING","content":{"template":"steps","progress":0.5,"current_step":1,"total_steps":1,"step_labels":["` + strings.Repeat("x", 33) + `"]}}`,
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
			body:       `{"state":"ONGOING","content":{"template":"countdown","progress":0.5,"end_date":1800000000}}`,
			wantStatus: 200,
		},
		{
			name:       "with start_date and warning_threshold",
			body:       `{"state":"ONGOING","content":{"template":"countdown","progress":0.5,"start_date":1700000000,"end_date":1800000000,"warning_threshold":60}}`,
			wantStatus: 200,
		},
		{
			name:       "end_date zero",
			body:       `{"state":"ONGOING","content":{"template":"countdown","progress":0.5,"end_date":0}}`,
			wantStatus: 400,
		},
		{
			name:       "start_date not before end_date",
			body:       `{"state":"ONGOING","content":{"template":"countdown","progress":0.5,"start_date":1800000000,"end_date":1700000000}}`,
			wantStatus: 400,
		},
		{
			name:       "start_date zero",
			body:       `{"state":"ONGOING","content":{"template":"countdown","progress":0.5,"start_date":0,"end_date":1800000000}}`,
			wantStatus: 400,
		},
		{
			name:       "negative warning_threshold",
			body:       `{"state":"ONGOING","content":{"template":"countdown","progress":0.5,"end_date":1800000000,"warning_threshold":-1}}`,
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
			body:       `{"state":"ONGOING","content":{"template":"timeline","progress":0.5,"value":{"CPU":72.5}}}`,
			wantStatus: 200,
		},
		{
			name:       "multiple series with thresholds",
			body:       `{"state":"ONGOING","content":{"template":"timeline","progress":0.5,"value":{"CPU":50,"MEM":30},"scale":"linear","unit":"%","decimals":1,"thresholds":[{"value":80,"color":"#FF3B30","label":"high"}]}}`,
			wantStatus: 200,
		},
		{
			name:       "missing value",
			body:       `{"state":"ONGOING","content":{"template":"timeline","progress":0.5}}`,
			wantStatus: 400,
		},
		{
			name:       "value not a map",
			body:       `{"state":"ONGOING","content":{"template":"timeline","progress":0.5,"value":42}}`,
			wantStatus: 400,
		},
		{
			name:       "invalid scale",
			body:       `{"state":"ONGOING","content":{"template":"timeline","progress":0.5,"value":{"a":1},"scale":"weird"}}`,
			wantStatus: 400,
		},
		{
			name:       "decimals out of range",
			body:       `{"state":"ONGOING","content":{"template":"timeline","progress":0.5,"value":{"a":1},"decimals":20}}`,
			wantStatus: 400,
		},
		{
			name:       "too many series",
			body:       `{"state":"ONGOING","content":{"template":"timeline","progress":0.5,"value":{"a":1,"b":2,"c":3,"d":4,"e":5}}}`,
			wantStatus: 400,
		},
		{
			name:       "too many thresholds",
			body:       `{"state":"ONGOING","content":{"template":"timeline","progress":0.5,"value":{"a":1},"thresholds":[{"value":1},{"value":2},{"value":3},{"value":4},{"value":5},{"value":6}]}}`,
			wantStatus: 400,
		},
		{
			name:       "threshold bad color",
			body:       `{"state":"ONGOING","content":{"template":"timeline","progress":0.5,"value":{"a":1},"thresholds":[{"value":1,"color":"neon"}]}}`,
			wantStatus: 400,
		},
		{
			name:       "series key too long",
			body:       `{"state":"ONGOING","content":{"template":"timeline","progress":0.5,"value":{"` + strings.Repeat("x", 33) + `":1}}}`,
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
