package cipoll

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/ci"
)

// An unset active tier takes the shared default, so an adapter that leaves it out
// gets the same cadence a bridge's config load would have resolved.
func TestNewAppliesTheActiveDefault(t *testing.T) {
	tests := []struct {
		name   string
		idle   time.Duration
		active time.Duration
		want   time.Duration
	}{
		{name: "unset takes the shared default", idle: 60 * time.Second, active: 0, want: 15 * time.Second},
		// Deliberately not 15s: that is what the default would produce anyway, so the
		// case could not tell "kept" from "overwritten".
		{name: "set is kept", idle: 60 * time.Second, active: 30 * time.Second, want: 30 * time.Second},
		{name: "equal is left alone", idle: 10 * time.Second, active: 10 * time.Second, want: 10 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			opts.Polling.IdleInterval = tc.idle
			opts.Polling.Interval = tc.active
			p := New(newFakeForge(t), nil, opts)
			if p.opts.Polling.Interval != tc.want {
				t.Errorf("Interval = %s, want %s", p.opts.Polling.Interval, tc.want)
			}
			if p.opts.Polling.IdleInterval != tc.idle {
				t.Errorf("IdleInterval = %s, want %s", p.opts.Polling.IdleInterval, tc.idle)
			}
		})
	}
}

// The whole point of the split: a run already on someone's lock screen advances on
// the fast tier while the expensive per-repo detection sweep stays on the slow one.
func TestPoll_ActiveTierAdvancesBetweenDetectionPasses(t *testing.T) {
	const otherRepo = "owner/other"

	f := newFakeForge(t)
	var idleProbes atomic.Int32
	f.activeRuns = func(repo string) ([]Run, error) {
		if repo != otherRepo {
			t.Errorf("detection probed %q, which is already tracked", repo)
		}
		idleProbes.Add(1)
		return nil, nil
	}
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		return []ci.Job{job("Build", ci.StatusInProgress, "")}, nil
	}

	opts := testOptions()
	opts.Polling.IdleInterval = time.Hour // detection must not come due a second time
	opts.Polling.Interval = time.Millisecond
	p, _ := trackedPoller(t, opts, f, liveTrackedRun(priorDurations()))
	p.repos = []string{testRepo, otherRepo}

	for range 3 {
		if err := p.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	if got := idleProbes.Load(); got != 1 {
		t.Errorf("detection ran %d times across 3 cycles, want 1: the idle tier must not follow the active one", got)
	}
	if _, _, live := f.counts(); live != 3 {
		t.Errorf("the tracked run advanced %d times, want 3", live)
	}
}

// Pins the pacing arithmetic: it only ever slows detection down, it withholds a
// reserve sized from the runs in flight, and it needs no state to unwind.
func TestEffectiveIdleInterval(t *testing.T) {
	const repos = 44

	tests := []struct {
		name      string
		known     bool
		remaining int
		resetIn   time.Duration
		idle      time.Duration
		active    time.Duration
		tracked   int
		want      time.Duration
	}{
		{
			// 44 repos a minute for an hour is 2,640 requests, well inside what is
			// left, so nothing changes.
			name:  "a comfortable budget leaves the configured interval alone",
			known: true, remaining: 4900, resetIn: time.Hour, idle: time.Minute,
			want: time.Minute,
		},
		{
			// The same plan needs 2,640 but only 1,000 are left, so the gap widens to
			// 44 * 3600 / 1000.
			name:  "a tight budget stretches detection to fit",
			known: true, remaining: 1000, resetIn: time.Hour, idle: time.Minute,
			want: 158 * time.Second,
		},
		{
			// One tracked run at 15s needs ~240 requests to reach a reset an hour out,
			// and that reserve is withheld from detection - so 100 remaining leaves
			// nothing to detect with and detection waits the window out.
			name:  "a reserve that consumes the remainder waits for the reset",
			known: true, remaining: 100, resetIn: time.Hour, idle: time.Minute,
			active: 15 * time.Second, tracked: 1,
			want: time.Hour,
		},
		{
			// The reserve scales with the number of cards on screen: the same budget
			// that funds one run leaves nothing over for a second.
			name:  "the reserve scales with tracked runs",
			known: true, remaining: 300, resetIn: time.Hour, idle: time.Minute,
			active: 15 * time.Second, tracked: 2,
			want: time.Hour,
		},
		{
			// Before the first response there is nothing to pace against. This is also
			// every self-hosted Forgejo, which publishes no rate-limit headers at all.
			name:  "an unknown budget leaves the configured interval alone",
			known: false, idle: time.Minute, resetIn: time.Hour,
			want: time.Minute,
		},
		{
			// The window has rolled over; the next response re-reads the real numbers.
			name:  "a window already past leaves the configured interval alone",
			known: true, remaining: 0, resetIn: -time.Minute, idle: time.Minute,
			want: time.Minute,
		},
		{
			// Pacing only ever slows detection down, never speeds it up.
			name:  "a huge budget does not shorten the configured interval",
			known: true, remaining: 5000, resetIn: time.Hour, idle: 5 * time.Minute,
			want: 5 * time.Minute,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeForge(t)
			if tc.known {
				f.setBudget(tc.remaining, tc.resetIn)
			}
			opts := testOptions()
			opts.Polling.IdleInterval = tc.idle
			if tc.active > 0 {
				opts.Polling.Interval = tc.active
			}
			opts.HourlyRequestBudget = 5000
			p := New(f, nil, opts)
			for i := range tc.tracked {
				p.tracked["owner/tracked"+strconv.Itoa(i)] = &trackedRun{}
			}

			got := p.effectiveIdleInterval(repos)
			// A second of slack: the arithmetic reads a live clock, so the reset
			// distance shrinks slightly between setBudget and here.
			if diff := got - tc.want; diff > time.Second || diff < -2*time.Second {
				t.Errorf("effectiveIdleInterval = %s, want ~%s", got, tc.want)
			}
		})
	}
}

// This is the assertion that matters about degrading gracefully: when the allowance
// runs out, discovery and detection stop, and the run already on someone's lock
// screen keeps moving. The old behavior was to block the loop's only goroutine
// until the window reset, with every tracked card frozen.
func TestPoll_ExhaustedBudgetStopsDetectionButNotTrackedRuns(t *testing.T) {
	const otherRepo = "owner/other"

	f := newFakeForge(t)
	f.repos = []string{testRepo, otherRepo}
	var idleProbes atomic.Int32
	f.activeRuns = func(string) ([]Run, error) {
		idleProbes.Add(1)
		return nil, nil
	}
	// A step name that changes every tick, so redundant-tick suppression cannot be
	// what makes the patches stop.
	var tick atomic.Int32
	f.liveJobs = func(string, int64) ([]ci.Job, error) {
		name := "Build"
		if tick.Add(1)%2 == 0 {
			name = "Test"
		}
		return []ci.Job{job(name, ci.StatusInProgress, "")}, nil
	}
	f.setBudget(4900, time.Second) // comfortable to begin with

	opts := testOptions()
	opts.Owner = "owner"
	opts.HourlyRequestBudget = 5000
	// Short enough that detection would be due on every cycle if the budget allowed.
	opts.Polling.IdleInterval = 10 * time.Millisecond
	opts.Polling.Interval = 10 * time.Millisecond
	p, patches := trackedPoller(t, opts, f, liveTrackedRun(priorDurations()))
	p.repos = []string{testRepo, otherRepo}

	ctx := context.Background()
	if err := p.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := idleProbes.Load(); got != 1 {
		t.Fatalf("detection ran %d times on a healthy budget, want 1", got)
	}

	// The window runs dry: one tracked run at 10ms needs ~101 requests to reach a
	// reset a second out, so 50 remaining leaves detection nothing.
	f.setBudget(50, time.Second)

	if err := p.refreshRepos(ctx); err != nil {
		t.Fatalf("a skipped discovery must not surface as an error: %v", err)
	}
	if repoCalls, _, _ := f.counts(); repoCalls != 0 {
		t.Errorf("discovery ran %d times with nothing spendable, want 0", repoCalls)
	}

	for range 3 {
		if err := p.Poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := idleProbes.Load(); got != 1 {
		t.Errorf("detection ran %d times total, want 1: it must stop once the budget is spent", got)
	}
	// Four cycles, four advances of the tracked run: the active tier never sheds.
	if _, _, live := f.counts(); live != 4 {
		t.Errorf("the tracked run advanced %d times, want 4", live)
	}
	if got := patches(4); len(got) != 4 {
		t.Errorf("got %d patches, want 4: a degraded poller must keep the card alive", len(got))
	}
}

// Discovery is the first thing dropped, and it must come back on its own once the
// window refills - there is no state to unwind.
func TestRefreshRepos_ResumesWhenTheBudgetRecovers(t *testing.T) {
	f := newFakeForge(t)
	f.repos = []string{testRepo}

	opts := testOptions()
	opts.Owner = "owner"
	opts.HourlyRequestBudget = 5000
	opts.Polling.Interval = 15 * time.Second
	p, _, _ := newTestPoller(t, opts, f)
	p.tracked["owner/tracked"] = &trackedRun{} // one card to reserve for

	// That card alone needs ~240 requests to reach a reset an hour out, so 100 left
	// is nothing to spend on detection.
	f.setBudget(100, time.Hour)
	ctx := context.Background()
	if err := p.refreshRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if repoCalls, _, _ := f.counts(); repoCalls != 0 {
		t.Fatalf("discovery ran %d times with nothing spendable, want 0", repoCalls)
	}

	f.setBudget(4900, time.Hour)
	if err := p.refreshRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if repoCalls, _, _ := f.counts(); repoCalls != 1 {
		t.Errorf("discovery ran %d times after the budget recovered, want 1", repoCalls)
	}
}

// The pacing log reports a state change, not a change of interval: the paced value
// drifts every tick as the window drains, so comparing durations would warn on
// almost every tick for the rest of the window.
func TestClaimIdlePass_LogsPacingOncePerTransition(t *testing.T) {
	var buf bytes.Buffer
	f := newFakeForge(t)

	opts := testOptions()
	opts.Polling.IdleInterval = time.Minute
	opts.Polling.Interval = 15 * time.Second
	opts.HourlyRequestBudget = 5000
	opts.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	p := New(f, nil, opts)
	p.repos = make([]string, 44)
	for i := range p.repos {
		p.repos[i] = "owner/repo" + strconv.Itoa(i)
	}

	// Tight enough to pace, and with a reset that keeps moving so the computed
	// interval differs on every call.
	f.setBudget(1000, time.Hour)
	for range 5 {
		p.claimIdlePass()
	}
	if got := strings.Count(buf.String(), "paced by the forge's request budget"); got != 1 {
		t.Errorf("logged the pacing %d times across 5 claims, want 1", got)
	}

	buf.Reset()
	f.setBudget(4900, time.Hour)
	for range 5 {
		p.claimIdlePass()
	}
	if got := strings.Count(buf.String(), "back to the configured value"); got != 1 {
		t.Errorf("logged the recovery %d times across 5 claims, want 1", got)
	}
}

// The startup line is the whole early-warning story: nothing else tells an operator
// that a repo count and an interval do not fit until the bridge quietly runs out of
// allowance mid-window. 44 repos at 30s is 5,280 requests an hour against 5,000, the
// configuration that actually shipped.
func TestLogRequestBudget(t *testing.T) {
	repos := make([]string, 44)
	for i := range repos {
		repos[i] = "owner/repo" + strconv.Itoa(i)
	}

	tests := []struct {
		name     string
		idle     time.Duration
		budget   int
		wantWarn bool
	}{
		{name: "44 repos at 30s exceeds the budget", idle: 30 * time.Second, budget: 5000, wantWarn: true},
		{name: "44 repos at 60s fits", idle: time.Minute, budget: 5000, wantWarn: false},
		{name: "no budget figure never warns", idle: time.Second, budget: 0, wantWarn: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			opts := testOptions()
			opts.Owner = "owner"
			opts.Polling.IdleInterval = tc.idle
			opts.Polling.Interval = min(15*time.Second, tc.idle)
			opts.HourlyRequestBudget = tc.budget
			opts.Logger = slog.New(slog.NewTextHandler(&buf, nil))

			p := New(newFakeForge(t), nil, opts)
			p.repos = repos
			p.logRequestBudget()

			out := buf.String()
			if !strings.Contains(out, "estimated_requests_per_hour") {
				t.Fatalf("the startup line must state the computed rate, got %q", out)
			}
			if gotWarn := strings.Contains(out, "level=WARN"); gotWarn != tc.wantWarn {
				t.Errorf("warned = %v, want %v; line was %q", gotWarn, tc.wantWarn, out)
			}
		})
	}
}

// staleAfter has to outlast whichever tier is slower, or a run gets evicted one
// cycle after it was created.
func TestStaleAfterOutlastsBothTiers(t *testing.T) {
	opts := testOptions()
	opts.PushWard.StaleTimeout = 0
	opts.Polling.IdleInterval = 10 * time.Minute
	opts.Polling.Interval = 15 * time.Second
	p := New(newFakeForge(t), nil, opts)

	if got, want := p.staleAfter(), 10*time.Minute+staleEvictionGrace; got != want {
		t.Errorf("staleAfter = %s, want %s", got, want)
	}
}
