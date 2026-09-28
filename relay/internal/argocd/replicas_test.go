package argocd

import (
	"context"
	"net/http"
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

// replica is one relay pod: its own handler and grace timers over a store it
// shares with the other pods.
type replica struct {
	mux http.Handler
	h   *Handler
}

func setupReplicas(t *testing.T, cfg *config.ArgoCDConfig, srvURL string) (a, b replica) {
	t.Helper()
	lifecycle.SetRetryDelay(10 * time.Millisecond)
	store := state.NewMemoryStore()
	newReplica := func() replica {
		mux, api := humautil.NewTestAPI()
		h := RegisterRoutes(api, store, client.NewPool(srvURL, nil), cfg)
		t.Cleanup(h.StopAll)
		return replica{mux: mux, h: h}
	}
	return newReplica(), newReplica()
}

// longGrace keeps every grace timer from firing during a test, so whatever
// creates the activity is the overdue promotion under test.
func longGrace() *config.ArgoCDConfig {
	cfg := testConfig()
	cfg.SyncGracePeriod = time.Minute
	return cfg
}

// backdate moves an app's grace period start into the past, as if it had
// been pending for d.
func backdate(t *testing.T, h *Handler, appName string, d time.Duration) {
	t.Helper()
	ctx := context.Background()
	app, ok, err := h.loadApp(ctx, testKey, appName)
	if err != nil || !ok || !app.Pending {
		t.Fatalf("expected %s pending in the store, got app=%+v ok=%v err=%v", appName, app, ok, err)
	}
	app.PendingSince = time.Now().Add(-d).UnixMilli()
	if err := h.saveApp(ctx, testKey, appName, app); err != nil {
		t.Fatal(err)
	}
}

// crashed stands for replica A's pod going away mid-grace: its timers are gone
// and its pending row stays behind, older than the grace period.
func crashed(t *testing.T, a replica, appName string) {
	t.Helper()
	a.h.StopAll()
	backdate(t, a.h, appName, 2*time.Minute)
}

func TestOverdue(t *testing.T) {
	h := &Handler{config: longGrace(), overdueSlack: defaultOverdueSlack}
	slack := defaultOverdueSlack
	tests := []struct {
		name string
		app  trackedAppState
		want bool
	}{
		{"not pending", trackedAppState{PendingSince: 1}, false},
		{"pending within grace", trackedAppState{Pending: true, PendingSince: time.Now().UnixMilli()}, false},
		{"pending within the slack", trackedAppState{Pending: true, PendingSince: time.Now().Add(-time.Minute - slack/2).UnixMilli()}, false},
		{"pending past grace and slack", trackedAppState{Pending: true, PendingSince: time.Now().Add(-time.Minute - 2*slack).UnixMilli()}, true},
		// An older pod may still be timing it; armBackstop covers the row.
		{"pending row from before pending_since", trackedAppState{Pending: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.overdue(&tt.app); got != tt.want {
				t.Errorf("overdue = %v, want %v", got, tt.want)
			}
		})
	}
}

// The grace timer on A fires with whatever B wrote in the meantime. The grace
// period is long enough that a slow -race runner still gets B's write in first.
func TestReplicas_GraceTimerSeesOtherReplicasStep(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	cfg := testConfig()
	cfg.SyncGracePeriod = 2 * time.Second
	a, b := setupReplicas(t, cfg, srv.URL)

	sendWebhook(t, a.mux, `{"app":"shared","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, b.mux, `{"app":"shared","event":"sync-succeeded","revision":"r1"}`)
	if n := len(testutil.GetCalls(calls, mu)); n != 0 {
		t.Fatalf("expected 0 calls during grace, got %d", n)
	}

	recorded := testutil.WaitForCalls(t, calls, mu, 2, 10*time.Second)
	if len(recorded) != 2 {
		t.Fatalf("expected create + update from A's timer, got %d calls", len(recorded))
	}
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Rolling out..." {
		t.Errorf("expected A to pick up B's step 2, got %q", update.Content.State)
	}
}

func TestReplicas_OverduePendingPromotedOnSyncSucceeded(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	a, b := setupReplicas(t, longGrace(), srv.URL)

	sendWebhook(t, a.mux, `{"app":"orphan","event":"sync-running","revision":"r1"}`)
	crashed(t, a, "orphan")

	w := sendWebhook(t, b.mux, `{"app":"orphan","event":"sync-succeeded","revision":"r1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected create + step 2 update, got %d calls", len(recorded))
	}
	if recorded[0].Method != http.MethodPost || recorded[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", recorded[0].Method, recorded[0].Path)
	}
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Rolling out..." || update.Content.CurrentStep == nil || *update.Content.CurrentStep != 2 {
		t.Errorf("expected step 2 Rolling out..., got %q step %v", update.Content.State, update.Content.CurrentStep)
	}

	app, ok, _ := b.h.loadApp(context.Background(), testKey, "orphan")
	if !ok || app.Pending || app.PendingSince != 0 || app.Step != 2 {
		t.Errorf("expected a tracked, non-pending app at step 2, got %+v", app)
	}
}

func TestReplicas_OverduePendingPromotedOnSyncRunning(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	a, b := setupReplicas(t, longGrace(), srv.URL)

	sendWebhook(t, a.mux, `{"app":"orphan","event":"sync-running","revision":"r1"}`)
	crashed(t, a, "orphan")

	// A repeated sync-running for the same revision shows the sync instead of
	// starting another grace period.
	sendWebhook(t, b.mux, `{"app":"orphan","event":"sync-running","revision":"r1"}`)

	recorded := testutil.GetCalls(calls, mu)
	if len(recorded) != 2 {
		t.Fatalf("expected create + step 1 update, got %d calls", len(recorded))
	}
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Syncing..." {
		t.Errorf("expected Syncing..., got %q", update.Content.State)
	}
	b.h.mu.Lock()
	timers := len(b.h.graceTimers)
	b.h.mu.Unlock()
	if timers != 0 {
		t.Errorf("expected no grace timer on B after promotion, got %d", timers)
	}
}

func TestReplicas_OverduePendingOnDeployedCreatesActivity(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	a, b := setupReplicas(t, longGrace(), srv.URL)

	sendWebhook(t, a.mux, `{"app":"orphan","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, a.mux, `{"app":"orphan","event":"sync-succeeded","revision":"r1"}`)
	crashed(t, a, "orphan")

	sendWebhook(t, b.mux, `{"app":"orphan","event":"deployed","revision":"r1"}`)

	recorded := testutil.WaitForCalls(t, calls, mu, 3, 5*time.Second)
	// create + phase1(ONGOING Deployed) + phase2(ENDED Deployed) = 3
	if len(recorded) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(recorded))
	}
	if recorded[0].Method != http.MethodPost {
		t.Errorf("expected the activity to be created first, got %s", recorded[0].Method)
	}
	var end pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[2].Body, &end)
	if end.State != pushward.StateEnded || end.Content.State != "Deployed" {
		t.Errorf("expected ENDED Deployed, got %s %q", end.State, end.Content.State)
	}
	// The ender drops the row right after the ENDED update returns.
	for deadline := time.Now().Add(5 * time.Second); appExists(t, b.h, "orphan"); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("expected the app to be removed after its end")
		}
	}
}

// Within the grace period B leaves the pending app to A's timer, as a single
// replica would: a fast sync stays silent.
func TestReplicas_NotOverdueKeepsGrace(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	a, b := setupReplicas(t, longGrace(), srv.URL)

	sendWebhook(t, a.mux, `{"app":"fast","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, b.mux, `{"app":"fast","event":"sync-succeeded","revision":"r1"}`)

	app, ok, _ := b.h.loadApp(context.Background(), testKey, "fast")
	if !ok || !app.Pending || app.Step != 2 {
		t.Fatalf("expected a pending app at step 2, got %+v", app)
	}

	sendWebhook(t, b.mux, `{"app":"fast","event":"deployed","revision":"r1"}`)

	if n := len(testutil.GetCalls(calls, mu)); n != 0 {
		t.Fatalf("expected 0 calls for a no-op sync, got %d", n)
	}
	if appExists(t, a.h, "fast") {
		t.Error("expected the no-op sync to be dropped from the store")
	}
	if !a.h.hasTombstone(context.Background(), testKey, "fast") {
		t.Error("expected a tombstone for the skipped sync")
	}
}

// shortGrace is a grace period that expires within a test, with the backstop
// slack cut down to match.
func shortGrace() *config.ArgoCDConfig {
	cfg := testConfig()
	cfg.SyncGracePeriod = time.Second
	return cfg
}

func shortSlack(rs ...replica) {
	for _, r := range rs {
		r.h.overdueSlack = 50 * time.Millisecond
	}
}

func graceTimers(h *Handler) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.graceTimers)
}

// A goes away inside the grace period; B only sees sync-succeeded, which is not
// overdue yet. B's backstop still produces the card once the grace runs out.
func TestReplicas_BackstopWhenHolderGone(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	a, b := setupReplicas(t, shortGrace(), srv.URL)
	shortSlack(a, b)

	sendWebhook(t, a.mux, `{"app":"orphan","event":"sync-running","revision":"r1"}`)
	a.h.StopAll()
	sendWebhook(t, b.mux, `{"app":"orphan","event":"sync-succeeded","revision":"r1"}`)
	if n := len(testutil.GetCalls(calls, mu)); n != 0 {
		t.Fatalf("expected 0 calls inside the grace period, got %d", n)
	}

	recorded := testutil.WaitForCalls(t, calls, mu, 2, 10*time.Second)
	if len(recorded) != 2 {
		t.Fatalf("expected create + step 2 update from B's backstop, got %d calls", len(recorded))
	}
	var update pushward.UpdateRequest
	testutil.UnmarshalBody(t, recorded[1].Body, &update)
	if update.Content.State != "Rolling out..." {
		t.Errorf("expected Rolling out..., got %q", update.Content.State)
	}
}

// With the holder alive, its timer creates the card and B's backstop, firing
// overdueSlack later, finds the app resolved and adds nothing.
func TestReplicas_BackstopYieldsToLiveHolder(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	a, b := setupReplicas(t, shortGrace(), srv.URL)
	shortSlack(a, b)

	sendWebhook(t, a.mux, `{"app":"shared","event":"sync-running","revision":"r1"}`)
	sendWebhook(t, b.mux, `{"app":"shared","event":"sync-succeeded","revision":"r1"}`)
	if graceTimers(b.h) != 1 {
		t.Fatal("expected B to arm a backstop")
	}

	if n := len(testutil.WaitForCalls(t, calls, mu, 2, 10*time.Second)); n != 2 {
		t.Fatalf("expected create + update from A's timer, got %d calls", n)
	}
	for deadline := time.Now().Add(10 * time.Second); graceTimers(b.h) != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("B's backstop never fired")
		}
	}
	if n := len(testutil.GetCalls(calls, mu)); n != 2 {
		t.Errorf("expected the backstop to add nothing, got %d calls", n)
	}
}

// legacyRow writes a pending row the way a pod from before pending_since did.
func legacyRow(t *testing.T, h *Handler, appName string) {
	t.Helper()
	if err := h.saveApp(context.Background(), testKey, appName, &trackedAppState{
		Slug:     slugForApp(appName),
		Revision: "r1",
		Step:     1,
		Pending:  true,
	}); err != nil {
		t.Fatal(err)
	}
}

// A pending row from an older pod has no start time. It is not promoted on
// sight: B backs it up with a full grace period of its own.
func TestReplicas_LegacyPendingRowGetsBackstop(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	_, b := setupReplicas(t, shortGrace(), srv.URL)
	legacyRow(t, b.h, "legacy")

	sendWebhook(t, b.mux, `{"app":"legacy","event":"sync-succeeded","revision":"r1"}`)
	if n := len(testutil.GetCalls(calls, mu)); n != 0 {
		t.Fatalf("expected no immediate promotion, got %d calls", n)
	}

	recorded := testutil.WaitForCalls(t, calls, mu, 2, 10*time.Second)
	if len(recorded) != 2 {
		t.Fatalf("expected create + step 2 update after the grace period, got %d calls", len(recorded))
	}
	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, recorded[0].Body, &create)
	if create.Slug != "argocd-legacy" {
		t.Errorf("expected slug argocd-legacy, got %s", create.Slug)
	}
}

// The rollout case: an older pod wrote the pending row and deployed follows
// quickly. It is a no-op sync and stays silent.
func TestReplicas_LegacyPendingRowNoOpSync(t *testing.T) {
	srv, calls, mu := testutil.MockPushWardServer(t)
	_, b := setupReplicas(t, longGrace(), srv.URL)
	legacyRow(t, b.h, "legacy")

	sendWebhook(t, b.mux, `{"app":"legacy","event":"deployed","revision":"r1"}`)

	if n := len(testutil.GetCalls(calls, mu)); n != 0 {
		t.Fatalf("expected a no-op sync to stay silent, got %d calls", n)
	}
	if appExists(t, b.h, "legacy") {
		t.Error("expected the no-op sync to be dropped from the store")
	}
}

func TestReplicas_OverduePromotionCreateFails(t *testing.T) {
	srv, _, _ := customMockServer(t, http.MethodPost)
	a, b := setupReplicas(t, longGrace(), srv.URL)

	sendWebhook(t, a.mux, `{"app":"orphan","event":"sync-running","revision":"r1"}`)
	crashed(t, a, "orphan")

	w := sendWebhook(t, b.mux, `{"app":"orphan","event":"sync-succeeded","revision":"r1"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
	if appExists(t, b.h, "orphan") {
		t.Error("expected the app to be removed after the create failed")
	}
}
