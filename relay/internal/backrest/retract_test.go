package backrest

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

type retractHarness struct {
	h     http.Handler
	store state.Store
	calls *[]testutil.APICall
	mu    *sync.Mutex
}

func newRetractHarness(t *testing.T) *retractHarness {
	t.Helper()
	return newRetractHarnessWith(t, testutil.MockOptions{})
}

func newRetractHarnessWith(t *testing.T, opts testutil.MockOptions) *retractHarness {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServerWith(t, opts)
	store := state.NewMemoryStore()
	mux, api := humautil.NewTestAPI()
	h := RegisterRoutes(api, store, client.NewPool(srv.URL, nil), testConfig())
	t.Cleanup(h.Ender().StopAll)
	return &retractHarness{h: mux, store: store, calls: calls, mu: mu}
}

func (rh *retractHarness) post(t *testing.T, query, event string) {
	t.Helper()
	body := fmt.Sprintf(`{"event":%q,"plan":"daily","repo":"nas","error":""}`, event)
	if !strings.HasSuffix(event, "_SUCCESS") {
		body = fmt.Sprintf(`{"event":%q,"plan":"daily","repo":"nas","error":"boom"}`, event)
	}
	if w := sendQuery(t, rh.h, query, body); w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", event, w.Code, w.Body.String())
	}
}

func (rh *retractHarness) retracts(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, c := range testutil.CallsTo(testutil.GetCalls(rh.calls, rh.mu), http.MethodPost, "/notifications/retract") {
		var body struct {
			CollapseID string `json:"collapse_id"`
		}
		testutil.UnmarshalBody(t, c.Body, &body)
		out = append(out, body.CollapseID)
	}
	return out
}

func (rh *retractHarness) record(t *testing.T, op string) failure {
	t.Helper()
	raw, err := rh.store.Get(context.Background(), "backrest", "hlk_test", failureKey(&backrestPayload{Plan: "daily", Repo: "nas"}, op), "")
	if err != nil {
		t.Fatal(err)
	}
	var rec failure
	if raw != nil {
		testutil.UnmarshalBody(t, raw, &rec)
	}
	return rec
}

// recorded reports whether a failure push for op is on record.
func (rh *retractHarness) recorded(t *testing.T, op string) bool {
	t.Helper()
	return rh.record(t, op).Slug != ""
}

var (
	opSlug    = text.SlugHash("backrest", "dailynas", 4)
	alertSlug = text.SlugHash("backrest-alert", "dailynas"+condAnyError, 4)
)

// A failure pushed with ?retract=1 is taken back by the next success, and the
// any-error alert with it, each once.
func TestRetractFailureOnSuccess(t *testing.T) {
	rh := newRetractHarness(t)
	rh.post(t, "retract=1", condSnapshotError)
	rh.post(t, "retract=1", condAnyError)
	if !rh.recorded(t, "SNAPSHOT") || !rh.recorded(t, "ANY") {
		t.Fatal("failure pushes with ?retract=1 left no record")
	}
	rh.post(t, "retract=1", condSnapshotSuccess)
	rh.post(t, "retract=1", condSnapshotSuccess)
	if rs := rh.retracts(t); len(rs) != 2 || rs[0] != opSlug || rs[1] != alertSlug {
		t.Errorf("retracts %q, want %s then %s", rs, opSlug, alertSlug)
	}
	if rh.recorded(t, "SNAPSHOT") || rh.recorded(t, "ANY") {
		t.Error("records left after the success")
	}
}

// Another operation's success leaves the snapshot failure up.
func TestRetractOnlyItsOwnOperation(t *testing.T) {
	rh := newRetractHarness(t)
	rh.post(t, "retract=1", condSnapshotError)
	rh.post(t, "retract=1", condPruneSuccess)
	if rs := rh.retracts(t); len(rs) != 0 {
		t.Errorf("a prune success retracted %q", rs)
	}
	if !rh.recorded(t, "SNAPSHOT") {
		t.Error("a prune success cleared the snapshot failure's record")
	}
}

// A retract that does not reach leaves the record, so the next success of the
// operation tries again.
func TestRetractKeptWhenItMisses(t *testing.T) {
	zero := 0
	for _, tc := range []struct {
		name string
		opts testutil.MockOptions
	}{
		{"no device", testutil.MockOptions{RetractDevices: &zero}},
		{"refused", testutil.MockOptions{RetractStatus: http.StatusForbidden}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rh := newRetractHarnessWith(t, tc.opts)
			rh.post(t, "retract=1", condSnapshotError)
			rh.post(t, "", condSnapshotSuccess)
			if rec := rh.record(t, "SNAPSHOT"); rec.Slug != opSlug || rec.Gen != 0 {
				t.Fatalf("record after a missed retract = %+v, want it kept", rec)
			}
			rh.post(t, "", condSnapshotSuccess)
			if rs := rh.retracts(t); len(rs) != 2 || rs[0] != opSlug || rs[1] != opSlug {
				t.Errorf("retracts %q, want %s tried by both successes", rs, opSlug)
			}
		})
	}
}

// The retract is a delayed silent push, so once one reached, the plan and
// repo's next pushes go out under a new id it cannot take down, and still
// replace each other there. So does the any-error alert's.
func TestRetractMovesLaterPushesToNewID(t *testing.T) {
	rh := newRetractHarness(t)
	rh.post(t, "retract=1", condSnapshotError)
	rh.post(t, "retract=1", condAnyError)
	rh.post(t, "retract=1", condSnapshotSuccess)
	rh.post(t, "retract=1", condForgetError)
	rh.post(t, "retract=1", condPruneError)
	rh.post(t, "retract=1", condAnyError)

	ns := notifications(t, testutil.GetCalls(rh.calls, rh.mu))
	var ids []string
	for _, n := range ns {
		ids = append(ids, n.CollapseID)
	}
	want := []string{opSlug, alertSlug, opSlug + "-1", opSlug + "-1", alertSlug + "-1"}
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Fatalf("pushed under %q, want %q", ids, want)
	}
	if ns[2].ActivitySlug != opSlug {
		t.Errorf("activity slug %q, want the activity's own %s", ns[2].ActivitySlug, opSlug)
	}
	if rec := rh.record(t, "PRUNE"); rec.Slug != opSlug+"-1" || rec.Op != "PRUNE" {
		t.Errorf("record = %+v, want the prune failure under its new id", rec)
	}

	rh.post(t, "retract=1", condPruneSuccess)
	rh.post(t, "channels=notification", condCheckSuccess)
	if rs := rh.retracts(t); strings.Join(rs, " ") != strings.Join([]string{opSlug, alertSlug, opSlug + "-1", alertSlug + "-1"}, " ") {
		t.Errorf("retracts %q", rs)
	}
	ns = notifications(t, testutil.GetCalls(rh.calls, rh.mu))
	if last := ns[len(ns)-1]; last.CollapseID != opSlug+"-2" {
		t.Errorf("push after the second retract under %q, want %s", last.CollapseID, opSlug+"-2")
	}
}

// Every operation pushes under the plan and repo's one collapse id, so a
// prune failure replaces a check failure on the device. The check passing
// then must not take back the prune failure; the prune passing does.
func TestRetractFailureReplacedByAnotherOperation(t *testing.T) {
	rh := newRetractHarness(t)
	rh.post(t, "retract=1", condCheckError)
	rh.post(t, "retract=1", condPruneError)
	rh.post(t, "retract=1", condCheckSuccess)
	if rs := rh.retracts(t); len(rs) != 0 {
		t.Fatalf("the check passing retracted %q while the prune failure is up", rs)
	}
	rh.post(t, "retract=1", condPruneSuccess)
	if rs := rh.retracts(t); len(rs) != 1 || rs[0] != opSlug {
		t.Errorf("retracts %q, want %s once", rs, opSlug)
	}
	if rh.recorded(t, "PRUNE") {
		t.Error("record left after the prune passed")
	}
}

func TestRetractSuccessWithoutRecord(t *testing.T) {
	rh := newRetractHarness(t)
	rh.post(t, "retract=1", condSnapshotSuccess)
	// Without ?retract=1 a failure writes no record.
	rh.post(t, "", condSnapshotError)
	rh.post(t, "retract=1", condSnapshotSuccess)
	if rs := rh.retracts(t); len(rs) != 0 {
		t.Errorf("retracts %q without a record", rs)
	}
	if rh.recorded(t, "SNAPSHOT") {
		t.Error("a failure without ?retract=1 wrote a record")
	}
}

// Under channels=notification the success goes out under the failure's
// collapse id and replaces it, so the record is dropped without a retract.
func TestRetractReplacedBySuccessPush(t *testing.T) {
	rh := newRetractHarness(t)
	rh.post(t, "channels=notification&retract=1", condSnapshotError)
	rh.post(t, "channels=notification&retract=1", condSnapshotSuccess)
	if rs := rh.retracts(t); len(rs) != 0 {
		t.Errorf("retracts %q, want none", rs)
	}
	if rh.recorded(t, "SNAPSHOT") {
		t.Error("record left after the success replaced the failure")
	}
	ns := notifications(t, testutil.GetCalls(rh.calls, rh.mu))
	if len(ns) != 2 || ns[1].CollapseID != opSlug {
		t.Errorf("notifications = %+v, want the success under %s", ns, opSlug)
	}
}

// Another operation's success push replaces the failure too.
func TestRetractReplacedByAnotherOperationsPush(t *testing.T) {
	rh := newRetractHarness(t)
	rh.post(t, "channels=notification&retract=1", condCheckError)
	rh.post(t, "channels=notification&retract=1", condPruneSuccess)
	if rh.recorded(t, "CHECK") {
		t.Error("record left after a prune success push replaced the check failure")
	}
	rh.post(t, "retract=1", condCheckSuccess)
	if rs := rh.retracts(t); len(rs) != 0 {
		t.Errorf("retracts %q, want none", rs)
	}
}
