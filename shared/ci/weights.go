package ci

import "time"

// StepWeightFloor is the minimum weight any step group receives, so a step with
// a near-zero or unmeasurable duration still renders as a thin pill instead of
// vanishing, and clock skew (completed before started) can't yield a
// zero/negative weight.
//
// It doubles as the "unmeasured" sentinel: GroupWeights seeds every group it
// saw to the floor whether or not it could time it, so a value at or below the
// floor carries no duration information. LiveAnchor relies on that.
const StepWeightFloor = 1.0

// GroupWeights maps each step group's label to a pill weight, sized by how long
// that group ran in the given (finished) run. A group's weight is its wall-clock
// SPAN: from the earliest start among its jobs to the latest completion. Matrix
// shards that run in parallel span about as long as the slowest one; shards that
// queue behind each other on a busy runner span their sum. Either way it is the
// time the group held the run up, and it starts where ComputeSteps anchors the
// live window - the group's first start - so the pill and the countdown agree.
// A job stamped on one side only still contributes that side; only a group with
// neither is unmeasurable. Weights are in seconds; the client normalizes. Keyed
// by group name (not index) so ProjectWeights can re-attach them to the current
// run's labels even if the forge reveals the groups in a different order.
//
// Returns nil when no group has a measurable span (the run never finished, or
// timestamps are missing), which is the signal that there is nothing to size
// pills by - not a signal to omit the wire field, which is never safe on a
// reused slug. See UniformWeights.
func GroupWeights(jobs []Job) map[string]float64 {
	type bounds struct{ start, end time.Time }
	spans := make(map[string]bounds)
	for _, job := range jobs {
		base := BaseJobName(job.Name)
		b := spans[base]
		b.start = earliest(b.start, job.StartedAt)
		b.end = latest(b.end, job.CompletedAt)
		spans[base] = b
	}

	weights := make(map[string]float64, len(spans))
	measured := false
	for base, b := range spans {
		weights[base] = StepWeightFloor
		if b.start.IsZero() || b.end.IsZero() {
			continue
		}
		// A non-positive span is clock skew (completed before started), not a
		// fast group: leave it at the floor rather than inventing a duration.
		if d := b.end.Sub(b.start); d > 0 {
			measured = true
			weights[base] = max(StepWeightFloor, d.Seconds())
		}
	}
	if !measured {
		return nil
	}
	return weights
}

// earliest returns the earlier of cur and ts, treating a zero ts as "unknown"
// rather than as the epoch. It is the one definition of "when a group started"
// that ComputeSteps and GroupWeights share, which is what keeps the live window
// anchored where the measured span begins.
func earliest(cur, ts time.Time) time.Time {
	if !ts.IsZero() && (cur.IsZero() || ts.Before(cur)) {
		return ts
	}
	return cur
}

// latest is earliest's counterpart for completions.
func latest(cur, ts time.Time) time.Time {
	if !ts.IsZero() && ts.After(cur) {
		return ts
	}
	return cur
}

// WeightsSource names, for the log, where a run's weights came from.
type WeightsSource string

const (
	WeightsMeasured WeightsSource = "measured"
	WeightsSplit    WeightsSource = "run-duration split"
	WeightsNone     WeightsSource = "none"
)

// BaselineWeights is what a prior run contributes to sizing and anchoring the
// next one: its groups' measured spans, or - when no group could be measured but
// the run's own length is known - that length spread evenly over labels, so the
// pills stay equal and each step counts down toward the run's average rather
// than not at all. Either way no group is allowed to outweigh the run it was
// part of: a shard re-run hours later, or a task row a forge touched after the
// fact, would otherwise stretch a span across the gap. A zero run duration
// neither splits nor clamps.
func BaselineWeights(jobs []Job, labels []string, run time.Duration) (map[string]float64, WeightsSource) {
	source := WeightsMeasured
	weights := GroupWeights(jobs)
	if weights == nil {
		source = WeightsSplit
		weights = EvenWeights(labels, run)
	}
	if weights == nil {
		return nil, WeightsNone
	}
	if limit := run.Seconds(); limit > StepWeightFloor {
		for name, w := range weights {
			if w > limit {
				weights[name] = limit
			}
		}
	}
	return weights, source
}

// EvenWeights spreads a run's wall-clock evenly over its step groups. Returns
// nil when there is nothing to spread, or when the share would not clear
// StepWeightFloor, so the "nil means unmeasured" convention holds.
func EvenWeights(labels []string, total time.Duration) map[string]float64 {
	if len(labels) == 0 || total <= 0 {
		return nil
	}
	share := total.Seconds() / float64(len(labels))
	if share <= StepWeightFloor {
		return nil
	}
	out := make(map[string]float64, len(labels))
	for _, l := range labels {
		out[l] = share
	}
	return out
}

// meanWeight is the neutral duration estimate for a group the prior run did not
// measure: the mean of every weight in byName, floored. ok is false when there is
// no history to average at all, which lets ProjectWeights tell "no history" apart
// from "the mean happens to be small" - LiveAnchor does not need the distinction,
// since an absent history averages to zero and fails its floor check anyway.
//
// Floor entries are deliberately included in the average. GroupWeights seeds every
// group it saw to the floor, so excluding them would compute the mean of only the
// slow groups and hand an unmeasured group an estimate biased high; counting them
// keeps it the mean of the run.
func meanWeight(byName map[string]float64) (float64, bool) {
	if len(byName) == 0 {
		return 0, false
	}
	sum := 0.0
	for _, w := range byName {
		sum += w
	}
	mean := sum / float64(len(byName))
	if mean < StepWeightFloor {
		mean = StepWeightFloor
	}
	return mean, true
}

// ProjectWeights builds a per-step weight slice aligned to labels, looking each
// label up in the name-keyed historical weights. A label with no history (a job
// added since the prior run) gets meanWeight, a neutral estimate. Each weight
// tracks its own label regardless of group order.
//
// The result is len(labels), which is NOT the same as total_steps: a caller that
// has a total but no labels yet (a seed built after the jobs endpoint failed)
// gets a zero-length slice here, and omitempty drops that from the JSON just as
// it drops nil. Size the wire field against total_steps and treat this as an
// estimate to accept only when it already fits - see cipoll's payloadWeights.
// Returns nil when there is no history at all.
func ProjectWeights(labels []string, byName map[string]float64) []float64 {
	mean, ok := meanWeight(byName)
	if !ok {
		return nil
	}
	out := make([]float64, len(labels))
	for i, l := range labels {
		if w, ok := byName[l]; ok {
			out[i] = w
		} else {
			out[i] = mean
		}
	}
	return out
}

// UniformWeights returns n weights at the floor: the wire form of "no history".
// Unlike omitting step_weights it keeps the array the same length as
// total_steps, and that is the whole point. A poller reuses one slug across runs
// and the server merges content per RFC 7396, so an omitted array is not "no
// weights" - it is the PREVIOUS run's weights carried onto a new total. The
// server then rejects the whole payload for a length it was never sent, and
// keeps rejecting it until some run happens to have the old step count.
//
// The pills stay equal-width, since the client normalizes, but note this is not
// byte-for-byte the old rendering: a valid weights array of the right length is
// also what selects the client's segmented layout over its matrix one. A run
// with fan-out lands on the weighted matrix and keeps its stacked rows; a run
// without fan-out draws the segmented bar instead of the matrix. Equal widths
// either way, and it is the same layout a measured run of that workflow already
// gets, so the card stops changing shape depending on whether the last run
// happened to be measurable.
func UniformWeights(n int) []float64 {
	if n <= 0 {
		return nil
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = StepWeightFloor
	}
	return out
}
