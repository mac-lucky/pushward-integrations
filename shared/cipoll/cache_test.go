package cipoll

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/mac-lucky/pushward-integrations/shared/ci"
)

func TestShapeCache_PrefersSuccess(t *testing.T) {
	c := newShapeCache(maxSeeds)
	good := map[string]float64{"Lint": 5, "Build": 300, "Test": 40}
	c.put(testRepo, "99", seedEntry{shape: threeStepShape(), weights: good, runID: 41, success: true})

	// A failed run stopped early: its durations would count the next run down
	// to the wrong deadline, so it never displaces a successful measurement.
	c.put(testRepo, "99", seedEntry{shape: threeStepShape(), weights: map[string]float64{"Lint": 5, "Build": 12, "Test": 1}, runID: 42, success: false})
	got, ok := c.get(testRepo, "99")
	if !ok || got.runID != 41 || !reflect.DeepEqual(got.weights, good) {
		t.Fatalf("entry = %+v, want the successful run 41 kept", got)
	}

	// The next success replaces it.
	c.put(testRepo, "99", seedEntry{shape: threeStepShape(), weights: map[string]float64{"Lint": 6, "Build": 310, "Test": 42}, runID: 43, success: true})
	if got, _ := c.get(testRepo, "99"); got.runID != 43 {
		t.Errorf("runID = %d, want the newer success 43", got.runID)
	}

	// With nothing better stored, a failed run is still a seed.
	c.put(testRepo, "77", seedEntry{shape: threeStepShape(), weights: map[string]float64{"Lint": 5}, runID: 44, success: false})
	if got, ok := c.get(testRepo, "77"); !ok || got.runID != 44 {
		t.Errorf("entry = %+v ok=%v, want the failed run filed when nothing else is", got, ok)
	}
}

// TestShapeCache_KeepsMeasuredWeightsOverUnmeasured covers the final tick whose
// task page came back short: the shape is fresh, but some or all of the
// durations it would file are missing, and one such tick must not throw away
// what the same shape already measured.
func TestShapeCache_KeepsMeasuredWeightsOverUnmeasured(t *testing.T) {
	c := newShapeCache(maxSeeds)
	weights := map[string]float64{"Lint": 5, "Build": 300, "Test": 40}
	c.put(testRepo, "99", seedEntry{shape: threeStepShape(), weights: weights, runID: 41, success: true})
	c.put(testRepo, "99", seedEntry{shape: threeStepShape(), weights: nil, runID: 42, success: true})

	got, _ := c.get(testRepo, "99")
	if got.runID != 42 {
		t.Errorf("runID = %d, want the newer run 42", got.runID)
	}
	if !reflect.DeepEqual(got.weights, weights) {
		t.Errorf("weights = %v, want the measured ones carried over", got.weights)
	}

	// Half a page: the groups the new run measured win, the rest keep the stored
	// value. Build's earlier 300 stands; Test's fresh 45 replaces its 40.
	partial := map[string]float64{"Lint": 6, "Build": ci.StepWeightFloor, "Test": 45}
	c.put(testRepo, "99", seedEntry{shape: threeStepShape(), weights: partial, runID: 43, success: true})
	got, _ = c.get(testRepo, "99")
	if want := map[string]float64{"Lint": 6, "Build": 300, "Test": 45}; !reflect.DeepEqual(got.weights, want) {
		t.Errorf("weights = %v, want the merge %v", got.weights, want)
	}
	if !reflect.DeepEqual(partial, map[string]float64{"Lint": 6, "Build": ci.StepWeightFloor, "Test": 45}) {
		t.Error("put must not mutate the caller's map")
	}

	// A different shape is a different workflow definition: its weights would
	// not line up with the labels, so nil stays nil.
	changed := threeStepShape()
	changed.StepLabels = []string{"Lint", "Build", "Deploy"}
	c.put(testRepo, "99", seedEntry{shape: changed, weights: nil, runID: 43, success: true})
	if got, _ := c.get(testRepo, "99"); got.weights != nil {
		t.Errorf("weights = %v, want nil once the labels changed", got.weights)
	}
}

func TestShapeCache_EvictsOldest(t *testing.T) {
	c := newShapeCache(3)
	for i := range 3 {
		c.put(testRepo, strconv.Itoa(i), seedEntry{shape: threeStepShape(), weights: nil, runID: int64(i), success: true})
	}
	// Touching 0 makes it the newest; the next insert then evicts 1, not 0.
	c.put(testRepo, "0", seedEntry{shape: threeStepShape(), weights: nil, runID: 10, success: true})
	c.put(testRepo, "3", seedEntry{shape: threeStepShape(), weights: nil, runID: 3, success: true})

	if _, ok := c.get(testRepo, "1"); ok {
		t.Error("the oldest untouched entry should have been evicted")
	}
	for _, key := range []string{"0", "2", "3"} {
		if _, ok := c.get(testRepo, key); !ok {
			t.Errorf("entry %q should have survived", key)
		}
	}
}

func TestShapeCache_IgnoresUnfileable(t *testing.T) {
	c := newShapeCache(maxSeeds)
	c.put(testRepo, "", seedEntry{shape: threeStepShape(), weights: nil, runID: 1, success: true})
	c.put(testRepo, "99", seedEntry{shape: ci.StepInfo{}, weights: nil, runID: 2, success: true})
	for _, key := range []string{"", "99"} {
		if _, ok := c.get(testRepo, key); ok {
			t.Errorf("entry %q filed, want nothing for a blank key or an empty shape", key)
		}
	}
}

// TestShapeCache_CopiesSlices pins the aliasing rule: a caller that keeps
// mutating its scan after filing it must not reach the stored entry, and the
// per-tick scalars of that scan are not filed with it.
func TestShapeCache_CopiesSlices(t *testing.T) {
	c := newShapeCache(maxSeeds)
	shape := threeStepShape()
	shape.CurrentStep, shape.Progress = 2, 0.5
	c.put(testRepo, "99", seedEntry{shape: shape, weights: nil, runID: 1, success: true})
	shape.StepLabels[0] = "Mutated"

	got, _ := c.get(testRepo, "99")
	if got.shape.StepLabels[0] != "Lint" {
		t.Errorf("stored label = %q, want the value at put time", got.shape.StepLabels[0])
	}
	if got.shape.CurrentStep != 0 || got.shape.Progress != 0 {
		t.Errorf("stored shape = %+v, want the per-tick scalars left out", got.shape)
	}
}
