package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/backrest/internal/backrest"
	"github.com/mac-lucky/pushward-integrations/backrest/internal/config"
	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// fakeBackrest returns a scripted sequence of operation windows, one per poll,
// repeating the last once it runs out.
type fakeBackrest struct {
	mu       sync.Mutex
	windows  [][]backrest.Operation
	call     int
	logs     map[string]string
	logCalls int
	logErr   error
	err      error
}

func (f *fakeBackrest) GetOperations(_ context.Context, _ int64) ([]backrest.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if len(f.windows) == 0 {
		return nil, nil
	}
	i := f.call
	if i >= len(f.windows) {
		i = len(f.windows) - 1
	}
	f.call++
	return f.windows[i], nil
}

func (f *fakeBackrest) GetLogs(_ context.Context, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logCalls++
	if f.logErr != nil {
		return "", f.logErr
	}
	return f.logs[ref], nil
}

func (f *fakeBackrest) logCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logCalls
}

// A prune rewrites its own output as it works, so tests need to as well.
func (f *fakeBackrest) setLog(ref, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs[ref] = body
}

func testConfig() *config.Config {
	return &config.Config{
		PushWard: sharedconfig.PushWardConfig{
			Priority:       2,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
		Polling: config.PollingConfig{
			Interval:     5 * time.Second,
			IdleInterval: 30 * time.Second,
			LastN:        50,
		},
		// Mirrors the shipped defaults so tests exercise real behavior.
		Render: config.RenderConfig{LiveProgress: true, Logs: true, MaxETA: 7 * 24 * time.Hour},
	}
}

// testClock is where every harness starts. Tests that care how old an operation
// is date it from here rather than from the fixture's own timestamps.
var testClock = time.Unix(1785050000, 0)

// harness wires a poller to the contract-validating mock server and a fake
// Backrest, with a clock the test drives.
type harness struct {
	p     *Poller
	br    *fakeBackrest
	calls *[]testutil.APICall
	mu    *sync.Mutex
	clock time.Time
}

func newHarness(t *testing.T, cfg *config.Config, br *fakeBackrest) *harness {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	t.Cleanup(srv.Close)
	return wire(t, cfg, br, srv, calls, mu)
}

// newRejectingHarness is newHarness against a server that turns down the first
// n frames, which is how a PushWard outage looks from here. The rejection is a
// 400, so the client fails fast instead of spending its own retry budget and
// the test's wall clock on each one.
func newRejectingHarness(t *testing.T, cfg *config.Config, br *fakeBackrest, n int) *harness {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServerFailingPatches(t, n)
	return wire(t, cfg, br, srv, calls, mu)
}

// gatedServer records every PushWard call and hands each PATCH to gate, which
// answers with the status to send and may block first.
//
// testutil's failing mock turns down a fixed prefix of PATCHes, and the seed is
// always the first of them, so it cannot express either of the two cases below:
// an activity that is up and has a later frame refused, and a frame held open
// while shutdown runs.
func gatedServer(t *testing.T, gate func(n int) int) (*httptest.Server, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	var calls []testutil.APICall
	var mu sync.Mutex
	patches := 0

	record := func(r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, testutil.APICall{Method: r.Method, Path: r.URL.Path, Body: body})
		mu.Unlock()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /activities", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("PATCH /activities/", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		mu.Lock()
		patches++
		n := patches
		mu.Unlock()
		w.WriteHeader(gate(n))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls, &mu
}

func wire(t *testing.T, cfg *config.Config, br *fakeBackrest, srv *httptest.Server, calls *[]testutil.APICall, mu *sync.Mutex) *harness {
	t.Helper()
	pw := pushward.NewClient(srv.URL, "hlk_test")
	p := New(cfg, br, pw)

	h := &harness{p: p, br: br, calls: calls, mu: mu, clock: testClock}
	p.now = func() time.Time { return h.clock }
	return h
}

func (h *harness) advance(d time.Duration) { h.clock = h.clock.Add(d) }

func (h *harness) poll(t *testing.T) {
	t.Helper()
	if err := h.p.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

func (h *harness) recorded() []testutil.APICall {
	return testutil.GetCalls(h.calls, h.mu)
}

// pushCount is how many frames reached the API. One push is one PATCH: the POST
// that precedes the seed creates the activity and carries no frame.
func (h *harness) pushCount() int {
	n := 0
	for _, c := range h.recorded() {
		if c.Method == "PATCH" {
			n++
		}
	}
	return n
}

// content unmarshals the content object of a recorded call.
//
// It decodes into pushward.Content rather than a local mirror of it, so a
// renamed JSON key fails here instead of quietly decoding to a zero value and
// letting assertions pass on a field the server never received.
func content(t *testing.T, call testutil.APICall) pushward.Content {
	t.Helper()
	var body struct {
		State   string           `json:"state"`
		Content pushward.Content `json:"content"`
	}
	testutil.UnmarshalBody(t, call.Body, &body)
	return body.Content
}

func activityState(t *testing.T, call testutil.APICall) string {
	t.Helper()
	var body struct {
		State string `json:"state"`
	}
	testutil.UnmarshalBody(t, call.Body, &body)
	return body.State
}

func loadOp(t *testing.T, name string) backrest.Operation {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", name)) // #nosec G304 -- this module's own testdata dir, not user input
	if err != nil {
		t.Fatal(err)
	}
	var op backrest.Operation
	if err := json.Unmarshal(raw, &op); err != nil {
		t.Fatal(err)
	}
	return op
}

func loadText(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", name)) // #nosec G304 -- this module's own testdata dir, not user input
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// runningBackup builds an in-flight backup at a given byte count.
func runningBackup(id int64, bytesDone, totalBytes int64) backrest.Operation {
	return backrest.Operation{
		ID:            backrest.Int64(id),
		FlowID:        backrest.Int64(id),
		InstanceID:    "laptop",
		RepoID:        "nas",
		PlanID:        "appdata",
		Status:        backrest.StatusInProgress,
		UnixTimeStart: backrest.Int64(1785050000000),
		Backup: &backrest.OperationBackup{
			LastStatus: &backrest.BackupProgressEntry{
				Status: &backrest.BackupProgressStatus{
					TotalBytes: backrest.Int64(totalBytes),
					BytesDone:  backrest.Int64(bytesDone),
					TotalFiles: 100,
					FilesDone:  backrest.Int64(bytesDone * 100 / totalBytes),
				},
			},
		},
	}
}

// finishedBackup is a completed backup stamped with an end time, which is what
// decides whether the first poll announces it.
func finishedBackup(t *testing.T, id int64, endedAgo time.Duration) backrest.Operation {
	t.Helper()
	op := loadOp(t, "backup_success.json")
	op.ID = backrest.Int64(id)
	op.UnixTimeEnd = backrest.Int64(testClock.Add(-endedAgo).UnixMilli())
	// Moving only the end would date it before the fixture's own start, which
	// Operation.Elapsed would refuse to render for a kind that has no restic
	// summary to fall back on.
	op.UnixTimeStart = backrest.Int64(testClock.Add(-endedAgo - time.Minute).UnixMilli())
	return op
}

// The first window a freshly started bridge sees is a wall of finished
// operations from previous days. Announcing those would push an activity for
// every one of them.
func TestFirstPollDoesNotAnnounceHistory(t *testing.T) {
	done := finishedBackup(t, 12208, 24*time.Hour)
	prune := loadOp(t, "prune_success.json")
	br := &fakeBackrest{windows: [][]backrest.Operation{{done, prune}}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	// The priming pass announces nothing whatever it decides. Suppression only
	// shows up on the tick after it, where a row it did not record as done
	// falls through and is treated as an outcome that just landed.
	h.poll(t)

	if calls := h.recorded(); len(calls) != 0 {
		t.Fatalf("two polls made %d API calls, want 0: %+v", len(calls), calls)
	}
}

// A rollout or a node drain restarts this process while a backup is running,
// and the backup finishes in the gap. Suppressing that outcome as history
// strands the activity the previous process opened: it sits on the Lock Screen
// showing the progress it stopped at until the server's stale timeout expires.
func TestFirstPollClosesAJustFinishedOperation(t *testing.T) {
	done := finishedBackup(t, 777, time.Minute)
	br := &fakeBackrest{windows: [][]backrest.Operation{{done}, {done}}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)

	calls := waitForEnded(t, h, 0)
	if calls[0].Method != "POST" || calls[0].Path != "/activities" {
		t.Fatalf("first call = %s %s, want the activity re-adopted", calls[0].Method, calls[0].Path)
	}
	final := content(t, calls[len(calls)-1])
	if !strings.HasPrefix(final.State, stateComplete) {
		t.Errorf("state = %q, want the completion line", final.State)
	}
}

// Past the stale timeout there is no activity left for the outcome to land on,
// so an older row is history like any other.
func TestFirstPollSuppressesAnOperationEndedLongAgo(t *testing.T) {
	done := finishedBackup(t, 777, 2*time.Hour)
	br := &fakeBackrest{windows: [][]backrest.Operation{{done}, {done}}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t) // the tick where a row priming failed to record as done surfaces

	if calls := h.recorded(); len(calls) != 0 {
		t.Fatalf("announced an operation that ended 2h ago in %d calls: %+v", len(calls), calls)
	}
}

// The adoption window is the server's stale timeout because that is exactly how
// long the stranded activity survives; any other duration either misses cards
// that are still on screen or reopens ones the server already aged out.
func TestFirstPollAdoptionWindowIsTheStaleTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.PushWard.StaleTimeout = 5 * time.Minute

	done := finishedBackup(t, 777, 10*time.Minute)
	br := &fakeBackrest{windows: [][]backrest.Operation{{done}, {done}}}
	h := newHarness(t, cfg, br)

	h.poll(t)
	h.poll(t)

	// The same operation is adopted under testConfig's 30m stale timeout, and
	// still would be under any of the other durations in that config.
	if calls := h.recorded(); len(calls) != 0 {
		t.Fatalf("adopted an operation twice as old as the stale timeout in %d calls: %+v", len(calls), calls)
	}
}

// An operation Backrest never stamped with an end time cannot be shown to be
// recent, and guessing would announce the whole window on every restart.
func TestFirstPollSuppressesAnOperationWithNoEndTime(t *testing.T) {
	done := finishedBackup(t, 777, time.Minute)
	done.UnixTimeEnd = 0
	br := &fakeBackrest{windows: [][]backrest.Operation{{done}, {done}}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)

	if calls := h.recorded(); len(calls) != 0 {
		t.Fatalf("announced an operation with no end time in %d calls: %+v", len(calls), calls)
	}
}

// endedWithin is asserted directly for the two inputs no plausible window makes
// visible through poll: an unstamped row, which arithmetic alone would read as
// a 1970 finish and only reject because 1970 is far away, and a stamp from the
// future, which a window bounded only above reads as maximally recent.
func TestEndedWithinRejectsUnstampedAndFutureRows(t *testing.T) {
	h := newHarness(t, testConfig(), &fakeBackrest{})

	// A century, so "decades old" cannot stand in for "never stamped".
	if h.p.endedWithin(&backrest.Operation{UnixTimeEnd: 0}, 100*365*24*time.Hour) {
		t.Error("an unstamped row was read as a finish at the epoch")
	}
	future := &backrest.Operation{UnixTimeEnd: backrest.Int64(testClock.Add(6 * time.Hour).UnixMilli())}
	if h.p.endedWithin(future, 30*time.Minute) {
		t.Error("a stamp 6h ahead of this clock was read as recent")
	}
	// Skew small enough to be routine still has to count, or a restart during a
	// backup that has just finished loses the outcome it exists to deliver.
	fresh := &backrest.Operation{UnixTimeEnd: backrest.Int64(testClock.Add(2 * time.Second).UnixMilli())}
	if !h.p.endedWithin(fresh, 30*time.Minute) {
		t.Error("a stamp 2s ahead of this clock was rejected as skew")
	}
}

// ...but it must still pick up a backup that was already running when the
// bridge started.
func TestFirstPollTracksAlreadyRunningBackup(t *testing.T) {
	running := runningBackup(500, 20, 100)
	br := &fakeBackrest{windows: [][]backrest.Operation{{running}, {running}}}
	h := newHarness(t, testConfig(), br)

	h.poll(t) // priming
	h.poll(t)

	calls := h.recorded()
	if len(calls) < 2 {
		t.Fatalf("got %d calls, want a create and a seed", len(calls))
	}
	if calls[0].Method != "POST" || calls[0].Path != "/activities" {
		t.Errorf("first call = %s %s, want POST /activities", calls[0].Method, calls[0].Path)
	}
}

func TestBackupBarAdvances(t *testing.T) {
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, 1000)},   // priming window
		{runningBackup(1, 100, 1000)}, // create + seed at 10%
		{runningBackup(1, 500, 1000)}, // 50%
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	h.advance(10 * time.Second)
	h.poll(t)

	calls := h.recorded()
	var progress []float64
	for _, c := range calls {
		if c.Method == "POST" {
			continue
		}
		progress = append(progress, content(t, c).Progress)
	}
	if len(progress) < 2 {
		t.Fatalf("got %d content updates, want at least 2: %+v", len(progress), calls)
	}
	if progress[0] != 0.1 {
		t.Errorf("seed progress = %v, want 0.1", progress[0])
	}
	if progress[len(progress)-1] != 0.5 {
		t.Errorf("final progress = %v, want 0.5", progress[len(progress)-1])
	}
}

// The state line is where the transfer rate lives, and it only appears once
// there are two samples to derive one from.
func TestStateLineCarriesBytesAndRate(t *testing.T) {
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, 100_000_000)},
		{runningBackup(1, 10_000_000, 100_000_000)},
		{runningBackup(1, 30_000_000, 100_000_000)},
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	h.advance(10 * time.Second)
	h.poll(t)

	calls := h.recorded()
	last := content(t, calls[len(calls)-1])
	if !strings.Contains(last.State, " of ") {
		t.Errorf("state = %q, want a \"<done> of <total>\" phrase", last.State)
	}
	if !strings.Contains(last.State, "/s") {
		t.Errorf("state = %q, want a transfer rate", last.State)
	}
}

// Before restic finishes scanning there is no total to divide by. Saying so
// beats showing "0 B of 0 B".
func TestScanningPhaseHasNoFabricatedTotals(t *testing.T) {
	op := runningBackup(1, 0, 1)
	op.Backup.LastStatus.Status.TotalBytes = 0
	br := &fakeBackrest{windows: [][]backrest.Operation{{op}, {op}}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)

	calls := h.recorded()
	seed := content(t, calls[1])
	if seed.State != stateScanning {
		t.Errorf("state = %q, want %q", seed.State, stateScanning)
	}
	if seed.Progress != 0 {
		t.Errorf("progress = %v, want 0", seed.Progress)
	}
}

// The throttle has to gate on the underlying numbers, not on the rendered
// state line.
//
// That line carries a formatted byte count and transfer rate, both of which
// move on nearly every 5s tick. Comparing it made the 2%-progress test and the
// heartbeat below it unreachable, and an hour of steady backup pushed 207 times
// where it should push 5. This test fails at ~200 if the state string ever
// becomes a push trigger again.
func TestThrottleIsNotDefeatedByTheStateLine(t *testing.T) {
	const total = 200 << 30
	const rate = 5 << 20 // bytes per second

	windows := make([][]backrest.Operation, 0, 722)
	windows = append(windows, []backrest.Operation{runningBackup(1, 0, total)})
	for i := 1; i <= 720; i++ {
		windows = append(windows, []backrest.Operation{runningBackup(1, rate*int64(i)*5, total)})
	}
	h := newHarness(t, testConfig(), &fakeBackrest{windows: windows})

	h.poll(t) // priming
	for i := 0; i < 720; i++ {
		h.poll(t)
		h.advance(5 * time.Second)
	}

	// One create + one seed, then a push per 2% of progress. An hour at 5 MB/s
	// covers ~9% of 200 GB, so a handful - nowhere near one per tick.
	pushes := 0
	for _, c := range h.recorded() {
		if c.Method == "PATCH" {
			pushes++
		}
	}
	if pushes > 20 {
		t.Errorf("%d pushes over 720 ticks, want well under 20 - the throttle is not gating", pushes)
	}
	if pushes == 0 {
		t.Error("no pushes at all, so the bar never moved")
	}
	t.Logf("720 ticks produced %d pushes", pushes)
}

// A tick that says nothing new must not become a push: a multi-hour backup
// polled every 5s would otherwise spend thousands of them redrawing one frame.
func TestUnchangedTickIsSuppressed(t *testing.T) {
	same := runningBackup(1, 500, 1000)
	br := &fakeBackrest{windows: [][]backrest.Operation{{same}, {same}, {same}, {same}}}
	h := newHarness(t, testConfig(), br)

	h.poll(t) // priming
	h.poll(t) // create + seed
	before := len(h.recorded())

	h.advance(5 * time.Second)
	h.poll(t)
	h.advance(5 * time.Second)
	h.poll(t)

	if after := len(h.recorded()); after != before {
		t.Errorf("made %d extra calls for unchanged ticks, want 0", after-before)
	}
}

// The server ends an activity that goes quiet for stale_timeout, so silence has
// a ceiling even when nothing changes.
func TestHeartbeatBreaksTheSilence(t *testing.T) {
	same := runningBackup(1, 500, 1000)
	br := &fakeBackrest{windows: [][]backrest.Operation{{same}, {same}, {same}, {same}}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	before := len(h.recorded())

	// Just short of the interval, silence still holds.
	h.advance(h.p.heartbeat() - time.Second)
	h.poll(t)
	if got := len(h.recorded()); got != before {
		t.Fatalf("made %d calls before the heartbeat was due, want 0", got-before)
	}

	h.advance(2 * time.Second)
	h.poll(t)
	if after := len(h.recorded()); after != before+1 {
		t.Errorf("made %d calls after the heartbeat interval, want 1", after-before)
	}
}

// The keep-alive exists to beat the server's stale clock, so it has to track
// stale_timeout rather than sit at a fixed interval. Pinning it would burn
// pushes on a long quiet backup and, on a short stale_timeout, would stop
// protecting the activity at all.
func TestHeartbeatDerivesFromStaleTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.PushWard.StaleTimeout = 40 * time.Minute
	h := newHarness(t, cfg, &fakeBackrest{})
	if got, want := h.p.heartbeat(), 20*time.Minute; got != want {
		t.Errorf("heartbeat = %v, want %v (half of stale_timeout)", got, want)
	}

	// Below the floor the derived value would push far too often.
	cfg.PushWard.StaleTimeout = 20 * time.Second
	h = newHarness(t, cfg, &fakeBackrest{})
	if got := h.p.heartbeat(); got != heartbeatFloor {
		t.Errorf("heartbeat = %v, want the %v floor", got, heartbeatFloor)
	}
}

func TestLiveProgressAnchorIsSent(t *testing.T) {
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, 1000)},
		{runningBackup(1, 100, 1000)},
		{runningBackup(1, 200, 1000)},
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	h.advance(10 * time.Second)
	h.poll(t)

	calls := h.recorded()
	last := content(t, calls[len(calls)-1])
	if last.LiveProgress == nil || !*last.LiveProgress {
		t.Fatalf("live_progress = %v, want true", last.LiveProgress)
	}
	if last.EndDate == nil {
		t.Fatal("end_date is missing, so there is nothing for iOS to animate toward")
	}
	if *last.EndDate <= h.clock.Unix() {
		t.Errorf("end_date %d is not in the future (now %d)", *last.EndDate, h.clock.Unix())
	}
}

// live_progress is off until a rate can be measured: one sample is a byte
// count, not a speed.
func TestNoAnchorBeforeTwoSamples(t *testing.T) {
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, 1000)},
		{runningBackup(1, 100, 1000)},
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)

	calls := h.recorded()
	seed := content(t, calls[1])
	if seed.EndDate != nil {
		t.Errorf("end_date = %d on the first frame, want none", *seed.EndDate)
	}
}

// Re-sending the anchor restarts the client-side animation, so an estimate that
// only wobbles must not reach the payload.
//
// The third sample is deliberately a little slower than the second (9000 bytes
// where the previous tick moved 10000). That shifts the raw estimate by a few
// seconds - enough to change the end date if it were recomputed blindly, and
// well inside the drift the threshold is there to absorb.
func TestWobblingRateKeepsItsAnchor(t *testing.T) {
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, 100_000)},
		{runningBackup(1, 10_000, 100_000)},
		{runningBackup(1, 20_000, 100_000)},
		{runningBackup(1, 29_000, 100_000)},
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	h.advance(10 * time.Second)
	h.poll(t)
	first := endDateOfLast(t, h)

	h.advance(10 * time.Second)
	h.poll(t)
	second := endDateOfLast(t, h)

	if first == 0 || second == 0 {
		t.Fatalf("expected an anchor on both ticks, got %d and %d", first, second)
	}
	if first != second {
		t.Errorf("anchor moved from %d to %d on a wobble, want it held", first, second)
	}
}

// A real slowdown is a different matter: the old anchor would have the bar
// finishing long before the backup does, so it has to be replaced.
func TestRealSlowdownMovesTheAnchor(t *testing.T) {
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, 100_000)},
		{runningBackup(1, 10_000, 100_000)},
		{runningBackup(1, 20_000, 100_000)},
		// A tenth of the previous tick's throughput.
		{runningBackup(1, 21_000, 100_000)},
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	h.advance(10 * time.Second)
	h.poll(t)
	first := endDateOfLast(t, h)

	h.advance(10 * time.Second)
	h.poll(t)
	second := endDateOfLast(t, h)

	if first == 0 || second == 0 {
		t.Fatalf("expected an anchor on both ticks, got %d and %d", first, second)
	}
	if second <= first {
		t.Errorf("anchor went from %d to %d after a 10x slowdown, want it pushed out", first, second)
	}
}

// slowSampleGap is how far the clock moves between the two samples. The frame
// carrying a rejected estimate has no anchor to push on, so it only goes out
// once the heartbeat is due - half of the 30m stale_timeout in testConfig.
const slowSampleGap = time.Hour

// slowTerabyteBackup is the case that made the ceiling configurable: 1.5 TB
// moving at ~9 MB/s, which estimates out to roughly two days.
func slowTerabyteBackup() *fakeBackrest {
	const total = 1_500_000_000_000
	const rate = 9_000_000 // bytes per second
	moved := rate * int64(slowSampleGap/time.Second)
	return &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, total)},
		{runningBackup(1, 0, total)},
		{runningBackup(1, moved, total)},
	}}
}

// A two-day estimate has to reach the phone. The 12h ceiling this replaced
// rejected it outright, so the user's real backup showed a static bar and no
// countdown for its whole run - on exactly the backups where the remaining time
// is worth the most.
func TestLongBackupStillGetsACountdown(t *testing.T) {
	cfg := testConfig()
	h := newHarness(t, cfg, slowTerabyteBackup())

	h.poll(t) // priming
	h.poll(t) // create + seed, first sample
	h.advance(slowSampleGap)
	h.poll(t)

	end := endDateOfLast(t, h)
	if end == 0 {
		t.Fatal("no end_date on a two-day estimate, so iOS has nothing to count down")
	}
	eta := time.Duration(end-h.clock.Unix()) * time.Second
	// Guards the fixture as much as the code: an estimate that happened to land
	// under 12h would pass this test against the old constant too.
	if eta <= 12*time.Hour {
		t.Fatalf("eta = %v, want a fixture that sits past the old 12h ceiling", eta)
	}
	if eta > cfg.Render.MaxETA {
		t.Errorf("eta = %v, beyond the configured max of %v", eta, cfg.Render.MaxETA)
	}
}

// The ceiling still has to bite. Past it the estimate is noise - a backup that
// stalls in its first minutes produces one of any size - and an end date that
// far out is worse than none.
func TestETAPastMaxIsNotAnchored(t *testing.T) {
	cfg := testConfig()
	cfg.Render.MaxETA = time.Hour
	h := newHarness(t, cfg, slowTerabyteBackup())

	h.poll(t)
	h.poll(t)
	h.advance(slowSampleGap)
	h.poll(t)

	frames := 0
	for i, c := range h.recorded() {
		if c.Method == "POST" {
			continue
		}
		frames++
		if got := content(t, c); got.EndDate != nil {
			t.Errorf("call %d carried end_date %d for an estimate past max_eta", i, *got.EndDate)
		}
	}
	// Without a frame after the second sample there was never an estimate to
	// reject and the assertion above proves nothing.
	if frames < 2 {
		t.Fatalf("got %d frames, want the seed plus the tick that had a rate to work from", frames)
	}
}

func endDateOfLast(t *testing.T, h *harness) int64 {
	t.Helper()
	calls := h.recorded()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Method == "POST" {
			continue
		}
		c := content(t, calls[i])
		if c.EndDate != nil {
			return *c.EndDate
		}
		return 0
	}
	return 0
}

func TestCompletionFrameIsGreenAndFull(t *testing.T) {
	running := runningBackup(12208, 500, 1000)
	done := loadOp(t, "backup_success.json")
	done.ID = 12208
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(12208, 0, 1000)},
		{running},
		{done},
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	h.poll(t)

	calls := h.recorded()
	final := content(t, calls[len(calls)-1])
	if final.Progress != 1.0 {
		t.Errorf("progress = %v, want 1", final.Progress)
	}
	if final.AccentColor != pushward.ColorGreen {
		t.Errorf("accent_color = %q, want green", final.AccentColor)
	}
	if !strings.HasPrefix(final.State, stateComplete) {
		t.Errorf("state = %q, want it to start with %q", final.State, stateComplete)
	}
	// The closing report is the interesting half: what was stored, and how long
	// it took.
	if !strings.Contains(final.State, "files") {
		t.Errorf("state = %q, want the file count", final.State)
	}
}

// Merge-patch preserves omitted fields, so a completion frame that simply left
// live_progress out would inherit true from the last running tick and keep
// animating a bar that has stopped.
func TestCompletionTurnsLiveProgressOff(t *testing.T) {
	done := loadOp(t, "backup_success.json")
	done.ID = 1
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, 1000)},
		{runningBackup(1, 100, 1000)},
		{runningBackup(1, 200, 1000)},
		{done},
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	h.advance(10 * time.Second)
	h.poll(t)
	h.poll(t)

	calls := h.recorded()
	final := content(t, calls[len(calls)-1])
	if final.LiveProgress == nil {
		t.Fatal("live_progress is absent from the completion frame, so the running frame's true survives")
	}
	if *final.LiveProgress {
		t.Error("live_progress = true on a finished backup")
	}
}

// A backup that died at 94% and one that never started are different events.
// Snapping the bar back to zero would render them identically.
func TestFailedBackupKeepsItsProgress(t *testing.T) {
	failed := loadOp(t, "backup_error_partial.json")
	failed.ID = 1
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, 1000)},
		{runningBackup(1, 100, 1000)},
		{failed},
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	h.poll(t)

	calls := h.recorded()
	final := content(t, calls[len(calls)-1])
	if final.AccentColor != pushward.ColorRed {
		t.Errorf("accent_color = %q, want red", final.AccentColor)
	}
	if final.Progress < 0.9 {
		t.Errorf("progress = %v, want it left where restic stopped (~0.95)", final.Progress)
	}
}

// The list of files restic could not read is the actionable half of a warning,
// and a single state line cannot hold it.
func TestPerFileErrorsBecomeALogView(t *testing.T) {
	warned := loadOp(t, "backup_warning_errors.json")
	warned.ID = 1
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, 1000)},
		{runningBackup(1, 100, 1000)},
		{warned},
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	h.poll(t)

	calls := h.recorded()
	final := content(t, calls[len(calls)-1])
	if final.Template != pushward.TemplateLog {
		t.Fatalf("template = %q, want %q", final.Template, pushward.TemplateLog)
	}
	if len(final.Lines) != 2 {
		t.Fatalf("got %d log lines, want 2", len(final.Lines))
	}
	if !strings.Contains(final.Lines[0].Text, "session.db") {
		t.Errorf("lines[0] = %q, want the unreadable file", final.Lines[0].Text)
	}
	if final.Lines[0].Level != pushward.LogError {
		t.Errorf("lines[0].level = %q, want error", final.Lines[0].Level)
	}
	if final.AccentColor != pushward.ColorOrange {
		t.Errorf("accent_color = %q, want orange for a warning", final.AccentColor)
	}
	// The warning still finished a snapshot, and its state line has to say so:
	// "Complete (warnings)" on its own reads as a run that stored nothing.
	if !strings.Contains(final.State, "files") {
		t.Errorf("state = %q, want the warning to carry what it stored", final.State)
	}
}

// Prune has no percent-done anywhere in the protocol, so it renders as its log
// rather than a fabricated bar.
func TestPruneRendersAsLog(t *testing.T) {
	prune := loadOp(t, "prune_success.json")
	prune.ID = 900
	prune.Status = backrest.StatusInProgress

	// Two lines older than the fixture's own, which stops at the 20 non-blank
	// ones the template takes: on the fixture alone nothing is ever trimmed, so
	// a ceiling that had stopped working would read as one that still does.
	older := "unlocking repository\nchecking for stale locks\n"

	br := &fakeBackrest{
		windows: [][]backrest.Operation{{}, {prune}},
		logs:    map[string]string{prune.OutputLogref(): older + loadText(t, "prune_output.log")},
	}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)

	calls := h.recorded()
	seed := content(t, calls[1])
	if seed.Template != pushward.TemplateLog {
		t.Fatalf("template = %q, want %q", seed.Template, pushward.TemplateLog)
	}
	if seed.State != statePruning {
		t.Errorf("state = %q, want %q", seed.State, statePruning)
	}
	if seed.Progress != 0 {
		t.Errorf("progress = %v, want 0 - prune reports none", seed.Progress)
	}
	// A literal, not maxLogLines: comparing against the constant the renderer
	// reads would pass at whatever ceiling it was given, including one the
	// server turns the frame down for.
	if len(seed.Lines) != 20 {
		t.Fatalf("got %d lines, want the template's 20-line ceiling", len(seed.Lines))
	}
	// The tail is what matters, newest first.
	if seed.Lines[0].Text != "done" {
		t.Errorf("lines[0] = %q, want the newest line first", seed.Lines[0].Text)
	}
	for _, l := range seed.Lines {
		if strings.Contains(l.Text, "stale locks") {
			t.Errorf("the oldest line survived the trim: %q", l.Text)
		}
	}
}

// Neither a prune nor a check has a bar to animate, and their frames are
// merge-patched onto whatever the last tick sent. Leaving live_progress out
// would have a repo task started right after a backup inherit that backup's
// true and animate a bar pinned at zero.
func TestRunningRepoTaskTurnsLiveProgressOff(t *testing.T) {
	prune := loadOp(t, "prune_success.json")
	prune.ID = 900
	prune.Status = backrest.StatusInProgress

	br := &fakeBackrest{
		windows: [][]backrest.Operation{{}, {prune}},
		logs:    map[string]string{prune.OutputLogref(): loadText(t, "prune_output.log")},
	}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)

	seed := content(t, h.recorded()[1])
	if seed.LiveProgress == nil {
		t.Fatal("live_progress is absent from a running prune, so a previous frame's true survives")
	}
	if *seed.LiveProgress {
		t.Error("live_progress = true on a prune, which reports no progress at all")
	}
}

// A running prune is polled every few seconds while its log barely moves, so
// the tail is cached between refreshes.
func TestRunningPruneCachesItsLog(t *testing.T) {
	prune := loadOp(t, "prune_success.json")
	prune.ID = 900
	prune.Status = backrest.StatusInProgress

	br := &fakeBackrest{
		windows: [][]backrest.Operation{{}, {prune}, {prune}, {prune}},
		logs:    map[string]string{prune.OutputLogref(): loadText(t, "prune_output.log")},
	}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	fetched := br.logCallCount()

	h.advance(5 * time.Second)
	h.poll(t)
	h.advance(5 * time.Second)
	h.poll(t)

	if got := br.logCallCount(); got != fetched {
		t.Errorf("re-fetched the log %d times inside the refresh window, want 0", got-fetched)
	}

	// A literal, not logRefreshInterval: advancing by the constant the code reads
	// would pass at any value it took, including one short enough to re-fetch on
	// every tick.
	h.advance(15 * time.Second)
	h.poll(t)
	if got := br.logCallCount(); got != fetched+1 {
		t.Errorf("fetched %d times after the refresh interval, want 1", got-fetched)
	}
}

// Reading the log is the only way to learn whether a prune's frame has anything
// new on it, so the read happens whether or not it turns into a frame. What must
// not follow is a frame per read: a prune sits on the same output for minutes at
// a time, and each frame is a server write and an APNs update for a card nobody
// has changed.
func TestRunningPruneDoesNotResendAnUnchangedLog(t *testing.T) {
	prune := loadOp(t, "prune_success.json")
	prune.ID = 900
	prune.Status = backrest.StatusInProgress

	br := &fakeBackrest{
		windows: [][]backrest.Operation{{}, {prune}},
		logs:    map[string]string{prune.OutputLogref(): loadText(t, "prune_output.log")},
	}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	for elapsed := time.Duration(0); elapsed < time.Minute; elapsed += h.p.cfg.Polling.Interval {
		h.poll(t)
		h.advance(h.p.cfg.Polling.Interval)
	}

	// One read per 15s of the minute. Written as a count rather than derived
	// from logRefreshInterval, so shortening that constant fails here instead of
	// moving the bar along with it.
	if got := br.logCallCount(); got != 4 {
		t.Errorf("read the log %d times in a minute, want one per refresh interval", got)
	}
	if got := h.pushCount(); got != 1 {
		t.Errorf("sent %d frames for output that never changed, want the seed alone", got)
	}
}

// The log is the only thing on a prune's card that moves, so a changed tail has
// to reach the Lock Screen on the log's own cadence. Pacing the read by the
// keep-alive instead works out at 15 minutes on the default 30m stale_timeout,
// by which time most prunes are over.
func TestRunningPruneLogChangeDoesNotWaitForTheHeartbeat(t *testing.T) {
	prune := loadOp(t, "prune_success.json")
	prune.ID = 900
	prune.Status = backrest.StatusInProgress

	br := &fakeBackrest{
		windows: [][]backrest.Operation{{}, {prune}},
		logs:    map[string]string{prune.OutputLogref(): loadText(t, "prune_output.log")},
	}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	br.setLog(prune.OutputLogref(), loadText(t, "prune_output.log")+"\nremoving 3 old packs\n")

	// The worst lag the design allows: a full refresh interval, plus the tick
	// that notices it has passed.
	lag := logRefreshInterval + h.p.cfg.Polling.Interval
	if lag >= h.p.heartbeat() {
		t.Fatalf("log lag %v is not below the heartbeat %v, so this proves nothing", lag, h.p.heartbeat())
	}
	for waited := time.Duration(0); waited < lag; waited += h.p.cfg.Polling.Interval {
		h.advance(h.p.cfg.Polling.Interval)
		h.poll(t)
	}

	calls := h.recorded()
	last := content(t, calls[len(calls)-1])
	if len(last.Lines) == 0 || last.Lines[0].Text != "removing 3 old packs" {
		t.Fatalf("newest line = %+v, want the line the prune wrote %v ago", last.Lines, lag)
	}
	// Landing it must not mean sending on every tick of that window.
	if got := h.pushCount(); got != 2 {
		t.Errorf("sent %d frames, want the seed and the one the changed tail earned", got)
	}
}

// A Backrest that will not hand the log over must not be asked on every tick,
// which is what happens if the read clock only moves on a successful answer -
// and it happens exactly when the host is least able to answer.
func TestFailingLogFetchStaysOnTheRefreshCadence(t *testing.T) {
	prune := loadOp(t, "prune_success.json")
	prune.ID = 900
	prune.Status = backrest.StatusInProgress

	br := &fakeBackrest{
		windows: [][]backrest.Operation{{}, {prune}},
		logs:    map[string]string{prune.OutputLogref(): loadText(t, "prune_output.log")},
		logErr:  context.DeadlineExceeded,
	}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	for elapsed := time.Duration(0); elapsed < time.Minute; elapsed += h.p.cfg.Polling.Interval {
		h.poll(t)
		h.advance(h.p.cfg.Polling.Interval)
	}

	// The same one read per 15s a log that answers gets, as a literal for the
	// same reason.
	if got := br.logCallCount(); got != 4 {
		t.Errorf("re-asked for the failing log %d times in a minute, want one per refresh interval", got)
	}
	// The tail never changes because it never arrives, so nothing after the seed
	// has anything to say.
	if got := h.pushCount(); got != 1 {
		t.Errorf("sent %d frames off a failing log, want the seed alone", got)
	}
}

// A prune with logs turned off has no tail to change, so the keep-alive is the
// only thing left that can speak for it. It still has to speak: without a frame
// inside the stale timeout the server ends the activity mid-prune.
func TestPruneWithoutLogsPushesOnlyOnTheHeartbeat(t *testing.T) {
	prune := loadOp(t, "prune_success.json")
	prune.ID = 900
	prune.Status = backrest.StatusInProgress

	br := &fakeBackrest{
		windows: [][]backrest.Operation{{}, {prune}},
		logs:    map[string]string{prune.OutputLogref(): loadText(t, "prune_output.log")},
	}
	cfg := testConfig()
	cfg.Render.Logs = false
	h := newHarness(t, cfg, br)

	h.poll(t)
	for elapsed := time.Duration(0); elapsed < time.Minute; elapsed += cfg.Polling.Interval {
		h.poll(t)
		h.advance(cfg.Polling.Interval)
	}
	if got := h.pushCount(); got != 1 {
		t.Fatalf("sent %d frames in a minute with logs off, want the seed alone", got)
	}

	h.advance(h.p.heartbeat())
	h.poll(t)
	if got := h.pushCount(); got != 2 {
		t.Errorf("sent %d frames once the heartbeat fell due, want the seed and one keep-alive", got)
	}
}

// Turning logs off has to skip the fetch, not just drop the lines.
func TestLogsDisabledSkipsTheFetch(t *testing.T) {
	prune := loadOp(t, "prune_success.json")
	prune.ID = 900
	prune.Status = backrest.StatusInProgress

	br := &fakeBackrest{
		windows: [][]backrest.Operation{{}, {prune}},
		logs:    map[string]string{prune.OutputLogref(): loadText(t, "prune_output.log")},
	}
	cfg := testConfig()
	cfg.Render.Logs = false
	h := newHarness(t, cfg, br)

	h.poll(t)
	h.poll(t)

	if br.logCallCount() != 0 {
		t.Errorf("fetched the log %d times with logs disabled", br.logCallCount())
	}
	seed := content(t, h.recorded()[1])
	if seed.Template != pushward.TemplateGeneric {
		t.Errorf("template = %q, want generic when there are no lines", seed.Template)
	}
}

// "no errors were found" contains the word "error". Classifying a clean check
// as a failure would be worse than not classifying it at all.
func TestCleanCheckLineIsNotAnError(t *testing.T) {
	lines := outputLines(loadText(t, "check_output.log"))
	if len(lines) == 0 {
		t.Fatal("no lines produced")
	}
	if lines[0].Text != "no errors were found" {
		t.Fatalf("lines[0] = %q, want the newest line", lines[0].Text)
	}
	if lines[0].Level != pushward.LogInfo {
		t.Errorf("lines[0].level = %q, want info", lines[0].Level)
	}
}

func TestLogLineClassification(t *testing.T) {
	cases := map[string]string{
		"no errors were found":             pushward.LogInfo,
		"Fatal: unable to save snapshot":   pushward.LogError,
		"check failed for pack 0a1b":       pushward.LogError,
		"Save(<lock/x>) retrying after 1s": pushward.LogWarn,
		"loading all snapshots...":         pushward.LogInfo,
	}
	for line, want := range cases {
		if got := lineLevel(line); got != want {
			t.Errorf("lineLevel(%q) = %q, want %q", line, got, want)
		}
	}
}

// The state lines are a cross-bridge contract, not a display preference: the
// relay's own backrest provider can be pointed at the same PushWard account, and
// a check reported as "Check Passed" by one and "Check passed" by the other
// reads as two different events.
//
// Every string here is a literal on purpose. Comparing against the constants the
// renderer reads would pass whatever wording they were given.
func TestStateWordingIsPinnedToTheRelayProvider(t *testing.T) {
	backupOf := func(s backrest.Status) *backrest.Operation {
		return &backrest.Operation{Status: s, Backup: &backrest.OperationBackup{}}
	}
	pruneOf := func(s backrest.Status) *backrest.Operation {
		return &backrest.Operation{Status: s, Prune: &backrest.OperationPrune{}}
	}
	checkOf := func(s backrest.Status) *backrest.Operation {
		return &backrest.Operation{Status: s, Check: &backrest.OperationCheck{}}
	}

	ended := []struct {
		op   *backrest.Operation
		want string
	}{
		{pruneOf(backrest.StatusSuccess), "Pruned"},
		{pruneOf(backrest.StatusError), "Prune Failed"},
		{checkOf(backrest.StatusSuccess), "Check Passed"},
		{checkOf(backrest.StatusError), "Check Failed"},
		{backupOf(backrest.StatusSuccess), "Complete"},
		{backupOf(backrest.StatusWarning), "Complete (warnings)"},
		{backupOf(backrest.StatusUserCancelled), "Cancelled"},
		{backupOf(backrest.StatusSystemCancelled), "Cancelled"},
		{backupOf(backrest.StatusError), "Failed"},
	}
	for _, tc := range ended {
		if got := endStateText(tc.op); got != tc.want {
			t.Errorf("endStateText(%s %s) = %q, want %q", tc.op.Kind(), tc.op.Status, got, tc.want)
		}
	}

	// The lines a frame carries while the work is still in flight.
	if got := repoTaskContent(pruneOf(backrest.StatusInProgress), nil).State; got != "Pruning..." {
		t.Errorf("running prune state = %q, want %q", got, "Pruning...")
	}
	if got := repoTaskContent(checkOf(backrest.StatusInProgress), nil).State; got != "Checking..." {
		t.Errorf("running check state = %q, want %q", got, "Checking...")
	}
	if got := backupRunningState(backupOf(backrest.StatusInProgress), 0); got != "Scanning..." {
		t.Errorf("pre-scan backup state = %q, want %q", got, "Scanning...")
	}
	if got := orphanContent("Backrest", 0).State; got != "Interrupted" {
		t.Errorf("orphan state = %q, want %q", got, "Interrupted")
	}
}

// Prune and check are the two outcomes resolved by kind rather than by status,
// and getting the pass/fail pairing backwards would report a clean prune as a
// failure. Driven end to end, because the renderer is only half of it: the
// bridge also has to reach these two through handleTerminal at all.
func TestFinishedRepoTasksRenderTheirOwnOutcome(t *testing.T) {
	prune := loadOp(t, "prune_success.json")
	prune.ID = 900
	check := loadOp(t, "check_error.json")
	check.ID = 901

	br := &fakeBackrest{
		windows: [][]backrest.Operation{{}, {prune, check}},
		logs: map[string]string{
			prune.OutputLogref(): loadText(t, "prune_output.log"),
			check.OutputLogref(): loadText(t, "check_output.log"),
		},
	}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)

	frames := make(map[string]pushward.Content)
	for _, c := range h.recorded() {
		if c.Method != "PATCH" {
			continue
		}
		frames[strings.TrimPrefix(c.Path, "/activities/")] = content(t, c)
	}

	pruned, ok := frames[slugFor(&prune)]
	if !ok {
		t.Fatalf("no frame for the finished prune, got %d frames", len(frames))
	}
	if pruned.State != "Pruned" {
		t.Errorf("successful prune state = %q, want %q", pruned.State, "Pruned")
	}
	if pruned.AccentColor != pushward.ColorGreen {
		t.Errorf("successful prune accent_color = %q, want green", pruned.AccentColor)
	}

	failed, ok := frames[slugFor(&check)]
	if !ok {
		t.Fatalf("no frame for the finished check, got %d frames", len(frames))
	}
	if failed.State != "Check Failed" {
		t.Errorf("failed check state = %q, want %q", failed.State, "Check Failed")
	}
	if failed.AccentColor != pushward.ColorRed {
		t.Errorf("failed check accent_color = %q, want red", failed.AccentColor)
	}
	// A check that failed is worth reading, so its output is the frame.
	if failed.Template != pushward.TemplateLog {
		t.Errorf("failed check template = %q, want %q", failed.Template, pushward.TemplateLog)
	}
}

// An operation that started and finished inside one poll interval never shows
// as running, but its outcome is still the part worth seeing.
func TestOutcomeMissedBetweenPollsStillShows(t *testing.T) {
	done := loadOp(t, "backup_success.json")
	done.ID = 777
	br := &fakeBackrest{windows: [][]backrest.Operation{{}, {done}}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)

	calls := h.recorded()
	if len(calls) < 2 {
		t.Fatalf("got %d calls, want the activity to be created and seeded", len(calls))
	}
	if calls[0].Method != "POST" {
		t.Errorf("first call = %s, want the create", calls[0].Method)
	}
	final := content(t, calls[1])
	if !strings.HasPrefix(final.State, stateComplete) {
		t.Errorf("state = %q, want the completion line", final.State)
	}
}

// A terminal row stays in the query window long after its activity has been
// closed. The two-phase end deletes the tracking entry once it finishes, so
// from that moment the row looks untracked again - and without the done set
// every later poll would open a second activity for the same backup.
//
// The wait matters: poll again before phase 2 lands and the `ending` flag
// hides the bug.
func TestTerminalRowIsAnnouncedOnce(t *testing.T) {
	done := loadOp(t, "backup_success.json")
	done.ID = 777
	br := &fakeBackrest{windows: [][]backrest.Operation{{}, {done}, {done}, {done}}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	waitForEnded(t, h, 0)
	waitForUntracked(t, h, 777)
	after := len(h.recorded())

	h.poll(t)
	h.poll(t)

	if got := len(h.recorded()); got != after {
		t.Errorf("made %d further calls for the same finished operation, want 0", got-after)
	}
}

// The done set is what keeps a terminal row from being announced twice, and it
// grows by one entry per operation the bridge ever sees. A row that has scrolled
// out of the query window cannot come back, so its entry is a slow leak on a
// process meant to run for months.
func TestBookkeepingIsDroppedOnceARowLeavesTheWindow(t *testing.T) {
	old := finishedBackup(t, 777, 24*time.Hour)
	br := &fakeBackrest{windows: [][]backrest.Operation{{old}, {}}}
	h := newHarness(t, testConfig(), br)

	h.poll(t) // priming records it as history
	h.p.mu.Lock()
	recorded := len(h.p.done)
	h.p.mu.Unlock()
	if recorded != 1 {
		t.Fatalf("the done set holds %d entries after priming, want the one row it saw", recorded)
	}

	h.poll(t) // the row has aged out of the query window

	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	if left := len(h.p.done); left != 0 {
		t.Errorf("the done set still holds %d entries for a row that is gone, want 0", left)
	}
}

// waitForUntracked blocks until the two-phase end has dropped its bookkeeping,
// which is the state in which a re-announcement would happen.
func waitForUntracked(t *testing.T, h *harness, opID int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.p.mu.Lock()
		_, still := h.p.tracked[opID]
		h.p.mu.Unlock()
		if !still {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the tracking entry was never released")
}

// Giving up on an activity marks its operation done, and a done operation is
// never looked at again - so a run of rejected frames costs the outcome as well
// as the frames. A PushWard blip must not be what decides that, or a backup that
// finished during it is left on the Lock Screen mid-flight until the server ages
// it out.
func TestShortOutageDoesNotBuryACompletedBackup(t *testing.T) {
	done := loadOp(t, "backup_success.json")
	done.ID = 777

	cfg := testConfig()
	// Two minutes of a real outage, not a count of attempts: the attempts a
	// window that long buys depend on how often this operation had cause to send.
	ticks := int(2 * time.Minute / cfg.Polling.Interval)

	br := &fakeBackrest{windows: [][]backrest.Operation{{}, {done}}}
	h := newRejectingHarness(t, cfg, br, ticks)

	h.poll(t) // priming, on an empty window
	for i := 0; i < ticks; i++ {
		h.poll(t)
		h.advance(cfg.Polling.Interval)
	}
	h.poll(t) // the outage clears

	calls := waitForEnded(t, h, 0)
	final := content(t, calls[len(calls)-1])
	if !strings.HasPrefix(final.State, stateComplete) {
		t.Errorf("state = %q, want the outcome the outage held up", final.State)
	}
}

// The window measures one unbroken run of rejections, so a frame that lands has
// to clear it. Left running, two unrelated blips ten minutes apart add up to a
// give-up on an activity that was healthy for the whole stretch between them.
func TestALandedFrameClearsTheFailureClock(t *testing.T) {
	cfg := testConfig()
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{},
		{runningBackup(1, 10_000_000, 100_000_000)},
		{runningBackup(1, 20_000_000, 100_000_000)},
	}}
	h := newRejectingHarness(t, cfg, br, 1) // only the first frame is turned down

	h.poll(t) // priming
	h.poll(t) // rejected
	h.advance(cfg.Polling.Interval)
	h.poll(t) // lands

	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	tr, ok := h.p.tracked[1]
	if !ok {
		t.Fatal("the operation was dropped, so the rejection was never survived at all")
	}
	if !tr.failingSince.IsZero() {
		t.Errorf("the failure clock still reads %v after a frame landed", tr.failingSince)
	}
}

// A frame the server turned down was never delivered, so nothing about it may be
// recorded as sent. Here the rejected frame is the seed itself, so the whole
// record has to stay clear: a phase, a progress or a push stamp left behind by an
// activity that was never created has the next tick comparing its frame against
// one the server has never seen.
func TestARejectedFrameIsNotRecordedAsSent(t *testing.T) {
	cfg := testConfig()
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{},
		{runningBackup(1, 300, 1000)},
	}}
	h := newRejectingHarness(t, cfg, br, 1)

	h.poll(t) // priming
	h.poll(t) // the frame the server turns down

	h.p.mu.Lock()
	tr, ok := h.p.tracked[1]
	h.p.mu.Unlock()
	if !ok {
		t.Fatal("the operation was dropped, so a single rejection ended it")
	}
	if tr.seeded {
		t.Error("the activity is marked seeded off a frame that never landed")
	}
	if tr.lastPhase != "" || tr.lastProgress != 0 || !tr.lastPushAt.IsZero() {
		t.Errorf("a rejected frame was recorded as sent: phase=%q progress=%v at=%v",
			tr.lastPhase, tr.lastProgress, tr.lastPushAt)
	}
}

// The same rule once the activity is up, where it is visible from the outside: a
// rejected patch recorded as sent looks like a repeat to the throttle, so the
// retry is dropped and the card stays frozen on the frame before it until the
// heartbeat falls due - half of stale_timeout, 15 minutes on the defaults.
func TestARejectedUpdateIsSentAgainOnTheNextTick(t *testing.T) {
	cfg := testConfig()
	// The live-progress anchor is not what this is about, and left on it moves
	// enough between ticks to re-send the frame on its own account.
	cfg.Render.LiveProgress = false

	br := &fakeBackrest{windows: [][]backrest.Operation{
		{},
		{runningBackup(1, 100, 1000)},
		{runningBackup(1, 300, 1000)},
	}}
	// The seed lands, so the throttle has a frame to compare against; the one
	// after it is turned down.
	srv, calls, mu := gatedServer(t, func(n int) int {
		if n == 2 {
			return http.StatusBadRequest
		}
		return http.StatusOK
	})
	h := wire(t, cfg, br, srv, calls, mu)

	h.poll(t) // priming
	h.poll(t) // the seed
	h.advance(cfg.Polling.Interval)
	h.poll(t) // the frame the server turns down
	h.advance(cfg.Polling.Interval)
	h.poll(t) // the same frame again, which the phone has still never seen

	if got := h.pushCount(); got != 3 {
		t.Errorf("sent %d frames, want the rejected one sent again (3)", got)
	}
}

// The limit still has to bite. An activity the server will never accept holds
// its entry forever otherwise, and interval() reads that entry as live work and
// keeps the poll loop at its fast rate for as long as the row stays in the query
// window - over a week on a nightly-backup instance.
func TestPersistentRejectionIsEventuallyAbandoned(t *testing.T) {
	done := loadOp(t, "backup_success.json")
	done.ID = 777

	cfg := testConfig()
	br := &fakeBackrest{windows: [][]backrest.Operation{{}, {done}}}
	h := newRejectingHarness(t, cfg, br, 1_000_000)

	h.poll(t) // priming
	h.poll(t) // the rejection that starts the clock
	// A literal, not maxSendFailureWindow: advancing by the constant the code
	// reads would pass at any value it was given, including one so long that the
	// entry it exists to release is held for days.
	h.advance(11 * time.Minute)
	h.poll(t) // the tick that crosses the window
	after := len(h.recorded())

	h.poll(t)
	h.poll(t)
	if got := len(h.recorded()); got != after {
		t.Errorf("made %d further calls for an activity the server refuses, want 0", got-after)
	}
	if got := h.p.interval(); got != cfg.Polling.IdleInterval {
		t.Errorf("poll interval = %v, want the idle rate back once nothing is tracked", got)
	}
}

// An operation that leaves the window without ever being seen finished would
// otherwise show as running until the server's stale timeout.
//
// It closes as "Interrupted" in orange, not "Failed" in red: nothing is known
// to have gone wrong, the bridge just lost sight of it. The bar stays where it
// stopped, and the subtitle survives so the card still says which plan it was.
func TestOperationLeavingTheWindowIsClosed(t *testing.T) {
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, 1000)},
		{runningBackup(1, 300, 1000)},
		{},
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	before := len(h.recorded())
	h.poll(t)

	calls := waitForEnded(t, h, before)
	final := content(t, calls[len(calls)-1])

	if final.State != stateLostTrack {
		t.Errorf("state = %q, want %q", final.State, stateLostTrack)
	}
	if final.AccentColor != pushward.ColorOrange {
		t.Errorf("accent_color = %q, want orange", final.AccentColor)
	}
	if final.Progress != 0.3 {
		t.Errorf("progress = %v, want the 0.3 it reached", final.Progress)
	}
	if final.Subtitle == "" {
		t.Error("subtitle is empty, so the card cannot say which plan stalled")
	}
	if final.LiveProgress == nil || *final.LiveProgress {
		t.Error("live_progress is not switched off on the closing frame")
	}
}

func TestTwoPhaseEndReachesEnded(t *testing.T) {
	done := loadOp(t, "backup_success.json")
	done.ID = 1
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, 1000)},
		{runningBackup(1, 100, 1000)},
		{done},
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	before := len(h.recorded())
	h.poll(t)

	calls := waitForEnded(t, h, before)

	// Phase 1 re-sends the outcome as ONGOING so it lands on the Dynamic
	// Island; phase 2 is what dismisses it.
	var states []string
	for _, c := range calls[before:] {
		if c.Method == "PATCH" {
			states = append(states, activityState(t, c))
		}
	}
	if len(states) < 2 {
		t.Fatalf("got states %v, want an ongoing frame before the ended one", states)
	}
	if states[len(states)-1] != pushward.StateEnded {
		t.Errorf("final state = %q, want ended", states[len(states)-1])
	}
	if states[len(states)-2] != pushward.StateOngoing {
		t.Errorf("penultimate state = %q, want ongoing", states[len(states)-2])
	}
}

func waitForEnded(t *testing.T, h *harness, from int) []testutil.APICall {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		calls := h.recorded()
		for _, c := range calls[from:] {
			if c.Method == "PATCH" && activityState(t, c) == pushward.StateEnded {
				return calls
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no ENDED frame arrived within the timeout")
	return nil
}

// The relay's Backrest provider can be pointed at the same account. If the two
// hashed the same input they would overwrite each other's frames.
func TestSlugDiffersFromTheRelayScheme(t *testing.T) {
	op := &backrest.Operation{PlanID: "appdata", RepoID: "nas", Backup: &backrest.OperationBackup{}}
	got := slugFor(op)

	// The relay hashes plan and repo concatenated with no separator, under the
	// same prefix and hash width. Comparing the two hash inputs would only
	// restate how this test builds them; comparing the slugs themselves is what
	// the collision is.
	if relay := text.SlugHash("backrest", op.PlanID+op.RepoID, 4); got == relay {
		t.Errorf("slug = %q, which is what the relay's backrest provider would publish to", got)
	}
	if !strings.HasPrefix(got, "backrest-") {
		t.Errorf("slug = %q, want the backrest- prefix", got)
	}
}

// A prune starting while a backup is still closing out must not replace it.
func TestKindSeparatesSlugs(t *testing.T) {
	backup := &backrest.Operation{PlanID: "_system_", RepoID: "nas", Backup: &backrest.OperationBackup{}}
	prune := &backrest.Operation{PlanID: "_system_", RepoID: "nas", Prune: &backrest.OperationPrune{}}
	if slugFor(backup) == slugFor(prune) {
		t.Error("backup and prune on the same repo share a slug")
	}
}

func TestPollErrorIsReported(t *testing.T) {
	br := &fakeBackrest{err: context.DeadlineExceeded}
	h := newHarness(t, testConfig(), br)
	if err := h.p.poll(context.Background()); err == nil {
		t.Fatal("poll returned nil, want the upstream error")
	}
}

// Polling has to speed up while something is running and slow down when it is
// not, or an idle Backrest is asked every 5s whether tonight's backup started.
func TestIntervalFollowsActivity(t *testing.T) {
	cfg := testConfig()
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{},
		{runningBackup(1, 100, 1000)},
	}}
	h := newHarness(t, cfg, br)

	h.poll(t)
	if got := h.p.interval(); got != cfg.Polling.IdleInterval {
		t.Errorf("idle interval = %v, want %v", got, cfg.Polling.IdleInterval)
	}

	h.poll(t)
	if got := h.p.interval(); got != cfg.Polling.Interval {
		t.Errorf("active interval = %v, want %v", got, cfg.Polling.Interval)
	}
}

// An operation in its two-phase close is not live work: the frames left to send
// are on their own timers and no poll will produce another. Counting it holds
// the loop at the fast rate through both phases, and through the whole
// end_display_time between them.
func TestIntervalDropsToIdleWhileAnActivityCloses(t *testing.T) {
	cfg := testConfig()
	// Long enough that the close is still pending when the interval is read.
	cfg.PushWard.EndDelay = time.Minute

	done := loadOp(t, "backup_success.json")
	done.ID = 777
	br := &fakeBackrest{windows: [][]backrest.Operation{{}, {done}}}
	h := newHarness(t, cfg, br)
	defer h.p.shutdown()

	h.poll(t)
	h.poll(t)

	h.p.mu.Lock()
	tr, ok := h.p.tracked[777]
	closing := ok && tr.ending
	h.p.mu.Unlock()
	if !closing {
		t.Fatal("the operation is not mid-close, so the interval it reports proves nothing")
	}

	if got := h.p.interval(); got != cfg.Polling.IdleInterval {
		t.Errorf("interval = %v with nothing tracked but a closing activity, want the idle rate %v",
			got, cfg.Polling.IdleInterval)
	}
}

// Run is the whole process loop: it has to keep polling on its own timer and,
// when its context goes, hand off to shutdown rather than returning while end
// timers are still armed. main returns from Run straight into process exit, so a
// timer left running is a frame with nobody left to send it.
func TestRunPollsUntilItsContextIsCancelled(t *testing.T) {
	cfg := testConfig()
	cfg.Polling.Interval = 2 * time.Millisecond
	cfg.Polling.IdleInterval = 2 * time.Millisecond
	// Long enough that the close is still only armed when the context dies,
	// which is what makes what shutdown does with it observable at all.
	cfg.PushWard.EndDelay = 300 * time.Millisecond

	done := loadOp(t, "backup_success.json")
	done.ID = 777
	br := &fakeBackrest{windows: [][]backrest.Operation{{}, {done}}}
	h := newHarness(t, cfg, br)

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { returned <- h.p.Run(ctx) }()

	// Waiting on the frames rather than on the poll count: a window is handed
	// out before the poll has done anything with it, and cancelling in that gap
	// kills the create on its way out, leaving shutdown nothing to walk.
	waitFor(t, "the finished backup's frames", func() bool { return len(h.recorded()) >= 2 })
	cancel()

	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run never returned, so shutdown is waiting on something it did not stop")
	}

	// Returning is only half of it: the armed phase has to have been stopped
	// too, or it fires at a process that has already gone.
	settled := len(h.recorded())
	time.Sleep(2 * cfg.PushWard.EndDelay)
	if got := len(h.recorded()); got != settled {
		t.Errorf("%d frames went out after Run returned, want the end timers stopped", got-settled)
	}
}

// shutdown stops the armed phases rather than only waiting on them. Phase 1 sends
// a final ONGOING frame and arms phase 2 behind it, so one that fires as the
// process leaves puts a card on the Lock Screen with no ENDED coming for it.
func TestShutdownStopsAPendingClose(t *testing.T) {
	cfg := testConfig()
	// Long enough that phase 1 is still only armed when shutdown runs.
	cfg.PushWard.EndDelay = 200 * time.Millisecond

	done := loadOp(t, "backup_success.json")
	done.ID = 777
	br := &fakeBackrest{windows: [][]backrest.Operation{{}, {done}}}
	h := newHarness(t, cfg, br)

	h.poll(t) // priming
	h.poll(t) // the completion frame, which schedules the close

	settled := len(h.recorded())
	h.p.shutdown()
	time.Sleep(2 * cfg.PushWard.EndDelay)

	if got := len(h.recorded()); got != settled {
		t.Errorf("%d frames went out after shutdown, want the armed phase stopped", got-settled)
	}
}

// The other half of the same rule: a phase already sending must not be cut off.
// main returns from Run into process exit, so shutdown's wait is the only thing
// keeping the process alive while a closing frame is on the wire.
func TestShutdownWaitsForAFrameAlreadyInFlight(t *testing.T) {
	cfg := testConfig()
	cfg.PushWard.EndDelay = time.Millisecond

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	// httptest's Close blocks on the outstanding request, so the gate has to be
	// opened ahead of it whichever way the test leaves.
	free := func() { once.Do(func() { close(release) }) }

	// The seed is the first frame; the second is end phase 1.
	srv, calls, mu := gatedServer(t, func(n int) int {
		if n == 2 {
			close(entered)
			<-release
		}
		return http.StatusOK
	})
	t.Cleanup(free)

	done := loadOp(t, "backup_success.json")
	done.ID = 777
	br := &fakeBackrest{windows: [][]backrest.Operation{{}, {done}}}
	h := wire(t, cfg, br, srv, calls, mu)

	h.poll(t) // priming
	h.poll(t) // the completion frame, which schedules the close

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the closing frame never reached the server")
	}

	returned := make(chan struct{})
	go func() {
		h.p.shutdown()
		close(returned)
	}()

	select {
	case <-returned:
		t.Fatal("shutdown returned while a closing frame was still on the wire")
	case <-time.After(100 * time.Millisecond):
	}

	free()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown never returned once the frame had landed")
	}
}

// The two closing frames fire from timers, by which time the poll context that
// produced them is usually gone - a rollout cancels it seconds after the backup
// it was watching finished. They run on a context detached from it for that
// reason: sending on the poll context would abandon the close half-way and leave
// the card on the Lock Screen until the server ages it out.
func TestEndFramesSurviveTheirPollContext(t *testing.T) {
	done := loadOp(t, "backup_success.json")
	done.ID = 777
	br := &fakeBackrest{windows: [][]backrest.Operation{{}, {done}}}
	h := newHarness(t, testConfig(), br)

	h.poll(t) // priming

	ctx, cancel := context.WithCancel(context.Background())
	if err := h.p.poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	cancel()

	waitForEnded(t, h, 0)
}

// waitFor blocks until cond holds, so a test driving Run does not have to guess
// how long a poll takes on a loaded box.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSampleTracksStall(t *testing.T) {
	tr := &tracked{}
	base := time.Unix(1785050000, 0)
	tr.sample(0, base)
	tr.sample(1000, base.Add(10*time.Second))
	moving := tr.speed
	if moving <= 0 {
		t.Fatalf("speed = %v after a real transfer, want positive", moving)
	}

	// A counter that stops advancing is a stall, and folding its zero in is
	// what makes the ETA grow rather than freeze.
	tr.sample(1000, base.Add(20*time.Second))
	if tr.speed >= moving {
		t.Errorf("speed = %v after a stalled tick, want it below %v", tr.speed, moving)
	}
}

func TestSampleIgnoresRewind(t *testing.T) {
	tr := &tracked{}
	base := time.Unix(1785050000, 0)
	tr.sample(1000, base)
	tr.sample(2000, base.Add(10*time.Second))
	before := tr.speed

	// A counter going backwards is impossible; it must not produce a negative
	// rate and an ETA in the past.
	tr.sample(500, base.Add(20*time.Second))
	if tr.speed != before {
		t.Errorf("speed = %v after an impossible sample, want it unchanged at %v", tr.speed, before)
	}
	if tr.speed < 0 {
		t.Errorf("speed = %v, want non-negative", tr.speed)
	}
}

// A re-run that starts while the previous run is still closing shares its slug.
// Without superseding the old record, its phase 2 sends ENDED to the new run's
// card and the rest of that backup goes unreported.
func TestReRunSupersedesAClosingActivity(t *testing.T) {
	done := loadOp(t, "backup_success.json")
	done.ID, done.PlanID, done.RepoID = 1, "appdata", "nas"
	rerun := runningBackup(2, 100, 1000)

	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 500, 1000)},
		{done},        // op 1 finishes, two-phase end is armed
		{done, rerun}, // op 2 starts on the same plan, same slug
		{done, rerun},
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	h.poll(t)
	h.poll(t)

	// Give the superseded timers every chance to fire if they were not closed.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	slug := slugFor(&rerun)
	for _, c := range h.recorded() {
		if c.Method != "PATCH" || !strings.HasSuffix(c.Path, slug) {
			continue
		}
		if activityState(t, c) == pushward.StateEnded {
			t.Fatalf("the running re-run's activity was ENDED by the previous run's close-out")
		}
	}
}

// A status-less tick reports zero bytes. Folding that into the rate estimate
// would reset the counter and make the next real reading look like the whole
// backup arrived in one interval.
func TestStatuslessTickDoesNotCorruptTheRate(t *testing.T) {
	blank := runningBackup(1, 2_000_000, 10_000_000)
	blank.Backup.LastStatus = nil

	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, 10_000_000)},
		{runningBackup(1, 1_000_000, 10_000_000)},
		{runningBackup(1, 2_000_000, 10_000_000)},
		{blank},
		{runningBackup(1, 3_000_000, 10_000_000)},
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	h.advance(10 * time.Second)
	h.poll(t)
	rate := h.p.tracked[1].speed

	h.advance(10 * time.Second)
	h.poll(t) // status-less
	h.advance(10 * time.Second)
	h.poll(t)

	after := h.p.tracked[1].speed
	// The true rate never changed, so the estimate must not jump.
	if after > rate*1.25 {
		t.Errorf("rate went %.0f -> %.0f B/s across a status-less tick, want it steady", rate, after)
	}
}

// The log template takes at most 20 lines and rejects an empty one, so both
// limits are contract rather than taste - a frame that breaks either is turned
// down whole and the outcome never reaches the phone. The numbers are literals
// for that reason.
func TestErrorLinesRespectTheLogTemplateLimits(t *testing.T) {
	over := &backrest.Operation{Backup: &backrest.OperationBackup{}}
	for i := 0; i < 25; i++ {
		over.Backup.Errors = append(over.Backup.Errors, backrest.BackupProgressError{
			Item:    fmt.Sprintf("/data/live/file-%02d", i),
			During:  "archival",
			Message: "permission denied",
		})
	}
	if got := len(errorLines(over)); got != 20 {
		t.Errorf("25 per-file errors produced %d lines, want the template's 20", got)
	}

	// Backrest fills in what restic gave it, so an entry that names neither a
	// file nor a reason is possible and would render as an empty line.
	blank := &backrest.Operation{Backup: &backrest.OperationBackup{Errors: []backrest.BackupProgressError{
		{Item: "/data/live/session.db", During: "archival", Message: "permission denied"},
		{During: "archival"},
		{Item: "/data/live/cache.sock", During: "archival", Message: "unsupported file type: socket"},
	}}}
	lines := errorLines(blank)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want the two that name a file", len(lines))
	}
	for i, l := range lines {
		if l.Text == "" {
			t.Errorf("lines[%d] is empty, which the template rejects", i)
		}
	}
}

// restic's retry lines quote a full repository URL and run well past what one
// log entry may hold. Over the limit the server rejects the frame outright, so
// the whole outcome is lost rather than the tail of one line.
func TestTruncateLineCapsAtTheTemplateLimit(t *testing.T) {
	long := "check failed for pack " + strings.Repeat("a", 600)
	got := truncateLine(long)
	if n := len([]rune(got)); n != 512 {
		t.Errorf("a %d-rune line truncated to %d, want the template's 512", len([]rune(long)), n)
	}

	short := "no errors were found"
	if got := truncateLine(short); got != short {
		t.Errorf("truncateLine(%q) = %q, want it untouched", short, got)
	}
}

// restic quotes the repository URL in its retry lines. A REST/S3/B2 repo can
// carry credentials there, and those must not reach the Lock Screen.
func TestLogLinesRedactCredentials(t *testing.T) {
	// #nosec G101 -- a synthetic line whose whole purpose is to carry the
	// credentials this test asserts are stripped.
	in := `Save(<data/abc>) returned error, retrying after 1.2s: Post "http://someuser:hunter2@backup.example.com:8000/host/data/abc": timeout`
	got := truncateLine(in)

	for _, secret := range []string{"someuser", "hunter2", "someuser:hunter2@"} {
		if strings.Contains(got, secret) {
			t.Errorf("line still carries %q: %s", secret, got)
		}
	}
	// The useful part has to survive.
	if !strings.Contains(got, "backup.example.com:8000") {
		t.Errorf("host was lost in redaction: %s", got)
	}
	if !strings.Contains(got, "timeout") {
		t.Errorf("error text was lost in redaction: %s", got)
	}
}

// Turning live_progress off has to actually stop the anchor being sent, not
// merely stop it being honored.
func TestLiveProgressDisabledSendsNoAnchor(t *testing.T) {
	cfg := testConfig()
	cfg.Render.LiveProgress = false
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, 1000)},
		{runningBackup(1, 100, 1000)},
		{runningBackup(1, 300, 1000)},
	}}
	h := newHarness(t, cfg, br)

	h.poll(t)
	h.poll(t)
	h.advance(10 * time.Second)
	h.poll(t)

	for i, c := range h.recorded() {
		if c.Method == "POST" {
			continue
		}
		if got := content(t, c); got.EndDate != nil {
			t.Errorf("call %d carried end_date %d with live_progress disabled", i, *got.EndDate)
		}
	}
}

// shouldPush's clauses have to work independently: with only one trigger
// exercised at a time, each must fire on its own.
func TestShouldPushTriggersIndependently(t *testing.T) {
	now := time.Unix(1785050000, 0)
	base := func() *tracked {
		return &tracked{
			lastPhase: phaseRunning, lastTemplate: pushward.TemplateGeneric,
			lastProgress: 0.50, lastPushAt: now, liveEnd: 900,
		}
	}
	same := pushward.Content{
		Template: pushward.TemplateGeneric, Progress: 0.50,
		EndDate: pushward.Int64Ptr(900),
	}

	if base().shouldPush(same, phaseRunning, now, time.Minute) {
		t.Error("an identical frame was pushed")
	}

	bigger := same
	bigger.Progress = 0.50 + progressChangeFrac
	if !base().shouldPush(bigger, phaseRunning, now, time.Minute) {
		t.Error("a full progress step did not push")
	}

	tiny := same
	tiny.Progress = 0.50 + progressChangeFrac/2
	if base().shouldPush(tiny, phaseRunning, now, time.Minute) {
		t.Error("a half progress step pushed; the throttle is not holding")
	}

	if !base().shouldPush(same, phaseScanning, now, time.Minute) {
		t.Error("a phase change did not push")
	}

	moved := same
	moved.EndDate = pushward.Int64Ptr(901)
	if !base().shouldPush(moved, phaseRunning, now, time.Minute) {
		t.Error("a moved anchor did not push")
	}

	written := same
	written.Lines = []pushward.LogLine{{Text: "removing 3 old packs", Level: pushward.LogInfo}}
	if !base().shouldPush(written, phaseRunning, now, time.Minute) {
		t.Error("a new log line did not push")
	}

	// A separately built slice of the same lines, so this compares values and
	// not the identity of the one the last frame carried.
	sent := base()
	sent.lastLines = []pushward.LogLine{{Text: "removing 3 old packs", Level: pushward.LogInfo}}
	if sent.shouldPush(written, phaseRunning, now, time.Minute) {
		t.Error("an unchanged log tail pushed")
	}

	if !base().shouldPush(same, phaseRunning, now.Add(2*time.Minute), time.Minute) {
		t.Error("the heartbeat did not push")
	}
}
