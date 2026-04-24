package testutil_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func createActivity(t *testing.T, url, slug, name string) {
	t.Helper()
	body := fmt.Sprintf(`{"slug":%q,"name":%q}`, slug, name)
	resp, err := http.Post(url+"/activities", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("setup: create activity got %d", resp.StatusCode)
	}
}

func TestCreateActivity(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "valid create",
			body:       `{"slug":"my-app","name":"My App"}`,
			wantStatus: 201,
		},
		{
			name:       "missing slug",
			body:       `{"name":"My App"}`,
			wantStatus: 400,
		},
		{
			name:       "missing name",
			body:       `{"slug":"my-app-2"}`,
			wantStatus: 400,
		},
		{
			name:       "invalid slug with special chars",
			body:       `{"slug":"my app!@#","name":"Bad Slug"}`,
			wantStatus: 400,
		},
		{
			name:       "priority out of range",
			body:       `{"slug":"priority-11","name":"P11","priority":11}`,
			wantStatus: 400,
		},
		{
			name:       "ended_ttl zero",
			body:       `{"slug":"ttl-zero","name":"TTL Zero","ended_ttl":0}`,
			wantStatus: 400,
		},
		{
			name:       "ended_ttl too large",
			body:       `{"slug":"ttl-big","name":"TTL Big","ended_ttl":3000000}`,
			wantStatus: 400,
		},
		{
			name:       "stale_ttl zero",
			body:       `{"slug":"stale-zero","name":"Stale Zero","stale_ttl":0}`,
			wantStatus: 400,
		},
		{
			name:       "stale_ttl too large",
			body:       `{"slug":"stale-big","name":"Stale Big","stale_ttl":3000000}`,
			wantStatus: 400,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := testutil.MockPushWardServer(t)
			resp, err := http.Post(srv.URL+"/activities", "application/json", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("got status %d, want %d", resp.StatusCode, tt.wantStatus)
			}
		})
	}
}

// POST /activities is an upsert — the server returns 201 on duplicate slug
// with X-Resource-Action: updated, rather than 409.
func TestCreateActivity_DuplicateSlug_Upserts(t *testing.T) {
	srv, _, _ := testutil.MockPushWardServer(t)
	createActivity(t, srv.URL, "dup-slug", "First")

	body := `{"slug":"dup-slug","name":"Second"}`
	resp, err := http.Post(srv.URL+"/activities", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Errorf("got status %d, want 201", resp.StatusCode)
	}
	if action := resp.Header.Get("X-Resource-Action"); action != "updated" {
		t.Errorf("got X-Resource-Action %q, want %q", action, "updated")
	}
}

func TestUpdateActivity(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{
			name:       "valid generic update",
			body:       `{"state":"ONGOING","content":{"template":"generic","progress":0.5}}`,
			wantStatus: 200,
		},
		{
			name:       "valid alert with severity critical",
			body:       `{"state":"ONGOING","content":{"template":"alert","progress":0.0,"severity":"critical"}}`,
			wantStatus: 200,
		},
		{
			name:       "alert with invalid severity error",
			body:       `{"state":"ONGOING","content":{"template":"alert","progress":0.0,"severity":"error"}}`,
			wantStatus: 400,
		},
		{
			name:       "alert with missing severity",
			body:       `{"state":"ONGOING","content":{"template":"alert","progress":0.0}}`,
			wantStatus: 400,
		},
		{
			name:       "invalid state",
			body:       `{"state":"INVALID","content":{"template":"generic","progress":0.0}}`,
			wantStatus: 400,
		},
		{
			name:       "invalid template",
			body:       `{"state":"ONGOING","content":{"template":"unknown","progress":0.0}}`,
			wantStatus: 400,
		},
		{
			name:       "progress out of range",
			body:       `{"state":"ONGOING","content":{"template":"generic","progress":1.5}}`,
			wantStatus: 400,
		},
		{
			name:       "invalid color",
			body:       `{"state":"ONGOING","content":{"template":"generic","progress":0.5,"accent_color":"neon"}}`,
			wantStatus: 400,
		},
		{
			name:       "url missing scheme",
			body:       `{"state":"ONGOING","content":{"template":"generic","progress":0.5,"url":"example.com/page"}}`,
			wantStatus: 400,
		},
		{
			name:       "steps missing total_steps",
			body:       `{"state":"ONGOING","content":{"template":"steps","progress":0.5,"current_step":1}}`,
			wantStatus: 400,
		},
		{
			name:       "steps current_step exceeds total_steps",
			body:       `{"state":"ONGOING","content":{"template":"steps","progress":0.5,"current_step":5,"total_steps":3}}`,
			wantStatus: 400,
		},
		{
			name:       "countdown missing end_date",
			body:       `{"state":"ONGOING","content":{"template":"countdown","progress":0.5}}`,
			wantStatus: 400,
		},
		{
			name:       "gauge missing value",
			body:       `{"state":"ONGOING","content":{"template":"gauge","progress":0.5,"min_value":0,"max_value":100}}`,
			wantStatus: 400,
		},
		{
			name:       "gauge value out of range",
			body:       `{"state":"ONGOING","content":{"template":"gauge","progress":0.5,"value":150,"min_value":0,"max_value":100}}`,
			wantStatus: 400,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := testutil.MockPushWardServer(t)
			createActivity(t, srv.URL, "test-app", "Test App")

			req, err := http.NewRequest(http.MethodPatch, srv.URL+"/activities/test-app", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("got status %d, want %d", resp.StatusCode, tt.wantStatus)
			}
		})
	}
}

func TestUpdateActivity_NonExistentSlug(t *testing.T) {
	srv, _, _ := testutil.MockPushWardServer(t)

	body := `{"state":"ONGOING","content":{"template":"generic","progress":0.5}}`
	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/activities/no-such-app", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("got status %d, want 404", resp.StatusCode)
	}
}

func TestUnknownRoute_Passthrough(t *testing.T) {
	srv, _, _ := testutil.MockPushWardServer(t)

	resp, err := http.Get(srv.URL + "/some/unknown/path")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("got status %d, want 200", resp.StatusCode)
	}
}

func TestAPICallRecording(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)

	createActivity(t, srv.URL, "rec-app", "Recorded App")

	body := `{"state":"ONGOING","content":{"template":"generic","progress":0.5}}`
	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/activities/rec-app", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected 2 recorded calls, got %d", len(recorded))
	}

	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("call[0]: got %s %s, want POST /activities", recorded[0].Method, recorded[0].Path)
	}

	var createBody map[string]interface{}
	if err := json.Unmarshal(recorded[0].Body, &createBody); err != nil {
		t.Fatalf("failed to unmarshal call[0] body: %v", err)
	}
	if createBody["slug"] != "rec-app" {
		t.Errorf("call[0] slug: got %v, want rec-app", createBody["slug"])
	}

	if recorded[1].Method != "PATCH" || recorded[1].Path != "/activities/rec-app" {
		t.Errorf("call[1]: got %s %s, want PATCH /activities/rec-app", recorded[1].Method, recorded[1].Path)
	}
}
