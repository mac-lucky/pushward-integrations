package cipoll

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/ci"
)

func TestShapeCache_PrefersSuccess(t *testing.T) {
	c := newShapeCache(maxSeeds)
	good := map[string]float64{"Lint": 5, "Build": 300, "Test": 40}
	c.put(testRepo, "99", "main", seedEntry{shape: threeStepShape(), weights: good, runID: 41, success: true})

	// A failed run stopped early: its durations would count the next run down
	// to the wrong deadline, so it never displaces a successful measurement.
	c.put(testRepo, "99", "main", seedEntry{shape: threeStepShape(), weights: map[string]float64{"Lint": 5, "Build": 12, "Test": 1}, runID: 42, success: false})
	got, match := c.get(testRepo, "99", "main")
	if match == seedMiss || got.runID != 41 || !reflect.DeepEqual(got.weights, good) {
		t.Fatalf("entry = %+v, want the successful run 41 kept", got)
	}

	// The next success replaces it.
	c.put(testRepo, "99", "main", seedEntry{shape: threeStepShape(), weights: map[string]float64{"Lint": 6, "Build": 310, "Test": 42}, runID: 43, success: true})
	if got, _ := c.get(testRepo, "99", "main"); got.runID != 43 {
		t.Errorf("runID = %d, want the newer success 43", got.runID)
	}

	// With nothing better stored, a failed run is still a seed.
	c.put(testRepo, "77", "main", seedEntry{shape: threeStepShape(), weights: map[string]float64{"Lint": 5}, runID: 44, success: false})
	if got, match := c.get(testRepo, "77", "main"); match == seedMiss || got.runID != 44 {
		t.Errorf("entry = %+v match=%q, want the failed run filed when nothing else is", got, match)
	}
}

// TestShapeCache_KeepsMeasuredWeightsOverUnmeasured covers the final tick whose
// task page came back short: the shape is fresh, but some or all of the
// durations it would file are missing, and one such tick must not throw away
// what the same shape already measured.
func TestShapeCache_KeepsMeasuredWeightsOverUnmeasured(t *testing.T) {
	c := newShapeCache(maxSeeds)
	weights := map[string]float64{"Lint": 5, "Build": 300, "Test": 40}
	c.put(testRepo, "99", "main", seedEntry{shape: threeStepShape(), weights: weights, runID: 41, success: true})
	c.put(testRepo, "99", "main", seedEntry{shape: threeStepShape(), weights: nil, runID: 42, success: true})

	got, _ := c.get(testRepo, "99", "main")
	if got.runID != 42 {
		t.Errorf("runID = %d, want the newer run 42", got.runID)
	}
	if !reflect.DeepEqual(got.weights, weights) {
		t.Errorf("weights = %v, want the measured ones carried over", got.weights)
	}

	// Half a page: the groups the new run measured win, the rest keep the stored
	// value. Build, absent from the fresh map, keeps its earlier 300; Test's
	// fresh 45 replaces its 40.
	partial := map[string]float64{"Lint": 6, "Test": 45}
	c.put(testRepo, "99", "main", seedEntry{shape: threeStepShape(), weights: partial, runID: 43, success: true})
	got, _ = c.get(testRepo, "99", "main")
	if want := map[string]float64{"Lint": 6, "Build": 300, "Test": 45}; !reflect.DeepEqual(got.weights, want) {
		t.Errorf("weights = %v, want the merge %v", got.weights, want)
	}
	if !reflect.DeepEqual(partial, map[string]float64{"Lint": 6, "Test": 45}) {
		t.Error("put must not mutate the caller's map")
	}

	// A fresh floor value is a one-second measurement, not a gap to fill: it
	// beats the stored 300.
	c.put(testRepo, "99", "main", seedEntry{shape: threeStepShape(), weights: map[string]float64{"Build": ci.StepWeightFloor}, runID: 44, success: true})
	got, _ = c.get(testRepo, "99", "main")
	if want := map[string]float64{"Lint": 6, "Build": ci.StepWeightFloor, "Test": 45}; !reflect.DeepEqual(got.weights, want) {
		t.Errorf("weights = %v, want the fresh second kept: %v", got.weights, want)
	}

	// A different shape is a different workflow definition: its weights would
	// not line up with the labels, so nil stays nil.
	changed := threeStepShape()
	changed.StepLabels = []string{"Lint", "Build", "Deploy"}
	c.put(testRepo, "99", "main", seedEntry{shape: changed, weights: nil, runID: 43, success: true})
	if got, _ := c.get(testRepo, "99", "main"); got.weights != nil {
		t.Errorf("weights = %v, want nil once the labels changed", got.weights)
	}
}

func TestShapeCache_EvictsOldest(t *testing.T) {
	c := newShapeCache(3)
	for i := range 3 {
		c.put(testRepo, strconv.Itoa(i), "main", seedEntry{shape: threeStepShape(), weights: nil, runID: int64(i), success: true})
	}
	// Touching 0 makes it the newest; the next insert then evicts 1, not 0.
	c.put(testRepo, "0", "main", seedEntry{shape: threeStepShape(), weights: nil, runID: 10, success: true})
	c.put(testRepo, "3", "main", seedEntry{shape: threeStepShape(), weights: nil, runID: 3, success: true})

	if _, match := c.get(testRepo, "1", "main"); match != seedMiss {
		t.Error("the oldest untouched entry should have been evicted")
	}
	for _, key := range []string{"0", "2", "3"} {
		if _, match := c.get(testRepo, key, "main"); match == seedMiss {
			t.Errorf("entry %q should have survived", key)
		}
	}
}

func TestShapeCache_IgnoresUnfileable(t *testing.T) {
	c := newShapeCache(maxSeeds)
	c.put(testRepo, "", "main", seedEntry{shape: threeStepShape(), weights: nil, runID: 1, success: true})
	c.put(testRepo, "99", "main", seedEntry{shape: ci.StepInfo{}, weights: nil, runID: 2, success: true})
	for _, key := range []string{"", "99"} {
		if _, match := c.get(testRepo, key, "main"); match != seedMiss {
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
	c.put(testRepo, "99", "main", seedEntry{shape: shape, weights: nil, runID: 1, success: true})
	shape.StepLabels[0] = "Mutated"

	got, _ := c.get(testRepo, "99", "main")
	if got.shape.StepLabels[0] != "Lint" {
		t.Errorf("stored label = %q, want the value at put time", got.shape.StepLabels[0])
	}
	if got.shape.CurrentStep != 0 || got.shape.Progress != 0 {
		t.Errorf("stored shape = %+v, want the per-tick scalars left out", got.shape)
	}
}

// TestShapeCache_PrefersTheRunsOwnRef: a ref with a finished run of its own
// keeps its own shape, however much newer another ref's run is. A tag path
// with a publish job and a pull request path without one are different
// workflows to the card, even when they share a file.
func TestShapeCache_PrefersTheRunsOwnRef(t *testing.T) {
	c := newShapeCache(maxSeeds)
	tag := threeStepShape()
	tag.StepLabels = []string{"Lint", "Build", "Publish"}
	c.put(testRepo, "99", "v1.2.3", seedEntry{shape: tag, runID: 40, success: true})
	c.put(testRepo, "99", "main", seedEntry{shape: threeStepShape(), runID: 41, success: true})

	got, match := c.get(testRepo, "99", "v1.2.3")
	if match != seedSameRef || got.runID != 40 {
		t.Errorf("entry = %+v match=%q, want run 40 on the tag's own ref", got, match)
	}
	got, match = c.get(testRepo, "99", "feature/new")
	if match != seedAnyRef || got.runID != 41 {
		t.Errorf("entry = %+v match=%q, want the newest any-ref run 41", got, match)
	}
	if _, match := c.get(testRepo, "77", "main"); match != seedMiss {
		t.Errorf("match = %q for a workflow never filed, want a miss", match)
	}
}

// TestShapeCache_AnyRefPrefersTheNewestSuccess mirrors the forge's any-ref
// rung: a failure that stopped early on one branch must not outrank a success
// on another, but it is still a seed when nothing succeeded anywhere.
func TestShapeCache_AnyRefPrefersTheNewestSuccess(t *testing.T) {
	c := newShapeCache(maxSeeds)
	c.put(testRepo, "99", "main", seedEntry{shape: threeStepShape(), runID: 40, success: true})
	c.put(testRepo, "99", "feature/a", seedEntry{shape: threeStepShape(), runID: 41, success: false})
	if got, match := c.get(testRepo, "99", "v1"); match != seedAnyRef || got.runID != 40 {
		t.Errorf("entry = %+v match=%q, want the success on main over the newer failure", got, match)
	}
	c.put(testRepo, "99", "feature/b", seedEntry{shape: threeStepShape(), runID: 42, success: true})
	if got, _ := c.get(testRepo, "99", "v1"); got.runID != 42 {
		t.Errorf("runID = %d, want the newest success 42", got.runID)
	}

	only := newShapeCache(maxSeeds)
	only.put(testRepo, "99", "feature/a", seedEntry{shape: threeStepShape(), runID: 41, success: false})
	if got, match := only.get(testRepo, "99", "v1"); match != seedAnyRef || got.runID != 41 {
		t.Errorf("entry = %+v match=%q, want the failure when it is all there is", got, match)
	}
}

// TestShapeCache_SplitsDurationWhenUnmeasured: an entry the final tick could
// not measure still seeds durations from the run's own length, and one it
// measured partially hands the rest their share. Built on read, never stored,
// so the next merge sees measurements only.
func TestShapeCache_SplitsDurationWhenUnmeasured(t *testing.T) {
	c := newShapeCache(maxSeeds)
	c.put(testRepo, "99", "main", seedEntry{shape: threeStepShape(), weights: nil, duration: 300 * time.Second, runID: 41, success: true})
	got, _ := c.get(testRepo, "99", "main")
	if w, src := got.seedWeights(); !reflect.DeepEqual(w, map[string]float64{"Lint": 100, "Build": 100, "Test": 100}) || src != ci.WeightsSplit {
		t.Errorf("seedWeights = %v (%s), want an even split", w, src)
	}
	if got.weights != nil {
		t.Error("the split must not be stored as a measurement")
	}

	c.put(testRepo, "99", "main", seedEntry{shape: threeStepShape(), weights: map[string]float64{"Build": 200}, duration: 300 * time.Second, runID: 42, success: true})
	got, _ = c.get(testRepo, "99", "main")
	if w, src := got.seedWeights(); !reflect.DeepEqual(w, map[string]float64{"Lint": 100, "Build": 200, "Test": 100}) || src != ci.WeightsMeasuredSplit {
		t.Errorf("seedWeights = %v (%s), want the measured group kept and the rest split", w, src)
	}

	c.put(testRepo, "99", "main", seedEntry{shape: threeStepShape(), weights: nil, runID: 43, success: true})
	got, _ = c.get(testRepo, "99", "main")
	// Run 43 measured nothing, but under the same labels it keeps 42's Build.
	if w, _ := got.seedWeights(); w["Build"] != 200 {
		t.Errorf("seedWeights = %v, want the stored measurement carried over", w)
	}
}
