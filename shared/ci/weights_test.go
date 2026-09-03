package ci

import (
	"reflect"
	"testing"
	"time"
)

// at builds a timestamp offset from a fixed base, keeping the durations in the
// tables below readable as seconds.
func at(offset time.Duration) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(offset)
}

func TestGroupWeights(t *testing.T) {
	// Lint 5s; Build matrix runs in parallel (120/300/60s) so the group weighs the
	// longest = 300s; Deploy 40s. Keyed by group name, not position.
	jobs := []Job{
		{
			Name: "Lint", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			StartedAt: at(0), CompletedAt: at(5 * time.Second),
		},
		{
			Name: "Build (ubuntu)", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			StartedAt: at(5 * time.Second), CompletedAt: at(2*time.Minute + 5*time.Second),
		},
		{
			Name: "Build (macos)", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			StartedAt: at(5 * time.Second), CompletedAt: at(5*time.Minute + 5*time.Second),
		},
		{
			Name: "Build (windows)", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			StartedAt: at(5 * time.Second), CompletedAt: at(time.Minute + 5*time.Second),
		},
		{
			Name: "Deploy", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			StartedAt: at(5*time.Minute + 5*time.Second), CompletedAt: at(5*time.Minute + 45*time.Second),
		},
	}
	got := GroupWeights(jobs)
	want := map[string]float64{"Lint": 5, "Build": 300, "Deploy": 40}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GroupWeights = %v, want %v", got, want)
	}
	// One entry per group ComputeSteps produces, so ProjectWeights can size every
	// label.
	if n := ComputeSteps(jobs).TotalSteps; len(got) != n {
		t.Errorf("len(weights)=%d, want groups=%d", len(got), n)
	}
}

func TestGroupWeights_NoDurations(t *testing.T) {
	// Queued/in-progress jobs have no completion, and a completed job missing a
	// start is unmeasurable - with nothing to measure, return nil so callers omit
	// step_weights and pills render equal-width.
	jobs := []Job{
		{Name: "Lint", Status: StatusQueued},
		{Name: "Build", Status: StatusInProgress, StartedAt: at(5 * time.Second)},
		{
			Name: "Deploy", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			CompletedAt: at(5*time.Minute + 45*time.Second),
		},
	}
	if got := GroupWeights(jobs); got != nil {
		t.Errorf("GroupWeights = %v, want nil", got)
	}
}

func TestGroupWeights_Floor(t *testing.T) {
	// A present-but-unmeasurable group sits alongside a measured one: it keeps the
	// floor (a thin pill) rather than collapsing, and the measured group wins its
	// real duration.
	jobs := []Job{
		{
			Name: "Lint", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			CompletedAt: at(5 * time.Second),
		}, // no StartedAt -> unmeasurable
		{
			Name: "Build", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			StartedAt: at(5 * time.Second), CompletedAt: at(10 * time.Second),
		},
	}
	got := GroupWeights(jobs)
	want := map[string]float64{"Lint": StepWeightFloor, "Build": 5}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GroupWeights = %v, want %v", got, want)
	}
}

// TestGroupWeights_ClockSkew pins that a job whose completion precedes its start
// is unmeasurable rather than negative. Forgejo joins its timings from a
// separate task list, so a mismatched pair is a real possibility there.
func TestGroupWeights_ClockSkew(t *testing.T) {
	jobs := []Job{
		{
			Name: "Lint", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			StartedAt: at(10 * time.Second), CompletedAt: at(5 * time.Second),
		},
		{
			Name: "Build", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			StartedAt: at(0), CompletedAt: at(20 * time.Second),
		},
	}
	got := GroupWeights(jobs)
	want := map[string]float64{"Lint": StepWeightFloor, "Build": 20}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GroupWeights = %v, want %v", got, want)
	}
}

// TestGroupWeights_SerializedShardsUseTheSpan pins the span over MAX: shards
// that queue behind each other on a busy runner hold the run up for their sum,
// and that is the window the countdown has to cover. MAX would give 100 here
// and end the ETA after the first shard.
func TestGroupWeights_SerializedShardsUseTheSpan(t *testing.T) {
	jobs := []Job{
		{
			Name: "tofu (a)", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			StartedAt: at(0), CompletedAt: at(100 * time.Second),
		},
		{
			Name: "tofu (b)", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			StartedAt: at(100 * time.Second), CompletedAt: at(200 * time.Second),
		},
	}
	if got := GroupWeights(jobs)["tofu"]; got != 200 {
		t.Errorf("tofu = %v, want the 200s span", got)
	}
}

// TestGroupWeights_StaggeredShards covers the in-between case: parallel shards
// whose pickups were staggered by runner capacity. The span is first start to
// last end, not the slowest shard on its own.
func TestGroupWeights_StaggeredShards(t *testing.T) {
	jobs := []Job{
		{
			Name: "test (a)", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			StartedAt: at(0), CompletedAt: at(300 * time.Second),
		},
		{
			Name: "test (b)", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			StartedAt: at(60 * time.Second), CompletedAt: at(330 * time.Second),
		},
	}
	if got := GroupWeights(jobs)["test"]; got != 330 {
		t.Errorf("test = %v, want the 330s span", got)
	}
}

// TestGroupWeights_StartOnlyShardTightensTheSpan pins the one-sided rule: a
// shard with a start but no completion still says when the group began. Requiring
// both stamps on one job would discard that and measure from the later shard.
func TestGroupWeights_StartOnlyShardTightensTheSpan(t *testing.T) {
	jobs := []Job{
		{Name: "build (a)", Status: StatusInProgress, StartedAt: at(0)},
		{
			Name: "build (b)", Status: StatusCompleted, Conclusion: ConclusionSuccess,
			StartedAt: at(40 * time.Second), CompletedAt: at(100 * time.Second),
		},
	}
	if got := GroupWeights(jobs)["build"]; got != 100 {
		t.Errorf("build = %v, want 100s from the earlier shard's start", got)
	}
}

func TestEvenWeights(t *testing.T) {
	labels := []string{"lint", "test", "deploy"}
	got := EvenWeights(labels, 300*time.Second)
	want := map[string]float64{"lint": 100, "test": 100, "deploy": 100}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EvenWeights = %v, want %v", got, want)
	}
	// Nothing to spread, or a share that does not clear the floor: nil, so the
	// "nil means unmeasured" convention the anchor relies on still holds.
	if got := EvenWeights(nil, 300*time.Second); got != nil {
		t.Errorf("EvenWeights(no labels) = %v, want nil", got)
	}
	if got := EvenWeights(labels, 0); got != nil {
		t.Errorf("EvenWeights(zero total) = %v, want nil", got)
	}
	if got := EvenWeights(labels, 3*time.Second); got != nil {
		t.Errorf("EvenWeights(sub-floor share) = %v, want nil", got)
	}
}

func TestBaselineWeights(t *testing.T) {
	measured := []Job{
		{Name: "Lint", Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: at(0), CompletedAt: at(5 * time.Second)},
		{Name: "Build", Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: at(5 * time.Second), CompletedAt: at(305 * time.Second)},
		{Name: "Test", Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: at(305 * time.Second), CompletedAt: at(345 * time.Second)},
	}
	untimed := []Job{
		{Name: "Lint", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "Build", Status: StatusCompleted, Conclusion: ConclusionSuccess},
		{Name: "Test", Status: StatusCompleted, Conclusion: ConclusionSuccess},
	}
	labels := []string{"Lint", "Build", "Test"}

	tests := []struct {
		name   string
		jobs   []Job
		run    time.Duration
		want   map[string]float64
		source WeightsSource
	}{
		{
			name: "measured", jobs: measured, run: 345 * time.Second,
			want: map[string]float64{"Lint": 5, "Build": 300, "Test": 40}, source: WeightsMeasured,
		},
		{
			// No group ran longer than its run: a shard re-run hours later, or a
			// task row touched after the fact, cannot stretch a span past it.
			name: "clamped to the run", jobs: measured, run: 200 * time.Second,
			want: map[string]float64{"Lint": 5, "Build": 200, "Test": 40}, source: WeightsMeasured,
		},
		{
			// Every row rewritten after the run: nothing measurable, but the run's
			// own length is known, so each step counts down toward the average.
			name: "split when unmeasured", jobs: untimed, run: 300 * time.Second,
			want: map[string]float64{"Lint": 100, "Build": 100, "Test": 100}, source: WeightsSplit,
		},
		{name: "nothing at all", jobs: untimed, run: 0, want: nil, source: WeightsNone},
		{name: "a zero run clamps nothing", jobs: measured, run: 0, want: map[string]float64{"Lint": 5, "Build": 300, "Test": 40}, source: WeightsMeasured},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, source := BaselineWeights(tc.jobs, labels, tc.run)
			if !reflect.DeepEqual(got, tc.want) || source != tc.source {
				t.Errorf("BaselineWeights = %v (%s), want %v (%s)", got, source, tc.want, tc.source)
			}
		})
	}
}

func TestMeanWeight(t *testing.T) {
	tests := []struct {
		name   string
		byName map[string]float64
		want   float64
		wantOK bool
	}{
		{
			name:   "mean of the run",
			byName: map[string]float64{"Lint": 5, "Build": 300, "Deploy": 40},
			want:   115,
			wantOK: true,
		},
		{
			// Floored groups count toward the average on purpose; see the doc comment.
			name:   "floored groups are part of the average",
			byName: map[string]float64{"build": StepWeightFloor, "test": StepWeightFloor, "deploy": 100},
			want:   34,
			wantOK: true,
		},
		{
			// The value still has to be a legal pill width.
			name:   "sub-floor mean is clamped up",
			byName: map[string]float64{"Lint": 0.5},
			want:   StepWeightFloor,
			wantOK: true,
		},
		{
			// Distinguishable from "the mean is small": callers decline outright. An
			// empty map takes the same branch - len does not tell it from nil.
			name:   "no history at all",
			byName: nil,
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := meanWeight(tc.byName)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("meanWeight = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProjectWeights(t *testing.T) {
	byName := map[string]float64{"Lint": 5, "Build": 300, "Deploy": 40}

	// The core fix: weights follow their label by NAME, so a live scan that
	// surfaces the groups in a different order than the prior run still sizes each
	// pill correctly (positional alignment would have shifted them).
	got := ProjectWeights([]string{"Deploy", "Lint", "Build"}, byName)
	if want := []float64{40, 5, 300}; !reflect.DeepEqual(got, want) {
		t.Errorf("reordered projection = %v, want %v", got, want)
	}

	// A label the forge added since the prior run has no history -> mean (115) of
	// the known weights; the length still equals len(labels).
	got = ProjectWeights([]string{"Lint", "Format", "Build", "Deploy"}, byName)
	if want := []float64{5, 115, 300, 40}; !reflect.DeepEqual(got, want) {
		t.Errorf("unknown-label projection = %v, want %v", got, want)
	}

	// No history -> nil. What the send does with that is the caller's call, and
	// on a reused slug omitting the field is the one wrong answer.
	if got := ProjectWeights([]string{"Lint", "Build"}, nil); got != nil {
		t.Errorf("ProjectWeights(nil map) = %v, want nil", got)
	}

	// A sub-floor mean is clamped up so a padded pill stays visible.
	if got := ProjectWeights([]string{"Lint", "New"}, map[string]float64{"Lint": 0.5}); !reflect.DeepEqual(got, []float64{0.5, StepWeightFloor}) {
		t.Errorf("clamp = %v, want [0.5 %v]", got, StepWeightFloor)
	}
}

func TestUniformWeights(t *testing.T) {
	// Every entry at the floor: positive, so the server's per-entry check passes,
	// and all equal, so the pills come out the same width. The length is the
	// point - it is what an omission cannot promise once the server merges the
	// previous run's array forward. Equal-width is not the same as absent
	// though; see UniformWeights for the layout that trades away.
	if got, want := UniformWeights(3), []float64{StepWeightFloor, StepWeightFloor, StepWeightFloor}; !reflect.DeepEqual(got, want) {
		t.Errorf("UniformWeights(3) = %v, want %v", got, want)
	}
	// A shape with no steps has no array to send; an empty non-nil slice would be
	// dropped by omitempty anyway, so say nil and mean it.
	if got := UniformWeights(0); got != nil {
		t.Errorf("UniformWeights(0) = %v, want nil", got)
	}
	if got := UniformWeights(-1); got != nil {
		t.Errorf("UniformWeights(-1) = %v, want nil", got)
	}
}
