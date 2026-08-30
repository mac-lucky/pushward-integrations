package backrest

import (
	"encoding/json"
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

func testConfig() *config.BackrestConfig {
	return &config.BackrestConfig{
		BaseProviderConfig: config.BaseProviderConfig{
			Enabled:        true,
			Priority:       1,
			CleanupDelay:   1 * time.Hour,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
	}
}

func newHandler(t *testing.T, cfg *config.BackrestConfig) (http.Handler, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	mux, _, calls, mu := newHandlerWithEnder(t, cfg)
	return mux, calls, mu
}

func newHandlerWithEnder(t *testing.T, cfg *config.BackrestConfig) (http.Handler, *lifecycle.Ender, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServer(t)

	mux, api := humautil.NewTestAPI()
	h := RegisterRoutes(api, state.NewMemoryStore(), client.NewPool(srv.URL, nil), cfg)
	return mux, h.Ender(), calls, mu
}

func send(t *testing.T, h http.Handler, payload string) *httptest.ResponseRecorder {
	t.Helper()
	return sendQuery(t, h, "", payload)
}

// wantSuccessDetail is what summaryDetail renders for the snapshot-success
// payload used across these tests (2468421632 bytes, 42 new + 156 changed
// files, 45000 ms).
const wantSuccessDetail = "2.3 GB · 198 files · 45s"

// Spelled out rather than composed from the production constants, so a change
// to either the state text or the separator has to be made here too.
const (
	stateCompletePrefix = "Complete · "
	stateFailedPrefix   = "Failed: "
)

// notifications decodes the recorded notification bodies, for the sites that
// assert on their content. Use testutil.CountPath when only the count matters.
func notifications(t *testing.T, recorded []testutil.APICall) []pushward.SendNotificationRequest {
	t.Helper()
	var out []pushward.SendNotificationRequest
	for _, c := range recorded {
		if c.Method == http.MethodPost && c.Path == "/notifications" {
			var req pushward.SendNotificationRequest
			testutil.UnmarshalBody(t, c.Body, &req)
			out = append(out, req)
		}
	}
	return out
}

// activityCalls drops the notification POSTs so index-based assertions keep a
// stable numbering whether or not an event also notified.
func activityCalls(recorded []testutil.APICall) []testutil.APICall {
	var out []testutil.APICall
	for _, c := range recorded {
		if c.Path != "/notifications" {
			out = append(out, c)
		}
	}
	return out
}

// stepsFrame is the shape every steps-template frame is asserted against.
type stepsFrame struct {
	state string
	color string
	icon  string
	step  int
}

func assertStepsFrame(t *testing.T, body json.RawMessage, want stepsFrame) pushward.UpdateRequest {
	t.Helper()
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, body, &req)
	c := req.Content
	if c.State != want.state {
		t.Errorf("state = %q, want %q", c.State, want.state)
	}
	if c.Template != pushward.TemplateSteps {
		t.Errorf("template = %q, want %q", c.Template, pushward.TemplateSteps)
	}
	if c.AccentColor != want.color {
		t.Errorf("accent = %q, want %q", c.AccentColor, want.color)
	}
	if c.Icon != want.icon {
		t.Errorf("icon = %q, want %q", c.Icon, want.icon)
	}
	if c.CurrentStep == nil || *c.CurrentStep != want.step {
		t.Errorf("current_step = %v, want %d", c.CurrentStep, want.step)
	}
	if c.TotalSteps == nil || *c.TotalSteps != 2 {
		t.Errorf("total_steps = %v, want 2", c.TotalSteps)
	}
	if len(c.StepLabels) != 2 || c.StepLabels[0] != "Running" || c.StepLabels[1] != "Done" {
		t.Errorf("step_labels = %v, want [Running Done]", c.StepLabels)
	}
	return req
}

func TestSnapshotLifecycle(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send START
	w := send(t, h, `{
		"event": "CONDITION_SNAPSHOT_START",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Send SUCCESS
	w = send(t, h, `{
		"event": "CONDITION_SNAPSHOT_SUCCESS",
		"plan": "daily-backup",
		"repo": "local-repo",
		"snapshot_id": "abc123def",
		"data_added": 2468421632,
		"files_new": 42,
		"files_changed": 156,
		"duration_ms": 45000
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// START: create + ONGOING.
	// SUCCESS: create + the final frame + the ender's two phases.
	recorded := testutil.WaitForCalls(t, calls, mu, 6, 2*time.Second)
	if len(recorded) != 6 {
		t.Fatalf("expected 6 calls, got %d", len(recorded))
	}

	// Verify create from START
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "daily-backup" {
		t.Errorf("expected name 'daily-backup', got %s", createReq.Name)
	}
	if createReq.Priority != 1 {
		t.Errorf("expected priority 1, got %d", createReq.Priority)
	}

	// Verify initial ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.State != stateBackingUp {
		t.Errorf("expected state %q, got %s", stateBackingUp, update.Content.State)
	}
	if update.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", update.Content.Template)
	}
	if update.Content.Icon != "arrow.triangle.2.circlepath" {
		t.Errorf("expected icon arrow.triangle.2.circlepath, got %s", update.Content.Icon)
	}
	if update.Content.AccentColor != pushward.ColorBlue {
		t.Errorf("expected blue color, got %s", update.Content.AccentColor)
	}
	if update.Content.Progress != 0 {
		t.Errorf("expected progress 0, got %f", update.Content.Progress)
	}
	if update.Content.Subtitle != "Backrest · daily-backup · local-repo" {
		t.Errorf("expected subtitle 'Backrest · daily-backup · local-repo', got %q", update.Content.Subtitle)
	}
	if update.Content.CurrentStep == nil || *update.Content.CurrentStep != 1 {
		t.Errorf("expected current_step 1, got %v", update.Content.CurrentStep)
	}
	if update.Content.TotalSteps == nil || *update.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", update.Content.TotalSteps)
	}
	if len(update.Content.StepLabels) != 2 || update.Content.StepLabels[0] != "Running" || update.Content.StepLabels[1] != "Done" {
		t.Errorf("expected step_labels [Running, Done], got %v", update.Content.StepLabels)
	}

	// Phase 1: ONGOING with final content (from SUCCESS)
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1)
	if phase1.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1.State)
	}
	if phase1.Content.State != stateCompletePrefix+wantSuccessDetail {
		t.Errorf("expected state %q, got %s", stateCompletePrefix+wantSuccessDetail, phase1.Content.State)
	}
	if phase1.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", phase1.Content.Template)
	}
	if phase1.Content.AccentColor != pushward.ColorGreen {
		t.Errorf("expected green color, got %s", phase1.Content.AccentColor)
	}
	if phase1.Content.Icon != "checkmark.circle.fill" {
		t.Errorf("expected icon checkmark.circle.fill, got %s", phase1.Content.Icon)
	}
	if phase1.Content.CurrentStep == nil || *phase1.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", phase1.Content.CurrentStep)
	}
	if phase1.Content.TotalSteps == nil || *phase1.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", phase1.Content.TotalSteps)
	}
	if len(phase1.Content.StepLabels) != 2 || phase1.Content.StepLabels[0] != "Running" || phase1.Content.StepLabels[1] != "Done" {
		t.Errorf("expected step_labels [Running, Done], got %v", phase1.Content.StepLabels)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[5].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
	if phase2.Content.State != stateCompletePrefix+wantSuccessDetail {
		t.Errorf("expected state %q, got %s", stateCompletePrefix+wantSuccessDetail, phase2.Content.State)
	}

	// A routine success must not add a push on top of the Live Activity, or
	// enabling notifications would spam every scheduled backup.
	if n := notifications(t, recorded); len(n) != 0 {
		t.Errorf("expected no notification for a successful snapshot, got %d", len(n))
	}
}

func TestSnapshotError(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send START
	send(t, h, `{
		"event": "CONDITION_SNAPSHOT_START",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)

	// Send ERROR
	w := send(t, h, `{
		"event": "CONDITION_SNAPSHOT_ERROR",
		"plan": "daily-backup",
		"repo": "local-repo",
		"duration_ms": 5000,
		"error": "repository not found"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.WaitForCalls(t, calls, mu, 7, 2*time.Second)
	// START: create + ONGOING                                          = 2
	// ERROR: create + final ONGOING + notification + phase1 + phase2  = 5
	if len(recorded) != 7 {
		t.Fatalf("expected 7 calls, got %d", len(recorded))
	}

	// A failed snapshot is worth interrupting for, so it also notifies.
	notifs := notifications(t, recorded)
	if len(notifs) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifs))
	}
	if want := stateFailedPrefix + "repository not found"; notifs[0].Body != want {
		t.Errorf("expected notification body %q, got %q", want, notifs[0].Body)
	}
	if notifs[0].Level != pushward.LevelTimeSensitive {
		t.Errorf("expected time-sensitive level, got %q", notifs[0].Level)
	}
	if notifs[0].ActivitySlug == "" {
		t.Error("expected the notification to deep-link into its activity")
	}
	recorded = activityCalls(recorded)

	// Phase 1: red/failed
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1)
	if phase1.Content.State != stateFailedPrefix+"repository not found" {
		t.Errorf("expected state %q, got %s", stateFailedPrefix+"repository not found", phase1.Content.State)
	}
	if phase1.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", phase1.Content.Template)
	}
	if phase1.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red color, got %s", phase1.Content.AccentColor)
	}
	if phase1.Content.Icon != "xmark.circle.fill" {
		t.Errorf("expected icon xmark.circle.fill, got %s", phase1.Content.Icon)
	}
	if phase1.Content.CurrentStep == nil || *phase1.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", phase1.Content.CurrentStep)
	}
	if phase1.Content.TotalSteps == nil || *phase1.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", phase1.Content.TotalSteps)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[5].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestSnapshotWarning(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send START
	send(t, h, `{
		"event": "CONDITION_SNAPSHOT_START",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)

	// Send WARNING
	w := send(t, h, `{
		"event": "CONDITION_SNAPSHOT_WARNING",
		"plan": "daily-backup",
		"repo": "local-repo",
		"data_added": 1048576
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.WaitForCalls(t, calls, mu, 7, 2*time.Second)
	if len(recorded) != 7 {
		t.Fatalf("expected 7 calls, got %d", len(recorded))
	}

	// A partial backup is a warning, which still notifies.
	if n := testutil.CountPath(recorded, "/notifications"); n != 1 {
		t.Fatalf("expected 1 notification, got %d", n)
	}
	recorded = activityCalls(recorded)

	// Phase 1: orange/warning
	var phase1 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1)
	if want := stateCompleteWarnings + " · " + "1.0 MB"; phase1.Content.State != want {
		t.Errorf("expected state %q, got %s", want, phase1.Content.State)
	}
	if phase1.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", phase1.Content.Template)
	}
	if phase1.Content.AccentColor != pushward.ColorOrange {
		t.Errorf("expected orange color, got %s", phase1.Content.AccentColor)
	}
	if phase1.Content.Icon != "exclamationmark.triangle.fill" {
		t.Errorf("expected icon exclamationmark.triangle.fill, got %s", phase1.Content.Icon)
	}
	if phase1.Content.CurrentStep == nil || *phase1.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", phase1.Content.CurrentStep)
	}
	if phase1.Content.TotalSteps == nil || *phase1.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", phase1.Content.TotalSteps)
	}
}

func TestSummaryDetail(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	f := func(v float64) *float64 { return &v }

	tests := []struct {
		name string
		p    backrestPayload
		want string
	}{
		{
			// Every pre-existing template sends no stats at all. It must render
			// as a bare outcome, not as a row of zeroes.
			name: "no stats sent",
			p:    backrestPayload{},
			want: "",
		},
		{
			name: "legacy template: data_added only",
			p:    backrestPayload{DataAdded: i(2468421632)},
			want: "2.3 GB",
		},
		{
			name: "full stats",
			p: backrestPayload{
				DataAdded:     i(2468421632),
				FilesNew:      i(42),
				FilesChanged:  i(156),
				TotalDuration: f(45),
			},
			want: "2.3 GB · 198 files · 45s",
		},
		{
			// The one that matters on a real tree: 65k files walked, 348 backed
			// up. Reporting the walk count next to 79 GB added is meaningless.
			name: "new + changed wins over the files restic walked",
			p: backrestPayload{
				FilesNew:            i(334),
				FilesChanged:        i(14),
				FilesUnmodified:     i(65007),
				TotalFilesProcessed: i(65355),
			},
			want: "348 files",
		},
		{
			// A template that sends only the totals still has something to say.
			name: "total_files_processed stands in when nothing finer is sent",
			p:    backrestPayload{TotalFilesProcessed: i(198)},
			want: "198 files",
		},
		{
			// restic's own timing wins over Backrest's task wall clock.
			name: "total_duration preferred over duration_ms",
			p:    backrestPayload{TotalDuration: f(45), DurationMs: 99000},
			want: "45s",
		},
		{
			name: "duration_ms used when total_duration absent",
			p:    backrestPayload{DurationMs: 45000},
			want: "45s",
		},
		{
			// A genuinely empty backup reports zeroes, which must not be
			// confused with "the template omitted the field".
			name: "explicit zeroes render nothing",
			p:    backrestPayload{DataAdded: i(0), TotalFilesProcessed: i(0)},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := summaryDetail(&tc.p); got != tc.want {
				t.Errorf("summaryDetail() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Pins the handler to Backrest's Hook.Condition enum (proto/v1/config.proto,
// v1.14.1). Kind and problem are asserted too, so a condition wired to the
// wrong branch fails here instead of in production.
func TestSpecForCoversAllConditions(t *testing.T) {
	tests := []struct {
		cond    string
		kind    eventKind
		problem bool
	}{
		{condAnyError, kindAlert, true},
		{condSnapshotStart, kindStart, false},
		{condSnapshotEnd, kindEnd, false},
		{condSnapshotError, kindEnd, true},
		{condSnapshotWarning, kindEnd, true},
		{condSnapshotSuccess, kindEnd, false},
		{condSnapshotSkipped, kindAlert, false},
		{condPruneStart, kindStart, false},
		{condPruneError, kindEnd, true},
		{condPruneSuccess, kindEnd, false},
		{condCheckStart, kindStart, false},
		{condCheckError, kindEnd, true},
		{condCheckSuccess, kindEnd, false},
		{condForgetStart, kindStart, false},
		{condForgetError, kindEnd, true},
		{condForgetSuccess, kindEnd, false},
	}
	if len(tests)+1 != 17 {
		t.Fatalf("Hook.Condition has 17 values, table has %d plus condUnknown", len(tests))
	}
	// CONDITION_UNKNOWN is Backrest's "no condition matched" sentinel, never
	// delivered, so it is deliberately unmapped and takes the unrecognised-event
	// path along with any condition a future Backrest release adds.
	if _, ok := specFor(&backrestPayload{Event: condUnknown}); ok {
		t.Error("condUnknown should be unmapped")
	}

	for _, tc := range tests {
		t.Run(tc.cond, func(t *testing.T) {
			spec, ok := specFor(&backrestPayload{Event: tc.cond})
			if !ok {
				t.Fatalf("specFor(%s) not handled", tc.cond)
			}
			if spec.kind != tc.kind {
				t.Errorf("kind = %d, want %d", spec.kind, tc.kind)
			}
			if spec.problem != tc.problem {
				t.Errorf("problem = %v, want %v", spec.problem, tc.problem)
			}
			if tc.kind != kindIgnore && spec.state == "" {
				t.Error("expected a non-empty state")
			}
		})
	}
}

// SNAPSHOT_END carries no outcome of its own, so it is the one condition whose
// spec depends on the payload.
func TestSpecForSnapshotEndUsesError(t *testing.T) {
	spec, _ := specFor(&backrestPayload{Event: condSnapshotEnd, Error: "boom"})
	if !spec.problem || spec.state != stateFailed {
		t.Errorf("END with an error should be a failure, got %+v", spec)
	}
}

func TestRepoOperationLifecycles(t *testing.T) {
	tests := []struct {
		name       string
		startEvent string
		doneEvent  string
		startState string
		doneState  string
	}{
		{"prune", condPruneStart, condPruneSuccess, statePruning, statePruned},
		{"check", condCheckStart, condCheckSuccess, stateChecking, stateCheckPassed},
		{"forget", condForgetStart, condForgetSuccess, stateApplyingRetention, stateRetentionApplied},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, calls, mu := newHandler(t, testConfig())

			body := `{"event":"%s","plan":"daily-backup","repo":"local-repo"}`
			for _, ev := range []string{tc.startEvent, tc.doneEvent} {
				if w := send(t, h, fmt.Sprintf(body, ev)); w.Code != http.StatusOK {
					t.Fatalf("%s: expected 200, got %d (%s)", ev, w.Code, w.Body.String())
				}
			}

			// start: create + ONGOING; done: create + final ONGOING + the
			// ender's two phases.
			recorded := testutil.WaitForCalls(t, calls, mu, 6, 2*time.Second)
			if len(recorded) != 6 {
				t.Fatalf("expected 6 calls, got %d", len(recorded))
			}
			if n := testutil.CountPath(recorded, "/notifications"); n != 0 {
				t.Errorf("a successful repo operation must not push, got %d", n)
			}

			assertStepsFrame(t, recorded[1].Body, stepsFrame{tc.startState, pushward.ColorBlue, iconRunning, 1})
			assertStepsFrame(t, recorded[3].Body, stepsFrame{tc.doneState, pushward.ColorGreen, iconOK, 2})

			final := assertStepsFrame(t, recorded[5].Body, stepsFrame{tc.doneState, pushward.ColorGreen, iconOK, 2})
			if final.State != pushward.StateEnded {
				t.Errorf("expected the last frame to be ENDED, got %s", final.State)
			}
		})
	}
}

func TestForgetError(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	// Send FORGET_START
	send(t, h, `{
		"event": "CONDITION_FORGET_START",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)

	// Send FORGET_ERROR
	w := send(t, h, `{
		"event": "CONDITION_FORGET_ERROR",
		"plan": "daily-backup",
		"repo": "local-repo",
		"error": "permission denied"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.WaitForCalls(t, calls, mu, 7, 2*time.Second)
	// FORGET_START: create + ONGOING                                         = 2
	// FORGET_ERROR: create + final ONGOING + notification + phase1 + phase2  = 5
	if len(recorded) != 7 {
		t.Fatalf("expected 7 calls, got %d", len(recorded))
	}
	if n := testutil.CountPath(recorded, "/notifications"); n != 1 {
		t.Fatalf("expected 1 notification, got %d", n)
	}
	recorded = activityCalls(recorded)

	// Phase 1: red/failed
	var phase1fe pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase1fe)
	if want := stateRetentionFailed + ": " + "permission denied"; phase1fe.Content.State != want {
		t.Errorf("expected state %q, got %s", want, phase1fe.Content.State)
	}
	if phase1fe.Content.Template != "steps" {
		t.Errorf("expected template 'steps', got %s", phase1fe.Content.Template)
	}
	if phase1fe.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red color, got %s", phase1fe.Content.AccentColor)
	}
	if phase1fe.Content.Icon != "xmark.circle.fill" {
		t.Errorf("expected icon xmark.circle.fill, got %s", phase1fe.Content.Icon)
	}
	if phase1fe.Content.CurrentStep == nil || *phase1fe.Content.CurrentStep != 2 {
		t.Errorf("expected current_step 2, got %v", phase1fe.Content.CurrentStep)
	}
	if phase1fe.Content.TotalSteps == nil || *phase1fe.Content.TotalSteps != 2 {
		t.Errorf("expected total_steps 2, got %v", phase1fe.Content.TotalSteps)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[5].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestAnyError(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"event": "CONDITION_ANY_ERROR",
		"plan": "daily-backup",
		"repo": "local-repo",
		"error": "repository lock held by PID 1234"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.WaitForCalls(t, calls, mu, 5, 2*time.Second)
	// create + ONGOING + notification + phase1(ONGOING) + phase2(ENDED) = 5
	if len(recorded) != 5 {
		t.Fatalf("expected 5 calls, got %d", len(recorded))
	}
	if n := testutil.CountPath(recorded, "/notifications"); n != 1 {
		t.Fatalf("expected 1 notification, got %d", n)
	}
	recorded = activityCalls(recorded)

	// Verify create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "alert" {
		t.Errorf("expected template 'alert', got %s", update.Content.Template)
	}
	if update.Content.Severity != "critical" {
		t.Errorf("expected severity 'critical', got %s", update.Content.Severity)
	}
	// No badge: the only label this branch could produce is the bare word
	// "Error", which says less than the critical styling already does.
	if update.Content.SeverityLabel != "" {
		t.Errorf("expected no severity_label, got %q", update.Content.SeverityLabel)
	}
	if update.Content.AccentColor != pushward.ColorRed {
		t.Errorf("expected red color, got %s", update.Content.AccentColor)
	}
	if update.Content.Icon != "exclamationmark.triangle.fill" {
		t.Errorf("expected icon exclamationmark.triangle.fill, got %s", update.Content.Icon)
	}
	if update.Content.State != "repository lock held by PID 1234" {
		t.Errorf("expected state 'repository lock held by PID 1234', got %s", update.Content.State)
	}
	if update.Content.Subtitle != "Backrest · daily-backup · local-repo" {
		t.Errorf("expected subtitle 'Backrest · daily-backup · local-repo', got %q", update.Content.Subtitle)
	}

	// Phase 1: ONGOING with same content
	var phase1ae pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1ae)
	if phase1ae.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1ae.State)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestSnapshotSkipped(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"event": "CONDITION_SNAPSHOT_SKIPPED",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.WaitForCalls(t, calls, mu, 4, 2*time.Second)
	// create + ONGOING + phase1(ONGOING) + phase2(ENDED) = 4
	if len(recorded) != 4 {
		t.Fatalf("expected 4 calls, got %d", len(recorded))
	}

	// Verify create
	if recorded[0].Method != "POST" || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}

	// Verify ONGOING update
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", update.State)
	}
	if update.Content.Template != "alert" {
		t.Errorf("expected template 'alert', got %s", update.Content.Template)
	}
	if update.Content.Severity != "info" {
		t.Errorf("expected severity 'info', got %s", update.Content.Severity)
	}
	if update.Content.AccentColor != pushward.ColorBlue {
		t.Errorf("expected blue color, got %s", update.Content.AccentColor)
	}
	if update.Content.Icon != "info.circle.fill" {
		t.Errorf("expected icon info.circle.fill, got %s", update.Content.Icon)
	}
	if update.Content.State != stateSnapshotSkipped {
		t.Errorf("expected state %q, got %s", stateSnapshotSkipped, update.Content.State)
	}
	// Backrest emits no severity_label at all; the stock badge carries the
	// severity and the state line already names the condition.
	if update.Content.SeverityLabel != "" {
		t.Errorf("expected no severity_label, got %q", update.Content.SeverityLabel)
	}
	if update.Content.Subtitle != "Backrest · daily-backup · local-repo" {
		t.Errorf("expected subtitle 'Backrest · daily-backup · local-repo', got %q", update.Content.Subtitle)
	}

	// Phase 1: ONGOING with same content
	var phase1ss pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &phase1ss)
	if phase1ss.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING (phase 1), got %s", phase1ss.State)
	}

	// Phase 2: ENDED
	var phase2 pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[3].Body, &phase2)
	if phase2.State != pushward.StateEnded {
		t.Errorf("expected ENDED (phase 2), got %s", phase2.State)
	}
}

func TestSnapshotStart_APIFailure_Returns502(t *testing.T) {
	// Server returns 400 for all requests (not retried by client).
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	store := state.NewMemoryStore()
	pool := client.NewPool(srv.URL, nil)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, store, pool, testConfig())

	w := send(t, mux, `{
		"event": "CONDITION_SNAPSHOT_START",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
}

// sendQuery posts with optional query-parameter overrides on the webhook URL.
func sendQuery(t *testing.T, h http.Handler, query, payload string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/backrest"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer hlk_test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// The hook template lives in the user's own config and cannot be migrated, so
// the original six-field body has to keep rendering. Above all it must not
// report "0 B" just because the template omits the stats.
func TestLegacyTemplateStillWorks(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{
		"event": "CONDITION_SNAPSHOT_SUCCESS",
		"plan": "daily-backup",
		"repo": "local-repo",
		"snapshot_id": "abc123def",
		"data_added": 0,
		"error": ""
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	recorded := activityCalls(testutil.WaitForCalls(t, calls, mu, 4, 2*time.Second))
	var final pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[len(recorded)-1].Body, &final)
	if final.Content.State != stateComplete {
		t.Errorf("expected bare %q with no stats sent, got %q", stateComplete, final.Content.State)
	}
}

// Backrest delivers END only to hooks that do not also subscribe to the
// specific outcome, so END has to carry the outcome itself.
func TestSnapshotEndResolvesOutcome(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantState string
		wantColor string
		wantNotif int
	}{
		{
			name:      "end without error is a success",
			body:      `{"event":"CONDITION_SNAPSHOT_END","plan":"p","repo":"r","data_added":1048576}`,
			wantState: stateCompletePrefix + "1.0 MB",
			wantColor: pushward.ColorGreen,
			wantNotif: 0,
		},
		{
			name:      "end with error is a failure",
			body:      `{"event":"CONDITION_SNAPSHOT_END","plan":"p","repo":"r","error":"repository not found"}`,
			wantState: stateFailedPrefix + "repository not found",
			wantColor: pushward.ColorRed,
			wantNotif: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, calls, mu := newHandler(t, testConfig())
			if w := send(t, h, tc.body); w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
			}
			all := testutil.WaitForCalls(t, calls, mu, 4+tc.wantNotif, 2*time.Second)
			if n := notifications(t, all); len(n) != tc.wantNotif {
				t.Errorf("expected %d notifications, got %d", tc.wantNotif, len(n))
			}
			recorded := activityCalls(all)
			var final pushward.UpdateRequest
			testutil.UnmarshalBody(t, recorded[len(recorded)-1].Body, &final)
			if final.Content.State != tc.wantState {
				t.Errorf("expected state %q, got %q", tc.wantState, final.Content.State)
			}
			if final.Content.AccentColor != tc.wantColor {
				t.Errorf("expected color %q, got %q", tc.wantColor, final.Content.AccentColor)
			}
		})
	}
}

func TestNotificationOnlyChannelCoversSuccess(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendQuery(t, h, "channels=notification", `{
		"event": "CONDITION_SNAPSHOT_SUCCESS",
		"plan": "daily-backup",
		"repo": "local-repo",
		"data_added": 2468421632,
		"total_files_processed": 198,
		"total_duration": 45
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	recorded := testutil.WaitForCalls(t, calls, mu, 1, 2*time.Second)
	for _, c := range recorded {
		if strings.HasPrefix(c.Path, "/activities") {
			t.Fatalf("expected no activity calls with channels=notification, got %s %s", c.Method, c.Path)
		}
	}
	notifs := notifications(t, recorded)
	if len(notifs) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifs))
	}
	if want := stateCompletePrefix + wantSuccessDetail; notifs[0].Body != want {
		t.Errorf("expected body %q, got %q", want, notifs[0].Body)
	}
	// No activity exists to deep-link into; sending its slug anyway would be
	// rejected with 422 notification.activity_not_found.
	if notifs[0].ActivitySlug != "" {
		t.Errorf("expected no activity_slug, got %q", notifs[0].ActivitySlug)
	}
}

// A failure normally pushes. With the notification surface off it stays silent.
func TestActivityOnlySuppressesNotification(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendQuery(t, h, "channels=activity", `{
		"event": "CONDITION_SNAPSHOT_ERROR",
		"plan": "daily-backup",
		"repo": "local-repo",
		"error": "boom"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	recorded := testutil.WaitForCalls(t, calls, mu, 4, 2*time.Second)
	if n := notifications(t, recorded); len(n) != 0 {
		t.Fatalf("channels=activity must not push, got %d", len(n))
	}
}

func TestAlertNotificationOnly(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendQuery(t, h, "channels=notification", `{
		"event": "CONDITION_ANY_ERROR",
		"plan": "daily-backup",
		"repo": "local-repo",
		"error": "repository lock held by PID 1234"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	recorded := testutil.WaitForCalls(t, calls, mu, 1, 2*time.Second)
	for _, c := range recorded {
		if strings.HasPrefix(c.Path, "/activities") {
			t.Fatalf("expected no activity calls, got %s %s", c.Method, c.Path)
		}
	}
	notifs := notifications(t, recorded)
	if len(notifs) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifs))
	}
	if notifs[0].ActivitySlug != "" {
		t.Errorf("expected no activity_slug, got %q", notifs[0].ActivitySlug)
	}
	if notifs[0].Level != pushward.LevelTimeSensitive {
		t.Errorf("expected time-sensitive, got %q", notifs[0].Level)
	}
}

// A start is not an outcome, so it must not fall back to a push: every
// scheduled backup would notify on start.
func TestStartSuppressedDoesNothing(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := sendQuery(t, h, "channels=notification", `{
		"event": "CONDITION_SNAPSHOT_START",
		"plan": "daily-backup",
		"repo": "local-repo"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if recorded := testutil.GetCalls(calls, mu); len(recorded) != 0 {
		t.Errorf("expected no upstream calls, got %d", len(recorded))
	}
}

// A condition a future Backrest release adds must not 500 the hook.
func TestUnmappedEventIsAccepted(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{"event":"CONDITION_SOMETHING_NEW","plan":"p","repo":"r"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if recorded := testutil.GetCalls(calls, mu); len(recorded) != 0 {
		t.Errorf("expected no upstream calls, got %d", len(recorded))
	}
}

// Prune, check and forget run against a repo, and Backrest fills .Plan.Id with
// the "_system_" sentinel rather than leaving it empty. Neither that nor a
// genuinely absent plan may reach the user.
func TestRepoOnlyPayloadFallbacks(t *testing.T) {
	cases := map[string]string{
		"system sentinel": `"plan":"_system_",`,
		"plan omitted":    ``,
	}
	for name, planField := range cases {
		t.Run(name, func(t *testing.T) {
			assertRepoOnlyFallbacks(t, planField)
		})
	}
}

func assertRepoOnlyFallbacks(t *testing.T, planField string) {
	t.Helper()
	h, calls, mu := newHandler(t, testConfig())

	w := send(t, h, `{"event":"CONDITION_CHECK_START",`+planField+`"repo":"local-repo"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	recorded := testutil.WaitForCalls(t, calls, mu, 2, 2*time.Second)

	var createReq pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &createReq)
	if createReq.Name != "Backup" {
		t.Errorf("expected name fallback 'Backup', got %q", createReq.Name)
	}
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.Subtitle != "Backrest · local-repo" {
		t.Errorf("expected subtitle without a plan segment, got %q", update.Content.Subtitle)
	}
}

func TestNotificationMetadata(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	send(t, h, `{
		"event": "CONDITION_SNAPSHOT_ERROR",
		"task": "backup for plan \"daily-backup\"",
		"plan": "daily-backup",
		"repo": "local-repo",
		"snapshot_id": "abc123def",
		"error": "repository not found"
	}`)
	recorded := testutil.WaitForCalls(t, calls, mu, 5, 2*time.Second)

	notifs := notifications(t, recorded)
	if len(notifs) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifs))
	}
	want := map[string]string{
		"event":       condSnapshotError,
		"plan":        "daily-backup",
		"repo":        "local-repo",
		"snapshot_id": "abc123def",
		"task":        `backup for plan "daily-backup"`,
	}
	for k, v := range want {
		if got := notifs[0].Metadata[k]; got != v {
			t.Errorf("metadata[%q] = %q, want %q", k, got, v)
		}
	}
}

// Every value is attacker-controlled, so none may pass through unbounded.
func TestNotificationMetadataIsBounded(t *testing.T) {
	long := strings.Repeat("A", 5000)
	md := notificationMetadata(&backrestPayload{
		Event: condAnyError, Plan: long, Repo: long, SnapshotID: long, Task: long,
	})
	for k, v := range md {
		if k == "event" {
			continue
		}
		if len(v) > 100 {
			t.Errorf("metadata[%q] is %d chars, want <= 100", k, len(v))
		}
	}
}

// The shipped snapshot_warning fixture carries both an error and a full
// summary, and only one of them fits the state line.
func TestEndStatePrefersErrorOverSummary(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	p := &backrestPayload{
		Error:               "partial backup, 3 files may not have been read completely.",
		DataAdded:           i(2468421632),
		TotalFilesProcessed: i(198),
	}
	got := endState(eventSpecs[condSnapshotWarning], p)
	if !strings.HasPrefix(got, stateCompleteWarnings+": ") {
		t.Errorf("expected the error to win, got %q", got)
	}
	if strings.Contains(got, "2.3 GB") {
		t.Errorf("expected the summary to be dropped, got %q", got)
	}
}

// The mixed-hook case: Backrest allows several hooks with different URLs, so a
// start can arrive on /backrest and the outcome on ?channels=notification.
func TestSuppressedEndClosesPriorActivity(t *testing.T) {
	h, calls, mu := newHandler(t, testConfig())

	send(t, h, `{"event":"CONDITION_SNAPSHOT_START","plan":"daily-backup","repo":"local-repo"}`)
	testutil.WaitForCalls(t, calls, mu, 2, 2*time.Second)

	w := sendQuery(t, h, "channels=notification", `{
		"event": "CONDITION_SNAPSHOT_ERROR",
		"plan": "daily-backup",
		"repo": "local-repo",
		"error": "repository not found"
	}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	// create + ongoing + notification + the ender's two phases.
	recorded := testutil.WaitForCalls(t, calls, mu, 5, 2*time.Second)

	var ended bool
	for _, c := range recorded {
		if c.Method == http.MethodPut || c.Method == http.MethodPost || c.Method == http.MethodPatch {
			if strings.HasPrefix(c.Path, "/activities/") {
				var req pushward.UpdateRequest
				testutil.UnmarshalBody(t, c.Body, &req)
				if req.State == pushward.StateEnded {
					ended = true
				}
			}
		}
	}
	if !ended {
		t.Error("the activity opened by START was never ended, so it would hang until the stale TTL")
	}

	// The push is about an activity that does exist, so it has to deep-link into
	// it. channels=notification says nothing about whether one is open.
	notifs := notifications(t, recorded)
	if len(notifs) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifs))
	}
	if notifs[0].ActivitySlug == "" {
		t.Error("notification dropped the deep link to the activity it is reporting on")
	}
}

// newHandlerFailingNotifications builds a handler whose upstream accepts every
// activity call but rejects notifications.
func newHandlerFailingNotifications(t *testing.T, cfg *config.BackrestConfig) http.Handler {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, _, _ := testutil.MockPushWardServerRejecting(t, http.StatusUnprocessableEntity, 0)

	mux, api := humautil.NewTestAPI()
	RegisterRoutes(api, state.NewMemoryStore(), client.NewPool(srv.URL, nil), cfg)
	return mux
}

// With the activity suppressed the push is the only delivery path. Swallowing
// its failure answers 200 to a backup failure that reached nobody.
func TestNotificationOnlyFailurePropagates(t *testing.T) {
	h := newHandlerFailingNotifications(t, testConfig())

	w := sendQuery(t, h, "channels=notification", `{
		"event": "CONDITION_SNAPSHOT_ERROR",
		"plan": "daily-backup",
		"repo": "local-repo",
		"error": "repository not found"
	}`)
	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 when the only delivery surface fails, got %d (%s)", w.Code, w.Body.String())
	}
}

// The mirror case: with an activity also delivering the outcome, a failed push
// must not fail the webhook.
func TestNotificationFailureToleratedAlongsideActivity(t *testing.T) {
	h := newHandlerFailingNotifications(t, testConfig())

	w := send(t, h, `{
		"event": "CONDITION_SNAPSHOT_ERROR",
		"plan": "daily-backup",
		"repo": "local-repo",
		"error": "repository not found"
	}`)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when the activity carried the outcome, got %d (%s)", w.Code, w.Body.String())
	}
}
