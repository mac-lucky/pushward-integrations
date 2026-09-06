package ci

import (
	"slices"
	"time"
)

// StepWeightFloor is the minimum weight a measured step group receives, so a
// sub-second group still renders as a thin pill instead of vanishing. It is a
// render floor and nothing more: a group at the floor took up to one second. A
// group the run could not time has no entry at all; see GroupWeights.
const StepWeightFloor = 1.0

// shardSpan is one job's stamped interval; either side may be zero.
type shardSpan struct{ start, end time.Time }

// GroupWeights maps each step group's label to a pill weight, sized by how long
// that group ran in the given (finished) run. A group's weight is its BUSY time:
// the union of its shards' [start, end] intervals, so parallel matrix shards
// weigh about as long as the slowest one, shards queued behind each other on a
// busy runner weigh their sum, and a shard requeued or re-run after an idle gap
// adds its own length and not the gap. Either way it is time the group was
// working, and it is measured from the group's first start, where ComputeSteps
// anchors the live window, so the pill and the countdown agree. A gap in the
// prior run is not time the next run is expected to spend. Weights are in
// seconds; the client normalizes. Keyed by group name (not index) so
// ProjectWeights can re-attach them to the current run's labels even if the
// forge reveals the groups in a different order.
//
// Every entry is a measurement. A group the run could not time is absent, and
// the readers already handle that: ProjectWeights draws it at the mean of the
// timed groups and LiveAnchor counts it down to the same. Returns nil when no
// group could be timed (the run never finished, or timestamps are missing),
// which is the signal that there is nothing to size pills by - not a signal to
// omit the wire field, which is never safe on a reused slug. See
// UniformWeights.
func GroupWeights(jobs []Job) map[string]float64 {
	type group struct {
		first, last time.Time
		shards      []shardSpan
	}
	groups := make(map[string]*group)
	for _, job := range jobs {
		base := BaseJobName(job.Name)
		g, ok := groups[base]
		if !ok {
			g = &group{}
			groups[base] = g
		}
		g.first = earliest(g.first, job.StartedAt)
		g.last = latest(g.last, job.CompletedAt)
		if !job.StartedAt.IsZero() || !job.CompletedAt.IsZero() {
			g.shards = append(g.shards, shardSpan{job.StartedAt, job.CompletedAt})
		}
	}

	var weights map[string]float64
	for base, g := range groups {
		busy, ok := busyTime(g.shards, g.first, g.last)
		if !ok {
			continue
		}
		if weights == nil {
			weights = make(map[string]float64, len(groups))
		}
		weights[base] = max(StepWeightFloor, busy.Seconds())
	}
	return weights
}

// busyTime is how long at least one shard of the group was running: the union
// of the shards' intervals. A shard stamped on one side borrows the other from
// the group (a start-only shard runs to the group's last end, an end-only one
// from its first start), which still says when the group began or ended, and
// reduces to the group's outer span when no shard is fully stamped. That is
// also the limit of the union: a start-only shard ahead of a gap spans it, and
// the run clamp in FillWeights is the backstop.
//
// ok is false with nothing to sum: no start at all, no end at all, or only
// pairs whose completion precedes their start, which is clock skew rather than
// speed. A zero-length pair is a measurement of nothing, not a missing one: a
// forge that stamps a skipped job with both sides at once should keep that
// job's hairline rather than hand it the mean.
func busyTime(shards []shardSpan, first, last time.Time) (time.Duration, bool) {
	if first.IsZero() || last.IsZero() {
		return 0, false
	}
	ivs := make([]shardSpan, 0, len(shards))
	for _, s := range shards {
		if s.start.IsZero() {
			s.start = first
		}
		if s.end.IsZero() {
			s.end = last
		}
		if s.end.Before(s.start) {
			continue
		}
		ivs = append(ivs, s)
	}
	if len(ivs) == 0 {
		return 0, false
	}
	slices.SortFunc(ivs, func(a, b shardSpan) int { return a.start.Compare(b.start) })
	var total time.Duration
	cur := ivs[0]
	for _, s := range ivs[1:] {
		if !s.start.After(cur.end) {
			cur.end = latest(cur.end, s.end)
			continue
		}
		total += cur.end.Sub(cur.start)
		cur = s
	}
	return total + cur.end.Sub(cur.start), true
}

// earliest returns the earlier of cur and ts, treating a zero ts as "unknown"
// rather than as the epoch. It is the one definition of "when a group started"
// that ComputeSteps and GroupWeights share, which is what keeps the live window
// anchored where the measured busy time begins.
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
	WeightsMeasured      WeightsSource = "measured"
	WeightsMeasuredSplit WeightsSource = "measured + split"
	WeightsSplit         WeightsSource = "run-duration split"
	WeightsNone          WeightsSource = "none"
)

// BaselineWeights is what a prior run contributes to sizing and anchoring the
// next one: its groups' measured busy times, with the run's length spread
// evenly over the groups it could not time, or - when it timed none but its
// own length is known - over all of them, so the pills stay equal and each
// step counts down toward the run's average rather than not at all. See
// FillWeights for the rules.
func BaselineWeights(jobs []Job, labels []string, run time.Duration) (map[string]float64, WeightsSource) {
	return FillWeights(GroupWeights(jobs), labels, run)
}

// FillWeights completes a set of measured weights against the run they came
// from. Two things happen. No group is allowed to outweigh the run it was part
// of: a shard re-run hours later, or a task row a forge touched after the fact,
// would otherwise stretch a busy time across the gap. And a label the run could
// not time takes the run's even share (its length over the label count), not
// the mean of the timed groups: one recovered one-second job would otherwise
// drag every other pill to a hairline and hand the countdown a one-second
// estimate, leaving the card worse off than if nothing had been recovered at
// all. The share is run/N rather than what is left after the timed groups,
// because groups that ran in series consume the whole run and would leave
// nothing to share.
//
// A zero run neither clamps nor splits; with nothing measured either, the
// result is nil. measured is not mutated.
func FillWeights(measured map[string]float64, labels []string, run time.Duration) (map[string]float64, WeightsSource) {
	even := EvenWeights(labels, run)
	if measured == nil {
		if even == nil {
			return nil, WeightsNone
		}
		return even, WeightsSplit
	}
	weights := make(map[string]float64, len(measured)+len(even))
	limit := run.Seconds()
	for name, w := range measured {
		if limit > StepWeightFloor && w > limit {
			w = limit
		}
		weights[name] = w
	}
	source := WeightsMeasured
	for name, share := range even {
		if _, ok := weights[name]; !ok {
			weights[name] = share
			source = WeightsMeasuredSplit
		}
	}
	return weights, source
}

// EvenWeights spreads a run's wall-clock evenly over its step groups. Returns
// nil when there is nothing to spread, or when the share would not clear
// StepWeightFloor: every pill would be a hairline and every window already
// spent, so there is no estimate worth having.
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

// meanWeight is the neutral duration estimate for a group with no entry: the
// mean of every weight in byName, floored. Every entry is a measurement, a
// one-second group included, so this is the mean of what the run timed. ok is
// false with nothing to average at all, which is the one case with no estimate:
// ProjectWeights returns nil on it and LiveAnchor declines.
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
// label up in the name-keyed historical weights. A label with no entry (a job
// added since the prior run, or one that run could not time) gets meanWeight,
// a neutral estimate. Each weight tracks its own label regardless of group
// order.
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
