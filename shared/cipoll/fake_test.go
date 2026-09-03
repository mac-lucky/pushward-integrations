package cipoll

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/ci"
	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
)

const testRepo = "owner/repo"

// fakeForge is a scripted Forge. Every hook is a function so a test can advance
// the workflow between ticks; the ones left nil either report "nothing there" or
// fail the test, whichever makes the omission visible.
type fakeForge struct {
	t *testing.T

	mu sync.Mutex

	// ListRepos
	repos     []string
	reposErr  error
	repoCalls int

	// The scripted hooks. ActiveRuns/LiveJobs/GetRun fail the test when a poll
	// reaches them unset, which is what the "must not be called" cases assert.
	activeRuns func(repo string) ([]Run, error)
	liveJobs   func(repo string, runID int64) ([]ci.Job, error)
	getRun     func(repo string, runID int64) (*Run, error)
	// baseline left nil means "no usable prior run", the common case.
	baseline func(repo string, run Run, ref string, wantTimings bool) (Baseline, error)
	// outcome left nil collapses to Success/Failed, the simpler of the two
	// mappings the real adapters implement.
	outcome func(run Run, anyFailed bool) (string, string)

	baselineCalls   int
	lastWantTimings bool
	liveJobCalls    int

	// Budget. Unset means the forge publishes no allowance - every self-hosted
	// Forgejo - which is the default so pacing stays out of the way of tests that
	// are not about it.
	remaining   int
	resetAt     time.Time
	budgetKnown bool
}

func newFakeForge(t *testing.T) *fakeForge {
	t.Helper()
	return &fakeForge{t: t}
}

func (f *fakeForge) ListRepos(_ context.Context, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repoCalls++
	return f.repos, f.reposErr
}

func (f *fakeForge) ActiveRuns(_ context.Context, repo string) ([]Run, error) {
	f.mu.Lock()
	hook := f.activeRuns
	f.mu.Unlock()
	if hook == nil {
		f.t.Errorf("unexpected ActiveRuns(%q)", repo)
		return nil, nil
	}
	return hook(repo)
}

func (f *fakeForge) GetRun(_ context.Context, repo string, runID int64) (*Run, error) {
	f.mu.Lock()
	hook := f.getRun
	f.mu.Unlock()
	if hook == nil {
		// Fatalf, not Errorf: the loop dereferences what GetRun hands back, so
		// returning a nil run here would panic the whole package instead of
		// reporting which hook the test forgot.
		f.t.Fatalf("unexpected GetRun(%q, %d)", repo, runID)
		return nil, nil
	}
	return hook(repo, runID)
}

func (f *fakeForge) LiveJobs(_ context.Context, repo string, runID int64) ([]ci.Job, error) {
	f.mu.Lock()
	f.liveJobCalls++
	hook := f.liveJobs
	f.mu.Unlock()
	if hook == nil {
		f.t.Errorf("unexpected LiveJobs(%q, %d)", repo, runID)
		return nil, nil
	}
	return hook(repo, runID)
}

func (f *fakeForge) BaselineJobs(_ context.Context, repo string, run Run, ref string, wantTimings bool) (Baseline, error) {
	f.mu.Lock()
	f.baselineCalls++
	f.lastWantTimings = wantTimings
	hook := f.baseline
	f.mu.Unlock()
	if hook == nil {
		return Baseline{}, nil
	}
	return hook(repo, run, ref, wantTimings)
}

func (f *fakeForge) Outcome(run Run, anyFailed bool) (state, color string) {
	f.mu.Lock()
	hook := f.outcome
	f.mu.Unlock()
	if hook != nil {
		return hook(run, anyFailed)
	}
	if ci.JobFailed(run.Conclusion) || (run.Conclusion == "" && anyFailed) {
		return OutcomeFailed, pushward.ColorRed
	}
	return OutcomeSuccess, pushward.ColorGreen
}

func (f *fakeForge) Budget() (int, time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.remaining, f.resetAt, f.budgetKnown
}

// setBudget publishes an allowance, opting this forge into the loop's pacing.
func (f *fakeForge) setBudget(remaining int, resetIn time.Duration) {
	f.mu.Lock()
	f.remaining, f.resetAt, f.budgetKnown = remaining, time.Now().Add(resetIn), true
	f.mu.Unlock()
}

func (f *fakeForge) counts() (repoCalls, baselineCalls, liveJobCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repoCalls, f.baselineCalls, f.liveJobCalls
}

func (f *fakeForge) wantTimings() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastWantTimings
}

// --- option and job builders ---

func testOptions() Options {
	return Options{
		// Left unresolved so New's own derivation stays exercised. A bridge resolves
		// this in its config load, so production never hands New a zero active tier.
		Polling: sharedconfig.DefaultPollingConfig(),
		PushWard: sharedconfig.PushWardConfig{
			Priority:       1,
			CleanupDelay:   15 * time.Minute,
			StaleTimeout:   30 * time.Minute,
			EndDelay:       10 * time.Millisecond,
			EndDisplayTime: 10 * time.Millisecond,
		},
		// Mirrors the production default so tests exercise the shipped behavior.
		Render:      sharedconfig.DefaultRenderConfig(),
		TitlePrefix: "Forge",
		SlugPrefix:  "fk",
	}
}

func testOptionsRender(colors, weights bool) Options {
	opts := testOptions()
	opts.Render.StepColors = colors
	opts.Render.StepWeights = weights
	return opts
}

func job(name, status, conclusion string) ci.Job {
	return ci.Job{Name: name, Status: status, Conclusion: conclusion}
}

func runningJob(name string, startedAt time.Time) ci.Job {
	return ci.Job{Name: name, Status: ci.StatusInProgress, StartedAt: startedAt}
}

func doneJob(name string, startedAt, completedAt time.Time) ci.Job {
	return ci.Job{
		Name: name, Status: ci.StatusCompleted, Conclusion: ci.ConclusionSuccess,
		StartedAt: startedAt, CompletedAt: completedAt,
	}
}

// activeRun is the shape an idle probe returns for a run that is live.
func activeRun(id int64, name, branch string) Run {
	return Run{
		ID:          id,
		Name:        name,
		WorkflowKey: "99",
		Status:      ci.StatusInProgress,
		RawStatus:   ci.StatusInProgress,
		HeadBranch:  branch,
		HTMLURL:     "https://forge.example.com/owner/repo/actions/runs/" + strconv.FormatInt(id, 10),
		RepoURL:     "https://forge.example.com/owner/repo",
	}
}

// terminalRun is what GetRun returns once the run itself has stopped. It
// carries the workflow key, as both real forges' re-reads do, which is what
// files the finished run as the workflow's next seed.
func terminalRun(id int64, conclusion string) *Run {
	return &Run{
		ID:          id,
		WorkflowKey: "99",
		Status:      ci.StatusCompleted,
		RawStatus:   ci.StatusCompleted,
		Conclusion:  conclusion,
	}
}

// priorRunJobs is the finished run a seed measures: Lint 5s, a parallel Build
// matrix (300s / 120s, so the group weighs the 300s longest), Test 40s.
func priorRunJobs() []ci.Job {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return []ci.Job{
		doneJob("Lint", base, base.Add(5*time.Second)),
		doneJob("Build (ubuntu)", base.Add(5*time.Second), base.Add(5*time.Minute+5*time.Second)),
		doneJob("Build (macos)", base.Add(5*time.Second), base.Add(2*time.Minute+5*time.Second)),
		doneJob("Test", base.Add(5*time.Minute+5*time.Second), base.Add(5*time.Minute+45*time.Second)),
	}
}

// priorDurations is what the poller measures from priorRunJobs. Derived rather
// than transcribed, so retuning the fixture cannot leave the live-progress tests
// asserting against numbers nothing serves.
func priorDurations() map[string]float64 {
	return ci.GroupWeights(priorRunJobs())
}

// threeStepShape is the shape priorRunJobs reveals, derived for the same reason.
func threeStepShape() ci.StepInfo {
	return copyShape(ci.ComputeSteps(priorRunJobs()))
}

// captureLog points opts at a buffer that collects everything the poller logs
// at Info and above.
func captureLog(opts *Options) *bytes.Buffer {
	var buf bytes.Buffer
	opts.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return &buf
}

// --- poller wiring ---

// newTestPoller wires a poller to a fake forge and a mock PushWard server,
// pre-seeded with one repo to poll.
func newTestPoller(t *testing.T, opts Options, f Forge) (*Poller, *[]testutil.APICall, *sync.Mutex) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	p := New(f, pushward.NewClient(srv.URL, "hlk_test"), opts)
	p.repos = []string{testRepo}
	return p, calls, mu
}

// trackedPoller wires a poller whose repo is already tracked, and creates the
// activity up front. The creation matters: the mock 404s a PATCH to an unknown
// slug, and the anchor state only promotes after a patch lands, so without it
// every tick would look like the first one.
func trackedPoller(t *testing.T, opts Options, f Forge, tracked *trackedRun) (*Poller, func(int) []testutil.APICall) {
	t.Helper()
	srv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	if err := pw.CreateActivity(context.Background(), tracked.Slug, "Forge: repo", 1, 900, 1800); err != nil {
		t.Fatal(err)
	}
	p := New(f, pw, opts)
	p.tracked = map[string]*trackedRun{testRepo: tracked}

	// Waits for n frames and drops the setup POST, so callers index only what the
	// poller sent. WaitForCalls settles before returning, which matters for the
	// two-phase end: its frames arrive from timer goroutines.
	patches := func(n int) []testutil.APICall {
		t.Helper()
		var out []testutil.APICall
		for _, c := range testutil.WaitForCalls(t, calls, mu, n+1, 2*time.Second) {
			if c.Method == http.MethodPatch {
				out = append(out, c)
			}
		}
		return out
	}
	return p, patches
}

// liveTrackedRun is a run already seeded with the three-step shape and the prior
// run's per-group durations, which is the state pollActive needs to anchor a
// live step window.
func liveTrackedRun(weights map[string]float64) *trackedRun {
	return &trackedRun{
		RunID:            42,
		Slug:             "fk-repo",
		Name:             "CI",
		HTMLURL:          "https://forge.example.com/owner/repo/actions/runs/42",
		RepoURL:          "https://forge.example.com/owner/repo",
		maxTotalSteps:    3,
		maxStepRows:      []int{1, 1, 1},
		maxStepLabels:    []string{"Lint", "Build", "Test"},
		stepWeightByName: weights,
	}
}

// seedFrame reads the seeding PATCH out of a pollIdle run: the create POST comes
// first, the full-Content seed second.
func seedFrame(t *testing.T, calls *[]testutil.APICall, mu *sync.Mutex) (pushward.UpdateRequest, json.RawMessage) {
	t.Helper()
	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 PushWard calls (create + seed), got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req)
	return req, got[1].Body
}

// stepValue dereferences an optional step index for readable assertion failures.
// A nil pointer reports -1, which no valid current_step can be.
func stepValue(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}
