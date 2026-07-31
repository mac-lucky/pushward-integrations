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
// that group ran in the given (finished) run. A group's weight is the MAX
// member-job duration: matrix jobs run in parallel, so the longest one is the
// group's wall-clock contribution, and step_rows already conveys the fan-out
// count. Weights are in seconds; the client normalizes. Keyed by group name (not
// index) so ProjectWeights can re-attach them to the current run's labels even
// if the forge reveals the groups in a different order. Returns nil when no
// group has a measurable duration (the run never finished, or timestamps are
// missing), which is the signal that there is nothing to size pills by - not a
// signal to omit the wire field, which is never safe on a reused slug. See
// UniformWeights.
func GroupWeights(jobs []Job) map[string]float64 {
	weights := make(map[string]float64)
	measured := false

	for _, job := range jobs {
		base := BaseJobName(job.Name)
		if _, ok := weights[base]; !ok {
			weights[base] = StepWeightFloor
		}
		d := jobDuration(job)
		if d <= 0 {
			continue
		}
		measured = true
		if w := d.Seconds(); w > weights[base] {
			weights[base] = w
		}
	}

	if !measured {
		return nil
	}
	return weights
}

// ProjectWeights builds a per-step weight slice aligned to labels, looking each
// label up in the name-keyed historical weights. A label with no history (a job
// added since the prior run) gets the mean of the known weights, a neutral
// estimate. Each weight tracks its own label regardless of group order.
//
// The result is len(labels), which is NOT the same as total_steps: a caller that
// has a total but no labels yet (a seed built after the jobs endpoint failed)
// gets a zero-length slice here, and omitempty drops that from the JSON just as
// it drops nil. Size the wire field against total_steps and treat this as an
// estimate to accept only when it already fits - see cipoll's payloadWeights.
// Returns nil when there is no history at all.
func ProjectWeights(labels []string, byName map[string]float64) []float64 {
	if len(byName) == 0 {
		return nil
	}
	sum := 0.0
	for _, w := range byName {
		sum += w
	}
	mean := sum / float64(len(byName))
	if mean < StepWeightFloor {
		mean = StepWeightFloor
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

// jobDuration returns a finished job's wall-clock duration, or 0 when either
// timestamp is missing or the span is non-positive (clock skew).
func jobDuration(job Job) time.Duration {
	if job.StartedAt.IsZero() || job.CompletedAt.IsZero() {
		return 0
	}
	if d := job.CompletedAt.Sub(job.StartedAt); d > 0 {
		return d
	}
	return 0
}
