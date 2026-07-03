package gitea

import (
	"fmt"
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
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

func testConfig() *config.GiteaConfig {
	return &config.GiteaConfig{
		BaseProviderConfig: config.BaseProviderConfig{
			Enabled:        true,
			Priority:       3,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
			CleanupDelay:   1 * time.Hour,
			StaleTimeout:   4 * time.Hour,
		},
	}
}

func newHandler(t *testing.T) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL, nil)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, store, pool, testConfig())
	return mux, calls, mu
}

func send(t *testing.T, h http.Handler, path, payload string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s: expected 200, got %d (%s)", path, w.Code, w.Body.String())
	}
	return w
}

func runBody(action, status, conclusion string, id int64) string {
	return fmt.Sprintf(`{
		"action": %q,
		"workflow": {"name": "CI", "path": ".gitea/workflows/ci.yml"},
		"workflow_run": {"id": %d, "display_title": "Add feature", "head_branch": "main", "run_number": 7, "status": %q, "conclusion": %q, "html_url": "https://gitea.example.com/acme/app/actions/runs/%d"},
		"repository": {"full_name": "acme/app", "html_url": "https://gitea.example.com/acme/app"}
	}`, action, id, status, conclusion, id)
}

func jobBody(action, status, conclusion, name string, jobID, runID int64) string {
	return fmt.Sprintf(`{
		"action": %q,
		"workflow_job": {"id": %d, "run_id": %d, "name": %q, "status": %q, "conclusion": %q, "html_url": "https://gitea.example.com/acme/app/actions/runs/%d/jobs/%d"},
		"repository": {"full_name": "acme/app", "html_url": "https://gitea.example.com/acme/app"}
	}`, action, jobID, runID, name, status, conclusion, runID, jobID)
}

func forgejoBody(action string, id int64) string {
	return fmt.Sprintf(`{
		"action": %q,
		"prior_status": "running",
		"run": {"id": %d, "title": "Build and test", "workflow_id": "ci.yml", "prettyref": "main", "html_url": "https://forgejo.example.com/acme/app/actions/runs/%d", "repository": {"full_name": "acme/app", "html_url": "https://forgejo.example.com/acme/app"}}
	}`, action, id, id)
}

func lastEnded(t *testing.T, calls []testutil.APICall) pushward.UpdateRequest {
	t.Helper()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Method == "PATCH" {
			var u pushward.UpdateRequest
			testutil.UnmarshalBody(t, calls[i].Body, &u)
			if u.State == pushward.StateEnded {
				return u
			}
		}
	}
	t.Fatal("no ENDED update found")
	return pushward.UpdateRequest{}
}

func TestGiteaRunLifecycle(t *testing.T) {
	h, calls, mu := newHandler(t)

	send(t, h, "/gitea", runBody("requested", "queued", "", 42))
	send(t, h, "/gitea", jobBody("queued", "queued", "", "Build (ubuntu, node-20)", 100, 42))
	send(t, h, "/gitea", jobBody("in_progress", "in_progress", "", "Build (ubuntu, node-20)", 100, 42))
	send(t, h, "/gitea", jobBody("completed", "completed", "success", "Build (ubuntu, node-20)", 100, 42))
	send(t, h, "/gitea", runBody("completed", "completed", "success", 42))

	time.Sleep(100 * time.Millisecond)
	recorded := testutil.GetCalls(calls, mu)

	if n := testutil.CountPath(recorded, "/activities"); n != 1 {
		t.Fatalf("expected exactly 1 create (activity reused across the run), got %d", n)
	}

	// First call: create.
	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if !strings.HasPrefix(create.Slug, "gitea-") {
		t.Errorf("expected gitea- slug, got %s", create.Slug)
	}
	if create.Name != "CI" {
		t.Errorf("expected activity name CI, got %s", create.Name)
	}

	// Seed update should be steps template.
	var seed pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &seed)
	if seed.Content.Template != pushward.TemplateSteps {
		t.Errorf("expected steps template, got %s", seed.Content.Template)
	}
	if seed.Content.State != "Queued" {
		t.Errorf("expected seed state Queued, got %s", seed.Content.State)
	}

	// Final frame: ENDED, Success, green.
	end := lastEnded(t, recorded)
	if end.Content.State != "Success" {
		t.Errorf("expected final state Success, got %s", end.Content.State)
	}
	if end.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green, got %s", end.Content.AccentColor)
	}
}

func TestGiteaJobOnlyLazyCreate(t *testing.T) {
	h, calls, mu := newHandler(t)

	send(t, h, "/gitea", jobBody("in_progress", "in_progress", "", "Build", 100, 42))

	recorded := testutil.GetCalls(calls, mu)
	if n := testutil.CountPath(recorded, "/activities"); n != 1 {
		t.Fatalf("expected lazy create, got %d", n)
	}
	// create + ongoing = 2
	if len(recorded) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(recorded))
	}
	var upd pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &upd)
	if upd.Content.Template != pushward.TemplateSteps {
		t.Errorf("expected steps template, got %s", upd.Content.Template)
	}
}

func TestGiteaOlderRunDropped(t *testing.T) {
	h, calls, mu := newHandler(t)

	send(t, h, "/gitea", runBody("requested", "queued", "", 42))
	before := len(testutil.GetCalls(calls, mu))

	// A job from an older run must be dropped.
	send(t, h, "/gitea", jobBody("in_progress", "in_progress", "", "Build", 99, 41))

	after := testutil.GetCalls(calls, mu)
	if len(after) != before {
		t.Fatalf("expected older-run job to be dropped (no new calls), before=%d after=%d", before, len(after))
	}
}

func TestGiteaPostCompletedJobDropped(t *testing.T) {
	h, calls, mu := newHandler(t)

	send(t, h, "/gitea", runBody("requested", "queued", "", 42))
	send(t, h, "/gitea", runBody("completed", "completed", "success", 42))
	before := len(testutil.GetCalls(calls, mu))

	// A late job for the already-completed run must not resurrect it.
	send(t, h, "/gitea", jobBody("completed", "completed", "success", "Build", 100, 42))

	after := testutil.GetCalls(calls, mu)
	if len(after) != before {
		t.Fatalf("expected post-completion job to be dropped, before=%d after=%d", before, len(after))
	}
}

func TestGiteaDuplicateCompletedIdempotent(t *testing.T) {
	h, calls, mu := newHandler(t)

	send(t, h, "/gitea", runBody("requested", "queued", "", 42))
	send(t, h, "/gitea", runBody("completed", "completed", "success", 42))
	time.Sleep(60 * time.Millisecond)
	before := len(testutil.GetCalls(calls, mu))

	// Second completion for the same run is a no-op.
	send(t, h, "/gitea", runBody("completed", "completed", "success", 42))
	time.Sleep(60 * time.Millisecond)

	after := testutil.GetCalls(calls, mu)
	if len(after) != before {
		t.Fatalf("expected duplicate completion to be idempotent, before=%d after=%d", before, len(after))
	}
}

func TestGiteaSupersedeStopsPendingEnd(t *testing.T) {
	h, calls, mu := newHandler(t)

	send(t, h, "/gitea", runBody("requested", "queued", "", 42))
	send(t, h, "/gitea", runBody("completed", "completed", "success", 42))
	// Immediately supersede with a newer run before the pending end fires.
	send(t, h, "/gitea", runBody("requested", "queued", "", 43))

	time.Sleep(100 * time.Millisecond)
	recorded := testutil.GetCalls(calls, mu)

	// The superseded run's two-phase end must have been cancelled: no ENDED.
	for _, c := range recorded {
		if c.Method != "PATCH" {
			continue
		}
		var u pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &u)
		if u.State == pushward.StateEnded {
			t.Fatal("expected superseded run's pending end to be cancelled, but found an ENDED update")
		}
	}
}

func TestGiteaMatrixGroupingOverHTTP(t *testing.T) {
	h, calls, mu := newHandler(t)

	send(t, h, "/gitea", runBody("requested", "queued", "", 42))
	send(t, h, "/gitea", jobBody("in_progress", "in_progress", "", "Build (ubuntu)", 100, 42))
	send(t, h, "/gitea", jobBody("queued", "queued", "", "Build (windows)", 101, 42))

	recorded := testutil.GetCalls(calls, mu)
	var last pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[len(recorded)-1].Body, &last)
	if last.Content.TotalSteps == nil || *last.Content.TotalSteps != 1 {
		t.Fatalf("expected matrix jobs folded into 1 group, got total_steps %v", last.Content.TotalSteps)
	}
	if len(last.Content.StepRows) != 1 || last.Content.StepRows[0] != 2 {
		t.Errorf("expected one group holding 2 jobs, got rows %v", last.Content.StepRows)
	}
}

func TestGiteaPushIgnored(t *testing.T) {
	h, calls, mu := newHandler(t)

	send(t, h, "/gitea", `{"ref":"refs/heads/main","repository":{"full_name":"acme/app","html_url":"https://gitea.example.com/acme/app"}}`)

	if n := len(testutil.GetCalls(calls, mu)); n != 0 {
		t.Fatalf("expected no API calls for a push event, got %d", n)
	}
}

func TestForgejoOutcomes(t *testing.T) {
	cases := []struct {
		action string
		state  string
		color  string
	}{
		{"success", "Succeeded", pushward.ColorGreen},
		{"failure", "Failed", pushward.ColorRed},
		{"recover", "Recovered", pushward.ColorGreen},
	}
	for _, c := range cases {
		t.Run(c.action, func(t *testing.T) {
			h, calls, mu := newHandler(t)
			send(t, h, "/forgejo", forgejoBody(c.action, 55))
			time.Sleep(80 * time.Millisecond)

			recorded := testutil.GetCalls(calls, mu)
			if n := testutil.CountPath(recorded, "/activities"); n != 1 {
				t.Fatalf("expected 1 create, got %d", n)
			}
			var create pushward.CreateActivityRequest
			testutil.UnmarshalBody(t, recorded[0].Body, &create)
			if !strings.HasPrefix(create.Slug, "forgejo-") {
				t.Errorf("expected forgejo- slug, got %s", create.Slug)
			}
			end := lastEnded(t, recorded)
			if end.Content.Template != pushward.TemplateGeneric {
				t.Errorf("expected generic template, got %s", end.Content.Template)
			}
			if end.Content.State != c.state {
				t.Errorf("expected state %s, got %s", c.state, end.Content.State)
			}
			if end.Content.AccentColor != c.color {
				t.Errorf("expected color %s, got %s", c.color, end.Content.AccentColor)
			}
		})
	}
}

func TestForgejoRecoverAfterSuccess(t *testing.T) {
	h, calls, mu := newHandler(t)

	// Success then recover fire for the same run/slug; last writer wins.
	send(t, h, "/forgejo", forgejoBody("success", 55))
	send(t, h, "/forgejo", forgejoBody("recover", 55))
	time.Sleep(100 * time.Millisecond)

	recorded := testutil.GetCalls(calls, mu)
	end := lastEnded(t, recorded)
	if end.Content.State != "Recovered" {
		t.Errorf("expected last-writer Recovered, got %s", end.Content.State)
	}
}
