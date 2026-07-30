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

// LiveAnchor returns the unix window iOS animates the current step's pill
// across, measured from when the step actually started rather than from now, so
// a poll landing mid-step picks the bar up where it already is instead of
// restarting it.
//
// ok is false when there is nothing trustworthy to animate toward: no step
// running, no start stamped, no measurement for the group, or an estimate
// already spent. iOS renders the static bar in exactly those cases anyway, so a
// window sent regardless would buy nothing and cost a high-priority push.
//
// maxWindow bounds a corrupt prior-run duration; callers pass their own
// max-run-lifetime. The caller's own live-progress config gate stays with the
// caller: this decides only whether there is something worth animating.
func LiveAnchor(info StepInfo, byName map[string]float64, now time.Time, maxWindow time.Duration) (start, end int64, ok bool) {
	if info.CurrentStep < 1 {
		return 0, 0, false
	}
	// GroupWeights seeds every group it saw to StepWeightFloor, measured or not,
	// so a floor value carries no duration information: it means "draw a thin
	// pill", not "this took a second". Anything at or below it is unmeasured.
	secs, known := byName[info.CurrentStepName]
	if !known || secs <= StepWeightFloor {
		return 0, 0, false
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
		return 0, 0, false
	}
	if startAt.After(now) {
		// The runner's clock reads ahead of ours; anchoring forward would render
		// an empty bar until local time caught up.
		startAt = now
	}
	endAt := startAt.Add(time.Duration(math.Round(secs)) * time.Second)
	if !endAt.After(now.Add(minLiveWindow)) {
		return 0, 0, false
	}
	return startAt.Unix(), endAt.Unix(), true
}
