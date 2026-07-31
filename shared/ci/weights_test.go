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

	// No history -> nil so the send omits step_weights (equal-width pills).
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
