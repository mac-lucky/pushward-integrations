package cipoll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mac-lucky/pushward-integrations/shared/ci"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/syncx"
	"github.com/mac-lucky/pushward-integrations/shared/testutil"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// --- construction and helpers ---

func TestNew(t *testing.T) {
	f := newFakeForge(t)
	pw := pushward.NewClient("http://localhost", "key")
	opts := testOptions()
	opts.Repos = []string{"owner/repo1", "owner/repo2"}

	p := New(f, pw, opts)
	if p.forge != f {
		t.Error("expected forge to be set")
	}
	if p.pw != pw {
		t.Error("expected pushward client to be set")
	}
	if len(p.repos) != 2 {
		t.Errorf("expected 2 repos, got %d", len(p.repos))
	}
	if len(p.tracked) != 0 {
		t.Errorf("expected empty tracked map, got %d", len(p.tracked))
	}
}

// A caller that does not care which symbol the card shows should still get one:
// an empty Icon would render a blank slot on the Live Activity.
func TestNewDefaultsIcon(t *testing.T) {
	opts := testOptions()
	opts.Icon = ""
	if got := New(newFakeForge(t), nil, opts).opts.Icon; got != defaultIcon {
		t.Errorf("Icon = %q, want the default %q", got, defaultIcon)
	}

	opts.Icon = "hammer.fill"
	if got := New(newFakeForge(t), nil, opts).opts.Icon; got != "hammer.fill" {
		t.Errorf("an explicit Icon must win, got %q", got)
	}
}

// validate catches the Options a forge adapter can get wrong in a way the loop
// cannot recover from - a zero IdleInterval would panic time.NewTicker deep
// inside shared code, and a blank prefix silently persists a wrong slug.
func TestRunRejectsUnusableOptions(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Options)
		want string
	}{
		{name: "zero interval", mut: func(o *Options) { o.IdleInterval = 0 }, want: "IdleInterval"},
		{name: "negative interval", mut: func(o *Options) { o.IdleInterval = -time.Second }, want: "IdleInterval"},
		// A zero active tier means "inherit the idle one", but a negative is a
		// misconfiguration and must not be silently normalized into something valid.
		{name: "negative active interval", mut: func(o *Options) { o.Interval = -time.Second }, want: "Interval"},
		{name: "blank slug prefix", mut: func(o *Options) { o.SlugPrefix = "" }, want: "SlugPrefix"},
		{name: "blank title prefix", mut: func(o *Options) { o.TitlePrefix = "" }, want: "TitlePrefix"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			tc.mut(&opts)
			// Unset hooks: Run must reject before it polls anything.
			p, _, _ := newTestPoller(t, opts, newFakeForge(t))

			err := p.Run(context.Background())
			if err == nil {
				t.Fatal("expected Run to reject the options")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should name %s, got %v", tc.want, err)
			}
		})
	}
}

// staleAfter cannot key off StaleTimeout alone. It is a server-side TTL an
// operator may legally set to zero to take the server default, and a poll
// interval longer than it would then evict every healthy run one tick after it
// was created: a create/evict/re-create loop that never advances past the seed.
func TestStaleAfterOutlastsThePollInterval(t *testing.T) {
	tests := []struct {
		name         string
		staleTimeout time.Duration
		idle         time.Duration
		want         time.Duration
	}{
		{name: "stale timeout dominates", staleTimeout: 30 * time.Minute, idle: 30 * time.Second, want: 30*time.Minute + staleEvictionGrace},
		{name: "zero stale timeout falls back to the interval", staleTimeout: 0, idle: 60 * time.Second, want: 60*time.Second + staleEvictionGrace},
		{name: "short stale timeout does not beat a long interval", staleTimeout: 20 * time.Second, idle: 5 * time.Minute, want: 5*time.Minute + staleEvictionGrace},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			opts.PushWard.StaleTimeout = tc.staleTimeout
			opts.IdleInterval = tc.idle
			got := New(newFakeForge(t), nil, opts).staleAfter()
			if got != tc.want {
				t.Errorf("staleAfter = %s, want %s", got, tc.want)
			}
			// The property that matters: one poll interval can never expire a run.
			if got <= tc.idle {
				t.Errorf("staleAfter %s must exceed the poll interval %s", got, tc.idle)
			}
		})
	}
}

// A StaleTimeout of zero is legal, and used to evict the run one tick after
// creating it - the card reset to its seed frame forever.
func TestPollActive_ZeroStaleTimeoutDoesNotEvictAHealthyRun(t *testing.T) {
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{job("Build", ci.StatusInProgress, "")}, nil
	}
	opts := testOptions()
	opts.PushWard.StaleTimeout = 0
	opts.IdleInterval = 60 * time.Second

	tracked := liveTrackedRun(nil)
	// Polled one interval ago, which is the normal steady state.
	tracked.LastUpdate = time.Now().Add(-opts.IdleInterval)
	p, _ := trackedPoller(t, opts, f, tracked)

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	_, stillTracked := p.tracked[testRepo]
	p.mu.Unlock()
	if !stillTracked {
		t.Error("a healthy run polled one interval ago must not be evicted")
	}
}

// The server caps name and subtitle at 256 runes and the client fails fast on
// 4xx, so an over-long workflow name would fail the create outright and leave
// that repo showing nothing at all.
func TestPollIdle_BoundsTheCardStrings(t *testing.T) {
	long := strings.Repeat("x", 400)
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) { return []Run{activeRun(42, long, "main")}, nil }
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{job("Build", ci.StatusInProgress, "")}, nil
	}
	opts := testOptions()
	opts.TitlePrefix = long
	p, calls, mu := newTestPoller(t, opts, f)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected the create and the seed to land, got %d calls", len(got))
	}
	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, got[0].Body, &create)
	if n := utf8.RuneCountInString(create.Name); n > titleLimit {
		t.Errorf("name is %d runes, want at most %d", n, titleLimit)
	}
	var seed pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &seed)
	if n := utf8.RuneCountInString(seed.Content.Subtitle); n > subtitleLimit {
		t.Errorf("subtitle is %d runes, want at most %d", n, subtitleLimit)
	}
}

// A forge that maps a 404 to (nil, nil) breaks the documented contract, but it is
// a common enough Go habit that the loop must defer rather than panic.
func TestPollActive_NilRunFromGetRunDefersInsteadOfPanicking(t *testing.T) {
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{job("Build", ci.StatusCompleted, ci.ConclusionSuccess)}, nil
	}
	f.getRun = func(string, int64) (*Run, error) { return nil, nil }
	p, _, _ := newTestPoller(t, testOptions(), f)
	p.tracked[testRepo] = &trackedRun{
		RunID: 42, Slug: "fk-repo", Name: "CI",
		maxTotalSteps: 1, maxStepRows: []int{1},
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	tr, ok := p.tracked[testRepo]
	hasPendingEnd := ok && tr.endTimers != nil
	p.mu.Unlock()
	if !ok {
		t.Fatal("the run must stay tracked so the next tick can retry")
	}
	if hasPendingEnd {
		t.Error("a contract-breaking nil run must not schedule an end")
	}
}

// The seed lookup's failure is the loop's to report, so an adapter error has to
// reach it rather than being logged and dropped adapter-side.
func TestBaselineShape_ErrorKeepsTheLiveScan(t *testing.T) {
	f := newFakeForge(t)
	f.baseline = func(string, Run, bool) (Baseline, error) {
		return Baseline{}, errors.New("403 from the forge")
	}
	p := New(f, nil, testOptions())

	if _, ok := p.baselineShape(context.Background(), testRepo, activeRun(42, "CI", "main")); ok {
		t.Error("expected ok=false so the caller keeps its live scan")
	}
}

// Logging is injected, not reached for globally: importing this package must not
// be able to change a bridge's logging, and two bridges in one process stay
// separable.
func TestOptionsLoggerIsUsed(t *testing.T) {
	var buf bytes.Buffer
	opts := testOptions()
	opts.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	opts.Owner = "owner"

	f := newFakeForge(t)
	f.repos = []string{"owner/one", "owner/two"}
	p := New(f, nil, opts)

	if err := p.refreshRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "repo list updated") {
		t.Errorf("the injected logger should have received the update, got %q", buf.String())
	}

	// Left unset it still logs, just to the default.
	if New(newFakeForge(t), nil, testOptions()).log == nil {
		t.Error("an unset Logger must fall back to slog.Default(), not nil")
	}
}

func TestRepoName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"owner/repo", "repo"},
		{"mac-lucky/pushward-server", "pushward-server"},
		{"noslash", "noslash"},
		{"a/b/c", "b/c"},
		{"acme/", ""},
	}
	for _, tt := range tests {
		if got := repoName(tt.input); got != tt.want {
			t.Errorf("repoName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// Terminal is the single check that replaced two forge-specific ones, so it has
// to agree with the ladder's vocabulary exactly: anything short of completed
// means the run may still reveal another job wave.
func TestRunTerminal(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{ci.StatusCompleted, true},
		{ci.StatusInProgress, false},
		{ci.StatusQueued, false},
		{"", false},
	}
	for _, tt := range tests {
		if got := (Run{Status: tt.status}).Terminal(); got != tt.want {
			t.Errorf("Run{Status: %q}.Terminal() = %v, want %v", tt.status, got, tt.want)
		}
	}
}

// runAttrs feeds slog, which silently drops a trailing key with no value, so the
// field count has to stay even however many optional fields a forge reports.
func TestRunAttrs(t *testing.T) {
	p := New(newFakeForge(t), nil, testOptions())

	sparse := p.runAttrs(testRepo, Run{ID: 42, Name: "CI", HeadBranch: "main"}, "fk-repo")
	wantSparse := []any{
		"repo", testRepo, "run_id", int64(42),
		"workflow", "CI", "branch", "main", "slug", "fk-repo",
	}
	if !reflect.DeepEqual(sparse, wantSparse) {
		t.Errorf("attrs = %v, want %v", sparse, wantSparse)
	}

	full := p.runAttrs(testRepo, Run{ID: 42, Number: 33, Name: "CI", HeadBranch: "main", Event: "push"}, "fk-repo")
	wantFull := []any{
		"repo", testRepo, "run_id", int64(42), "run", int64(33),
		"workflow", "CI", "branch", "main", "event", "push", "slug", "fk-repo",
	}
	if !reflect.DeepEqual(full, wantFull) {
		t.Errorf("attrs = %v, want %v", full, wantFull)
	}
}

// liveAnchor's window arithmetic is ci.TestLiveAnchor's job. What belongs here is
// the config gate in front of it: the flag has to suppress an otherwise perfectly
// anchorable step.
func TestLiveAnchorConfigGate(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	info := ci.StepInfo{
		CurrentStep:          2,
		CurrentStepName:      "Build",
		CurrentStepStartedAt: now.Add(-30 * time.Second),
	}
	weights := map[string]float64{"Build": 300}

	opts := testOptions()
	opts.Render.LiveProgress = false
	if _, _, ok := New(newFakeForge(t), nil, opts).liveAnchor(info, weights, now); ok {
		t.Error("expected no anchor when live progress is disabled")
	}

	opts.Render.LiveProgress = true
	if _, _, ok := New(newFakeForge(t), nil, opts).liveAnchor(info, weights, now); !ok {
		t.Error("expected an anchor when live progress is enabled")
	}
}

// liveProgressOff is the explicit false that stops a carried-forward animation.
// With the feature disabled it must stay nil so the field is omitted entirely.
func TestLiveProgressOff(t *testing.T) {
	opts := testOptions()
	opts.Render.LiveProgress = false
	if got := New(newFakeForge(t), nil, opts).liveProgressOff(); got != nil {
		t.Errorf("expected nil when disabled so the key is omitted, got %v", *got)
	}

	opts.Render.LiveProgress = true
	got := New(newFakeForge(t), nil, opts).liveProgressOff()
	if got == nil || *got {
		t.Errorf("expected an explicit false, got %v", got)
	}
}

func TestShapeStepColorsDisabled(t *testing.T) {
	jobs := []ci.Job{
		job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
		job("Build", ci.StatusInProgress, ""),
	}

	off := New(newFakeForge(t), nil, testOptionsRender(false, false)).shape(jobs)
	if off.StepColors != nil {
		t.Errorf("step_colors must be nil when disabled, got %v", off.StepColors)
	}
	if len(off.StepRows) != off.TotalSteps || len(off.StepLabels) != off.TotalSteps {
		t.Errorf("rows/labels must survive the flag: rows=%v labels=%v", off.StepRows, off.StepLabels)
	}

	on := New(newFakeForge(t), nil, testOptionsRender(true, false)).shape(jobs)
	if want := []string{"purple", "blue"}; !reflect.DeepEqual(on.StepColors, want) {
		t.Errorf("step_colors = %v, want %v", on.StepColors, want)
	}
}

// --- pollIdle ---

func TestPollIdle_DiscoversAndTracksRun(t *testing.T) {
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) {
		return []Run{activeRun(42, "CI", "main")}, nil
	}
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{
			job("Build", ci.StatusInProgress, ""),
			job("Test", ci.StatusQueued, ""),
		}, nil
	}
	p, calls, mu := newTestPoller(t, testOptions(), f)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := testutil.GetCalls(calls, mu)
	if len(got) != 2 {
		t.Fatalf("expected 2 API calls (create + update), got %d", len(got))
	}
	if got[0].Method != "POST" || got[0].Path != "/activities" {
		t.Errorf("expected POST /activities, got %s %s", got[0].Method, got[0].Path)
	}

	wantSlug := text.SlugHash("fk", testRepo, SlugHashLen)
	if got[1].Method != "PATCH" || got[1].Path != "/activities/"+wantSlug {
		t.Errorf("expected PATCH /activities/%s, got %s %s", wantSlug, got[1].Method, got[1].Path)
	}

	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req)
	if req.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING state, got %s", req.State)
	}
	if req.Content.Template != "steps" {
		t.Errorf("expected steps template, got %s", req.Content.Template)
	}
	if req.Content.Icon != defaultIcon {
		t.Errorf("expected icon %q, got %q", defaultIcon, req.Content.Icon)
	}
	// Both links are the forge's own values, never composed here.
	if req.Content.URL != "https://forge.example.com/owner/repo/actions/runs/42" {
		t.Errorf("unexpected URL: %s", req.Content.URL)
	}
	if req.Content.SecondaryURL != "https://forge.example.com/owner/repo" {
		t.Errorf("unexpected SecondaryURL: %s", req.Content.SecondaryURL)
	}

	p.mu.Lock()
	tracked, ok := p.tracked[testRepo]
	p.mu.Unlock()
	if !ok {
		t.Fatal("expected repo to be tracked")
	}
	if tracked.RunID != 42 {
		t.Errorf("expected RunID 42, got %d", tracked.RunID)
	}
	if tracked.Slug != wantSlug {
		t.Errorf("expected slug %s, got %s", wantSlug, tracked.Slug)
	}
	if tracked.maxTotalSteps != 2 {
		t.Errorf("expected maxTotalSteps 2, got %d", tracked.maxTotalSteps)
	}
}

// The title names the forge, so two bridges watching the same repo produce cards
// a user can tell apart.
func TestPollIdle_TitleAndSlugCarryTheForgePrefix(t *testing.T) {
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) { return []Run{activeRun(42, "CI", "main")}, nil }
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{job("Build", ci.StatusInProgress, "")}, nil
	}
	opts := testOptions()
	opts.TitlePrefix = "Forgejo"
	opts.SlugPrefix = "fj"
	p, calls, mu := newTestPoller(t, opts, f)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := testutil.GetCalls(calls, mu)
	var create pushward.CreateActivityRequest
	testutil.UnmarshalBody(t, got[0].Body, &create)
	if create.Name != "Forgejo: repo" {
		t.Errorf("name = %q, want %q", create.Name, "Forgejo: repo")
	}
	if want := text.SlugHash("fj", testRepo, SlugHashLen); create.Slug != want {
		t.Errorf("slug = %q, want %q", create.Slug, want)
	}
}

func TestPollIdle_SkipsAlreadyTrackedRepo(t *testing.T) {
	// Every hook is unset, so any forge call fails the test.
	f := newFakeForge(t)
	p, _, _ := newTestPoller(t, testOptions(), f)
	p.tracked[testRepo] = &trackedRun{RunID: 100, Slug: "fk-repo"}

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPollIdle_NoRunsFound(t *testing.T) {
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) { return nil, nil }
	p, calls, mu := newTestPoller(t, testOptions(), f)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := testutil.GetCalls(calls, mu); len(got) != 0 {
		t.Errorf("expected 0 API calls when no runs found, got %d", len(got))
	}
	if len(p.tracked) != 0 {
		t.Errorf("expected nothing tracked, got %d", len(p.tracked))
	}
}

// A failed idle probe skips the repo for this tick rather than aborting the whole
// poll: the other repos in the list are unaffected by one forge 5xx.
func TestPollIdle_ProbeErrorSkipsRepo(t *testing.T) {
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) { return nil, errors.New("boom") }
	p, calls, mu := newTestPoller(t, testOptions(), f)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatalf("a per-repo probe failure must not fail the poll: %v", err)
	}
	if got := testutil.GetCalls(calls, mu); len(got) != 0 {
		t.Errorf("expected 0 API calls, got %d", len(got))
	}
	if len(p.tracked) != 0 {
		t.Errorf("expected nothing tracked, got %d", len(p.tracked))
	}
}

func TestPollIdle_PicksMostRecentRun(t *testing.T) {
	now := time.Now()
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) {
		old := activeRun(10, "Old", "main")
		old.CreatedAt = now.Add(-time.Hour)
		recent := activeRun(20, "New", "main")
		recent.CreatedAt = now
		return []Run{old, recent}, nil
	}
	f.liveJobs = func(_ string, runID int64) ([]ci.Job, error) {
		if runID != 20 {
			t.Errorf("jobs fetched for run %d, want the most recent run 20", runID)
		}
		return nil, nil
	}
	p, _, _ := newTestPoller(t, testOptions(), f)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	tracked := p.tracked[testRepo]
	p.mu.Unlock()
	if tracked.RunID != 20 {
		t.Errorf("expected most recent run (ID=20), got %d", tracked.RunID)
	}
}

// A pending end belongs to a run that has already finished. It must survive an
// idle tick that finds the same run still listed, or the completion frames are
// dropped whenever EndDelay+EndDisplayTime >= IdleInterval.
func TestPollIdle_KeepsPendingEndForTheSameRun(t *testing.T) {
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) { return []Run{activeRun(42, "CI", "main")}, nil }
	p, calls, mu := newTestPoller(t, testOptions(), f)

	tg := &syncx.TimerGroup{}
	tg.Reset(time.Hour, func() {})
	defer tg.Close()
	p.tracked[testRepo] = &trackedRun{RunID: 42, Slug: "fk-repo", endTimers: tg}

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	tr, ok := p.tracked[testRepo]
	p.mu.Unlock()
	if !ok || tr.endTimers == nil {
		t.Error("the pending end for the same run must be left intact")
	}
	if got := testutil.GetCalls(calls, mu); len(got) != 0 {
		t.Errorf("expected no new activity for the same run, got %d calls", len(got))
	}
}

// A genuinely new run does supersede the pending end. The old group is Closed
// (terminal), never Stopped: an in-flight phase-1 callback could otherwise re-arm
// phase 2 and fire a stale ENDED at the new run's repo-derived slug.
func TestPollIdle_NewRunSupersedesPendingEnd(t *testing.T) {
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) { return []Run{activeRun(43, "CI", "main")}, nil }
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{job("Build", ci.StatusInProgress, "")}, nil
	}
	p, _, _ := newTestPoller(t, testOptions(), f)

	fired := make(chan struct{}, 1)
	tg := &syncx.TimerGroup{}
	tg.Reset(50*time.Millisecond, func() { fired <- struct{}{} })
	p.tracked[testRepo] = &trackedRun{RunID: 42, Slug: "fk-repo", endTimers: tg}

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	tr, ok := p.tracked[testRepo]
	p.mu.Unlock()
	if !ok {
		t.Fatal("the superseding run must be tracked")
	}
	if tr.RunID != 43 {
		t.Errorf("RunID = %d, want the superseding run 43", tr.RunID)
	}
	if tr.endTimers != nil {
		t.Error("the new run must start with no pending end")
	}

	select {
	case <-fired:
		t.Error("the superseded run's end timer must not fire")
	case <-time.After(120 * time.Millisecond):
	}
}

func TestPollIdle_SeedsStepsFromPriorRun(t *testing.T) {
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) { return []Run{activeRun(42, "CI", "main")}, nil }
	// The current run has only revealed its first wave; forges create jobs lazily.
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{job("Lint", ci.StatusInProgress, "")}, nil
	}
	// The prior run revealed its full 6-step DAG.
	f.baseline = func(string, Run, bool) (Baseline, error) {
		var jobs []ci.Job
		for _, name := range []string{"Lint", "Build", "Test", "Scan", "Publish", "Notify"} {
			jobs = append(jobs, job(name, ci.StatusCompleted, ci.ConclusionSuccess))
		}
		return Baseline{Jobs: jobs, RunID: 41}, nil
	}
	p, calls, mu := newTestPoller(t, testOptions(), f)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	req, _ := seedFrame(t, calls, mu)
	if got := stepValue(req.Content.TotalSteps); got != 6 {
		t.Errorf("expected seeded TotalSteps=6, got %d", got)
	}
	wantLabels := []string{"Lint", "Build", "Test", "Scan", "Publish", "Notify"}
	if !reflect.DeepEqual(req.Content.StepLabels, wantLabels) {
		t.Errorf("expected labels adopted wholesale from the prior run %v, got %v", wantLabels, req.Content.StepLabels)
	}
	if len(req.Content.StepRows) != 6 {
		t.Errorf("expected 6 seeded StepRows, got %v", req.Content.StepRows)
	}

	p.mu.Lock()
	tracked := p.tracked[testRepo]
	p.mu.Unlock()
	if tracked == nil || tracked.maxTotalSteps != 6 {
		t.Errorf("expected tracked.maxTotalSteps=6, got %v", tracked)
	}
}

func TestPollIdle_FallsBackWhenNoPriorRun(t *testing.T) {
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) { return []Run{activeRun(42, "CI", "feature")}, nil }
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{
			job("Build", ci.StatusInProgress, ""),
			job("Test", ci.StatusQueued, ""),
		}, nil
	}
	// baseline unset: no usable prior run.
	p, calls, mu := newTestPoller(t, testOptions(), f)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	req, _ := seedFrame(t, calls, mu)
	if got := stepValue(req.Content.TotalSteps); got != 2 {
		t.Errorf("expected fallback TotalSteps=2 from the current scan, got %d", got)
	}

	p.mu.Lock()
	tracked := p.tracked[testRepo]
	p.mu.Unlock()
	if tracked == nil || tracked.maxTotalSteps != 2 {
		t.Errorf("expected tracked.maxTotalSteps=2, got %v", tracked)
	}
}

func TestPollIdle_KeepsCurrentScanWhenPriorRunSmaller(t *testing.T) {
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) { return []Run{activeRun(42, "CI", "main")}, nil }
	// The current run already reveals 3 groups...
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{
			job("Lint", ci.StatusInProgress, ""),
			job("Build", ci.StatusQueued, ""),
			job("Test", ci.StatusQueued, ""),
		}, nil
	}
	// ...while the prior run only had 2, so the seed must not shrink the total.
	f.baseline = func(string, Run, bool) (Baseline, error) {
		return Baseline{
			Jobs: []ci.Job{
				job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
				job("Build", ci.StatusCompleted, ci.ConclusionSuccess),
			},
			RunID: 41,
		}, nil
	}
	p, calls, mu := newTestPoller(t, testOptions(), f)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	req, _ := seedFrame(t, calls, mu)
	if got := stepValue(req.Content.TotalSteps); got != 3 {
		t.Errorf("expected the current scan's TotalSteps=3 to be kept over a smaller prior shape, got %d", got)
	}
	wantLabels := []string{"Lint", "Build", "Test"}
	if !reflect.DeepEqual(req.Content.StepLabels, wantLabels) {
		t.Errorf("expected the current scan's labels %v, got %v", wantLabels, req.Content.StepLabels)
	}
}

// A failed live scan must not lose the run: the seed still lands, on the default
// single-step shape, and the run is tracked so later ticks can grow it.
func TestPollIdle_LiveScanErrorFallsBackToOneStep(t *testing.T) {
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) { return []Run{activeRun(42, "CI", "main")}, nil }
	f.liveJobs = func(string, int64) ([]ci.Job, error) { return nil, errors.New("boom") }
	p, calls, mu := newTestPoller(t, testOptions(), f)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	req, _ := seedFrame(t, calls, mu)
	if got := stepValue(req.Content.TotalSteps); got != 1 {
		t.Errorf("expected the default TotalSteps=1, got %d", got)
	}
	p.mu.Lock()
	_, ok := p.tracked[testRepo]
	p.mu.Unlock()
	if !ok {
		t.Error("a failed job scan must still leave the run tracked")
	}
}

// Both guards must skip the lookup entirely rather than asking the forge for a
// prior run it cannot identify.
func TestBaselineShape_ShortCircuits(t *testing.T) {
	f := newFakeForge(t)
	p := New(f, nil, testOptions())

	if _, ok := p.baselineShape(context.Background(), testRepo, Run{HeadBranch: "main"}); ok {
		t.Error("expected ok=false when WorkflowKey is blank")
	}
	if _, ok := p.baselineShape(context.Background(), testRepo, Run{WorkflowKey: "99"}); ok {
		t.Error("expected ok=false when the branch is blank")
	}
	if _, calls, _ := f.counts(); calls != 0 {
		t.Errorf("expected the forge not to be asked, got %d BaselineJobs calls", calls)
	}
}

// A prior run the forge finds but reports no jobs for is not a usable seed.
func TestBaselineShape_EmptyJobsIsNotASeed(t *testing.T) {
	f := newFakeForge(t)
	f.baseline = func(string, Run, bool) (Baseline, error) { return Baseline{RunID: 41}, nil }
	p := New(f, nil, testOptions())

	if _, ok := p.baselineShape(context.Background(), testRepo, activeRun(42, "CI", "main")); ok {
		t.Error("expected ok=false when the prior run reported no jobs")
	}
}

// The timing join can cost an extra API call, so the forge is told whether
// anything downstream will read the durations.
func TestBaselineShape_PassesWantTimings(t *testing.T) {
	tests := []struct {
		name    string
		colors  bool
		weights bool
		live    bool
		want    bool
	}{
		{name: "nothing consumes them", want: false},
		{name: "colors alone do not", colors: true, want: false},
		{name: "pill sizing does", weights: true, want: true},
		{name: "the live window does", live: true, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeForge(t)
			f.baseline = func(string, Run, bool) (Baseline, error) {
				return Baseline{Jobs: priorRunJobs(), RunID: 41}, nil
			}
			opts := testOptionsRender(tc.colors, tc.weights)
			opts.Render.LiveProgress = tc.live
			p := New(f, nil, opts)

			info, ok := p.baselineShape(context.Background(), testRepo, activeRun(42, "CI", "main"))
			if !ok {
				t.Fatal("expected a usable seed")
			}
			if got := f.wantTimings(); got != tc.want {
				t.Errorf("wantTimings = %v, want %v", got, tc.want)
			}
			// The durations are only measured when something reads them.
			if gotWeights := info.WeightsByName != nil; gotWeights != tc.want {
				t.Errorf("WeightsByName present = %v, want %v", gotWeights, tc.want)
			}
		})
	}
}

// seedOnce runs one pollIdle against the standard prior-run fixture and returns
// the seed frame plus its raw body, so callers can assert on the JSON keys that
// actually went over the wire.
func seedOnce(t *testing.T, opts Options) (pushward.Content, json.RawMessage) {
	t.Helper()
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) { return []Run{activeRun(42, "CI", "main")}, nil }
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{job("Lint", ci.StatusInProgress, "")}, nil
	}
	f.baseline = func(string, Run, bool) (Baseline, error) { return Baseline{Jobs: priorRunJobs(), RunID: 41}, nil }
	p, calls, mu := newTestPoller(t, opts, f)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	req, body := seedFrame(t, calls, mu)
	return req.Content, body
}

// TestPollIdle_RenderFlags pins the opt-in contract: step_colors and step_weights
// are sent only when their flag is on, and each toggles independently. step_rows
// and step_labels are not gated and must survive every combination.
func TestPollIdle_RenderFlags(t *testing.T) {
	tests := []struct {
		name        string
		colors      bool
		weights     bool
		wantColors  []string
		wantWeights []float64
	}{
		{name: "both off"},
		{name: "colors only", colors: true, wantColors: []string{"purple", "blue", "yellow"}},
		{name: "weights only", weights: true, wantWeights: []float64{5, 300, 40}},
		{
			name: "both on", colors: true, weights: true,
			wantColors:  []string{"purple", "blue", "yellow"},
			wantWeights: []float64{5, 300, 40},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			content, body := seedOnce(t, testOptionsRender(tc.colors, tc.weights))

			if !reflect.DeepEqual(content.StepColors, tc.wantColors) {
				t.Errorf("step_colors = %v, want %v", content.StepColors, tc.wantColors)
			}
			if !reflect.DeepEqual(content.StepWeights, tc.wantWeights) {
				t.Errorf("step_weights = %v, want %v", content.StepWeights, tc.wantWeights)
			}
			// A disabled field must be absent from the JSON, not present as null:
			// that is what makes the opt-out byte-identical to the payload the
			// bridge sent before the field existed.
			if got := bytes.Contains(body, []byte(`"step_colors"`)); got != tc.colors {
				t.Errorf("step_colors key present = %v, want %v; body: %s", got, tc.colors, body)
			}
			if got := bytes.Contains(body, []byte(`"step_weights"`)); got != tc.weights {
				t.Errorf("step_weights key present = %v, want %v; body: %s", got, tc.weights, body)
			}
			// Never gated: the fan-out layout and labels ship in every combination.
			if wantRows := []int{1, 2, 1}; !reflect.DeepEqual(content.StepRows, wantRows) {
				t.Errorf("step_rows = %v, want %v", content.StepRows, wantRows)
			}
			if content.TotalSteps == nil {
				t.Fatal("the seed must set total_steps")
			}
			if len(content.StepLabels) != *content.TotalSteps {
				t.Errorf("step_labels length %d must equal total_steps %d",
					len(content.StepLabels), *content.TotalSteps)
			}
		})
	}
}

func TestPollIdle_SeedsWeightsFromPriorRun(t *testing.T) {
	content, _ := seedOnce(t, testOptionsRender(true, true))
	// The seed frame must carry pill weights sized by the prior run's durations,
	// one per step, matching total_steps so the server's length check passes.
	want := []float64{5, 300, 40}
	if !reflect.DeepEqual(content.StepWeights, want) {
		t.Errorf("seed step_weights = %v, want %v", content.StepWeights, want)
	}
	if content.TotalSteps == nil {
		t.Fatal("the seed must set total_steps")
	}
	total := *content.TotalSteps
	// Regression guard: the seed must carry step_rows (fan-out) ALONGSIDE
	// step_weights (widths), not one instead of the other. The server accepts both
	// together (weighted-matrix layout); older clients ignore weights and render
	// the fan-out from step_rows. Re-splitting them here would silently drop the
	// fan-out. Every per-step slice must match total_steps.
	if wantRows := []int{1, 2, 1}; !reflect.DeepEqual(content.StepRows, wantRows) {
		t.Errorf("seed step_rows = %v, want %v (the Build matrix fans out to 2)", content.StepRows, wantRows)
	}
	if len(content.StepRows) != total || len(content.StepLabels) != total ||
		len(content.StepColors) != total || len(content.StepWeights) != total {
		t.Errorf("per-step slice lengths must all equal total_steps (%d): rows=%d labels=%d colors=%d weights=%d",
			total, len(content.StepRows), len(content.StepLabels),
			len(content.StepColors), len(content.StepWeights))
	}
}

// TestPollIdle_AssumesAnimationUntilProven pins the pessimistic start. The seed
// frame is what clears an animation carried over from the last run on this repo's
// slug, but the seed can fail, and the tracked entry is inserted before it is
// sent. Starting liveSent=true means the first tick that has nothing to animate
// sends the clear itself rather than trusting a frame that may never have landed;
// without it a run whose current step has no measurement would count toward the
// previous run's deadline for its whole life.
func TestPollIdle_AssumesAnimationUntilProven(t *testing.T) {
	for _, live := range []bool{true, false} {
		name := "enabled"
		if !live {
			name = "disabled"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeForge(t)
			f.activeRuns = func(string) ([]Run, error) { return []Run{activeRun(42, "CI", "main")}, nil }
			f.liveJobs = func(string, int64) ([]ci.Job, error) {
				return []ci.Job{job("Lint", ci.StatusInProgress, "")}, nil
			}
			opts := testOptions()
			opts.Render.LiveProgress = live
			p, _, _ := newTestPoller(t, opts, f)

			if err := p.pollIdle(context.Background()); err != nil {
				t.Fatal(err)
			}
			tracked, ok := p.tracked[testRepo]
			if !ok {
				t.Fatal("pollIdle must track the run")
			}
			if tracked.liveSent != live {
				t.Errorf("liveSent = %v, want %v: the clear has to be owed whenever the feature can animate",
					tracked.liveSent, live)
			}
		})
	}
}

// TestPollIdle_SeedStopsCarriedLiveProgress covers the shared slug. A run
// superseded before its end frames fire leaves live_progress on in stored
// content, and the seed merge-patches over it, so a card showing 0/N would
// otherwise inherit a countdown to the previous run's step.
func TestPollIdle_SeedStopsCarriedLiveProgress(t *testing.T) {
	on, onBody := seedOnce(t, testOptions())
	if on.LiveProgress == nil || *on.LiveProgress {
		t.Errorf("seed live_progress = %v, want false; body: %s", on.LiveProgress, onBody)
	}

	// Switched off, the seed must stay byte-identical to the one the bridge sent
	// before the anchors existed: absent, not an explicit false.
	opts := testOptions()
	opts.Render.LiveProgress = false
	_, offBody := seedOnce(t, opts)
	assertNoLiveWindow(t, offBody, "the disabled seed")
}

// --- pollActive ---

func TestPollActive_UpdatesOngoingRun(t *testing.T) {
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{
			job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
			job("Build", ci.StatusInProgress, ""),
			job("Test", ci.StatusQueued, ""),
		}, nil
	}
	p, patches := trackedPoller(t, testOptions(), f, liveTrackedRun(nil))

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := patches(1)
	if len(got) != 1 {
		t.Fatalf("expected 1 PATCH call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)
	if req.State != pushward.StateOngoing {
		t.Errorf("expected ONGOING, got %s", req.State)
	}
	if req.Content.State != "Build" {
		t.Errorf("expected state Build, got %s", req.Content.State)
	}
	// Progress is what fills the bar on the device: one of three groups done. A
	// tick that omitted it would decode as 0 here and still fail, which is the
	// point - nothing else in the suite reads this value back.
	if want := 1.0 / 3.0; req.Content.Progress != want {
		t.Errorf("progress = %v, want %v", req.Content.Progress, want)
	}
	// A tick PATCH must omit the seed-only fields so they are preserved
	// server-side under merge-patch.
	for _, f := range []struct {
		name string
		got  string
	}{
		{"accent_color", req.Content.AccentColor},
		{"icon", req.Content.Icon},
		{"template", string(req.Content.Template)},
		{"subtitle", req.Content.Subtitle},
		{"url", req.Content.URL},
	} {
		if f.got != "" {
			t.Errorf("expected the tick to omit %s, got %q", f.name, f.got)
		}
	}
}

func TestPollActive_NoJobs(t *testing.T) {
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) { return nil, nil }
	p, calls, mu := newTestPoller(t, testOptions(), f)
	p.tracked[testRepo] = &trackedRun{RunID: 42, Slug: "fk-repo"}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.GetCalls(calls, mu); len(got) != 0 {
		t.Errorf("expected 0 PushWard calls when the forge reports no jobs, got %d", len(got))
	}
}

func TestPollActive_SkipsRepoWithPendingEnd(t *testing.T) {
	// Every hook is unset, so any forge call fails the test.
	f := newFakeForge(t)
	p, _, _ := newTestPoller(t, testOptions(), f)

	tg := &syncx.TimerGroup{}
	tg.Reset(time.Hour, func() {}) // won't fire
	defer tg.Close()
	p.tracked[testRepo] = &trackedRun{RunID: 42, Slug: "fk-repo", endTimers: tg}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPollActive_MaxStepsClamping(t *testing.T) {
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{
			job("Build", ci.StatusInProgress, ""),
			job("Test", ci.StatusQueued, ""),
		}, nil
	}
	tracked := liveTrackedRun(nil)
	tracked.maxTotalSteps = 5 // higher than the current 2
	tracked.maxStepRows = []int{1, 1, 1, 1, 1}
	tracked.maxStepLabels = []string{"a", "b", "c", "d", "e"}
	p, patches := trackedPoller(t, testOptions(), f, tracked)

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := patches(1)
	if len(got) != 1 {
		t.Fatalf("expected 1 PATCH call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)
	if got := stepValue(req.Content.TotalSteps); got != 5 {
		t.Errorf("expected TotalSteps clamped to 5, got %d", got)
	}
}

// skippedGroupRun is seeded from a prior run that included an if-gated Deploy
// this run skips, so the live scan's group order is not a prefix of the seeded
// labels and a raw live index lands on the wrong one.
func skippedGroupRun() *trackedRun {
	return &trackedRun{
		RunID:         42,
		Slug:          "fk-repo",
		Name:          "CI",
		maxTotalSteps: 4,
		maxStepRows:   []int{1, 1, 1, 1},
		maxStepLabels: []string{"Lint", "Deploy", "Build", "Test"},
	}
}

// TestPollActive_CurrentStepFollowsTheRunningGroup pins the wire-visible half of
// the clamp. Substituting the seeded labels without moving current_step leaves
// the index addressing whichever group happens to sit at that position, and iOS
// draws the caption and the highlighted pill from step_labels[current-1], so the
// card ends up naming one group beside a state string naming another.
func TestPollActive_CurrentStepFollowsTheRunningGroup(t *testing.T) {
	f := newFakeForge(t)
	// Deploy never runs, so Build is second live but third as seeded.
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{
			job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
			job("Build", ci.StatusInProgress, ""),
		}, nil
	}
	p, patches := trackedPoller(t, testOptions(), f, skippedGroupRun())

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := patches(1)
	if len(got) != 1 {
		t.Fatalf("expected 1 PATCH call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)

	if got := stepValue(req.Content.CurrentStep); got != 3 {
		t.Errorf("current_step = %d, want 3 (Build's position in the seeded labels)", got)
	}
	// The state text and the pill the index selects have to agree.
	if req.Content.State != "Build" {
		t.Errorf("state = %q, want Build", req.Content.State)
	}
	if got := stepValue(req.Content.TotalSteps); got != 4 {
		t.Errorf("total_steps = %d, want the seeded 4", got)
	}
}

// TestPollActive_CurrentStepFollowsQueuedGroup covers the fallback path, where
// CurrentStepName is the literal "Queued" while the index still points at a real
// group. It is the case that proves the remap keys off the live label rather than
// off the state string, which would miss and leave the index stale.
func TestPollActive_CurrentStepFollowsQueuedGroup(t *testing.T) {
	f := newFakeForge(t)
	// Nothing running: Build is queued, so ComputeSteps reports "Queued".
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{
			job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
			job("Build", ci.StatusQueued, ""),
		}, nil
	}
	p, patches := trackedPoller(t, testOptions(), f, skippedGroupRun())

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := patches(1)
	if len(got) != 1 {
		t.Fatalf("expected 1 PATCH call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)

	if req.Content.State != "Queued" {
		t.Fatalf("state = %q, want Queued: the fixture must exercise the fallback", req.Content.State)
	}
	if got := stepValue(req.Content.CurrentStep); got != 3 {
		t.Errorf("current_step = %d, want 3 (Build's position in the seeded labels)", got)
	}
}

// A run parked on one long step yields identical scalars across polls, and each
// PATCH pushes to every subscribed device.
func TestPollActive_SkipsRedundantTicks(t *testing.T) {
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{
			job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
			job("Build", ci.StatusInProgress, ""),
			job("Test", ci.StatusQueued, ""),
		}, nil
	}
	// No weights, so the anchor declines and cannot manufacture a second frame.
	tracked := liveTrackedRun(nil)
	tracked.liveSent = false
	p, patches := trackedPoller(t, testOptions(), f, tracked)

	for i := 0; i < 3; i++ {
		if err := p.pollActive(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	if got := patches(1); len(got) != 1 {
		t.Errorf("expected 1 PATCH across three identical ticks, got %d", len(got))
	}
}

func TestPollActive_EvictsStaleRun(t *testing.T) {
	// Every hook is unset: eviction happens before any job fetch.
	f := newFakeForge(t)
	p, _, _ := newTestPoller(t, testOptions(), f)
	p.tracked[testRepo] = &trackedRun{
		RunID: 42,
		Slug:  "fk-repo",
		// Older than StaleTimeout + the 30s grace period.
		LastUpdate: time.Now().Add(-31 * time.Minute),
		trackedAt:  time.Now(),
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	_, stillTracked := p.tracked[testRepo]
	p.mu.Unlock()
	if stillTracked {
		t.Error("expected the vanished run to be evicted")
	}
}

// A run wedged in progress past maxRunLifetime must be reclaimed by guard 2
// (absolute age), even though guard 1 (stale LastUpdate) never fires because the
// jobs endpoint still returns data.
func TestPollActive_EvictsRunExceedingMaxLifetime(t *testing.T) {
	f := newFakeForge(t)
	// If guard 2 were removed, the recent LastUpdate keeps guard 1 from firing, so
	// the poll would reach LiveJobs and this hook would fail the test.
	p, calls, mu := newTestPoller(t, testOptions(), f)
	p.tracked[testRepo] = &trackedRun{
		RunID:         42,
		Slug:          "fk-repo",
		LastUpdate:    time.Now(),                      // recent: guard 1 must NOT fire
		trackedAt:     time.Now().Add(-13 * time.Hour), // > maxRunLifetime: guard 2 fires
		maxTotalSteps: 2,
		maxStepRows:   []int{1, 1},
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Give any erroneously scheduled end a chance to fire before asserting.
	time.Sleep(50 * time.Millisecond)

	p.mu.Lock()
	_, stillTracked := p.tracked[testRepo]
	p.mu.Unlock()
	if stillTracked {
		t.Error("expected the run exceeding max lifetime to be evicted")
	}
	// Eviction is silent: no ongoing tick, no two-phase end.
	if got := testutil.GetCalls(calls, mu); len(got) != 0 {
		t.Errorf("expected 0 PushWard calls on lifetime eviction, got %d", len(got))
	}
}

func TestPollActive_CompletesRun(t *testing.T) {
	tests := []struct {
		name       string
		conclusion string
		wantState  string
		wantColor  string
	}{
		{name: "success", conclusion: ci.ConclusionSuccess, wantState: "Success", wantColor: pushward.ColorGreen},
		{name: "failure", conclusion: ci.ConclusionFailure, wantState: "Failed", wantColor: pushward.ColorRed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeForge(t)
			f.liveJobs = func(string, int64) ([]ci.Job, error) {
				return []ci.Job{
					job("Build", ci.StatusCompleted, tc.conclusion),
					job("Test", ci.StatusCompleted, ci.ConclusionSuccess),
				}, nil
			}
			f.getRun = func(_ string, runID int64) (*Run, error) {
				return terminalRun(runID, tc.conclusion), nil
			}
			p, patches := trackedPoller(t, testOptions(), f, liveTrackedRun(nil))

			if err := p.pollActive(context.Background()); err != nil {
				t.Fatal(err)
			}

			got := patches(2)
			if len(got) != 2 {
				t.Fatalf("expected 2 calls (two-phase end), got %d", len(got))
			}

			var req1, req2 pushward.UpdateRequest
			testutil.UnmarshalBody(t, got[0].Body, &req1)
			testutil.UnmarshalBody(t, got[1].Body, &req2)

			if req1.State != pushward.StateOngoing {
				t.Errorf("phase 1: expected ONGOING, got %s", req1.State)
			}
			if req1.Content.State != tc.wantState {
				t.Errorf("phase 1: state = %q, want %q", req1.Content.State, tc.wantState)
			}
			if req1.Content.AccentColor != tc.wantColor {
				t.Errorf("phase 1: accent_color = %q, want %q", req1.Content.AccentColor, tc.wantColor)
			}
			if req2.State != pushward.StateEnded {
				t.Errorf("phase 2: expected ENDED, got %s", req2.State)
			}
			// The completion frame self-heals a seed that over-counted: the ladder's
			// phantom steps are shown as done rather than left mid-progress.
			if got, want := stepValue(req1.Content.CurrentStep), stepValue(req1.Content.TotalSteps); got != want {
				t.Errorf("the end frame must read N/N, got %d/%d", got, want)
			}
			if req1.Content.Progress != 1.0 {
				t.Errorf("the end frame must read a full bar, got progress %v", req1.Content.Progress)
			}
		})
	}
}

// The forge owns the outcome mapping, so a bridge that distinguishes cancelled
// and skipped keeps doing so through the shared loop.
func TestPollActive_CompletionUsesTheForgeOutcome(t *testing.T) {
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{job("Build", ci.StatusCompleted, ci.ConclusionCancelled)}, nil
	}
	f.getRun = func(_ string, runID int64) (*Run, error) {
		return terminalRun(runID, ci.ConclusionCancelled), nil
	}
	f.outcome = func(run Run, _ bool) (string, string) {
		if run.Conclusion == ci.ConclusionCancelled {
			return "Cancelled", pushward.ColorOrange
		}
		return "Complete", pushward.ColorGreen
	}
	p, patches := trackedPoller(t, testOptions(), f, liveTrackedRun(nil))

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := patches(2)
	if len(got) == 0 {
		t.Fatal("expected the end frames")
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)
	if req.Content.State != "Cancelled" {
		t.Errorf("state = %q, want Cancelled from the forge's own mapping", req.Content.State)
	}
	if req.Content.AccentColor != pushward.ColorOrange {
		t.Errorf("accent_color = %q, want %q", req.Content.AccentColor, pushward.ColorOrange)
	}
}

// All visible jobs are complete, but the run itself is still going (the forge is
// creating the next lazy job wave). pollActive must NOT end the activity early.
func TestPollActive_DefersEndWhileRunStillGoing(t *testing.T) {
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{job("Build", ci.StatusCompleted, ci.ConclusionSuccess)}, nil
	}
	f.getRun = func(_ string, runID int64) (*Run, error) {
		return &Run{ID: runID, Status: ci.StatusInProgress, RawStatus: "running"}, nil
	}
	p, _, _ := newTestPoller(t, testOptions(), f)
	p.tracked[testRepo] = &trackedRun{
		RunID: 42, Slug: "fk-repo", Name: "CI",
		maxTotalSteps: 1, maxStepRows: []int{1},
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	p.mu.Lock()
	tr, ok := p.tracked[testRepo]
	hasPendingEnd := ok && tr.endTimers != nil
	p.mu.Unlock()
	if hasPendingEnd {
		t.Error("scheduled an end while the run was still going")
	}
}

// A failed run re-read defers the end rather than guessing: ending on a
// half-known outcome would dismiss a card for a run that is still alive.
func TestPollActive_DefersEndWhenTheRunRereadFails(t *testing.T) {
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{job("Build", ci.StatusCompleted, ci.ConclusionSuccess)}, nil
	}
	f.getRun = func(string, int64) (*Run, error) { return nil, errors.New("boom") }
	p, calls, mu := newTestPoller(t, testOptions(), f)
	p.tracked[testRepo] = &trackedRun{
		RunID: 42, Slug: "fk-repo", Name: "CI",
		maxTotalSteps: 1, maxStepRows: []int{1},
	}

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	p.mu.Lock()
	tr, ok := p.tracked[testRepo]
	hasPendingEnd := ok && tr.endTimers != nil
	p.mu.Unlock()
	if !ok {
		t.Fatal("the run must stay tracked so the next tick can retry")
	}
	if hasPendingEnd {
		t.Error("scheduled an end on a failed run re-read")
	}
	if got := testutil.GetCalls(calls, mu); len(got) != 0 {
		t.Errorf("expected no frames, got %d", len(got))
	}
}

// --- live progress ---

// assertNoLiveWindow fails when any of the three animation fields reached the
// wire. All three matter together: a frame that drops live_progress but leaves
// start_date/end_date behind is the merge-patch carry-forward this feature has to
// defend against.
func assertNoLiveWindow(t *testing.T, body json.RawMessage, what string) {
	t.Helper()
	for _, key := range []string{`"live_progress"`, `"start_date"`, `"end_date"`} {
		if bytes.Contains(body, []byte(key)) {
			t.Errorf("%s must omit %s; body: %s", what, key, body)
		}
	}
}

// TestPollActive_LiveProgressAnchorsCurrentStep pins the anchor: the window runs
// from when the step actually started to that plus the prior run's duration for
// the group, so a poll landing mid-step picks the bar up where it already is
// instead of restarting it at zero.
func TestPollActive_LiveProgressAnchorsCurrentStep(t *testing.T) {
	buildStart := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{
			job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
			runningJob("Build", buildStart),
		}, nil
	}
	p, patches := trackedPoller(t, testOptions(), f, liveTrackedRun(priorDurations()))

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := patches(1)
	if len(got) != 1 {
		t.Fatalf("expected 1 PATCH call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)

	if req.Content.LiveProgress == nil || !*req.Content.LiveProgress {
		t.Fatalf("expected live_progress=true, got %v", req.Content.LiveProgress)
	}
	if req.Content.StartDate == nil || *req.Content.StartDate != buildStart.Unix() {
		t.Errorf("start_date = %v, want the job's started_at %d", req.Content.StartDate, buildStart.Unix())
	}
	if want := buildStart.Unix() + 300; req.Content.EndDate == nil || *req.Content.EndDate != want {
		t.Errorf("end_date = %v, want started_at + the prior run's 300s (%d)", req.Content.EndDate, want)
	}
}

// TestPollActive_LiveProgressAnchorsOncePerStep guards the rule the anchors exist
// under: restamping the window on a later tick of the same step would snap the
// pill back to empty and burn a high-priority push.
func TestPollActive_LiveProgressAnchorsOncePerStep(t *testing.T) {
	buildStart := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	tick := 0
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		tick++
		jobs := []ci.Job{
			job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
			runningJob("Build", buildStart),
		}
		if tick > 1 {
			// A queued job appears, so progress moves and the second tick patches.
			jobs = append(jobs, job("Test", ci.StatusQueued, ""))
		}
		return jobs, nil
	}
	p, patches := trackedPoller(t, testOptions(), f, liveTrackedRun(priorDurations()))

	for i := 0; i < 2; i++ {
		if err := p.pollActive(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	got := patches(2)
	if len(got) != 2 {
		t.Fatalf("expected 2 PATCH calls, got %d", len(got))
	}
	assertNoLiveWindow(t, got[1].Body, "a second tick on the same step")
}

// TestPollActive_LiveProgressStepChangeReanchors covers the other half: advancing
// current_step must move the window onto the new step, or merge-patch would leave
// it counting toward the previous step's deadline.
func TestPollActive_LiveProgressStepChangeReanchors(t *testing.T) {
	buildStart := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
	testStart := time.Now().Add(-5 * time.Second).UTC().Truncate(time.Second)
	// Test weighs 40s in priorRunJobs, which would leave only a 35s margin before
	// ci.LiveAnchor declines - a stalled CI box could turn that into a false
	// failure. Weighting it like Build gives the assertion 295s of headroom.
	weights := priorDurations()
	weights["Test"] = 300
	tick := 0
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		tick++
		if tick == 1 {
			return []ci.Job{
				job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
				runningJob("Build", buildStart),
			}, nil
		}
		return []ci.Job{
			job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
			job("Build", ci.StatusCompleted, ci.ConclusionSuccess),
			runningJob("Test", testStart),
		}, nil
	}
	p, patches := trackedPoller(t, testOptions(), f, liveTrackedRun(weights))

	for i := 0; i < 2; i++ {
		if err := p.pollActive(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	got := patches(2)
	if len(got) != 2 {
		t.Fatalf("expected 2 PATCH calls, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &req)
	if req.Content.LiveProgress == nil || !*req.Content.LiveProgress {
		t.Errorf("the re-anchor must carry live_progress=true, got %v", req.Content.LiveProgress)
	}
	if req.Content.StartDate == nil || *req.Content.StartDate != testStart.Unix() {
		t.Errorf("start_date = %v, want the new step's started_at %d", req.Content.StartDate, testStart.Unix())
	}
	if want := testStart.Unix() + 300; req.Content.EndDate == nil || *req.Content.EndDate != want {
		t.Errorf("end_date = %v, want the new step's window (%d)", req.Content.EndDate, want)
	}
}

// TestPollActive_LiveProgressSkipped is the wiring claim: when liveAnchor
// declines, none of the three fields reach the JSON. ci.TestLiveAnchor is the
// exhaustive table of WHY it declines, so this covers only the two cases that
// differ in wiring rather than in the decision: the feature switched off, and the
// decision itself coming back negative.
func TestPollActive_LiveProgressSkipped(t *testing.T) {
	tests := []struct {
		name     string
		liveFlag bool
		weights  map[string]float64
		// unstamped drops the running job's start, which is what a forge whose
		// timing join missed hands over.
		unstamped bool
	}{
		{name: "flag off", weights: priorDurations()},
		{name: "group absent from the prior run", liveFlag: true, weights: map[string]float64{"Lint": 5}},
		{name: "no start stamped", liveFlag: true, weights: priorDurations(), unstamped: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second)
			f := newFakeForge(t)
			f.liveJobs = func(string, int64) ([]ci.Job, error) {
				build := runningJob("Build", started)
				if tc.unstamped {
					build = job("Build", ci.StatusInProgress, "")
				}
				return []ci.Job{
					job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
					build,
				}, nil
			}
			opts := testOptions()
			opts.Render.LiveProgress = tc.liveFlag
			p, patches := trackedPoller(t, opts, f, liveTrackedRun(tc.weights))

			if err := p.pollActive(context.Background()); err != nil {
				t.Fatal(err)
			}

			got := patches(1)
			if len(got) != 1 {
				t.Fatalf("expected 1 PATCH call, got %d", len(got))
			}
			// Absent, not false: that is what keeps the payload byte-identical to
			// the one the bridge sent before the anchors existed.
			assertNoLiveWindow(t, got[0].Body, "an unanchorable step")
		})
	}
}

// TestPollActive_OverrunKeepsAnchor pins that a step outrunning its estimate
// costs no push. iOS drops a window whose end has passed and falls back to the
// static bar by itself, so switching live_progress off would broadcast to every
// subscribed device to change nothing a viewer can see.
func TestPollActive_OverrunKeepsAnchor(t *testing.T) {
	// Started well beyond Build's 300s estimate, so liveAnchor now declines.
	buildStart := time.Now().Add(-40 * time.Minute).UTC().Truncate(time.Second)
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{
			job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
			runningJob("Build", buildStart),
		}, nil
	}
	// The run is mid-step with an anchor already out on the wire for that step.
	tracked := liveTrackedRun(priorDurations())
	tracked.liveStepName = "Build"
	tracked.liveSent = true
	p, patches := trackedPoller(t, testOptions(), f, tracked)

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := patches(1)
	if len(got) != 1 {
		t.Fatalf("expected 1 PATCH call, got %d", len(got))
	}
	assertNoLiveWindow(t, got[0].Body, "a step still running past its estimate")

	// The claim is that the overrun costs no push, so it has to survive a tick
	// with nothing else moving. The first tick patches because lastPatchAt is
	// zero; this one has no scalar change, no shape change and no heartbeat due,
	// so the anchor logic is the only thing that could manufacture a frame.
	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got = patches(1); len(got) != 1 {
		t.Fatalf("an overrun must not push again, got %d PATCH calls", len(got))
	}
}

// TestPollActive_ClearsStaleLiveProgress covers advancing onto a step with no
// measurement. Merge-patch preserves the previous live_progress:true, so the new
// step would animate toward the old step's deadline unless it is switched off.
func TestPollActive_ClearsStaleLiveProgress(t *testing.T) {
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{
			job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
			job("Build", ci.StatusInProgress, ""),
		}, nil
	}
	tracked := liveTrackedRun(map[string]float64{"Lint": 5})
	tracked.liveStepName = "Lint"
	tracked.liveSent = true
	p, patches := trackedPoller(t, testOptions(), f, tracked)

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := patches(1)
	if len(got) != 1 {
		t.Fatalf("expected 1 PATCH call, got %d", len(got))
	}
	var req pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[0].Body, &req)
	if req.Content.LiveProgress == nil || *req.Content.LiveProgress {
		t.Errorf("expected live_progress=false to stop the stale window, got %v", req.Content.LiveProgress)
	}
	// Clearing must not also restamp the window: that is the carry-forward the
	// switch-off exists to stop, and it would leave the new step counting.
	if req.Content.StartDate != nil || req.Content.EndDate != nil {
		t.Errorf("clearing must not restamp the window: start=%v end=%v",
			req.Content.StartDate, req.Content.EndDate)
	}
}

// TestPollActive_EndFramesStopLiveProgress pins both halves of the two-phase end.
// The server only strips the anchors from an END push, so phase 1 (an ONGOING
// frame held for end_display_time) would otherwise sit there counting toward a
// deadline the run has already passed.
func TestPollActive_EndFramesStopLiveProgress(t *testing.T) {
	newForge := func(t *testing.T) *fakeForge {
		f := newFakeForge(t)
		f.liveJobs = func(string, int64) ([]ci.Job, error) {
			return []ci.Job{
				job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
				job("Build", ci.StatusCompleted, ci.ConclusionSuccess),
			}, nil
		}
		f.getRun = func(_ string, runID int64) (*Run, error) {
			return terminalRun(runID, ci.ConclusionSuccess), nil
		}
		return f
	}

	p, patches := trackedPoller(t, testOptions(), newForge(t), liveTrackedRun(priorDurations()))
	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := patches(2)
	if len(got) != 2 {
		t.Fatalf("expected both end frames, got %d PATCH calls", len(got))
	}
	for i, call := range got {
		var req pushward.UpdateRequest
		testutil.UnmarshalBody(t, call.Body, &req)
		if req.Content.LiveProgress == nil || *req.Content.LiveProgress {
			t.Errorf("end frame %d (%s) must send live_progress=false, got %v", i, req.State, req.Content.LiveProgress)
		}
	}

	// Switched off, the result frames stay byte-identical to the ones the bridge
	// sent before the anchors existed.
	opts := testOptions()
	opts.Render.LiveProgress = false
	off, offPatches := trackedPoller(t, opts, newForge(t), liveTrackedRun(priorDurations()))
	if err := off.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, call := range offPatches(2) {
		assertNoLiveWindow(t, call.Body, fmt.Sprintf("disabled end frame %d", i))
	}
}

// --- scheduleEnd ---

func TestScheduleEnd_TwoPhase(t *testing.T) {
	tests := []struct {
		name  string
		state string
		color string
	}{
		{name: "success", state: "Success", color: pushward.ColorGreen},
		{name: "failed", state: "Failed", color: pushward.ColorRed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeForge(t)
			p, patches := trackedPoller(t, testOptions(), f, liveTrackedRun(nil))

			content := pushward.Content{
				Template:     pushward.TemplateSteps,
				Progress:     1.0,
				State:        tc.state,
				Icon:         defaultIcon,
				Subtitle:     "repo / CI",
				AccentColor:  tc.color,
				CurrentStep:  pushward.IntPtr(2),
				TotalSteps:   pushward.IntPtr(2),
				URL:          "https://forge.example.com/owner/repo/actions/runs/42",
				SecondaryURL: "https://forge.example.com/owner/repo",
			}
			p.scheduleEnd(context.Background(), testRepo, content)

			got := patches(2)
			if len(got) != 2 {
				t.Fatalf("expected 2 API calls, got %d", len(got))
			}

			var req1, req2 pushward.UpdateRequest
			testutil.UnmarshalBody(t, got[0].Body, &req1)
			testutil.UnmarshalBody(t, got[1].Body, &req2)

			if got[0].Path != "/activities/fk-repo" {
				t.Errorf("phase 1: path = %s, want /activities/fk-repo", got[0].Path)
			}
			if req1.State != pushward.StateOngoing {
				t.Errorf("phase 1: expected ONGOING, got %s", req1.State)
			}
			if req2.State != pushward.StateEnded {
				t.Errorf("phase 2: expected ENDED, got %s", req2.State)
			}

			// Both phases carry identical content: the Dynamic Island shows the
			// final state for end_display_time before the card is dismissed.
			if !reflect.DeepEqual(req1.Content, req2.Content) {
				t.Errorf("content must be identical across phases:\nphase 1: %+v\nphase 2: %+v",
					req1.Content, req2.Content)
			}
			if req1.Content.State != tc.state || req1.Content.AccentColor != tc.color {
				t.Errorf("content = %q/%q, want %q/%q",
					req1.Content.State, req1.Content.AccentColor, tc.state, tc.color)
			}

			// The server handles cleanup via ended_ttl; the local entry goes away.
			p.mu.Lock()
			_, stillTracked := p.tracked[testRepo]
			p.mu.Unlock()
			if stillTracked {
				t.Error("expected the repo to be removed from tracked after the two-phase end")
			}
		})
	}
}

func TestScheduleEnd_CancelledByNewRun(t *testing.T) {
	f := newFakeForge(t)
	opts := testOptions()
	// Long enough to cancel before either phase fires.
	opts.PushWard.EndDelay = 500 * time.Millisecond
	opts.PushWard.EndDisplayTime = 500 * time.Millisecond
	p, calls, mu := newTestPoller(t, opts, f)
	p.tracked[testRepo] = &trackedRun{RunID: 300, Slug: "fk-repo", Name: "CI"}

	p.scheduleEnd(context.Background(), testRepo, pushward.Content{Template: pushward.TemplateSteps, State: OutcomeSuccess})

	// A new run takes over.
	time.Sleep(10 * time.Millisecond)
	p.mu.Lock()
	if tr, ok := p.tracked[testRepo]; ok && tr.endTimers != nil {
		tr.endTimers.Stop()
		delete(p.tracked, testRepo)
	}
	p.tracked[testRepo] = &trackedRun{RunID: 301, Slug: "fk-repo", Name: "CI v2"}
	p.mu.Unlock()

	// Wait past when the original timer would have fired.
	time.Sleep(200 * time.Millisecond)

	// Nothing may reach the wire: a phase-1 frame here would land on the new run's
	// slug carrying the old run's final content.
	if got := testutil.GetCalls(calls, mu); len(got) != 0 {
		t.Errorf("expected 0 API calls after cancellation, got %d", len(got))
	}

	p.mu.Lock()
	entry, ok := p.tracked[testRepo]
	p.mu.Unlock()
	if !ok {
		t.Fatal("expected the new run to be tracked")
	}
	if entry.RunID != 301 {
		t.Errorf("expected RunID 301, got %d", entry.RunID)
	}
}

func TestScheduleEnd_UnknownRepoIsANoop(t *testing.T) {
	p := New(newFakeForge(t), nil, testOptions())
	// Must not panic, and must not dereference the missing entry.
	p.scheduleEnd(context.Background(), "nonexistent", pushward.Content{})
}

// --- refreshRepos ---

func TestRefreshRepos_NoOwnerIsANoop(t *testing.T) {
	f := newFakeForge(t)
	opts := testOptions()
	opts.Repos = []string{testRepo}
	p := New(f, nil, opts)

	if err := p.refreshRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.repos) != 1 {
		t.Errorf("expected repos unchanged, got %d", len(p.repos))
	}
	if calls, _, _ := f.counts(); calls != 0 {
		t.Errorf("expected no discovery call without an owner, got %d", calls)
	}
}

func TestRefreshRepos_MergesDiscoveredAndConfigured(t *testing.T) {
	f := newFakeForge(t)
	f.repos = []string{"testowner/discovered1", "testowner/discovered2"}
	opts := testOptions()
	opts.Owner = "testowner"
	opts.Repos = []string{"other/configured"}
	p := New(f, nil, opts)

	if err := p.refreshRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Contents and order, not just the count: a regression that drops the
	// configured repo while double-counting a discovered one keeps the length.
	want := []string{"testowner/discovered1", "testowner/discovered2", "other/configured"}
	if !reflect.DeepEqual(p.watched(), want) {
		t.Errorf("repos = %v, want %v (discovered first, then configured)", p.watched(), want)
	}
}

func TestRefreshRepos_DeduplicatesRepos(t *testing.T) {
	f := newFakeForge(t)
	f.repos = []string{"owner/repo1", "owner/repo2"}
	opts := testOptions()
	opts.Owner = "owner"
	opts.Repos = []string{"owner/repo1"} // duplicate of discovered
	p := New(f, nil, opts)

	if err := p.refreshRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"owner/repo1", "owner/repo2"}
	if !reflect.DeepEqual(p.watched(), want) {
		t.Errorf("repos = %v, want %v (the configured duplicate must not repeat)", p.watched(), want)
	}
}

func TestRefreshRepos_RespectsCooldown(t *testing.T) {
	f := newFakeForge(t)
	opts := testOptions()
	opts.Owner = "owner"
	p := New(f, nil, opts)

	if err := p.refreshRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls, _, _ := f.counts(); calls != 1 {
		t.Fatalf("expected 1 discovery call on the first refresh, got %d", calls)
	}

	if err := p.refreshRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls, _, _ := f.counts(); calls != 1 {
		t.Errorf("expected no additional calls during the cooldown, got %d", calls)
	}
}

// --- poll and Run ---

func TestPoll_CallsBothPhases(t *testing.T) {
	f := newFakeForge(t)
	probed := false
	f.activeRuns = func(string) ([]Run, error) {
		probed = true
		return nil, nil
	}
	p, _, _ := newTestPoller(t, testOptions(), f)
	// A tracked repo with no pending end is what pollActive walks.
	p.tracked["other/tracked"] = &trackedRun{RunID: 1, Slug: "fk-other"}
	f.liveJobs = func(string, int64) ([]ci.Job, error) { return nil, nil }

	if err := p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !probed {
		t.Error("poll must run the idle phase")
	}
	if _, _, jobCalls := f.counts(); jobCalls == 0 {
		t.Error("poll must run the active phase")
	}
}

func TestRun_ShutsDownOnContextCancel(t *testing.T) {
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) { return nil, nil }
	opts := testOptions()
	opts.IdleInterval = 100 * time.Millisecond
	opts.Repos = []string{testRepo}
	p, _, _ := newTestPoller(t, opts, f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error on graceful shutdown, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within the timeout")
	}
}

// Shutdown drains in-flight end phases: the defer collects the timer groups under
// the lock, then Closes and Waits outside it, because a phase callback re-takes
// the same lock.
func TestRun_DrainsPendingEndTimers(t *testing.T) {
	f := newFakeForge(t)
	opts := testOptions()
	opts.IdleInterval = time.Hour // won't tick
	p, _, _ := newTestPoller(t, opts, f)
	p.repos = nil

	tg := &syncx.TimerGroup{}
	tg.Reset(time.Hour, func() {})
	p.tracked[testRepo] = &trackedRun{RunID: 42, Slug: "fk-repo", endTimers: tg}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within the timeout: the timer drain deadlocked")
	}
}

// DiscoveryRequired is the whole difference between the two bridges' startup
// behavior: one has an explicit repo list to fall back on, the other does not.
func TestRun_DiscoveryFailure(t *testing.T) {
	tests := []struct {
		name     string
		required bool
		wantErr  bool
	}{
		{name: "required is fatal", required: true, wantErr: true},
		{name: "optional carries on", required: false, wantErr: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeForge(t)
			f.reposErr = errors.New("403 from the forge")
			f.activeRuns = func(string) ([]Run, error) { return nil, nil }

			opts := testOptions()
			opts.Owner = "owner"
			opts.Repos = []string{testRepo}
			opts.IdleInterval = time.Hour
			opts.DiscoveryRequired = tc.required
			p, _, _ := newTestPoller(t, opts, f)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- p.Run(ctx) }()

			if tc.wantErr {
				select {
				case err := <-done:
					if err == nil {
						t.Error("expected a fatal error when discovery is required")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Run should have returned immediately")
				}
				return
			}

			// It should keep polling the configured repos instead of exiting.
			time.Sleep(50 * time.Millisecond)
			select {
			case err := <-done:
				t.Fatalf("Run exited early with %v; it should have carried on", err)
			default:
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("expected nil error on shutdown, got %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not exit within the timeout")
			}
		})
	}
}

// --- the branches the clamp and the promote-after-send ordering exist for ---

// TestPollActive_ShapeGrowthResendsTheLadder covers the branch the whole clamp
// exists for: a forge revealing a later job wave. The denominator must climb and
// the tick must carry the new ladder, because the server preserves the old slices
// under merge-patch and would otherwise keep drawing the smaller shape.
func TestPollActive_ShapeGrowthResendsTheLadder(t *testing.T) {
	tick := 0
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		tick++
		jobs := []ci.Job{
			job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
			job("Build", ci.StatusInProgress, ""),
		}
		if tick > 1 {
			// The needs/if-gated wave the forge had not created yet.
			jobs = append(jobs,
				job("Scan", ci.StatusQueued, ""),
				job("Publish", ci.StatusQueued, ""))
		}
		return jobs, nil
	}
	tracked := liveTrackedRun(nil)
	tracked.maxTotalSteps = 2
	tracked.maxStepRows = []int{1, 1}
	tracked.maxStepLabels = []string{"Lint", "Build"}
	tracked.shapeSent = 2 // the seed already landed
	tracked.liveSent = false
	p, patches := trackedPoller(t, testOptionsRender(true, false), f, tracked)

	for i := 0; i < 2; i++ {
		if err := p.pollActive(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	got := patches(2)
	if len(got) != 2 {
		t.Fatalf("expected 2 PATCH calls, got %d", len(got))
	}

	// Tick 1: nothing grew, so the unchanged ladder stays off the wire.
	if bytes.Contains(got[0].Body, []byte(`"step_rows"`)) {
		t.Errorf("an unchanged shape must not re-send step_rows; body: %s", got[0].Body)
	}

	// Tick 2: the shape grew, so the whole ladder ships again.
	var grown pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &grown)
	if want := 4; stepValue(grown.Content.TotalSteps) != want {
		t.Errorf("total_steps = %d, want %d after the new wave", stepValue(grown.Content.TotalSteps), want)
	}
	wantLabels := []string{"Lint", "Build", "Scan", "Publish"}
	if !reflect.DeepEqual(grown.Content.StepLabels, wantLabels) {
		t.Errorf("step_labels = %v, want %v", grown.Content.StepLabels, wantLabels)
	}
	if want := []int{1, 1, 1, 1}; !reflect.DeepEqual(grown.Content.StepRows, want) {
		t.Errorf("step_rows = %v, want %v", grown.Content.StepRows, want)
	}
	if len(grown.Content.StepColors) != 4 {
		t.Errorf("step_colors = %v, want one per step", grown.Content.StepColors)
	}

	// The cached shape is what later ticks clamp against, so it has to have moved.
	p.mu.Lock()
	defer p.mu.Unlock()
	tt := p.tracked[testRepo]
	if tt.maxTotalSteps != 4 || tt.shapeSent != 4 {
		t.Errorf("cached shape = %d/%d sent, want 4/4", tt.maxTotalSteps, tt.shapeSent)
	}
}

// A failed seed must not promote shapeSent, or every later tick skips the ladder
// forever and the card is left with no step_rows at all.
func TestPollIdle_FailedSeedLeavesTheLadderOwed(t *testing.T) {
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) { return []Run{activeRun(42, "CI", "main")}, nil }
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{
			job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
			job("Build", ci.StatusInProgress, ""),
		}, nil
	}

	// Fail the seed PATCH, then let the recovery tick through.
	srv, calls, mu := testutil.MockPushWardServerFailingPatches(t, 1)
	p := New(f, pushward.NewClient(srv.URL, "hlk_test"), testOptions())
	p.repos = []string{testRepo}

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	tt, ok := p.tracked[testRepo]
	shapeSent, maxTotal := 0, 0
	if ok {
		shapeSent, maxTotal = tt.shapeSent, tt.maxTotalSteps
	}
	p.mu.Unlock()
	if !ok {
		t.Fatal("a failed seed must still leave the run tracked")
	}
	if shapeSent != 0 {
		t.Errorf("shapeSent = %d, want 0: a failed seed owes the ladder", shapeSent)
	}
	if maxTotal != 2 {
		t.Errorf("maxTotalSteps = %d, want the scanned 2", maxTotal)
	}

	// The next tick pays the debt: shapeSent < maxTotalSteps makes shapeChanged true.
	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	var recovered pushward.UpdateRequest
	found := false
	for _, c := range testutil.GetCalls(calls, mu) {
		if c.Method == http.MethodPatch && bytes.Contains(c.Body, []byte(`"step_rows"`)) {
			testutil.UnmarshalBody(t, c.Body, &recovered)
			found = true
		}
	}
	if !found {
		t.Fatal("the recovery tick must re-send the ladder the failed seed never delivered")
	}
	if want := []int{1, 1}; !reflect.DeepEqual(recovered.Content.StepRows, want) {
		t.Errorf("recovered step_rows = %v, want %v", recovered.Content.StepRows, want)
	}
}

// A failed tick must not promote the scalars either, so the next tick re-evaluates
// and re-sends rather than concluding nothing changed.
func TestPollActive_FailedPatchIsRetriedNextTick(t *testing.T) {
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{
			job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
			job("Build", ci.StatusInProgress, ""),
			job("Test", ci.StatusQueued, ""),
		}, nil
	}
	srv, calls, mu := testutil.MockPushWardServerFailingPatches(t, 1)
	p := New(f, pushward.NewClient(srv.URL, "hlk_test"), testOptions())
	tracked := liveTrackedRun(nil)
	tracked.liveSent = false
	p.tracked = map[string]*trackedRun{testRepo: tracked}

	// First tick's PATCH fails.
	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	promoted := !p.tracked[testRepo].lastPatchAt.IsZero()
	p.mu.Unlock()
	if promoted {
		t.Error("a failed patch must not promote lastPatchAt")
	}

	// Second tick sees identical jobs and must still send, because nothing landed.
	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	patches := 0
	for _, c := range testutil.GetCalls(calls, mu) {
		if c.Method == http.MethodPatch {
			patches++
		}
	}
	if patches < 2 {
		t.Errorf("got %d PATCH attempts, want the failed one retried", patches)
	}
}

// The heartbeat is the only reason an otherwise-identical tick patches. Without
// it a run parked on one long step goes silent and the server dismisses the card.
func TestPollActive_HeartbeatPatchesAnUnchangedRun(t *testing.T) {
	f := newFakeForge(t)
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{
			job("Lint", ci.StatusCompleted, ci.ConclusionSuccess),
			job("Build", ci.StatusInProgress, ""),
			job("Test", ci.StatusQueued, ""),
		}, nil
	}
	tracked := liveTrackedRun(nil)
	tracked.liveSent = false
	opts := testOptions() // StaleTimeout 30m, so the heartbeat is due after 15m
	p, patches := trackedPoller(t, opts, f, tracked)

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := patches(1); len(got) != 1 {
		t.Fatalf("expected the first tick to patch, got %d", len(got))
	}

	// An identical tick is suppressed...
	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := patches(1); len(got) != 1 {
		t.Fatalf("an identical tick must be suppressed, got %d patches", len(got))
	}

	// ...until the heartbeat comes due.
	p.mu.Lock()
	p.tracked[testRepo].lastPatchAt = time.Now().Add(-(opts.PushWard.StaleTimeout/2 + time.Minute))
	p.mu.Unlock()

	if err := p.pollActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := patches(2)
	if len(got) != 2 {
		t.Fatalf("the heartbeat must patch an unchanged run, got %d patches", len(got))
	}
	// It carries the same scalars - its whole job is to prove the bridge is alive.
	var beat pushward.UpdateRequest
	testutil.UnmarshalBody(t, got[1].Body, &beat)
	if beat.Content.State != "Build" {
		t.Errorf("heartbeat state = %q, want the unchanged Build", beat.Content.State)
	}
}

// Close must cancel a phase 2 that is already armed, not merely stop it being
// re-armed. This is the load-bearing half of the Close-not-Stop choice: the slug
// is per-repo, so an ENDED delivered after a new run took the repo over would
// dismiss the new run's card.
//
// The between-phases RunID guard in scheduleEnd is belt-and-braces behind this -
// unreachable in production precisely because Close gets there first - so this
// tests the mechanism that does the work.
func TestScheduleEnd_CloseCancelsAnArmedPhaseTwo(t *testing.T) {
	f := newFakeForge(t)
	opts := testOptions()
	opts.PushWard.EndDelay = 20 * time.Millisecond
	opts.PushWard.EndDisplayTime = 500 * time.Millisecond

	srv, calls, mu := testutil.MockPushWardServer(t)
	pw := pushward.NewClient(srv.URL, "hlk_test")
	if err := pw.CreateActivity(context.Background(), "fk-repo", "Forge: repo", 1, 900, 1800); err != nil {
		t.Fatal(err)
	}
	p := New(f, pw, opts)
	p.tracked = map[string]*trackedRun{testRepo: {RunID: 300, Slug: "fk-repo", Name: "CI"}}

	p.scheduleEnd(context.Background(), testRepo, pushward.Content{
		Template: pushward.TemplateSteps, State: OutcomeSuccess, Progress: 1.0,
		Icon: defaultIcon, AccentColor: pushward.ColorGreen,
	})

	// Let phase 1 land and arm phase 2.
	time.Sleep(100 * time.Millisecond)
	var phase1 int
	for _, c := range testutil.GetCalls(calls, mu) {
		if c.Method == http.MethodPatch {
			phase1++
		}
	}
	if phase1 != 1 {
		t.Fatalf("expected phase 1 to have landed, got %d patches", phase1)
	}

	// Supersede the way pollIdle does: Close the group, then drop the entry.
	p.mu.Lock()
	p.tracked[testRepo].endTimers.Close()
	delete(p.tracked, testRepo)
	p.tracked[testRepo] = &trackedRun{RunID: 301, Slug: "fk-repo", Name: "CI v2"}
	p.mu.Unlock()

	// Well past when phase 2 would have fired.
	time.Sleep(600 * time.Millisecond)

	for _, c := range testutil.GetCalls(calls, mu) {
		if c.Method != http.MethodPatch {
			continue
		}
		var req pushward.UpdateRequest
		testutil.UnmarshalBody(t, c.Body, &req)
		if req.State == pushward.StateEnded {
			t.Error("Close left an armed phase 2 to dismiss the superseding run's card")
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	tr, ok := p.tracked[testRepo]
	if !ok {
		t.Fatal("the superseding run's entry was deleted")
	}
	if tr.RunID != 301 {
		t.Errorf("RunID = %d, want the superseding run 301", tr.RunID)
	}
}

// An explicit Icon has to reach the wire, not just the Options struct: asserting
// only New().opts.Icon would still pass if the loop hard-coded defaultIcon and
// every bridge silently lost its configured symbol.
func TestPollIdle_ExplicitIconReachesTheWire(t *testing.T) {
	f := newFakeForge(t)
	f.activeRuns = func(string) ([]Run, error) { return []Run{activeRun(42, "CI", "main")}, nil }
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{job("Build", ci.StatusInProgress, "")}, nil
	}
	opts := testOptions()
	opts.Icon = "hammer.fill"
	p, calls, mu := newTestPoller(t, opts, f)

	if err := p.pollIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	req, _ := seedFrame(t, calls, mu)
	if req.Content.Icon != "hammer.fill" {
		t.Errorf("icon = %q, want the configured hammer.fill", req.Content.Icon)
	}
}

// Run's periodic body - the ticker receive, the repeated refreshRepos and the
// repeated Poll - is what the process actually spends its life in, and no test
// reached it: every other Run test either uses a one-hour interval or cancels
// before the first tick.
func TestRun_PollsPeriodicallyAndHoldsTheDiscoveryCooldown(t *testing.T) {
	probes := 0
	var mu sync.Mutex
	f := newFakeForge(t)
	f.repos = []string{testRepo}
	f.activeRuns = func(string) ([]Run, error) {
		mu.Lock()
		probes++
		mu.Unlock()
		return nil, nil
	}

	opts := testOptions()
	opts.Owner = "owner"
	opts.IdleInterval = 10 * time.Millisecond
	p, _, _ := newTestPoller(t, opts, f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected a clean shutdown, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within the timeout")
	}

	mu.Lock()
	got := probes
	mu.Unlock()
	// The startup poll plus at least one ticked poll.
	if got < 2 {
		t.Errorf("probed %d times across ~15 intervals, want the ticker body to have run", got)
	}
	// Discovery is on a 5-minute cooldown, so all those ticks share one enumeration.
	if repoCalls, _, _ := f.counts(); repoCalls != 1 {
		t.Errorf("enumerated the owner %d times, want 1: the cooldown must hold across ticks", repoCalls)
	}
}
