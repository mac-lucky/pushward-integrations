package ci

import (
	"math"
	"time"
)

// minLiveWindow is how much of the estimate must still be ahead of us for an
// anchor to be worth sending. iOS drops any window whose end has passed, and one
// about to pass would flash the animated bar for an instant before reverting to
// the static one, so a step already at (or past) its estimate keeps the static
// bar and the X/N counter instead.
const minLiveWindow = 5 * time.Second

// AnchorDecline says which gate stopped LiveAnchor from anchoring. It exists for
// logs: every cause produces an identical payload (no window), so without a
// reason the only way to tell them apart is to re-derive the conditions at the
// call site or to run the bridge against a live forge and guess.
type AnchorDecline string

const (
	// DeclineNoStepRunning means no group is in progress at all: a run whose jobs
	// have all finished, or one that has revealed none yet.
	DeclineNoStepRunning AnchorDecline = "no step running"
	// DeclineQueued means the run is sitting on QueuedStepName. It has a step index,
	// so it is not DeclineNoStepRunning, but nothing is executing under the
	// placeholder and no forge measured it - see QueuedStepName.
	DeclineQueued AnchorDecline = "queued, nothing running under the placeholder"
	// DeclineUnmeasured means the prior run measured NOTHING anywhere, so there is
	// not even a mean to estimate from. A group unmeasured on its own falls back to
	// meanWeight and does not reach here. The usual cause for a workflow that rarely
	// runs: BaselineJobs found no finished run of it on this branch.
	DeclineUnmeasured AnchorDecline = "no measured duration for this step group"
	// DeclineNoStart means the forge has not stamped the running group's start.
	DeclineNoStart AnchorDecline = "forge has not stamped a start"
	// DeclineEstimateSpent means the group is already at or past its estimate.
	// Expected for any group that finishes in less than one poll interval.
	DeclineEstimateSpent AnchorDecline = "estimate already spent"
)

// LiveAnchor returns the unix window iOS animates the current step's pill
// across, measured from when the step actually started rather than from now, so
// a poll landing mid-step picks the bar up where it already is instead of
// restarting it.
//
// ok is false when there is nothing to animate toward, and why names which gate
// stopped it. iOS renders the static bar in exactly those cases anyway, so a
// window sent regardless would buy nothing and cost a high-priority push.
//
// A group the prior run did not measure is NOT one of those cases: it animates
// toward meanWeight, the same neutral estimate ProjectWeights already draws its
// pill at. The two must agree - refusing only the ETA put a counter that would not
// guess above a pill already sized by that guess - and on a forge whose durations
// arrive incomplete, most groups take this path.
//
// Note what this means for a short group: an anchor is only worth sending while
// more than minLiveWindow of the estimate is still ahead, so a group that
// finishes inside one poll interval will usually never produce an anchored frame
// at all. That is the intended outcome, not a missing feature - and it has
// nothing to do with how many jobs the run has, which is the wrong conclusion to
// draw from a single-job workflow that never animates.
//
// maxWindow bounds a corrupt prior-run duration; callers pass their own
// max-run-lifetime. The caller's own live-progress config gate stays with the
// caller: this decides only whether there is something worth animating.
func LiveAnchor(info StepInfo, byName map[string]float64, now time.Time, maxWindow time.Duration) (start, end int64, ok bool, why AnchorDecline) {
	if info.CurrentStep < 1 {
		return 0, 0, false, DeclineNoStepRunning
	}
	// GroupWeights seeds every group it saw to StepWeightFloor, measured or not, so
	// a floor value carries no duration information: it means "draw a thin pill",
	// not "this took a second".
	secs, known := byName[info.CurrentStepName]
	if !known || secs <= StepWeightFloor {
		if info.CurrentStepName == QueuedStepName {
			return 0, 0, false, DeclineQueued
		}
		// A mean at or below the floor means every group came back seeded, or there
		// is no history at all - which averages to zero and fails the same check.
		mean, _ := meanWeight(byName)
		if mean <= StepWeightFloor {
			return 0, 0, false, DeclineUnmeasured
		}
		secs = mean
	}
	// A corrupt prior-run timestamp can yield a duration of years. Left alone it
	// would put end_date past the server's 5-year ceiling, and since the client
	// fails fast on 4xx, every later patch for this activity would 422 and the
	// card would freeze.
	secs = math.Min(secs, maxWindow.Seconds())
	// No start means nothing is running yet: the queued placeholder, or a job
	// the forge has not stamped. Anchoring from poll time would animate a step
	// that has not begun, and would sit permanently offset by up to one poll
	// interval once the real start appears, since the step index has not changed
	// and nothing re-anchors.
	startAt := info.CurrentStepStartedAt
	if startAt.IsZero() {
		return 0, 0, false, DeclineNoStart
	}
	if startAt.After(now) {
		// The runner's clock reads ahead of ours; anchoring forward would render
		// an empty bar until local time caught up.
		startAt = now
	}
	endAt := startAt.Add(time.Duration(math.Round(secs)) * time.Second)
	if !endAt.After(now.Add(minLiveWindow)) {
		return 0, 0, false, DeclineEstimateSpent
	}
	return startAt.Unix(), endAt.Unix(), true, ""
}
