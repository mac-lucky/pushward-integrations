package poller

import (
	"context"
	"encoding/json"
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
)

// fakeBackrest returns a scripted sequence of operation windows, one per poll,
// repeating the last once it runs out.
type fakeBackrest struct {
	mu       sync.Mutex
	windows  [][]backrest.Operation
	call     int
	logs     map[string]string
	logCalls int
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
	return f.logs[ref], nil
}

func (f *fakeBackrest) logCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logCalls
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
		Render: config.RenderConfig{LiveProgress: true, Logs: true},
	}
}

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

	pw := pushward.NewClient(srv.URL, "hlk_test")
	p := New(cfg, br, pw)

	h := &harness{p: p, br: br, calls: calls, mu: mu, clock: time.Unix(1785050000, 0)}
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

// content unmarshals the content object of the nth recorded call.
func content(t *testing.T, call testutil.APICall) apiContent {
	t.Helper()
	var body struct {
		State   string     `json:"state"`
		Content apiContent `json:"content"`
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

type apiContent struct {
	Template     string  `json:"template"`
	Progress     float64 `json:"progress"`
	State        string  `json:"state"`
	Icon         string  `json:"icon"`
	Subtitle     string  `json:"subtitle"`
	AccentColor  string  `json:"accent_color"`
	LiveProgress *bool   `json:"live_progress"`
	StartDate    *int64  `json:"start_date"`
	EndDate      *int64  `json:"end_date"`
	Lines        []struct {
		Text  string `json:"text"`
		Level string `json:"level"`
	} `json:"lines"`
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

// The first window a freshly started bridge sees is a wall of finished
// operations from previous days. Announcing those would push an activity for
// every one of them.
func TestFirstPollDoesNotAnnounceHistory(t *testing.T) {
	done := loadOp(t, "backup_success.json")
	prune := loadOp(t, "prune_success.json")
	br := &fakeBackrest{windows: [][]backrest.Operation{{done, prune}}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)

	if calls := h.recorded(); len(calls) != 0 {
		t.Fatalf("first poll made %d API calls, want 0: %+v", len(calls), calls)
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
	br := &fakeBackrest{windows: [][]backrest.Operation{{same}, {same}, {same}}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	before := len(h.recorded())

	h.advance(heartbeatInterval + time.Second)
	h.poll(t)

	if after := len(h.recorded()); after != before+1 {
		t.Errorf("made %d calls after the heartbeat interval, want 1", after-before)
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
}

// Prune has no percent-done anywhere in the protocol, so it renders as its log
// rather than a fabricated bar.
func TestPruneRendersAsLog(t *testing.T) {
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
	// The fixture has 22 lines and the template takes 20; the tail is what
	// matters, newest first.
	if len(seed.Lines) != maxLogLines {
		t.Fatalf("got %d lines, want the %d-line ceiling", len(seed.Lines), maxLogLines)
	}
	if seed.Lines[0].Text != "done" {
		t.Errorf("lines[0] = %q, want the newest line first", seed.Lines[0].Text)
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

	h.advance(logRefreshInterval)
	h.poll(t)
	if got := br.logCallCount(); got != fetched+1 {
		t.Errorf("fetched %d times after the refresh interval, want 1", got-fetched)
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

// An operation that leaves the window without ever being seen finished would
// otherwise show as running until the server's stale timeout.
func TestOperationLeavingTheWindowIsClosed(t *testing.T) {
	br := &fakeBackrest{windows: [][]backrest.Operation{
		{runningBackup(1, 0, 1000)},
		{runningBackup(1, 100, 1000)},
		{},
	}}
	h := newHarness(t, testConfig(), br)

	h.poll(t)
	h.poll(t)
	before := len(h.recorded())
	h.poll(t)

	waitForEnded(t, h, before)
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

	// The relay concatenates plan and repo with no separator.
	relayInput := "appdata" + "nas"
	bridgeInput := "appdata" + "/" + "nas" + "/" + string(backrest.KindBackup)
	if relayInput == bridgeInput {
		t.Fatal("the two hash inputs are identical, so the slugs would collide")
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
