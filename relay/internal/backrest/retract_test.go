package backrest

import (
	"context"
	"fmt"
	"net/http"
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
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	srv, calls, mu := testutil.MockPushWardServer(t)
	store := state.NewMemoryStore()
	mux, api := humautil.NewTestAPI()
	h := RegisterRoutes(api, store, client.NewPool(srv.URL, nil), testConfig())
	t.Cleanup(h.Ender().StopAll)
	return &retractHarness{h: mux, store: store, calls: calls, mu: mu}
}

func (rh *retractHarness) post(t *testing.T, query, event string) {
	t.Helper()
	body := fmt.Sprintf(`{"event":%q,"plan":"daily","repo":"nas","error":""}`, event)
	if event != condSnapshotSuccess {
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

func (rh *retractHarness) recorded(t *testing.T, op string) bool {
	t.Helper()
	ok, err := rh.store.Exists(context.Background(), "backrest", "hlk_test", failureKey(&backrestPayload{Plan: "daily", Repo: "nas"}, op), "")
	if err != nil {
		t.Fatal(err)
	}
	return ok
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
	if !rh.recorded(t, "SNAPSHOT") {
		t.Error("a prune success cleared the snapshot failure's record")
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
