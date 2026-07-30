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
// missing) so callers omit step_weights and fall back to equal-width pills.
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
// estimate. The result is always len(labels), so step_weights never desyncs from
// total_steps, and each weight tracks its own label regardless of group order.
// Returns nil when there is no history, so callers omit step_weights and pills
// render equal-width.
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
